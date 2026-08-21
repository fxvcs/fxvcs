// Package sdk is the public, versioned Go facade for FXVCS.
//
// It is the canonical application boundary: the fxvcs CLI, the Git
// filter/diff/merge drivers, the target agent, and third-party Go programs all
// execute the same use cases through this package. It exposes typed requests,
// results, structured errors (*Error with a stable Code), progress events, and
// context cancellation. It never reads stdin, prints, prompts, or calls
// os.Exit.
//
// Stability: v0. See docs/sdk-stability.md. The exported surface is pinned by
// sdk/testdata/api-v0.golden and enforced by TestAPISurfaceIsPinned.
package sdk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/lock"
	"fxvcs.dev/fxvcs/internal/objectstore"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/repository"
	"fxvcs.dev/fxvcs/internal/state"
)

// Options configures a Client.
type Options struct {
	// Progress receives structured events. nil means Discard.
	Progress ProgressFunc
	// InstalledVersion overrides Version (tests and embedding hosts). It must
	// be a semantic version.
	InstalledVersion string
	// GitExecutable overrides the git binary used by Git-backed operations.
	GitExecutable string
	// LauncherSourcePath is the trusted headless fxvcs CLI/Engine executable
	// used for Git launcher installation. Empty uses the current executable.
	// fxvcs Desktop must set this to its bundled helper binary.
	LauncherSourcePath string
	// CacheDir overrides the host-wide immutable object cache directory
	// (default: FXVCS_CACHE_DIR or the OS user cache dir).
	CacheDir string
	// ConfigDir overrides the user configuration directory holding the
	// object-store credential file written by StorageLogin
	// (default: FXVCS_CONFIG_DIR or the OS user config dir).
	ConfigDir string
	// LockTimeout bounds how long an operation waits for a busy durable lock
	// before returning CodeBusy. Zero means the default wait (ten seconds):
	// with no daemon between this client and the other processes on the
	// machine, a busy lock is usually one that is about to be released. To
	// fail fast instead, set a duration short enough to expire immediately.
	LockTimeout time.Duration
}

// Client executes FXVCS operations. It is safe for concurrent use; operations
// serialize through durable cross-process locks, not in-memory state.
type Client struct {
	opts       Options
	installed  format.Version
	progressMu sync.Mutex
}

// New validates options and returns a Client.
func New(opts Options) (*Client, error) {
	v := opts.InstalledVersion
	if v == "" {
		v = Version
	}
	parsed, err := format.ParseVersion(v)
	if err != nil {
		// Name the value: this is set at build time by -ldflags, so the
		// person who sees it needs to know what the build put there.
		return nil, WrapError(CodeInvalidArgument,
			fmt.Sprintf("installed version %q is not a semantic version (major.minor.patch)", v), err)
	}
	if opts.Progress == nil {
		opts.Progress = Discard
	}
	if opts.LockTimeout == 0 {
		// A consumer that says nothing about locking wants the behaviour an
		// operator expects: wait a moment for whatever else is touching the
		// repository, rather than refusing paths a retry would have taken.
		opts.LockTimeout = app.DefaultLockTimeout
	}
	return &Client{opts: opts, installed: parsed}, nil
}

// InstalledVersion returns the version this client reports as installed.
func (c *Client) InstalledVersion() string { return c.installed.String() }

// ---------------------------------------------------------------------------
// Compatibility — the only operation permitted on a repository that requires a
// newer client.
// ---------------------------------------------------------------------------

// CompatibilityRequest identifies a repository checkout.
type CompatibilityRequest struct {
	// Path is any directory inside the checkout.
	Path string
}

// CompatibilityResult reports installed versus required versions. When
// UpgradeRequired is non-nil the caller must stop; no other SDK, CLI, filter,
// or agent operation will run against this repository with the current binary.
type CompatibilityResult struct {
	RepositoryRoot        string           `json:"repositoryRoot"`
	RepositoryID          string           `json:"repositoryID"`
	InstalledVersion      string           `json:"installedVersion"`
	MinimumFXVCSVersion   string           `json:"minimumFXVCSVersion"`
	GitIntegrationVersion int              `json:"gitIntegrationVersion"`
	Formats               FormatVersions   `json:"formats"`
	UpgradeRequired       *UpgradeRequired `json:"upgradeRequired,omitempty"`
}

// Compatibility runs the read-only compatibility diagnostic. It never opens
// local databases, starts filters, or migrates state.
func (c *Client) Compatibility(ctx context.Context, req CompatibilityRequest) (*CompatibilityResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, WrapError(CodeCancelled, "cancelled", err)
	}
	root, err := repository.Locate(req.Path)
	if err != nil {
		return nil, WrapError(CodeNotRepository, err.Error(), err)
	}
	repo, err := repository.LoadConfig(root)
	if err != nil {
		return nil, translateFormatError(root, err)
	}
	res := &CompatibilityResult{
		RepositoryRoot:        root,
		RepositoryID:          repo.Metadata.RepositoryID,
		InstalledVersion:      c.installed.String(),
		MinimumFXVCSVersion:   repo.Spec.MinimumFXVCSVersion,
		GitIntegrationVersion: repo.Spec.GitIntegrationVersion,
		Formats:               SupportedFormats(),
	}
	if err := repository.Preflight(root, repo, c.installed); err != nil {
		var ur *format.UpgradeRequiredError
		if errors.As(err, &ur) {
			res.UpgradeRequired = upgradeDetails(ur)
			return res, nil
		}
		return nil, translateFormatError(root, err)
	}
	return res, nil
}

// openRepo is the internal gate every normal operation passes through: it
// resolves the checkout through Git, loads and preflights the committed
// configuration, and returns the engine context. Callers must Close it.
func (c *Client) openRepo(ctx context.Context, path, operation string) (*app.Repo, error) {
	return c.open(ctx, path, operation, false)
}

// openDiagnostic opens for an operation that reports rather than acts, so it
// never repairs what it finds.
func (c *Client) openDiagnostic(ctx context.Context, path, operation string) (*app.Repo, error) {
	return c.open(ctx, path, operation, true)
}

func (c *Client) open(ctx context.Context, path, operation string, diagnostic bool) (*app.Repo, error) {
	if err := ctx.Err(); err != nil {
		return nil, WrapError(CodeCancelled, "cancelled", err)
	}
	r, err := app.Open(ctx, path, app.Options{
		Diagnostic:    diagnostic,
		Installed:     c.installed,
		Operation:     operation,
		GitExecutable: c.opts.GitExecutable,
		CacheDir:      c.opts.CacheDir,
		ConfigDir:     c.opts.ConfigDir,
		LockTimeout:   c.opts.LockTimeout,
	})
	if err != nil {
		return nil, translateError("", err)
	}
	return r, nil
}

// progress emits an event to the configured sink.
func (c *Client) progress(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	// Operations may run work concurrently (publish); events are still
	// delivered one at a time so consumers need no synchronization.
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	c.opts.Progress(ev)
}

// translateError maps engine/foundation errors to typed SDK errors. Errors
// that already are *Error pass through unchanged.
func translateError(root string, err error) error {
	if err == nil {
		return nil
	}
	var se *Error
	if errors.As(err, &se) {
		return err
	}
	var be *lock.BusyError
	if errors.As(err, &be) {
		e := WrapError(CodeBusy, be.Error(), err)
		if be.Owner != nil {
			e.Details = &BusyOwner{Operation: be.Owner.Operation, PID: be.Owner.PID, Host: be.Owner.Host, StartedAt: be.Owner.StartedAt}
		}
		return e
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return WrapError(CodeCancelled, "cancelled", err)
	case errors.Is(err, app.ErrInvalidArgument), errors.Is(err, app.ErrUnsupportedType):
		return WrapError(CodeInvalidArgument, err.Error(), err)
	case errors.Is(err, app.ErrNoSuchRemote), errors.Is(err, app.ErrRemoteUnbound), errors.Is(err, objectstore.ErrNotFound), errors.Is(err, state.ErrNotFound):
		return WrapError(CodeNotFound, err.Error(), err)
	case errors.Is(err, app.ErrNotGitWorktree):
		return WrapError(CodeGit, err.Error(), err)
	case errors.Is(err, objectstore.ErrUnavailable):
		return WrapError(CodeStorage, err.Error(), err)
	case errors.Is(err, pathsafe.ErrTraversal), errors.Is(err, pathsafe.ErrAbsolute):
		return WrapError(CodePathEscape, err.Error(), err)
	case errors.Is(err, state.ErrCorrupt):
		return WrapError(CodeIntegrity, err.Error(), err)
	}
	return translateFormatError(root, err)
}

func upgradeDetails(ur *format.UpgradeRequiredError) *UpgradeRequired {
	return &UpgradeRequired{
		InstalledVersion: ur.Installed.String(),
		RequiredVersion:  ur.Required.String(),
		Repository:       ur.Repository,
		Reason:           ur.Reason,
		UpgradeCommand:   "fxvcs upgrade --check",
	}
}

func translateFormatError(root string, err error) error {
	var ur *format.UpgradeRequiredError
	switch {
	case errors.As(err, &ur):
		e := WrapError(CodeUpgradeRequired, ur.Error(), err)
		e.Details = upgradeDetails(ur)
		return e
	case errors.Is(err, format.ErrNewerAPIVersion):
		e := WrapError(CodeUpgradeRequired, err.Error(), err)
		e.Details = &UpgradeRequired{InstalledVersion: Version, RequiredVersion: "unknown (newer apiVersion)", Repository: root, Reason: "apiVersion", UpgradeCommand: "fxvcs upgrade --check"}
		return e
	case errors.Is(err, format.ErrUnknownAPIVersion), errors.Is(err, format.ErrUnknownKind),
		errors.Is(err, format.ErrUnsupportedPointer), errors.Is(err, format.ErrUnsupportedSchemaDB):
		return WrapError(CodeUnsupportedFormat, err.Error(), err)
	case errors.Is(err, repository.ErrNotRepository):
		return WrapError(CodeNotRepository, err.Error(), err)
	}
	return WrapError(CodeIO, err.Error(), err)
}
