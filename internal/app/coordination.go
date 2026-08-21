package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/internal/credentials"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/objectstore"
	"fxvcs.dev/fxvcs/internal/objectstore/filesystem"
	"fxvcs.dev/fxvcs/internal/objectstore/s3"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/release"
)

// Coord is an opened coordination repository: the stack, its releases,
// environments and targets, and the component mirrors underneath it.
//
// It deliberately is not a *Repo. A coordination repository holds no tracked
// assets, no hydration state, and no publication ledger; giving it the same
// type would invite operations that make no sense there.
type Coord struct {
	*gitops.Repo
	opts Options
}

// OpenCoord locates and opens the coordination repository containing start.
func OpenCoord(ctx context.Context, start string, opts Options) (*Coord, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	repo, err := gitops.Open(ctx, start, gitops.Options{Installed: opts.Installed, GitExecutable: opts.GitExecutable})
	if err != nil {
		return nil, err
	}
	return &Coord{Repo: repo, opts: opts}, nil
}

// Now returns the clock.
func (c *Coord) Now() func() time.Time { return c.opts.Now }

// ---------------------------------------------------------------------------
// Object availability
// ---------------------------------------------------------------------------

// coordObjectChecker verifies that a pinned component's objects already exist
// on every remote the component marks required for publication.
//
// Release creation cannot trust the pre-push barrier: hooks are a local
// convenience and any Git client can skip them. So this repeats the question
// from the coordination side, against the same committed remote descriptors,
// using this machine's local bindings and credentials. A remote this machine
// cannot reach is reported as unverifiable rather than silently treated as
// present.
type coordObjectChecker struct {
	local     *domain.LocalConfig
	configDir string
}

// ObjectChecker returns the release object checker for this coordination
// repository, or nil when checking is disabled.
func (c *Coord) ObjectChecker(ctx context.Context) (release.ObjectChecker, error) {
	paths := coordLocalPaths(c.GitDir)
	local, err := LoadLocalConfig(paths)
	if err != nil {
		return nil, err
	}
	_ = ctx
	return &coordObjectChecker{local: local, configDir: c.opts.ConfigDir}, nil
}

// coordLocalPaths is the coordination repository's own local configuration
// file. It binds storage remotes by name for machines that create releases.
func coordLocalPaths(gitDir string) string {
	return filepath.Join(gitDir, "fxvcs", "config.yaml")
}

func (k *coordObjectChecker) CheckComponent(ctx context.Context, cfg *domain.Repository, ptrs map[string]pointer.Pointer) (release.ObjectReport, error) {
	rep := release.ObjectReport{Assets: len(ptrs)}
	if len(ptrs) == 0 {
		return rep, nil
	}
	for _, remote := range cfg.Spec.Storage.Remotes {
		if !remote.RequiredForPublication {
			continue
		}
		rep.Remotes = append(rep.Remotes, remote.Name)
		store, err := k.store(cfg.Metadata.StorageDomainID, remote)
		if err != nil {
			rep.Unverifiable = append(rep.Unverifiable, fmt.Sprintf("storage %q cannot be reached from this computer, so the assets of %s could not be verified: %v",
				remote.Name, cfg.Metadata.Name, err))
			continue
		}
		missing, err := k.missingOn(ctx, store, cfg.Metadata.StorageDomainID, ptrs)
		if err != nil {
			rep.Unverifiable = append(rep.Unverifiable, fmt.Sprintf("storage %q could not be queried, so the assets of %s could not be verified: %v",
				remote.Name, cfg.Metadata.Name, err))
			continue
		}
		for _, path := range missing {
			rep.Missing = append(rep.Missing, fmt.Sprintf("%s is not in storage %q yet; run `fxvcs objects publish` in %s and push again",
				path, remote.Name, cfg.Metadata.Name))
		}
	}
	return rep, nil
}

// missingOn returns the asset paths this store cannot serve.
//
// A whole-blob asset is one object: its own bytes. A chunked asset is a
// manifest plus every chunk it names, and all of them have to be there — an
// agent that finds 99 of 100 chunks cannot deploy the file, so "the manifest
// is present" is not the question worth asking.
func (k *coordObjectChecker) missingOn(ctx context.Context, store objectstore.Store, domainID string, ptrs map[string]pointer.Pointer) ([]string, error) {
	// First wave: the object that identifies each asset.
	roots := make([]string, 0, len(ptrs))
	byRoot := map[string][]string{}
	for path, p := range ptrs {
		d := rootObject(p)
		if _, seen := byRoot[d]; !seen {
			roots = append(roots, d)
		}
		byRoot[d] = append(byRoot[d], path)
	}
	sort.Strings(roots)
	present, err := objectstore.HasMany(ctx, store, domainID, roots)
	if err != nil {
		return nil, err
	}
	missing := map[string]bool{}
	for _, d := range roots {
		if !present[d] {
			for _, p := range byRoot[d] {
				missing[p] = true
			}
		}
	}

	// Second wave: the chunks of every chunked asset whose manifest is there.
	chunks := map[string][]string{} // chunk digest -> asset paths needing it
	var order []string
	for path, p := range ptrs {
		if p.Encoding == pointer.EncodingWhole || missing[path] {
			continue
		}
		man, err := readManifestFrom(ctx, store, domainID, p)
		if err != nil {
			missing[path] = true
			continue
		}
		for _, c := range man.Chunks {
			if _, seen := chunks[c.Digest]; !seen {
				order = append(order, c.Digest)
			}
			chunks[c.Digest] = append(chunks[c.Digest], path)
		}
	}
	if len(order) > 0 {
		sort.Strings(order)
		have, err := objectstore.HasMany(ctx, store, domainID, order)
		if err != nil {
			return nil, err
		}
		for _, d := range order {
			if !have[d] {
				for _, p := range chunks[d] {
					missing[p] = true
				}
			}
		}
	}

	out := make([]string, 0, len(missing))
	for p := range missing {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// rootObject is the object that must exist for an asset to be resolvable at
// all: the file itself for whole/1, the manifest for a chunked encoding.
func rootObject(p pointer.Pointer) string {
	if p.Encoding == pointer.EncodingWhole {
		return p.OID
	}
	return p.ManifestDigest
}

// readManifestFrom reads and validates one object manifest straight from a
// store. It never trusts the bytes: the manifest is accepted only when it
// canonicalizes back to the digest the pointer names.
func readManifestFrom(ctx context.Context, store objectstore.Store, domainID string, p pointer.Pointer) (domain.ObjectManifest, error) {
	rc, err := store.Get(ctx, domainID, p.ManifestDigest)
	if err != nil {
		return domain.ObjectManifest{}, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes+1))
	if err != nil {
		return domain.ObjectManifest{}, err
	}
	if len(data) > maxManifestBytes {
		return domain.ObjectManifest{}, fmt.Errorf("%w: manifest %s exceeds %d bytes", ErrIntegrity, p.ManifestDigest, maxManifestBytes)
	}
	return decodeManifest(data, p)
}

// maxManifestBytes bounds an untrusted manifest read. A manifest of a 1 TiB
// asset at the smallest chunk size is far below this.
const maxManifestBytes = 64 << 20

// coordCredentials resolves the identity for an s3 remote from the same
// sources a component checkout uses: remote-scoped environment variables, then
// unscoped ones, then the credential store, then the AWS default chain.
func coordCredentials(domainID, remote string, local *domain.LocalRemote, configDir string) (s3.Credentials, error) {
	if c, ok := envCredentials(credentialEnvPrefix + envToken(remote) + "_"); ok {
		return c, nil
	}
	if c, ok := envCredentials(credentialEnvPrefix); ok {
		return c, nil
	}
	profile := credentials.ProfileFor(domainID, remote)
	if local != nil && local.CredentialProfile != "" {
		profile = local.CredentialProfile
	}
	store, err := credentials.Open(configDir)
	if err != nil {
		return s3.Credentials{}, err
	}
	entry, err := store.Get(profile)
	switch {
	case err == nil:
		return s3.Credentials{AccessKeyID: entry.AccessKeyID, SecretAccessKey: entry.SecretAccessKey, SessionToken: entry.SessionToken}, nil
	case errors.Is(err, credentials.ErrNotFound):
		return s3.Credentials{}, nil
	default:
		return s3.Credentials{}, err
	}
}

func (k *coordObjectChecker) store(domainID string, remote domain.StorageRemote) (objectstore.Store, error) {
	var local *domain.LocalRemote
	for i := range k.local.Spec.Remotes {
		if k.local.Spec.Remotes[i].Name == remote.Name {
			local = &k.local.Spec.Remotes[i]
			break
		}
	}
	switch remote.Type {
	case "filesystem":
		if local == nil || local.Path == "" {
			return nil, fmt.Errorf("%w: no directory bound for %q here (bind one with: fxvcs storage add --name %s --type filesystem --path <dir>)",
				ErrRemoteUnbound, remote.Name, remote.Name)
		}
		return filesystem.New(local.Path)
	case "s3":
		cfg := s3.Config{Endpoint: remote.Endpoint, Bucket: remote.Bucket, Prefix: remote.Prefix,
			Region: remote.Region, Addressing: remote.Addressing, Compress: true}
		if local != nil {
			if local.Endpoint != "" {
				cfg.Endpoint = local.Endpoint
			}
			if local.Bucket != "" {
				cfg.Bucket = local.Bucket
			}
			if local.Prefix != "" {
				cfg.Prefix = local.Prefix
			}
			if local.Region != "" {
				cfg.Region = local.Region
			}
			if local.Addressing != "" {
				cfg.Addressing = local.Addressing
			}
		}
		if strings.TrimSpace(cfg.Bucket) == "" {
			return nil, fmt.Errorf("%w: s3 storage %q has no bucket", ErrInvalidArgument, remote.Name)
		}
		creds, err := coordCredentials(domainID, remote.Name, local, k.configDir)
		if err != nil {
			return nil, err
		}
		cfg.Credentials = creds
		return s3.New(cfg)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedType, remote.Type)
	}
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// StorageBind records how this machine reaches one storage remote from the
// coordination repository.
//
// A coordination repository owns no storage of its own — the remote
// descriptors belong to the component repositories. But release creation has
// to verify that every asset a component pins is already in storage, and for a
// filesystem remote that means knowing the directory on *this* machine.
// `fxvcs storage add` therefore falls back to this when it is run outside a
// component checkout: same command, same flags, purely local effect.
func (c *Coord) StorageBind(in StorageAddInput) (RemoteView, error) {
	if !remoteNameRE.MatchString(in.Name) {
		return RemoteView{}, fmt.Errorf("%w: storage name %q", ErrInvalidArgument, in.Name)
	}
	entry := domain.LocalRemote{Name: in.Name, Type: in.Type}
	switch in.Type {
	case "filesystem":
		if in.Path == "" || !filepath.IsAbs(in.Path) {
			return RemoteView{}, fmt.Errorf("%w: a filesystem storage needs an absolute --path", ErrInvalidArgument)
		}
		if _, err := filesystem.New(filepath.Clean(in.Path)); err != nil {
			return RemoteView{}, err
		}
		entry.Path = filepath.Clean(in.Path)
	case "s3":
		entry.Bucket, entry.Prefix = in.Bucket, in.Prefix
		entry.Endpoint, entry.Region, entry.Addressing = in.Endpoint, in.Region, in.Addressing
	default:
		return RemoteView{}, fmt.Errorf("%w: %q (supported: filesystem, s3)", ErrUnsupportedType, in.Type)
	}
	path := coordLocalPaths(c.GitDir)
	lc, err := LoadLocalConfig(path)
	if err != nil {
		return RemoteView{}, err
	}
	replaced := false
	for i, l := range lc.Spec.Remotes {
		if l.Name == in.Name {
			lc.Spec.Remotes[i] = entry
			replaced = true
		}
	}
	if !replaced {
		lc.Spec.Remotes = append(lc.Spec.Remotes, entry)
	}
	sort.Slice(lc.Spec.Remotes, func(i, j int) bool { return lc.Spec.Remotes[i].Name < lc.Spec.Remotes[j].Name })
	if err := SaveLocalConfig(path, lc); err != nil {
		return RemoteView{}, err
	}
	return RemoteView{Name: entry.Name, Type: entry.Type, Local: true, Bound: true, Path: entry.Path,
		Endpoint: entry.Endpoint, Bucket: entry.Bucket, Prefix: entry.Prefix,
		Region: entry.Region, Addressing: entry.Addressing}, nil
}

// NewStack builds an empty stack document.
func NewStack(stackID, name, profile, minimumVersion string) *domain.Stack {
	return &domain.Stack{
		Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindStack},
		Metadata: domain.StackMetadata{StackID: stackID, Name: name},
		Spec: domain.StackSpec{
			Profile:             profile,
			MinimumFXVCSVersion: minimumVersion,
			Components:          []domain.StackComponent{},
		},
	}
}
