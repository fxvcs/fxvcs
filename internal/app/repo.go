// Package app implements the FXVCS use cases behind the public SDK. It owns
// the opened-checkout context (Repo): Git directories resolved through Git,
// committed and local configuration, the object cache, the per-worktree state
// database, the shared publication ledger, configured object stores, and the
// cross-process lock set. sdk.Client methods are thin adapters over this
// package; nothing here prints, prompts, or exits.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/chunk"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/gitclient"
	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/lock"
	"fxvcs.dev/fxvcs/internal/objectstore"
	"fxvcs.dev/fxvcs/internal/objectstore/filesystem"
	"fxvcs.dev/fxvcs/internal/objectstore/s3"
	"fxvcs.dev/fxvcs/internal/repository"
	"fxvcs.dev/fxvcs/internal/state"
)

// Errors.
var (
	ErrNotRepository   = repository.ErrNotRepository
	ErrNoSuchRemote    = errors.New("app: no such storage remote")
	ErrRemoteUnbound   = errors.New("app: remote has no local binding on this machine")
	ErrUnsupportedType = errors.New("app: unsupported storage remote type")
	ErrInvalidArgument = errors.New("app: invalid argument")
	ErrNotGitWorktree  = errors.New("app: repository root is not a Git working tree")
)

// samePath compares two absolute paths after cleaning and symlink resolution.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// Options configures Open.
type Options struct {
	Installed format.Version
	Operation string // recorded in lock owner metadata
	// GitExecutable overrides the git binary.
	GitExecutable string
	// CacheDir overrides the host object cache root.
	CacheDir string
	// ConfigDir overrides the user configuration directory that holds the
	// object-store credential file (tests, encrypted volumes).
	ConfigDir string
	// LauncherSourcePath is the trusted headless fxvcs CLI/Engine executable
	// that may be installed for Git. Empty uses os.Executable. Embedding hosts
	// such as fxvcs Desktop must set this to their bundled helper, never the
	// Desktop application executable.
	LauncherSourcePath string
	// LockTimeout bounds waits for busy locks (0 = try once). Entry points
	// that serve an operator — the SDK client and the Git filter — pass
	// DefaultLockTimeout; zero stays fail-fast for callers that mean it.
	LockTimeout time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
	// Diagnostic marks an operation that reports on the repository rather
	// than acting on it (verify, doctor). Such an operation must not repair
	// what it finds: a check that silently fixes a damaged object and then
	// reports the repository healthy has destroyed the evidence and told the
	// operator nothing happened.
	Diagnostic bool
}

// DefaultLockTimeout is how long an operation waits for a lock another
// process holds before giving up on that path.
//
// FXVCS deliberately has no daemon, so the CLI, the desktop application and
// the filter processes Git spawns all take these locks independently, and two
// of them wanting the same path at the same moment is ordinary rather than
// exceptional. Every hold is bounded local work — one file's compare-and-swap,
// one clean — so the wait that absorbs a collision is short, and an operation
// that fails a path instead of waiting a moment for it makes the operator do
// by hand what the process could have done by waiting.
//
// Ten seconds is far longer than any legitimate hold and still short enough
// that a stuck lock is reported while someone is watching.
const DefaultLockTimeout = 10 * time.Second

// Repo is an opened FXVCS checkout.
type Repo struct {
	Root         string // working-tree top level (from git)
	GitDir       string
	GitCommonDir string
	Config       *domain.Repository
	Local        *domain.LocalConfig
	Domain       string // storageDomainID
	Git          *gitclient.Client
	Cache        *cache.Cache
	Paths        state.Paths
	Locks        *lock.Set

	opts      Options
	budget    cacheBudget
	handlesMu sync.Mutex
	stateDB   *state.DB
	ledger    *state.Ledger
	policy    *chunk.Policy
	stores    map[string]objectstore.Store
	// profiles memoises the credential-store profile names; see
	// hasStoredLogin. profilesMu guards it.
	profiles   *[]string
	profilesMu sync.Mutex
	// storesMu guards stores: Publish opens stores from several upload
	// goroutines at once.
	storesMu sync.Mutex
}

// Open resolves the checkout containing start, loads and preflights the
// committed configuration, loads local configuration, and opens the cache. It
// does not open the SQLite databases; use State()/Ledger() lazily so
// read-only diagnostics never create or migrate them.
func Open(ctx context.Context, start string, opts Options) (*Repo, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	// Committed configuration and the version gate come first so a client that
	// is too old reports exactly the upgrade requirement, even before Git runs.
	root, cfg, err := repository.Open(start, opts.Installed)
	if err != nil {
		return nil, err
	}
	git := gitclient.New(root)
	if opts.GitExecutable != "" {
		git.Executable = opts.GitExecutable
	}
	// One invocation for all three: opening a repository happens on every SDK
	// call, and three separate spawns were most of what a small query cost.
	top, gitDir, commonDir, err := git.Locations(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotGitWorktree, err)
	}
	if !samePath(top, root) {
		return nil, fmt.Errorf("%w: .fxvcs/repository.yaml is at %s but the Git working tree is %s", ErrNotGitWorktree, root, top)
	}
	paths := state.Locate(gitDir, commonDir)
	local, err := LoadLocalConfig(paths.ConfigYAML)
	if err != nil {
		return nil, err
	}
	c, err := cache.Open(opts.CacheDir)
	if err != nil {
		return nil, err
	}
	r := &Repo{
		Root: root, GitDir: gitDir, GitCommonDir: commonDir, Config: cfg, Local: local,
		Domain: cfg.Metadata.StorageDomainID, Git: git, Cache: c, Paths: paths,
		Locks: lock.NewSet(filepath.Join(gitDir, "fxvcs", "locks"), opts.Operation),
		opts:  opts,
	}
	return r, nil
}

// Close releases locks and databases.
func (r *Repo) Close() error {
	var errs []error
	if r.Locks != nil {
		r.Locks.ReleaseAll()
	}
	if r.stateDB != nil {
		errs = append(errs, r.stateDB.Close())
		r.stateDB = nil
	}
	if r.ledger != nil {
		errs = append(errs, r.ledger.Close())
		r.ledger = nil
	}
	return errors.Join(errs...)
}

// Installed returns the running binary version.
func (r *Repo) Installed() format.Version { return r.opts.Installed }

// Now returns the clock.
func (r *Repo) Now() time.Time { return r.opts.Now() }

// LockTimeout returns the configured lock wait.
func (r *Repo) LockTimeout() time.Duration { return r.opts.LockTimeout }

func (r *Repo) stateOptions() state.Options {
	return state.Options{
		InstalledVersion: r.opts.Installed.String(),
		RepositoryID:     r.Config.Metadata.RepositoryID,
		StorageDomainID:  r.Domain,
		Now:              r.opts.Now,
	}
}

// State opens (creating on demand) the per-worktree hydration database.
// Callers must hold LockCheckoutWorktree or a LockPath before mutating rows.
func (r *Repo) State(ctx context.Context) (*state.DB, error) {
	o := r.stateOptions()
	o.Create = true
	return r.state(ctx, o)
}

// StateIfExists opens the hydration database without creating it.
func (r *Repo) StateIfExists(ctx context.Context) (*state.DB, error) {
	return r.state(ctx, r.stateOptions())
}

// state memoizes the one handle this process has on the hydration database.
//
// The guard is not decoration. Two callers finding the handle unset both open
// it, and then there are two independent connection pools on one file: one
// rebuilds the WAL index while the other is reading, which is a real source of
// contention on a database that is supposed to have exactly one writer here —
// and the loser's handle is leaked, holding a connection open until the
// process exits.
func (r *Repo) state(ctx context.Context, o state.Options) (*state.DB, error) {
	r.handlesMu.Lock()
	defer r.handlesMu.Unlock()
	if r.stateDB != nil {
		return r.stateDB, nil
	}
	db, err := state.Open(ctx, r.Paths.StateDB, o)
	if err != nil {
		return nil, err
	}
	r.stateDB = db
	return db, nil
}

// Ledger opens (creating on demand) the shared publication ledger.
func (r *Repo) Ledger(ctx context.Context) (*state.Ledger, error) {
	o := r.stateOptions()
	o.Create = true
	return r.ledgerHandle(ctx, o)
}

// LedgerIfExists opens the ledger without creating it.
func (r *Repo) LedgerIfExists(ctx context.Context) (*state.Ledger, error) {
	return r.ledgerHandle(ctx, r.stateOptions())
}

func (r *Repo) ledgerHandle(ctx context.Context, o state.Options) (*state.Ledger, error) {
	r.handlesMu.Lock()
	defer r.handlesMu.Unlock()
	if r.ledger != nil {
		return r.ledger, nil
	}
	l, err := state.OpenLedger(ctx, r.Paths.PublicationDB, o)
	if err != nil {
		return nil, err
	}
	r.ledger = l
	return l, nil
}

// Lock acquires a lock in the documented order with the configured timeout.
func (r *Repo) Lock(ctx context.Context, level journal.Level, key string) (*lock.Handle, error) {
	return r.lockSet(ctx).Acquire(ctx, level, key, r.opts.LockTimeout)
}

// TryLock acquires without waiting, for callers whose work is optional when
// something else already holds the lock.
//
// It exists so that every acquisition in this package goes through the
// context's lock set. A call that reached for r.Locks directly would put
// concurrent paths back on the one journal.Sequence that per-worker sets exist
// to keep them off — and it would do so from the places least likely to be
// covered by a test, because they are the ones that only run when a path
// turns out to need nothing done to it.
func (r *Repo) TryLock(ctx context.Context, level journal.Level, key string) (*lock.Handle, error) {
	return r.lockSet(ctx).Acquire(ctx, level, key, 0)
}

type lockSetKey struct{}

// withLockSet gives one unit of work its own lock.Set.
//
// lock.Set holds a single journal.Sequence, which is documented as one per
// operation: it records what this operation holds so it can enforce the
// acquisition order. Independent paths processed at the same time are separate
// operations, and sharing one Sequence between them both races on it and makes
// their unrelated path locks look like one operation taking several
// same-level locks out of order.
func withLockSet(ctx context.Context, s *lock.Set) context.Context {
	return context.WithValue(ctx, lockSetKey{}, s)
}

func (r *Repo) lockSet(ctx context.Context) *lock.Set {
	if s, ok := ctx.Value(lockSetKey{}).(*lock.Set); ok && s != nil {
		return s
	}
	return r.Locks
}

// NewLockSet makes a lock.Set over the same lock files as r.Locks.
//
// The directory is taken from the existing set rather than recomputed: these
// are the files that exclude other processes, and a set rooted anywhere else
// would take locks nobody else looks at — mutual exclusion lost silently,
// with every test still passing.
func (r *Repo) NewLockSet() *lock.Set {
	return lock.NewSet(r.Locks.Dir, r.Locks.Operation)
}

// ---------------------------------------------------------------------------
// Local configuration
// ---------------------------------------------------------------------------

// LoadLocalConfig reads $GIT_COMMON_DIR/fxvcs/config.yaml; a missing file is
// an empty config.
func LoadLocalConfig(path string) (*domain.LocalConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is under the git common dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &domain.LocalConfig{Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindLocalConfig}}, nil
		}
		return nil, err
	}
	var env format.Envelope
	if err := yaml.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("app: parse local config envelope: %w", err)
	}
	if err := format.ValidateEnvelope(env, format.KindLocalConfig); err != nil {
		return nil, err
	}
	var lc domain.LocalConfig
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&lc); err != nil {
		return nil, fmt.Errorf("app: parse %s: %w", path, err)
	}
	return &lc, nil
}

// SaveLocalConfig writes the local config atomically.
func SaveLocalConfig(path string, lc *domain.LocalConfig) error {
	lc.APIVersion, lc.Kind = format.CurrentAPIVersion, format.KindLocalConfig
	data, err := yaml.Marshal(lc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SaveLocal persists r.Local.
func (r *Repo) SaveLocal() error { return SaveLocalConfig(r.Paths.ConfigYAML, r.Local) }

// ---------------------------------------------------------------------------
// Storage remotes
// ---------------------------------------------------------------------------

// RemoteView is the merged committed + local view of one storage remote.
type RemoteView struct {
	Name                     string
	Type                     string
	Committed                bool
	Local                    bool
	Bound                    bool   // has a usable local binding on this machine
	Path                     string // filesystem binding
	Endpoint, Bucket, Prefix string
	Region, Addressing       string
	RequiredForPublication   bool
	// CredentialProfile is the credential-store entry an s3 remote uses on
	// this machine; empty means the default profile.
	CredentialProfile string
	// HasCredentials reports that a stored login exists for an s3 remote. It
	// never exposes the secret itself.
	HasCredentials bool
}

// Remotes returns every known remote, committed first then local-only, in a
// stable order.
func (r *Repo) Remotes() []RemoteView {
	byName := map[string]*RemoteView{}
	var order []string
	for _, c := range r.Config.Spec.Storage.Remotes {
		v := &RemoteView{Name: c.Name, Type: c.Type, Committed: true, Endpoint: c.Endpoint, Bucket: c.Bucket,
			Prefix: c.Prefix, Region: c.Region, Addressing: c.Addressing, RequiredForPublication: c.RequiredForPublication}
		byName[c.Name] = v
		order = append(order, c.Name)
	}
	for _, l := range r.Local.Spec.Remotes {
		v, ok := byName[l.Name]
		if !ok {
			v = &RemoteView{Name: l.Name, Type: l.Type, Local: true, RequiredForPublication: l.RequiredForPublication}
			byName[l.Name] = v
			order = append(order, l.Name)
		}
		switch {
		case l.Type == "filesystem" && l.Path != "":
			v.Path = l.Path
			v.Bound = true
		case l.Type == "s3":
			if l.Endpoint != "" {
				v.Endpoint = l.Endpoint
			}
			if l.Bucket != "" {
				v.Bucket = l.Bucket
			}
			if l.Prefix != "" {
				v.Prefix = l.Prefix
			}
			if l.Region != "" {
				v.Region = l.Region
			}
			if l.Addressing != "" {
				v.Addressing = l.Addressing
			}
			v.CredentialProfile = l.CredentialProfile
		}
	}
	// An s3 remote is bound when this machine knows a bucket for it. Whether
	// it can also authenticate is a separate question (`storage test` and
	// `storage login` answer it), because credentials may come from the
	// environment or an instance role rather than from a stored login.
	for _, v := range byName {
		if v.Type == "s3" && v.Bucket != "" {
			v.Bound = true
			v.HasCredentials = r.hasStoredLogin(*v)
		}
	}
	out := make([]RemoteView, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out
}

// RequiredRemotes returns names of remotes that must hold every object before
// Git refs may become remotely reachable.
func (r *Repo) RequiredRemotes() []string {
	var names []string
	for _, v := range r.Remotes() {
		if v.RequiredForPublication {
			names = append(names, v.Name)
		}
	}
	sort.Strings(names)
	return names
}

// Store returns the object store client for a remote name.
func (r *Repo) Store(name string) (objectstore.Store, RemoteView, error) {
	r.storesMu.Lock()
	defer r.storesMu.Unlock()
	if s, ok := r.stores[name]; ok {
		for _, v := range r.Remotes() {
			if v.Name == name {
				return s, v, nil
			}
		}
	}
	for _, v := range r.Remotes() {
		if v.Name != name {
			continue
		}
		var s objectstore.Store
		switch v.Type {
		case "filesystem":
			if !v.Bound {
				return nil, v, fmt.Errorf("%w: %s (run: fxvcs storage add --name %s --type filesystem --path <dir>)", ErrRemoteUnbound, name, name)
			}
			fs, err := filesystem.New(v.Path)
			if err != nil {
				return nil, v, err
			}
			s = fs
		case "s3":
			if !v.Bound {
				return nil, v, fmt.Errorf("%w: %s (run: fxvcs storage add --name %s --type s3 --bucket <bucket> [--endpoint <url>])", ErrRemoteUnbound, name, name)
			}
			cfg, err := r.s3Config(v)
			if err != nil {
				return nil, v, err
			}
			s3s, err := s3.New(cfg)
			if err != nil {
				return nil, v, err
			}
			s = s3s
		default:
			return nil, v, fmt.Errorf("%w: %q", ErrUnsupportedType, v.Type)
		}
		if r.stores == nil {
			r.stores = map[string]objectstore.Store{}
		}
		r.stores[name] = s
		return s, v, nil
	}
	return nil, RemoteView{}, fmt.Errorf("%w: %q", ErrNoSuchRemote, name)
}

// setStore substitutes the client for a remote. Tests use it to observe or
// fault the transport; production code always goes through Store.
func (r *Repo) setStore(name string, s objectstore.Store) {
	r.storesMu.Lock()
	defer r.storesMu.Unlock()
	if r.stores == nil {
		r.stores = map[string]objectstore.Store{}
	}
	r.stores[name] = s
}

// ReadOrder returns remote names in read preference order: local ReadOrder
// first, then remaining remotes in committed/local order.
func (r *Repo) ReadOrder() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range r.Local.Spec.ReadOrder {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, v := range r.Remotes() {
		if !seen[v.Name] {
			seen[v.Name] = true
			out = append(out, v.Name)
		}
	}
	return out
}
