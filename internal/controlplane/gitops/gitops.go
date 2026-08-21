// Package gitops implements the coordination control plane over an ordinary
// Git repository (ADR 0004).
//
// Layout, from docs/spec/schemas.md and product scope §8:
//
//	fxvcs.yaml               Stack: composition, components, mounts
//	runtime-resources.yaml   RuntimeResourceCatalog: runtime:/external: entries
//	resource-overrides.yaml  ResourceOverrides: sparse activation/order overrides
//	releases/<name>.yaml     Release (immutable once committed)
//	environments/<name>.yaml Environment
//	targets/<name>.yaml      Target
//
// Every write lands in the working tree as reviewable YAML. Nothing here runs
// `git commit`, `git push`, or `git tag`: making a change authoritative is a
// human/CI review step, which is the whole point of a GitOps control plane.
package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"fxvcs.dev/fxvcs/internal/controlplane"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/gitclient"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/schema"
)

// File names and directories of the coordination layout.
const (
	StackFile            = "fxvcs.yaml"
	RuntimeResourcesFile = "runtime-resources.yaml"
	OverridesFile        = "resource-overrides.yaml"
	ReleasesDir          = "releases"
	EnvironmentsDir      = "environments"
	TargetsDir           = "targets"
)

// ErrNotStack is returned when no coordination repository is found.
var ErrNotStack = errors.New("gitops: not a coordination repository")

// nameRE constrains record names to what is safe as a single path segment on
// every supported platform and unambiguous in Git refs and cfg directives.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// ValidateName checks a release, environment, or target name.
func ValidateName(kind, name string) error {
	if !nameRE.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("%w: %s name %q must be 1-63 characters of letters, digits, '.', '-', or '_' and start with a letter or digit",
			controlplane.ErrInvalid, kind, name)
	}
	return nil
}

// Repo is an opened coordination repository.
type Repo struct {
	// Root is the working-tree top level holding fxvcs.yaml.
	Root string
	// GitDir is the coordination repository's $GIT_DIR; component mirrors and
	// other derived state live under GitDir/fxvcs.
	GitDir string
	// Stack is the parsed fxvcs.yaml.
	Stack *domain.Stack
	// Git runs against Root.
	Git *gitclient.Client

	installed format.Version
}

// Options configures Open.
type Options struct {
	Installed     format.Version
	GitExecutable string
}

// Locate walks up from start to the directory holding fxvcs.yaml.
func Locate(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	dir := abs
	for {
		if st, err := os.Stat(filepath.Join(dir, StackFile)); err == nil && !st.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%w: no %s in %s or any parent directory", ErrNotStack, StackFile, abs)
		}
		dir = parent
	}
}

// Open locates and loads the coordination repository containing start.
func Open(ctx context.Context, start string, opts Options) (*Repo, error) {
	root, err := Locate(start)
	if err != nil {
		return nil, err
	}
	return OpenAt(ctx, root, opts)
}

// LoadStack reads and validates fxvcs.yaml at the given repository root
// without opening Git or enforcing the installed-version compatibility gate.
func LoadStack(root string) (*domain.Stack, error) {
	return readStack(filepath.Join(root, StackFile))
}

// OpenAt loads the coordination repository rooted exactly at root.
func OpenAt(ctx context.Context, root string, opts Options) (*Repo, error) {
	ws, err := LoadStack(root)
	if err != nil {
		return nil, err
	}
	if err := format.CheckMinimumVersion(opts.Installed, ws.Spec.MinimumFXVCSVersion, root); err != nil {
		return nil, err
	}
	git := gitclient.New(root)
	if opts.GitExecutable != "" {
		git.Executable = opts.GitExecutable
	}
	gitDir, err := git.GitDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not inside a Git repository: %v", ErrNotStack, root, err)
	}
	return &Repo{Root: root, GitDir: gitDir, Stack: ws, Git: git, installed: opts.Installed}, nil
}

// Init creates or adopts a coordination repository at root. The directory must
// already be a Git working tree (`stack init` refuses to guess where a new
// repository belongs); an existing fxvcs.yaml is an error unless adopt is set,
// in which case the existing file is loaded unchanged.
func Init(ctx context.Context, root string, ws *domain.Stack, adopt bool, opts Options) (*Repo, error) {
	path := filepath.Join(root, StackFile)
	if _, err := os.Stat(path); err == nil {
		if !adopt {
			return nil, fmt.Errorf("%w: %s already exists", controlplane.ErrExists, StackFile)
		}
		return OpenAt(ctx, root, opts)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	git := gitclient.New(root)
	if opts.GitExecutable != "" {
		git.Executable = opts.GitExecutable
	}
	if _, err := git.GitDir(ctx); err != nil {
		return nil, fmt.Errorf("%w: %s is not a Git repository (run `git init` there first)", ErrNotStack, root)
	}
	if err := writeDocument(path, ws); err != nil {
		return nil, err
	}
	return OpenAt(ctx, root, opts)
}

// Path returns an absolute path inside the coordination repository.
func (r *Repo) Path(parts ...string) string {
	return filepath.Join(append([]string{r.Root}, parts...)...)
}

// Rel returns a repository-relative slash path for display.
func (r *Repo) Rel(abs string) string {
	rel, err := filepath.Rel(r.Root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

// ---------------------------------------------------------------------------
// Documents
// ---------------------------------------------------------------------------

func readStack(path string) (*domain.Stack, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path derives from Locate
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotStack, path)
		}
		return nil, err
	}
	ws, err := schema.LoadStack(data)
	if err != nil {
		return nil, &DocumentError{File: path, Err: err}
	}
	return ws, nil
}

// DocumentError names the file a validation failure came from.
type DocumentError struct {
	File string
	Err  error
}

func (e *DocumentError) Error() string { return e.File + ": " + e.Err.Error() }
func (e *DocumentError) Unwrap() error { return e.Err }

// SaveStack writes fxvcs.yaml after re-validating it.
func (r *Repo) SaveStack(ws *domain.Stack) error {
	path := r.Path(StackFile)
	if err := writeDocument(path, ws); err != nil {
		return err
	}
	r.Stack = ws
	return nil
}

// RuntimeResources loads runtime-resources.yaml; a missing file is an empty
// catalog, because a composition made only of component repositories is
// legitimate.
func (r *Repo) RuntimeResources() (*domain.RuntimeResourceCatalog, error) {
	path := r.Path(RuntimeResourcesFile)
	data, err := os.ReadFile(path) //nolint:gosec // path is inside the coordination root
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &domain.RuntimeResourceCatalog{
				Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindRuntimeResourceCatalog},
				Spec:     domain.RuntimeResourceCatalogSpec{Resources: []domain.RuntimeResource{}},
			}, nil
		}
		return nil, err
	}
	cat, err := schema.LoadRuntimeResourceCatalog(data)
	if err != nil {
		return nil, &DocumentError{File: path, Err: err}
	}
	return cat, nil
}

// SaveRuntimeResources writes runtime-resources.yaml.
func (r *Repo) SaveRuntimeResources(cat *domain.RuntimeResourceCatalog) error {
	return writeDocument(r.Path(RuntimeResourcesFile), cat)
}

// Overrides loads resource-overrides.yaml; a missing file means no overrides.
func (r *Repo) Overrides() (*domain.ResourceOverrides, error) {
	path := r.Path(OverridesFile)
	data, err := os.ReadFile(path) //nolint:gosec // path is inside the coordination root
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &domain.ResourceOverrides{
				Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindResourceOverrides},
				Spec:     domain.ResourceOverridesSpec{Resources: []domain.ResourceOverride{}},
			}, nil
		}
		return nil, err
	}
	ov, err := schema.LoadResourceOverrides(data)
	if err != nil {
		return nil, &DocumentError{File: path, Err: err}
	}
	return ov, nil
}

// ---------------------------------------------------------------------------
// Releases
// ---------------------------------------------------------------------------

// ReleasePath is the file a release name maps to.
func (r *Repo) ReleasePath(name string) string { return r.Path(ReleasesDir, name+".yaml") }

// GetRelease implements controlplane.ControlPlane.
func (r *Repo) GetRelease(_ context.Context, name string) (domain.Release, error) {
	if err := ValidateName("release", name); err != nil {
		return domain.Release{}, err
	}
	path := r.ReleasePath(name)
	data, err := os.ReadFile(path) //nolint:gosec // path is inside the coordination root
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.Release{}, fmt.Errorf("%w: release %q", controlplane.ErrNotFound, name)
		}
		return domain.Release{}, err
	}
	rel, err := schema.LoadRelease(data)
	if err != nil {
		return domain.Release{}, &DocumentError{File: path, Err: err}
	}
	if rel.Metadata.Name != name {
		return domain.Release{}, &DocumentError{File: path, Err: fmt.Errorf("%w: metadata.name is %q but the file is %s.yaml",
			controlplane.ErrInvalid, rel.Metadata.Name, name)}
	}
	return *rel, nil
}

// WriteRelease writes a new release record. A release name is permanently
// bound to its content digest: writing over an existing record is refused
// unless the semantic content is identical, in which case the write is a no-op
// and rewritten reports false.
func (r *Repo) WriteRelease(ctx context.Context, rel domain.Release) (path string, rewritten bool, err error) {
	if err := ValidateName("release", rel.Metadata.Name); err != nil {
		return "", false, err
	}
	path = r.ReleasePath(rel.Metadata.Name)
	want, err := integrity.ReleaseDigest(rel)
	if err != nil {
		return "", false, err
	}
	existing, err := r.GetRelease(ctx, rel.Metadata.Name)
	switch {
	case err == nil:
		got, derr := integrity.ReleaseDigest(existing)
		if derr != nil {
			return "", false, derr
		}
		if got != want {
			return path, false, fmt.Errorf("%w: release %q already exists with different content; create a new release name instead",
				controlplane.ErrImmutable, rel.Metadata.Name)
		}
		return path, false, nil
	case errors.Is(err, controlplane.ErrNotFound):
	default:
		return "", false, err
	}
	if err := writeDocument(path, rel); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// ListReleases returns release names in lexical order.
func (r *Repo) ListReleases() ([]string, error) { return r.listNames(ReleasesDir) }

// ---------------------------------------------------------------------------
// Environments and targets
// ---------------------------------------------------------------------------

// EnvironmentPath is the file an environment name maps to.
func (r *Repo) EnvironmentPath(name string) string {
	return r.Path(EnvironmentsDir, name+".yaml")
}

// GetEnvironment loads one environment.
func (r *Repo) GetEnvironment(name string) (domain.Environment, error) {
	if err := ValidateName("environment", name); err != nil {
		return domain.Environment{}, err
	}
	path := r.EnvironmentPath(name)
	data, err := os.ReadFile(path) //nolint:gosec // path is inside the coordination root
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.Environment{}, fmt.Errorf("%w: environment %q", controlplane.ErrNotFound, name)
		}
		return domain.Environment{}, err
	}
	env, err := schema.LoadEnvironment(data)
	if err != nil {
		return domain.Environment{}, &DocumentError{File: path, Err: err}
	}
	if env.Metadata.Name != name {
		return domain.Environment{}, &DocumentError{File: path, Err: fmt.Errorf("%w: metadata.name is %q but the file is %s.yaml",
			controlplane.ErrInvalid, env.Metadata.Name, name)}
	}
	return *env, nil
}

// SaveEnvironment writes one environment record.
func (r *Repo) SaveEnvironment(env domain.Environment) (string, error) {
	if err := ValidateName("environment", env.Metadata.Name); err != nil {
		return "", err
	}
	path := r.EnvironmentPath(env.Metadata.Name)
	return path, writeDocument(path, env)
}

// ListEnvironments returns environment names in lexical order.
func (r *Repo) ListEnvironments() ([]string, error) { return r.listNames(EnvironmentsDir) }

// TargetPath is the file a target name maps to.
func (r *Repo) TargetPath(name string) string { return r.Path(TargetsDir, name+".yaml") }

// GetTarget implements controlplane.ControlPlane.
func (r *Repo) GetTarget(_ context.Context, name string) (domain.Target, error) {
	if err := ValidateName("target", name); err != nil {
		return domain.Target{}, err
	}
	path := r.TargetPath(name)
	data, err := os.ReadFile(path) //nolint:gosec // path is inside the coordination root
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.Target{}, fmt.Errorf("%w: target %q", controlplane.ErrNotFound, name)
		}
		return domain.Target{}, err
	}
	t, err := schema.LoadTarget(data)
	if err != nil {
		return domain.Target{}, &DocumentError{File: path, Err: err}
	}
	if t.Metadata.Name != name {
		return domain.Target{}, &DocumentError{File: path, Err: fmt.Errorf("%w: metadata.name is %q but the file is %s.yaml",
			controlplane.ErrInvalid, t.Metadata.Name, name)}
	}
	return *t, nil
}

// SaveTarget writes one target record.
func (r *Repo) SaveTarget(t domain.Target) (string, error) {
	if err := ValidateName("target", t.Metadata.Name); err != nil {
		return "", err
	}
	path := r.TargetPath(t.Metadata.Name)
	return path, writeDocument(path, t)
}

// SetTargetRelease implements controlplane.ControlPlane. It edits desired
// state in the working tree; the change becomes authoritative only when a
// human or CI commits and pushes it.
func (r *Repo) SetTargetRelease(ctx context.Context, target, release string) error {
	t, err := r.GetTarget(ctx, target)
	if err != nil {
		return err
	}
	if _, err := r.GetRelease(ctx, release); err != nil {
		return err
	}
	t.Spec.Release = release
	_, err = r.SaveTarget(t)
	return err
}

// ListTargets returns target names in lexical order.
func (r *Repo) ListTargets() ([]string, error) { return r.listNames(TargetsDir) }

func (r *Repo) listNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(r.Path(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(names)
	return names, nil
}

// Component returns the stack component with the given repository ID.
func (r *Repo) Component(repositoryID string) (domain.StackComponent, bool) {
	for _, c := range r.Stack.Spec.Components {
		if c.RepositoryID == repositoryID {
			return c, true
		}
	}
	return domain.StackComponent{}, false
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// writeDocument validates a domain document against its schema and writes it
// as YAML, atomically, creating parent directories.
//
// Validation happens on the way out as well as on the way in: a record that
// this binary would refuse to read must never be produced by it, and the
// alternative — discovering it at deploy time on a target — is exactly the
// failure mode releases exist to prevent.
func writeDocument(path string, doc any) error {
	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	kind, err := documentKind(doc)
	if err != nil {
		return err
	}
	if issues := schema.Validate(kind, data); len(issues) > 0 {
		return &DocumentError{File: path, Err: issues}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // coordination records are reviewable, world-readable YAML
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func documentKind(doc any) (string, error) {
	switch d := doc.(type) {
	case *domain.Stack:
		return d.Kind, nil
	case domain.Stack:
		return d.Kind, nil
	case *domain.RuntimeResourceCatalog:
		return d.Kind, nil
	case domain.RuntimeResourceCatalog:
		return d.Kind, nil
	case *domain.Release:
		return d.Kind, nil
	case domain.Release:
		return d.Kind, nil
	case *domain.Environment:
		return d.Kind, nil
	case domain.Environment:
		return d.Kind, nil
	case *domain.Target:
		return d.Kind, nil
	case domain.Target:
		return d.Kind, nil
	}
	return "", fmt.Errorf("gitops: %T is not a coordination document", doc)
}
