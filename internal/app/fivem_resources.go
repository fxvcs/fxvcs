package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/profile/fivem/catalog"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
	"fxvcs.dev/fxvcs/internal/repository"
	"fxvcs.dev/fxvcs/internal/schema"
)

// FiveM component resource catalog operations: `resources sync`, `resources
// set`, and the release-style `resources validate`. They edit only
// .fxvcs/resources.yaml in the working tree, never commit, and serialize with
// each other through LockPath on that file.

// Errors.
var (
	// ErrProfileRequired: the repository profile is not fivem.
	ErrProfileRequired = errors.New("app: operation requires spec.profile fivem")
	// ErrDescriptorMissing: the repository has no accepted component descriptor.
	ErrDescriptorMissing = errors.New("app: no accepted component descriptor (sourceRoot/layout) in .fxvcs/repository.yaml")
	// ErrCatalogInvalid: .fxvcs/resources.yaml does not validate against its schema.
	ErrCatalogInvalid = errors.New("app: resource catalog is invalid")
	// ErrResourceNotFound: `resources set` named a path that is not in the catalog.
	ErrResourceNotFound = errors.New("app: resource path not in catalog")
	// ErrResourceScan: discovery or sandboxed manifest evaluation failed.
	ErrResourceScan = errors.New("app: resource scan failed")
	// ErrCatalogNotReleasable: the catalog failed release-style validation.
	ErrCatalogNotReleasable = errors.New("app: resource catalog is not releasable")
)

// ResourceScanError is the typed failure of discovery/manifest evaluation
// over the worktree source root. Manifests lists the offending manifest
// files (tree-relative) when the failure is attributable to one.
type ResourceScanError struct {
	Manifests []string
	Err       error
}

func (e *ResourceScanError) Error() string {
	if len(e.Manifests) > 0 {
		return fmt.Sprintf("app: resource scan failed at %s: %v", strings.Join(e.Manifests, ", "), e.Err)
	}
	return "app: resource scan failed: " + e.Err.Error()
}

func (e *ResourceScanError) Unwrap() error { return e.Err }

func (e *ResourceScanError) Is(target error) bool { return target == ErrResourceScan }

// ResourceNotFoundError names the unknown path and the known catalog paths.
type ResourceNotFoundError struct {
	Path  string
	Known []string
}

func (e *ResourceNotFoundError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("app: resource path %q not in catalog (catalog is empty; run: fxvcs resources sync)", e.Path)
	}
	return fmt.Sprintf("app: resource path %q not in catalog; known paths: %s", e.Path, strings.Join(e.Known, ", "))
}

func (e *ResourceNotFoundError) Is(target error) bool { return target == ErrResourceNotFound }

// CatalogInvalidError carries schema issues of .fxvcs/resources.yaml.
type CatalogInvalidError struct {
	File   string
	Issues schema.Issues
}

func (e *CatalogInvalidError) Error() string {
	return fmt.Sprintf("app: resource catalog %s is invalid: %v", e.File, e.Issues)
}

func (e *CatalogInvalidError) Unwrap() error { return e.Issues }

func (e *CatalogInvalidError) Is(target error) bool { return target == ErrCatalogInvalid }

// FiveMDescriptor returns the accepted component descriptor from the
// committed configuration, or a typed error pointing at `fxvcs init --profile
// fivem` when the repository is not a FiveM component.
func (r *Repo) FiveMDescriptor() (domain.ComponentDescriptor, error) {
	if r.Config.Spec.Profile != ProfileFiveM {
		return domain.ComponentDescriptor{}, fmt.Errorf("%w (spec.profile is %q; run: fxvcs init --profile fivem)", ErrProfileRequired, r.Config.Spec.Profile)
	}
	d := domain.ComponentDescriptor{SourceRoot: r.Config.Spec.SourceRoot, Layout: r.Config.Spec.Layout, ResourceName: r.Config.Spec.ResourceName}
	if d.Layout == "" {
		return domain.ComponentDescriptor{}, fmt.Errorf("%w; run: fxvcs init --profile fivem", ErrDescriptorMissing)
	}
	if d.SourceRoot == "" {
		d.SourceRoot = "."
	}
	return d, nil
}

// CatalogPath is the absolute path of .fxvcs/resources.yaml.
func (r *Repo) CatalogPath() string {
	return filepath.Join(r.Root, filepath.FromSlash(repository.ResourcesPath))
}

// LoadResourceCatalog reads and strictly validates the committed catalog. A
// missing file yields an empty catalog carrying this repository's ID and
// exists=false. raw holds the file bytes as read (nil when missing).
func (r *Repo) LoadResourceCatalog() (cat *domain.ResourceCatalog, raw []byte, exists bool, err error) {
	raw, err = os.ReadFile(r.CatalogPath()) //nolint:gosec // fixed committed path under the checkout root
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyCatalog(r.Config.Metadata.RepositoryID), nil, false, nil
		}
		return nil, nil, false, err
	}
	cat, err = schema.LoadResourceCatalog(raw)
	if err != nil {
		var iss schema.Issues
		if errors.As(err, &iss) {
			return nil, raw, true, &CatalogInvalidError{File: repository.ResourcesPath, Issues: iss}
		}
		return nil, raw, true, err
	}
	return cat, raw, true, nil
}

func emptyCatalog(repoID string) *domain.ResourceCatalog {
	return &domain.ResourceCatalog{
		Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindResourceCatalog},
		Metadata: domain.ResourceCatalogMetadata{RepositoryID: repoID},
		Spec:     domain.ResourceCatalogSpec{Resources: []domain.CatalogResource{}},
	}
}

// EncodeResourceCatalog renders the catalog in its canonical committed form:
// yaml.v3 over the domain struct (field order fixed by the struct, two-space
// indent, empty lists as `[]`, no document markers). Two catalogs that are
// equal as values encode to identical bytes.
func EncodeResourceCatalog(cat *domain.ResourceCatalog) ([]byte, error) {
	c := *cat
	c.APIVersion, c.Kind = format.CurrentAPIVersion, format.KindResourceCatalog
	c.Spec.Resources = make([]domain.CatalogResource, len(cat.Spec.Resources))
	for i, res := range cat.Spec.Resources {
		c.Spec.Resources[i] = normalizeCatalogResource(res)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&c); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func normalizeCatalogResource(res domain.CatalogResource) domain.CatalogResource {
	nonNil := func(l []string) []string {
		if l == nil {
			return []string{}
		}
		return l
	}
	res.After = nonNil(res.After)
	res.Before = nonNil(res.Before)
	res.Provides = nonNil(res.Provides)
	res.Dependencies = nonNil(res.Dependencies)
	res.Games = nonNil(res.Games)
	res.PersistentPaths = nonNil(res.PersistentPaths)
	if res.MutableMode == "" {
		res.MutableMode = catalog.MutableImmutable
	}
	if res.Status == "" {
		res.Status = catalog.StatusPresent
	}
	return res
}

// writeResourceCatalog validates the encoded catalog against the schema and
// then replaces the file atomically (temp file + rename in the same
// directory). It returns the bytes written and whether the file changed; an
// on-disk file that already holds exactly these bytes is left untouched.
func (r *Repo) writeResourceCatalog(cat *domain.ResourceCatalog, prev []byte) (data []byte, changed bool, err error) {
	data, err = EncodeResourceCatalog(cat)
	if err != nil {
		return nil, false, err
	}
	if iss := schema.Validate(format.KindResourceCatalog, data); len(iss) > 0 {
		return nil, false, &CatalogInvalidError{File: repository.ResourcesPath, Issues: iss}
	}
	if prev != nil && bytes.Equal(prev, data) {
		return data, false, nil
	}
	path := r.CatalogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".resources.yaml.*.tmp")
	if err != nil {
		return nil, false, err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, false, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return nil, false, err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil { //nolint:gosec // committed, world-readable file
		cleanup()
		return nil, false, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return nil, false, err
	}
	return data, true, nil
}

// ScanResources runs discovery and sandboxed manifest evaluation over the
// worktree using the accepted descriptor. Failures are typed
// *ResourceScanError; a manifest failure names the manifest.
func (r *Repo) ScanResources(ctx context.Context, desc domain.ComponentDescriptor) ([]discovery.DiscoveredResource, error) {
	res, err := r.ScanResourcesTree(ctx, desc)
	if err != nil {
		return nil, err
	}
	return res.Resources, nil
}

// ScanResourcesTree is ScanResources returning the manifests FXServer would
// walk past as well, so callers can report them.
func (r *Repo) ScanResourcesTree(ctx context.Context, desc domain.ComponentDescriptor) (discovery.ScanResult, error) {
	abs, err := filepath.Abs(r.Root)
	if err != nil {
		return discovery.ScanResult{}, &ResourceScanError{Err: err}
	}
	res, err := discovery.ScanTree(ctx, os.DirFS(abs), desc, discovery.ScanOptions{})
	if err != nil {
		se := &ResourceScanError{Err: err}
		var de *discovery.Error
		if errors.As(err, &de) && de.Reason == discovery.ReasonManifest && de.Path != "" {
			se.Manifests = []string{de.Path}
		}
		return discovery.ScanResult{}, se
	}
	return res, nil
}

// SkipWarnings renders skipped manifests as operator-facing warning lines.
func SkipWarnings(skipped []discovery.Skipped) []string {
	out := make([]string, 0, len(skipped))
	for _, sk := range skipped {
		out = append(out, fmt.Sprintf("ignored %s/%s: %s", sk.Path, sk.File, sk.Detail))
	}
	return out
}

// ---------------------------------------------------------------------------
// resources sync
// ---------------------------------------------------------------------------

// ResourcesSyncInput configures ResourcesSync.
type ResourcesSyncInput struct {
	// NewActivation applies to newly discovered resources: pending (default,
	// also when empty), enabled, or disabled. Ignored when FromCfg is set.
	NewActivation domain.Activation
	// FromCfg is a server.cfg whose ensure/start directives (followed through
	// exec) supply activation for resources without a decision: new entries
	// and entries still pending become enabled when the cfg starts their
	// name; every other new entry stays pending. Existing enabled/disabled
	// entries are never changed.
	FromCfg string
}

// ResourcesSyncOutput reports what the sync did.
type ResourcesSyncOutput struct {
	CatalogPath string   // repository-relative
	Added       []string // new entries
	Updated     []string // refreshed manifest-derived fields or revived tombstones
	Missing     []string // status missing (never deleted)
	Pending     []string // every entry left with activation pending
	Enabled     []string // entries whose activation was set by --from-cfg
	Unchanged   int
	Written     bool     // the file changed on disk
	Warnings    []string // skipped manifests and cfg import warnings
	Report      catalog.Report
	Catalog     *domain.ResourceCatalog
}

// ResourcesSync scans the source root, merges the discovered resources into
// .fxvcs/resources.yaml (preserving operator-owned fields, tombstoning
// missing entries), and writes the catalog atomically. It never commits. It
// is idempotent: a second run reports no changes and leaves the file
// byte-identical.
func (r *Repo) ResourcesSync(ctx context.Context, in ResourcesSyncInput) (*ResourcesSyncOutput, error) {
	desc, err := r.FiveMDescriptor()
	if err != nil {
		return nil, err
	}
	newActivation := in.NewActivation
	if newActivation == "" {
		newActivation = domain.ActivationPending
	}
	switch newActivation {
	case domain.ActivationEnabled, domain.ActivationDisabled, domain.ActivationPending:
	default:
		return nil, fmt.Errorf("%w: --new must be pending, enabled, or disabled (got %q)", ErrInvalidArgument, newActivation)
	}
	var cfg *CfgImport
	if in.FromCfg != "" {
		imported, err := ParseServerCfg(in.FromCfg)
		if err != nil {
			return nil, err
		}
		cfg = &imported
		// cfg import supplies activation for undecided entries only; new
		// entries it does not start remain pending, never disabled.
		newActivation = domain.ActivationPending
	}

	h, err := r.Lock(ctx, journal.LockPath, repository.ResourcesPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = h.Release() }()

	existing, raw, _, err := r.LoadResourceCatalog()
	if err != nil {
		return nil, err
	}
	scan, err := r.ScanResourcesTree(ctx, desc)
	if err != nil {
		return nil, err
	}
	discovered := scan.Resources
	merged, rep, err := catalog.Sync(existing, discovered, newActivation)
	if err != nil {
		return nil, err
	}
	if merged.Metadata.RepositoryID == "" {
		merged.Metadata.RepositoryID = r.Config.Metadata.RepositoryID
	}
	out := &ResourcesSyncOutput{
		CatalogPath: repository.ResourcesPath,
		Added:       rep.Added,
		Updated:     append(append([]string{}, rep.Refreshed...), rep.Revived...),
		Missing:     rep.Missing,
		Pending:     []string{},
		Enabled:     []string{},
		Unchanged:   len(rep.Unchanged),
		Warnings:    SkipWarnings(scan.Skipped),
		Report:      rep,
	}
	sort.Strings(out.Updated)
	if cfg != nil {
		out.Warnings = append(out.Warnings, cfg.Warnings...)
		for i := range merged.Spec.Resources {
			res := &merged.Spec.Resources[i]
			if res.Activation == domain.ActivationPending && res.Status == catalog.StatusPresent && cfg.Has(res.Name) {
				res.Activation = domain.ActivationEnabled
				out.Enabled = append(out.Enabled, res.Path)
			}
		}
	}
	for _, res := range merged.Spec.Resources {
		if res.Activation == domain.ActivationPending {
			out.Pending = append(out.Pending, res.Path)
		}
	}
	_, written, err := r.writeResourceCatalog(merged, raw)
	if err != nil {
		return nil, err
	}
	out.Written = written
	out.Catalog = merged
	return out, nil
}

// ---------------------------------------------------------------------------
// resources set
// ---------------------------------------------------------------------------

// ResourcesSetInput edits one catalog entry identified by its literal path.
// nil After/Before leave ordering unchanged; a non-nil empty list clears it.
type ResourcesSetInput struct {
	ResourcePath string
	Activation   domain.Activation // "" = unchanged
	After        []string
	Before       []string
}

// ResourcesSetOutput is the entry after the edit.
type ResourcesSetOutput struct {
	Resource domain.CatalogResource
	// Warnings names after/before references that no local resource (name or
	// provide) satisfies. They are accepted: a component may order against
	// resources of other components or the runtime; the stack validates
	// names globally.
	Warnings []string
	Written  bool
}

// ResourcesSet updates activation and ordering of one entry and writes the
// catalog atomically. It never commits.
func (r *Repo) ResourcesSet(ctx context.Context, in ResourcesSetInput) (*ResourcesSetOutput, error) {
	if _, err := r.FiveMDescriptor(); err != nil {
		return nil, err
	}
	if in.ResourcePath == "" {
		return nil, fmt.Errorf("%w: resource path is required", ErrInvalidArgument)
	}
	want, err := pathsafe.Normalize(strings.TrimSuffix(in.ResourcePath, "/"))
	if err != nil {
		return nil, fmt.Errorf("%w: resource path %q: %v", ErrInvalidArgument, in.ResourcePath, err)
	}
	switch in.Activation {
	case "", domain.ActivationEnabled, domain.ActivationDisabled, domain.ActivationPending:
	default:
		return nil, fmt.Errorf("%w: activation must be enabled, disabled, or pending (got %q)", ErrInvalidArgument, in.Activation)
	}
	for _, field := range []struct {
		name string
		list []string
	}{{"after", in.After}, {"before", in.Before}} {
		seen := map[string]bool{}
		for _, n := range field.list {
			if err := discovery.ValidateResourceName(n); err != nil {
				return nil, fmt.Errorf("%w: --%s: %v", ErrInvalidArgument, field.name, err)
			}
			if seen[n] {
				return nil, fmt.Errorf("%w: --%s: duplicate name %q", ErrInvalidArgument, field.name, n)
			}
			seen[n] = true
		}
	}
	if in.Activation == "" && in.After == nil && in.Before == nil {
		return nil, fmt.Errorf("%w: nothing to set (pass --activation, --after, or --before)", ErrInvalidArgument)
	}

	h, err := r.Lock(ctx, journal.LockPath, repository.ResourcesPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = h.Release() }()

	cat, raw, _, err := r.LoadResourceCatalog()
	if err != nil {
		return nil, err
	}
	idx := -1
	known := make([]string, 0, len(cat.Spec.Resources))
	local := map[string]bool{}
	for i, res := range cat.Spec.Resources {
		known = append(known, res.Path)
		local[res.Name] = true
		for _, p := range res.Provides {
			local[p] = true
		}
		if res.Path == want {
			idx = i
		}
	}
	if idx < 0 {
		return nil, &ResourceNotFoundError{Path: in.ResourcePath, Known: known}
	}
	res := &cat.Spec.Resources[idx]
	if in.Activation != "" {
		res.Activation = in.Activation
	}
	if in.After != nil {
		res.After = append([]string{}, in.After...)
	}
	if in.Before != nil {
		res.Before = append([]string{}, in.Before...)
	}
	out := &ResourcesSetOutput{Warnings: []string{}}
	for _, field := range []struct {
		name string
		list []string
	}{{"after", res.After}, {"before", res.Before}} {
		for _, n := range field.list {
			if n == res.Name {
				return nil, fmt.Errorf("%w: --%s: %q references itself", ErrInvalidArgument, field.name, n)
			}
			if !local[n] {
				out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %q is not a resource of this component; it must exist in the composition (validated by the stack)", field.name, n))
			}
		}
	}
	_, written, err := r.writeResourceCatalog(cat, raw)
	if err != nil {
		return nil, err
	}
	out.Resource = normalizeCatalogResource(*res)
	out.Written = written
	return out, nil
}

// ---------------------------------------------------------------------------
// resources validate (release-style)
// ---------------------------------------------------------------------------

// ResourcesValidateInput tunes ResourcesValidate.
type ResourcesValidateInput struct {
	// KnownNames are resource names supplied by other components, the runtime
	// catalog, or externals. after/before may reference them without a
	// warning.
	KnownNames []string
}

// ResourcesValidateOutput lists every finding. Valid is true when Problems
// is empty. Warnings are advisory (ordering references that only the
// composition can resolve).
type ResourcesValidateOutput struct {
	CatalogPath string
	Valid       bool
	Resources   int
	Problems    []catalog.Problem
	Warnings    []string
	Diff        catalog.DiffReport
}

// ResourcesValidate re-scans the worktree source root, diffs the committed
// catalog against the rescan (undeclared, missing, stale entries), runs
// release validation (pending/missing entries, names, ordering cycles,
// persistent paths, mutableMode) and schema validation of the YAML. It is
// what release creation runs against each pinned tree. Scan failures
// (including sandbox violations) are returned as a typed error; catalog
// findings are returned in Problems together with ErrCatalogNotReleasable.
func (r *Repo) ResourcesValidate(ctx context.Context, in ResourcesValidateInput) (*ResourcesValidateOutput, error) {
	desc, err := r.FiveMDescriptor()
	if err != nil {
		return nil, err
	}
	out := &ResourcesValidateOutput{CatalogPath: repository.ResourcesPath, Problems: []catalog.Problem{}, Warnings: []string{}, Diff: catalog.DiffReport{Undeclared: []string{}, Missing: []string{}, Stale: []catalog.StaleEntry{}}}
	add := func(path, field, msg string) {
		out.Problems = append(out.Problems, catalog.Problem{Path: path, Field: field, Message: msg})
	}
	scan, err := r.ScanResourcesTree(ctx, desc)
	if err != nil {
		return nil, err
	}
	discovered := scan.Resources
	out.Warnings = append(out.Warnings, SkipWarnings(scan.Skipped)...)
	cat, _, exists, err := r.LoadResourceCatalog()
	if err != nil {
		var ci *CatalogInvalidError
		if !errors.As(err, &ci) {
			return nil, err
		}
		for _, is := range ci.Issues {
			add("", "schema"+is.Path, is.Message)
		}
		out.Valid = false
		return out, fmt.Errorf("%w: %d problem(s)", ErrCatalogNotReleasable, len(out.Problems))
	}
	if !exists {
		add("", "catalog", repository.ResourcesPath+" does not exist; run: fxvcs resources sync")
	}
	if cat.Metadata.RepositoryID != r.Config.Metadata.RepositoryID {
		add("", "metadata.repositoryID", fmt.Sprintf("catalog repositoryID %q does not match repository %q", cat.Metadata.RepositoryID, r.Config.Metadata.RepositoryID))
	}
	out.Resources = len(cat.Spec.Resources)
	out.Diff = catalog.Diff(cat, discovered)
	for _, p := range out.Diff.Undeclared {
		add(p, "undeclared", "resource directory is not in the catalog; run: fxvcs resources sync")
	}
	for _, p := range out.Diff.Missing {
		add(p, "missing", "catalog entry has no resource directory")
	}
	for _, s := range out.Diff.Stale {
		add(s.Path, s.Field, fmt.Sprintf("stale: catalog %s, manifest %s", s.Catalog, s.Manifest))
	}
	// Ordering references outside this component are warnings here (the
	// stack validates names globally); everything else ValidateForRelease
	// reports is a problem.
	local := map[string]bool{}
	for _, res := range cat.Spec.Resources {
		local[res.Name] = true
		for _, p := range res.Provides {
			local[p] = true
		}
	}
	known := append([]string{}, in.KnownNames...)
	knownSet := map[string]bool{}
	for _, n := range known {
		knownSet[n] = true
	}
	for _, res := range cat.Spec.Resources {
		for _, field := range []struct {
			name string
			list []string
		}{{"after", res.After}, {"before", res.Before}} {
			for _, n := range field.list {
				if n == res.Name || local[n] || knownSet[n] {
					continue
				}
				if discovery.ValidateResourceName(n) != nil {
					continue // reported by schema/release validation
				}
				out.Warnings = append(out.Warnings, fmt.Sprintf("%s %s: %q is not a resource of this component; the stack must supply it", res.Path, field.name, n))
				knownSet[n] = true
				known = append(known, n)
			}
		}
	}
	if verr := catalog.ValidateForRelease(cat, catalog.ValidateOptions{KnownNames: known}); verr != nil {
		var ve *catalog.ValidationError
		if errors.As(verr, &ve) {
			out.Problems = append(out.Problems, ve.Problems...)
		} else {
			return nil, verr
		}
	}
	// Missing tombstones are reported once (by Diff) — drop the duplicate
	// status finding from release validation for the same path.
	out.Problems = dedupeProblems(out.Problems, out.Diff.Missing)
	sort.SliceStable(out.Problems, func(i, j int) bool {
		if out.Problems[i].Path != out.Problems[j].Path {
			return out.Problems[i].Path < out.Problems[j].Path
		}
		return out.Problems[i].Field < out.Problems[j].Field
	})
	out.Valid = len(out.Problems) == 0
	if !out.Valid {
		return out, fmt.Errorf("%w: %d problem(s)", ErrCatalogNotReleasable, len(out.Problems))
	}
	return out, nil
}

func dedupeProblems(in []catalog.Problem, missing []string) []catalog.Problem {
	miss := map[string]bool{}
	for _, m := range missing {
		miss[m] = true
	}
	out := in[:0]
	for _, p := range in {
		if p.Field == "status" && miss[p.Path] {
			continue
		}
		out = append(out, p)
	}
	return out
}
