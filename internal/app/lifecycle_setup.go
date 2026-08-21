package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"io"
	"os"
	"path/filepath"
	"strings"

	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/journal"
)

// SetupInput configures Setup.
type SetupInput struct {
	// LauncherPath is the absolute driver launcher to install; empty selects
	// the launcher already recorded in local config, else the per-user
	// default (filter.DefaultLauncherPath).
	LauncherPath string
	// RepairLauncher permits a trusted candidate to replace different bytes
	// reporting the same semantic version. Normal newest-wins setup refuses
	// that ambiguous case.
	RepairLauncher bool
}

// SetupResult reports what Setup did.
type SetupResult struct {
	LauncherPath string
	// LauncherInstalled is true when the trusted launcher candidate was
	// installed or updated at LauncherPath.
	LauncherInstalled bool
	LauncherOutcome   LauncherOutcome
	// LegacyLauncherOutcome reports compatibility maintenance of the exact
	// former macOS default. Arbitrary custom paths are never treated as legacy.
	LegacyLauncherPath    string
	LegacyLauncherOutcome LauncherOutcome
	// ConfigKeys lists the repository-local git config keys written.
	ConfigKeys []string
	HookPath   string
	// HookInstalled is true when the managed hook was written or refreshed.
	HookInstalled bool
	// HookForeign is true when a hook not managed by fxvcs occupies pre-push;
	// HookInstructions then tells the user how to chain it. Not an error.
	HookForeign      bool
	HookInstructions string
	// StateCreated / LedgerCreated report databases created by this call.
	StateCreated  bool
	LedgerCreated bool
}

// Setup configures this clone for FXVCS: it makes sure a launcher binary
// exists at the launcher path (copying the running executable there when the
// path is empty), writes the repository-local driver configuration, installs
// the managed pre-push hook without overwriting a user hook, records the
// launcher in local config, and creates the local databases so their schema
// exists before the first filter invocation. It is idempotent and never
// commits.
func (r *Repo) Setup(ctx context.Context, in SetupInput) (SetupResult, error) {
	launcher, useDefault, err := r.resolveLauncherPath(in.LauncherPath)
	if err != nil {
		return SetupResult{}, err
	}
	if !filepath.IsAbs(launcher) {
		return SetupResult{}, fmt.Errorf("%w: launcher path must be absolute: %q", ErrInvalidArgument, launcher)
	}
	res := SetupResult{LauncherPath: launcher}
	source, err := r.launcherSourcePath()
	if err != nil {
		return res, err
	}
	ensured, err := EnsureLauncher(ctx, LauncherEnsureInput{SourcePath: source, TargetPath: launcher, CandidateVersion: r.Installed(), LockTimeout: r.opts.LockTimeout, RepairEqualVersion: in.RepairLauncher})
	if err != nil {
		return res, err
	}
	res.LauncherInstalled = ensured.Changed
	res.LauncherOutcome = ensured.Outcome
	if useDefault {
		legacy, ok, lerr := filter.DefaultLegacyLauncherPath()
		if lerr != nil {
			return res, lerr
		}
		if ok && legacy != launcher {
			if _, statErr := os.Lstat(legacy); statErr == nil {
				compat, compatErr := EnsureLauncher(ctx, LauncherEnsureInput{SourcePath: source, TargetPath: legacy, CandidateVersion: r.Installed(), LockTimeout: r.opts.LockTimeout, RepairEqualVersion: in.RepairLauncher})
				if compatErr != nil {
					if errors.Is(compatErr, ErrLauncherVersionConflict) {
						res.LegacyLauncherPath = legacy
						res.LegacyLauncherOutcome = compat.Outcome
					} else {
						return res, fmt.Errorf("refresh legacy launcher compatibility path: %w", compatErr)
					}
				} else {
					res.LegacyLauncherPath = legacy
					res.LegacyLauncherOutcome = compat.Outcome
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return res, statErr
			}
		}
	}

	if err := filter.Install(ctx, r.Git, launcher); err != nil {
		return res, err
	}
	for _, e := range filter.GitConfigEntries(launcher) {
		res.ConfigKeys = append(res.ConfigKeys, e.Key)
	}

	hook, err := filter.InstallPrePushHook(ctx, r.Git, launcher)
	res.HookPath = hook.Path
	switch {
	case err == nil:
		res.HookInstalled = true
	case errors.Is(err, filter.ErrHookExists):
		res.HookForeign = true
		res.HookInstructions = filter.PrePushRefusalMessage(hook, launcher)
	default:
		return res, err
	}

	if err := r.recordLauncher(ctx, launcher); err != nil {
		return res, err
	}

	// Create the databases (schema at the current version) so the first
	// filter or hook invocation never has to create or migrate them.
	_, stateExisted := statFile(r.Paths.StateDB)
	if _, err := r.State(ctx); err != nil {
		return res, err
	}
	res.StateCreated = !stateExisted
	_, ledgerExisted := statFile(r.Paths.PublicationDB)
	if _, err := r.Ledger(ctx); err != nil {
		return res, err
	}
	res.LedgerCreated = !ledgerExisted
	return res, nil
}

// resolveLauncherPath migrates only the exact former default to the canonical
// platform path. An explicit path or any other recorded path remains custom.
func (r *Repo) resolveLauncherPath(explicit string) (path string, useDefault bool, err error) {
	if explicit != "" {
		return explicit, false, nil
	}
	canonical, err := filter.DefaultLauncherPath()
	if err != nil {
		return "", false, err
	}
	recorded := r.Local.Spec.LauncherPath
	if recorded == "" {
		return canonical, true, nil
	}
	legacy, ok, err := filter.DefaultLegacyLauncherPath()
	if err != nil {
		return "", false, err
	}
	if ok && recorded == legacy {
		return canonical, true, nil
	}
	return recorded, recorded == canonical, nil
}

func (r *Repo) launcherSourcePath() (string, error) {
	if r.opts.LauncherSourcePath != "" {
		return filepath.Abs(r.opts.LauncherSourcePath)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running executable to install as launcher: %w", err)
	}
	return filepath.Abs(exe)
}

func statFile(p string) (os.FileInfo, bool) {
	fi, err := os.Stat(p)
	return fi, err == nil
}

// recordLauncher stores the launcher path in local config under the
// local-config lock (re-reading first so concurrent storage edits survive).
func (r *Repo) recordLauncher(ctx context.Context, launcher string) error {
	h, err := r.Lock(ctx, journal.LockPublication, "local-config")
	if err != nil {
		return err
	}
	defer func() { _ = h.Release() }()
	lc, err := LoadLocalConfig(r.Paths.ConfigYAML)
	if err != nil {
		return err
	}
	if lc.Spec.LauncherPath != launcher {
		lc.Spec.LauncherPath = launcher
		if err := SaveLocalConfig(r.Paths.ConfigYAML, lc); err != nil {
			return err
		}
	}
	r.Local = lc
	return nil
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // executable/launcher paths chosen by fxvcs
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ErrTestBinaryLauncher refuses to install a Go test binary as the launcher
// anywhere outside the temp directory. Git would otherwise run the test
// binary as the filter/diff/merge driver of a real repository — and a binary
// whose only behaviour is to run a test suite would run it recursively.
//
// This is not "tests cannot use the SDK": a test binary may be its own
// launcher inside a temp directory, provided it delegates driver invocations
// back into the CLI. See fxvcs.dev/fxvcs/sdk/driver.
var ErrTestBinaryLauncher = errors.New("app: refusing to install a Go test binary as the driver launcher outside a temp directory")

// isTestBinary reports whether the running executable is a `go test` binary.
func isTestBinary(exe string) bool {
	base := filepath.Base(exe)
	return strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") || flag.Lookup("test.v") != nil
}

// installLauncherFrom atomically copies src over the launcher path.
func installLauncherFrom(src, path string) error {
	if isTestBinary(src) && !underTemp(path) {
		return fmt.Errorf("%w: %s -> %s", ErrTestBinaryLauncher, src, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // src is os.Executable or an explicit launcher source
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error { _ = tmp.Close(); _ = os.Remove(name); return err }
	if _, err := io.Copy(tmp, in); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o755); err != nil { //nolint:gosec // the launcher must be executable
		_ = os.Remove(name)
		return err
	}
	if err := replaceLauncherFile(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("install launcher at %s: %w", path, err)
	}
	syncDir(filepath.Dir(path))
	return nil
}

// underTemp reports whether path lies under the OS temp directory after
// symlink resolution. Tests redirect XDG_DATA_HOME/LOCALAPPDATA to temp dirs,
// so a launcher path derived from them resolves under temp as well.
func underTemp(path string) bool {
	tmp := os.TempDir()
	if r, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = r
	}
	dir := filepath.Dir(path)
	// The launcher dir may not exist yet; resolve the nearest existing parent.
	for {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return pathsafe.Within(filepath.Clean(tmp), r)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}
