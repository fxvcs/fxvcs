package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/durablefs"
	"fxvcs.dev/fxvcs/internal/fileprotect"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/materialize"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/profile/fivem"
	"fxvcs.dev/fxvcs/internal/release"
	"fxvcs.dev/fxvcs/internal/runtimecheck"
	"fxvcs.dev/fxvcs/internal/templatehook"
)

const assemblyMetadataVersion = 2

type AssemblyFile struct {
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Bytes     int64  `json:"bytes"`
	Mode      string `json:"mode"`
	Placement string `json:"placement"` // hardlink | executable-copy | mutable-copy
}

type AssemblyPersistentPath struct {
	RepositoryID string `json:"repositoryID"`
	ResourcePath string `json:"resourcePath"`
	Declared     string `json:"declared"`
	Path         string `json:"path"` // realization-relative symlink path
	Kind         string `json:"kind"`
}

type AssemblyMetadata struct {
	FormatVersion   int                      `json:"formatVersion"`
	RealizationID   string                   `json:"realizationID"`
	Inputs          domain.RealizationInputs `json:"inputs"`
	Files           []AssemblyFile           `json:"files"`
	PersistentPaths []AssemblyPersistentPath `json:"persistentPaths,omitempty"`
	MutableBytes    int64                    `json:"mutableBytes"`
}

type AssemblyResult struct {
	StagingPath         string
	Metadata            AssemblyMetadata
	MetadataPath        string
	VerificationDigests map[string]string // ephemeral byte digests, including secret-bearing generated files
}

type AssemblyHookOutput struct {
	ID             string
	Destination    string
	Mode           string
	Rendered       []byte
	ContainsSecret bool
}

type AssemblyOptions struct {
	Hooks                    []AssemblyHookOutput
	HookDigests              map[string]string
	SecretVersions           map[string]string
	RuntimeEvidence          *runtimecheck.Evidence
	ExternalVerifierEvidence map[string]string
	ResourceStateToken       []byte
}

type PublishedAssembly struct {
	Path    string
	Existed bool
}

// PublishReleaseStaging atomically moves a complete staging tree into its
// immutable realization name without replacing any existing directory. An
// existing realization is accepted only when its canonical metadata is byte
// identical to the expected assembly.
func PublishReleaseStaging(result AssemblyResult, deployRoot string) (PublishedAssembly, error) {
	wantStagingRoot := filepath.Join(deployRoot, layout.StateDir, "staging")
	stagingParent := filepath.Dir(filepath.Clean(result.StagingPath))
	if stagingParent != wantStagingRoot {
		return PublishedAssembly{}, fmt.Errorf("reconcile: staging path %s is outside %s", result.StagingPath, wantStagingRoot)
	}
	stagingInfo, err := os.Lstat(result.StagingPath)
	if err != nil {
		return PublishedAssembly{}, err
	}
	if !stagingInfo.IsDir() || stagingInfo.Mode()&os.ModeSymlink != 0 {
		return PublishedAssembly{}, errors.New("reconcile: staging path is not an ordinary directory")
	}
	wantMetadataPath := filepath.Join(result.StagingPath, layout.RuntimeDir, "realization.json")
	if filepath.Clean(result.MetadataPath) != wantMetadataPath {
		return PublishedAssembly{}, fmt.Errorf("reconcile: metadata path %s is outside the staging runtime", result.MetadataPath)
	}
	expectedMetadata, err := canonical.Marshal(result.Metadata)
	if err != nil {
		return PublishedAssembly{}, err
	}
	if err := verifyAssemblyMetadata(result.MetadataPath, expectedMetadata); err != nil {
		return PublishedAssembly{}, err
	}
	final, err := layout.Realization(deployRoot, result.Metadata.RealizationID)
	if err != nil {
		return PublishedAssembly{}, err
	}
	releases := layout.Releases(deployRoot)
	if err := os.MkdirAll(releases, 0o755); err != nil {
		return PublishedAssembly{}, err
	}
	if finalInfo, err := os.Lstat(final); err == nil {
		if !finalInfo.IsDir() || finalInfo.Mode()&os.ModeSymlink != 0 {
			return PublishedAssembly{}, fmt.Errorf("reconcile: existing realization %s is not an ordinary directory", final)
		}
		if err := verifyAssemblyMetadata(filepath.Join(final, layout.RuntimeDir, "realization.json"), expectedMetadata); err != nil {
			return PublishedAssembly{}, fmt.Errorf("reconcile: existing realization %s disagrees with expected metadata: %w", result.Metadata.RealizationID, err)
		}
		if err := verifyAssemblyTree(final, result.Metadata, materialize.Store{DeployRoot: deployRoot}, result.VerificationDigests); err != nil {
			return PublishedAssembly{}, fmt.Errorf("reconcile: existing realization %s failed verification: %w", result.Metadata.RealizationID, err)
		}
		if err := os.RemoveAll(result.StagingPath); err != nil {
			return PublishedAssembly{}, err
		}
		return PublishedAssembly{Path: final, Existed: true}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return PublishedAssembly{}, err
	}
	if err := renameDirectoryNoReplace(result.StagingPath, final); err != nil {
		// Resolve a publication race only by proving the winner is byte-exact.
		if verifyErr := verifyAssemblyMetadata(filepath.Join(final, layout.RuntimeDir, "realization.json"), expectedMetadata); verifyErr == nil {
			if treeErr := verifyAssemblyTree(final, result.Metadata, materialize.Store{DeployRoot: deployRoot}, result.VerificationDigests); treeErr == nil {
				_ = os.RemoveAll(result.StagingPath)
				return PublishedAssembly{Path: final, Existed: true}, nil
			}
		}
		return PublishedAssembly{}, fmt.Errorf("reconcile: publish realization without replacement: %w", err)
	}
	if err := syncDirectory(releases); err != nil {
		return PublishedAssembly{}, err
	}
	if err := syncDirectory(wantStagingRoot); err != nil {
		return PublishedAssembly{}, err
	}
	if err := verifyAssemblyTree(final, result.Metadata, materialize.Store{DeployRoot: deployRoot}, result.VerificationDigests); err != nil {
		return PublishedAssembly{}, fmt.Errorf("reconcile: published realization failed verification: %w", err)
	}
	return PublishedAssembly{Path: final}, nil
}

func verifyAssemblyMetadata(path string, expected []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("reconcile: %s is not an ordinary metadata file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, expected) {
		return errors.New("reconcile: realization metadata differs")
	}
	return nil
}

func verifyAssemblyTree(root string, metadata AssemblyMetadata, store materialize.Store, verificationDigests map[string]string) error {
	expected := make(map[string]AssemblyFile, len(metadata.Files)+1)
	for _, file := range metadata.Files {
		if _, err := pathsafe.Normalize(file.Path); err != nil {
			return err
		}
		expected[file.Path] = file
	}
	expected[path.Join(layout.RuntimeDir, "realization.json")] = AssemblyFile{Path: path.Join(layout.RuntimeDir, "realization.json"), Placement: "metadata"}
	persistent := make(map[string]AssemblyPersistentPath, len(metadata.PersistentPaths))
	for _, binding := range metadata.PersistentPaths {
		if _, err := pathsafe.Normalize(binding.Path); err != nil {
			return err
		}
		persistent[binding.Path] = binding
	}
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filePath == root || entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		want, ok := expected[relative]
		persistentBinding, isPersistent := persistent[relative]
		if !ok && !isPersistent {
			return fmt.Errorf("unexpected realization entry %s", relative)
		}
		seen[relative] = true
		if isPersistent {
			info, err := os.Lstat(filePath)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("%s is not the expected persistent symlink", relative)
			}
			linkTarget, err := os.Readlink(filePath)
			if err != nil || filepath.IsAbs(linkTarget) {
				return fmt.Errorf("%s has an invalid persistent symlink target", relative)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(filePath), linkTarget))
			expectedTarget, err := layout.PersistentPath(store.DeployRoot, persistentBinding.RepositoryID, persistentBinding.ResourcePath, persistentBinding.Declared)
			if err != nil || resolved != filepath.Clean(expectedTarget) {
				return fmt.Errorf("%s resolves to %s, want target-scoped persistent path %s", relative, resolved, expectedTarget)
			}
			return nil
		}
		if want.Placement == "metadata" {
			return nil
		}
		info, err := os.Lstat(filePath)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not an ordinary file", relative)
		}
		if info.Size() != want.Bytes {
			return fmt.Errorf("%s has %d bytes, want %d", relative, info.Size(), want.Bytes)
		}
		if want.Placement == "hardlink" {
			if err := store.Verify(want.Digest, want.Bytes); err != nil {
				return err
			}
			materializedPath, _ := store.Path(want.Digest)
			materializedInfo, err := os.Stat(materializedPath)
			if err != nil || !os.SameFile(info, materializedInfo) {
				return fmt.Errorf("%s is not linked to verified materialized content", relative)
			}
			return nil
		}
		file, err := os.Open(filePath) //nolint:gosec // G122: staging is agent-owned and non-writable; Lstat above rejects non-regular entries
		if err != nil {
			return err
		}
		h := sha256.New()
		_, copyErr := io.Copy(h, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		got := "sha256:" + hex.EncodeToString(h.Sum(nil))
		expectedDigest := want.Digest
		if want.Placement == "generated-secret" {
			expectedDigest = verificationDigests[relative]
			if expectedDigest == "" {
				return fmt.Errorf("%s requires ephemeral secret-bearing verification evidence", relative)
			}
		}
		if got != expectedDigest {
			return fmt.Errorf("%s hashes to %s, want %s", relative, got, expectedDigest)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for expectedPath := range expected {
		if !seen[expectedPath] {
			return fmt.Errorf("realization entry %s is missing", expectedPath)
		}
	}
	for expectedPath := range persistent {
		if !seen[expectedPath] {
			return fmt.Errorf("persistent realization entry %s is missing", expectedPath)
		}
	}
	return nil
}

// AssembleReleaseStaging builds an unselectable, same-filesystem realization
// staging tree. Publication and current activation are separate journaled
// operations; a successful return does not make this tree live.
func (c *Coord) AssembleReleaseStaging(ctx context.Context, rel domain.Release, inputs domain.RealizationInputs, realizationID, deployRoot string) (AssemblyResult, error) {
	return c.AssembleReleaseStagingWithOptions(ctx, rel, inputs, realizationID, deployRoot, AssemblyOptions{})
}

// AssembleReleaseStagingWithOptions adds already-resolved deterministic hook
// outputs to an unselectable realization. Secret-bearing byte digests remain
// ephemeral and are never persisted in realization metadata.
func (c *Coord) AssembleReleaseStagingWithOptions(ctx context.Context, rel domain.Release, inputs domain.RealizationInputs, realizationID, deployRoot string, options AssemblyOptions) (AssemblyResult, error) {
	wantID, err := integrity.RealizationID(inputs)
	if err != nil {
		return AssemblyResult{}, err
	}
	if wantID != realizationID {
		return AssemblyResult{}, fmt.Errorf("reconcile: realization ID %s does not match inputs %s", realizationID, wantID)
	}
	releaseDigest, err := canonical.Digest(rel)
	if err != nil {
		return AssemblyResult{}, err
	}
	if inputs.ReleaseName != rel.Metadata.Name || inputs.ReleaseDigest != releaseDigest || inputs.SourceSetDigest != rel.Spec.SourceSetDigest {
		return AssemblyResult{}, errors.New("reconcile: realization inputs do not match the pinned Release")
	}
	stagingRoot := filepath.Join(deployRoot, layout.StateDir, "staging")
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return AssemblyResult{}, err
	}
	staging, err := os.MkdirTemp(stagingRoot, realizationID+"-")
	if err != nil {
		return AssemblyResult{}, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(staging)
		}
	}()
	for _, dir := range []string{layout.ResourcesDir, layout.RuntimeDir, filepath.Join(layout.RuntimeDir, layout.RuntimeConfig)} {
		if err := os.MkdirAll(filepath.Join(staging, dir), 0o755); err != nil {
			return AssemblyResult{}, err
		}
	}

	if len(inputs.SecretVersions) != 0 || len(options.SecretVersions) != 0 {
		wantSecretVersions, err := canonical.Marshal(inputs.SecretVersions)
		if err != nil {
			return AssemblyResult{}, err
		}
		gotSecretVersions, err := canonical.Marshal(options.SecretVersions)
		if err != nil {
			return AssemblyResult{}, err
		}
		if !bytes.Equal(wantSecretVersions, gotSecretVersions) {
			return AssemblyResult{}, errors.New("reconcile: resolved secret versions do not match realization inputs")
		}
	}
	if len(inputs.HookDigests) != 0 || len(options.HookDigests) != 0 {
		wantHookDigests, err := canonical.Marshal(inputs.HookDigests)
		if err != nil {
			return AssemblyResult{}, err
		}
		gotHookDigests, err := canonical.Marshal(options.HookDigests)
		if err != nil {
			return AssemblyResult{}, err
		}
		if !bytes.Equal(wantHookDigests, gotHookDigests) {
			return AssemblyResult{}, errors.New("reconcile: resolved hook digests do not match realization inputs")
		}
	}
	var requiredRuntime []string
	for _, resource := range rel.Spec.RuntimeResources {
		if resource.Source == domain.ResourceSourceRuntime && resource.Activation == domain.ActivationEnabled {
			requiredRuntime = append(requiredRuntime, resource.ID)
		}
	}
	sort.Strings(requiredRuntime)
	if len(requiredRuntime) > 0 {
		if options.RuntimeEvidence == nil || options.RuntimeEvidence.SystemResourcesDigest == "" {
			return AssemblyResult{}, errors.New("reconcile: verified runtime-resource evidence is required")
		}
		verified := append([]string(nil), options.RuntimeEvidence.Verified...)
		sort.Strings(verified)
		if strings.Join(requiredRuntime, "\x00") != strings.Join(verified, "\x00") {
			return AssemblyResult{}, errors.New("reconcile: runtime-resource evidence does not match the Release requirements")
		}
	}
	requiredExternal := map[string]bool{}
	for _, resource := range rel.Spec.RuntimeResources {
		if resource.Source == domain.ResourceSourceExternal && resource.Activation == domain.ActivationEnabled {
			requiredExternal[resource.ID] = true
		}
	}
	if len(options.ExternalVerifierEvidence) != len(requiredExternal) {
		return AssemblyResult{}, errors.New("reconcile: external verifier evidence does not exactly cover enabled external resources")
	}
	for id := range requiredExternal {
		if !integrity.IsDigest(options.ExternalVerifierEvidence[id]) {
			return AssemblyResult{}, fmt.Errorf("reconcile: external resource %s lacks canonical verifier evidence", id)
		}
	}
	metadata := AssemblyMetadata{FormatVersion: assemblyMetadataVersion, RealizationID: realizationID, Inputs: inputs, Files: []AssemblyFile{}}
	verificationDigests := map[string]string{}
	store := materialize.Store{DeployRoot: deployRoot}
	components := append([]domain.ReleaseComponent(nil), rel.Spec.Components...)
	sort.Slice(components, func(i, j int) bool { return components[i].RepositoryID < components[j].RepositoryID })
	for _, component := range components {
		tree, err := c.Tree(ctx, component.RepositoryID, component.Commit)
		if err != nil {
			return AssemblyResult{}, err
		}
		root, _, err := release.AssetRoot(ctx, tree, component.StorageDomainID)
		if err != nil {
			return AssemblyResult{}, err
		}
		if root != component.AssetRoot {
			return AssemblyResult{}, fmt.Errorf("reconcile: component %s asset root changed during assembly", component.RepositoryID)
		}
		entries, err := tree.Entries(ctx)
		if err != nil {
			return AssemblyResult{}, err
		}
		pointers, err := release.Pointers(ctx, tree, component.StorageDomainID)
		if err != nil {
			return AssemblyResult{}, err
		}
		catalog, err := tree.ResourceCatalog(ctx)
		if err != nil {
			return AssemblyResult{}, err
		}
		catalogByPath := map[string]domain.CatalogResource{}
		for _, resource := range catalog.Spec.Resources {
			catalogByPath[resource.Path] = resource
		}
		resources := componentReleaseResources(rel, component.RepositoryID)
		for _, resource := range resources {
			catalogResource, exists := catalogByPath[resource.Path]
			if !exists || catalogResource.Status == "missing" {
				return AssemblyResult{}, fmt.Errorf("reconcile: pinned catalog has no present resource %s:%s", component.RepositoryID, resource.Path)
			}
			sourcePrefix, destinationBase := resourceRoots(component, resource)
			for _, persistentPath := range catalogResource.PersistentPaths {
				for _, entry := range entries {
					relative, included := relativeUnder(sourcePrefix, entry.Path)
					if included && (relative == persistentPath || strings.HasPrefix(relative, persistentPath+"/")) {
						return AssemblyResult{}, fmt.Errorf("reconcile: persistent path %s:%s:%s contains source-controlled seed data; v1 requires empty runtime-created persistent storage", component.RepositoryID, resource.Path, persistentPath)
					}
				}
			}
			for _, entry := range entries {
				relative, included := relativeUnder(sourcePrefix, entry.Path)
				if !included {
					continue
				}
				if !entry.IsRegular() {
					return AssemblyResult{}, fmt.Errorf("reconcile: unsupported Git mode %s at %s", entry.Mode, entry.Path)
				}
				destination := path.Join(destinationBase, relative)
				digest, size, err := ensureEntryMaterialized(ctx, tree, entry, pointers[entry.Path], store)
				if err != nil {
					return AssemblyResult{}, err
				}
				placement := "hardlink"
				if catalogResource.MutableMode == "copy" {
					placement = "mutable-copy"
					metadata.MutableBytes += size
				} else if entry.Mode == "100755" {
					placement = "executable-copy"
				}
				if placement == "hardlink" {
					if err := store.PlaceImmutable(digest, size, staging, destination); err != nil {
						return AssemblyResult{}, err
					}
				} else if err := copyMaterialized(store, digest, size, staging, destination, entry.Mode, placement == "mutable-copy"); err != nil {
					return AssemblyResult{}, err
				}
				metadata.Files = append(metadata.Files, AssemblyFile{Path: destination, Digest: digest, Bytes: size, Mode: entry.Mode, Placement: placement})
			}
			for _, persistentPath := range catalogResource.PersistentPaths {
				kind := materialize.PersistentKind(catalogResource.PersistentPathKinds[persistentPath])
				destination := path.Join(destinationBase, persistentPath)
				publishedDir, _ := layout.Realization(deployRoot, realizationID)
				if err := store.BindPersistentForPublication(staging, publishedDir, component.RepositoryID, resource.Path, persistentPath, destination, kind); err != nil {
					return AssemblyResult{}, fmt.Errorf("reconcile: bind persistent path %s:%s:%s: %w", component.RepositoryID, resource.Path, persistentPath, err)
				}
				metadata.PersistentPaths = append(metadata.PersistentPaths, AssemblyPersistentPath{RepositoryID: component.RepositoryID, ResourcePath: resource.Path, Declared: persistentPath, Path: destination, Kind: string(kind)})
			}
		}
	}
	for _, resource := range rel.Spec.Resources {
		if resource.Source == domain.ResourceSourceRuntime || resource.Source == domain.ResourceSourceExternal {
			continue
		}
		if resource.Source != domain.ResourceSourceComponent {
			return AssemblyResult{}, fmt.Errorf("reconcile: assembly of %s resource %s is not implemented", resource.Source, resource.Name)
		}
	}
	fragmentPaths := make([]string, 0, len(options.Hooks)+1)
	for _, hook := range options.Hooks {
		if hook.ID == "" {
			return AssemblyResult{}, errors.New("reconcile: hook output has no ID")
		}
		if strings.HasSuffix(strings.ToLower(hook.Destination), ".cfg") {
			if err := fivem.ValidateConfigFragment(hook.Rendered); err != nil {
				return AssemblyResult{}, fmt.Errorf("reconcile: hook %s generated invalid FiveM cfg: %w", hook.ID, err)
			}
			fragmentPaths = append(fragmentPaths, hook.Destination)
		}
		written, err := templatehook.Write(templatehook.WriteOptions{
			RealizationDir: staging, AllowedDestinationRoot: layout.RuntimeDir + "/" + layout.RuntimeConfig,
			Destination: hook.Destination, Mode: hook.Mode, ContainsSecret: hook.ContainsSecret,
		}, hook.Rendered)
		if err != nil {
			return AssemblyResult{}, fmt.Errorf("reconcile: write hook %s: %w", hook.ID, err)
		}
		info, err := os.Stat(written)
		if err != nil {
			return AssemblyResult{}, err
		}
		digest := sha256.Sum256(hook.Rendered)
		digestString := "sha256:" + hex.EncodeToString(digest[:])
		placement := "generated"
		metadataDigest := digestString
		if hook.ContainsSecret {
			placement = "generated-secret"
			metadataDigest = ""
			verificationDigests[hook.Destination] = digestString
		}
		metadata.Files = append(metadata.Files, AssemblyFile{Path: hook.Destination, Digest: metadataDigest, Bytes: info.Size(), Mode: hook.Mode, Placement: placement})
	}
	includeResourceState := len(options.ResourceStateToken) != 0
	if includeResourceState {
		config, err := fivem.ResourceStateConfig(options.ResourceStateToken)
		if err != nil {
			return AssemblyResult{}, err
		}
		configPath := fivem.HealthResourceConfigPath
		configDigest := sha256.Sum256(config)
		secretConfigPath := filepath.Join(staging, filepath.FromSlash(configPath))
		if err := writeDurableFile(secretConfigPath, config, 0o400); err != nil {
			return AssemblyResult{}, err
		}
		if err := fileprotect.Protect(secretConfigPath); err != nil {
			return AssemblyResult{}, err
		}
		metadata.Files = append(metadata.Files, AssemblyFile{Path: configPath, Bytes: int64(len(config)), Mode: "100400", Placement: "generated-secret"})
		verificationDigests[configPath] = "sha256:" + hex.EncodeToString(configDigest[:])
		fragmentPaths = append(fragmentPaths, configPath)
		files, err := fivem.ResourceStateFiles(rel.Spec.Resources)
		if err != nil {
			return AssemblyResult{}, err
		}
		for _, generated := range files {
			digest := sha256.Sum256(generated.Data)
			generatedPath := filepath.Join(staging, filepath.FromSlash(generated.Path))
			if err := os.MkdirAll(filepath.Dir(generatedPath), 0o755); err != nil {
				return AssemblyResult{}, err
			}
			if err := writeDurableFile(generatedPath, generated.Data, 0o444); err != nil {
				return AssemblyResult{}, err
			}
			metadata.Files = append(metadata.Files, AssemblyFile{Path: generated.Path, Digest: "sha256:" + hex.EncodeToString(digest[:]), Bytes: int64(len(generated.Data)), Mode: "100644", Placement: "generated"})
		}
	}
	runtimeConfig, err := fivem.RuntimeConfigWithHealth(rel.Spec.Resources, fragmentPaths, includeResourceState)
	if err != nil {
		return AssemblyResult{}, err
	}
	runtimeConfigPath := path.Join(layout.RuntimeDir, "fxvcs.cfg")
	runtimeConfigDigest := sha256.Sum256(runtimeConfig)
	if err := writeDurableFile(filepath.Join(staging, filepath.FromSlash(runtimeConfigPath)), runtimeConfig, 0o444); err != nil {
		return AssemblyResult{}, err
	}
	metadata.Files = append(metadata.Files, AssemblyFile{
		Path: runtimeConfigPath, Digest: "sha256:" + hex.EncodeToString(runtimeConfigDigest[:]), Bytes: int64(len(runtimeConfig)), Mode: "100644", Placement: "generated",
	})
	verificationDigests[runtimeConfigPath] = "sha256:" + hex.EncodeToString(runtimeConfigDigest[:])
	sort.Slice(metadata.Files, func(i, j int) bool { return metadata.Files[i].Path < metadata.Files[j].Path })
	sort.Slice(metadata.PersistentPaths, func(i, j int) bool { return metadata.PersistentPaths[i].Path < metadata.PersistentPaths[j].Path })
	metadataBytes, err := canonical.Marshal(metadata)
	if err != nil {
		return AssemblyResult{}, err
	}
	metadataPath := filepath.Join(staging, layout.RuntimeDir, "realization.json")
	if err := writeDurableFile(metadataPath, metadataBytes, 0o444); err != nil {
		return AssemblyResult{}, err
	}
	if err := syncAssemblyDirs(staging); err != nil {
		return AssemblyResult{}, err
	}
	ok = true
	return AssemblyResult{StagingPath: staging, Metadata: metadata, MetadataPath: metadataPath, VerificationDigests: verificationDigests}, nil
}

func componentReleaseResources(rel domain.Release, repositoryID string) []domain.ReleaseResource {
	var out []domain.ReleaseResource
	for _, resource := range rel.Spec.Resources {
		if resource.Source == "component" && resource.RepositoryID == repositoryID {
			out = append(out, resource)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EnsureOrder < out[j].EnsureOrder })
	return out
}

func resourceRoots(component domain.ReleaseComponent, resource domain.ReleaseResource) (string, string) {
	if component.Descriptor.Layout == domain.LayoutSingle {
		root := component.Descriptor.SourceRoot
		if root == "." {
			root = ""
		}
		return root, component.Mount
	}
	root := resource.Path
	if component.Descriptor.SourceRoot != "." {
		root = path.Join(component.Descriptor.SourceRoot, resource.Path)
	}
	return root, path.Join(component.Mount, resource.Path)
}

func relativeUnder(prefix, entry string) (string, bool) {
	if prefix == "" {
		if entry == ".fxvcs" || strings.HasPrefix(entry, ".fxvcs/") {
			return "", false
		}
		return entry, true
	}
	if entry == prefix {
		return "", false
	}
	if !strings.HasPrefix(entry, prefix+"/") {
		return "", false
	}
	return strings.TrimPrefix(entry, prefix+"/"), true
}

func ensureEntryMaterialized(ctx context.Context, tree *gitops.Tree, entry gitops.TreeEntry, ptr pointer.Pointer, store materialize.Store) (string, int64, error) {
	if ptr.OID != "" {
		if err := store.Verify(ptr.OID, ptr.Size); err != nil {
			return "", 0, fmt.Errorf("reconcile: pointer content %s is not materialized: %w", ptr.OID, err)
		}
		return ptr.OID, ptr.Size, nil
	}
	h := sha256.New()
	if err := tree.CopyBlob(ctx, entry.OID, h); err != nil {
		return "", 0, err
	}
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	reader, writer := io.Pipe()
	go func() {
		err := tree.CopyBlob(ctx, entry.OID, writer)
		_ = writer.CloseWithError(err)
	}()
	result, err := store.Put(ctx, digest, entry.Size, reader)
	_ = reader.Close()
	if err != nil {
		return "", 0, err
	}
	return digest, result.Bytes, nil
}

func copyMaterialized(store materialize.Store, digest string, size int64, realizationDir, relativePath, gitMode string, writable bool) error {
	if err := store.Verify(digest, size); err != nil {
		return err
	}
	source, _ := store.Path(digest)
	destination, err := pathsafe.Join(realizationDir, relativePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	mode := os.FileMode(0o444)
	if writable {
		mode = 0o644
	}
	if gitMode == "100755" {
		mode |= 0o111
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if n, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("reconcile: copy %s: %w", relativePath, err)
	} else if n != size {
		return fmt.Errorf("reconcile: copy %s: wrote %d of %d bytes", relativePath, n, size)
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return syncDirectory(filepath.Dir(destination))
}

func writeDurableFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok = true
	return syncDirectory(filepath.Dir(path))
}

func syncAssemblyDirs(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], string(filepath.Separator)) > strings.Count(dirs[j], string(filepath.Separator))
	})
	for _, dir := range dirs {
		if err := syncDirectory(dir); err != nil {
			return err
		}
	}
	return syncDirectory(filepath.Dir(root))
}

func syncDirectory(path string) error {
	return durablefs.SyncDir(path)
}
