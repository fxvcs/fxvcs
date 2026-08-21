// Package gitclient is a small exec-based Git client used by the filter,
// diff/merge drivers, the pre-push hook, and setup/upgrade. It never links a
// Git library: every call shells out to the installed `git`, runs with an
// explicit environment, captures stderr, and returns typed errors so callers
// can distinguish "git failed" from "git is missing" from "context cancelled".
//
// The client is deliberately narrow: it exposes only the plumbing FXVCS needs
// and never runs a porcelain command that could prompt, page, or open an
// editor.
package gitclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Errors.
var (
	// ErrGitNotFound is returned when the git executable cannot be located.
	ErrGitNotFound = errors.New("gitclient: git executable not found")
	// ErrNotRepository is returned when a command runs outside a Git repository.
	ErrNotRepository = errors.New("gitclient: not a git repository")
)

// Error is a failed git invocation. Stderr is captured verbatim (trimmed) so
// callers can surface Git's own message.
type Error struct {
	Args     []string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("git %s: exit %d", strings.Join(e.Args, " "), e.ExitCode)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Client runs git in a fixed directory with a fixed environment.
type Client struct {
	// Dir is the working directory for every command ("" = process cwd).
	Dir string
	// Env is the complete environment for git. nil inherits the process
	// environment. Tests pass an isolated environment (HOME, GIT_CONFIG_*).
	Env []string
	// Executable overrides the git binary ("" = "git" from PATH).
	Executable string
}

// New returns a client rooted at dir inheriting the process environment.
func New(dir string) *Client { return &Client{Dir: dir} }

// WithEnv returns a copy of the client using env as the complete environment.
func (c *Client) WithEnv(env []string) *Client {
	cp := *c
	cp.Env = append([]string(nil), env...)
	return &cp
}

// WithDir returns a copy of the client rooted at dir.
func (c *Client) WithDir(dir string) *Client {
	cp := *c
	cp.Dir = dir
	return &cp
}

// Available reports whether the git executable can be found.
func (c *Client) Available() bool {
	_, err := exec.LookPath(c.gitPath())
	return err == nil
}

func (c *Client) gitPath() string {
	if c.Executable != "" {
		return c.Executable
	}
	return "git"
}

// baseEnv returns the environment plus settings that keep git non-interactive
// and machine-parseable. Explicit user settings in c.Env win for everything
// except the terminal-prompt and locale guards, which are always enforced.
func (c *Client) baseEnv() []string {
	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+4)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_TERMINAL_PROMPT", "LC_ALL", "GIT_PAGER", "GIT_EDITOR":
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
		"GIT_PAGER=cat",
		"GIT_EDITOR=:",
	)
}

// Run executes git with args and returns stdout. Stderr is captured into the
// returned *Error on failure.
func (c *Client) Run(ctx context.Context, args ...string) ([]byte, error) {
	return c.RunInput(ctx, nil, args...)
}

// Command-line budget for path arguments.
//
// Every operating system caps how much argv a process may receive, and a
// repository with thousands of managed paths reaches that cap easily: macOS
// allows 1 MiB for argv and the environment together, and Windows allows
// 32 767 characters for the whole command line — the binding constraint. A
// command built from an unbounded path list therefore fails with
// "argument list too long" on exactly the repositories that need it most.
//
// These budgets are deliberately far below every platform limit: the cost of
// an extra Git process is milliseconds, and the cost of guessing wrong is an
// operation that cannot run at all.
const (
	maxPathArgBytes = 24 << 10
	maxPathArgCount = 512
)

// BatchPaths splits paths into command-line-sized batches and calls fn once
// per batch, in order. An empty list calls fn once with no paths only when
// callEmpty is set, which is what a "no pathspec means everything" command
// needs. The first error stops the walk.
func BatchPaths(paths []string, callEmpty bool, fn func(batch []string) error) error {
	if len(paths) == 0 {
		if callEmpty {
			return fn(nil)
		}
		return nil
	}
	start, size := 0, 0
	for i, p := range paths {
		n := len(p) + 1
		if i > start && (size+n > maxPathArgBytes || i-start >= maxPathArgCount) {
			if err := fn(paths[start:i]); err != nil {
				return err
			}
			start, size = i, 0
		}
		size += n
	}
	return fn(paths[start:])
}

// RunBatched runs `git <prefix...> <batch...>` once per path batch and
// concatenates the outputs. Use it for any command whose path list comes from
// the repository rather than from a fixed set.
func (c *Client) RunBatched(ctx context.Context, prefix []string, paths []string) ([]byte, error) {
	var out bytes.Buffer
	err := BatchPaths(paths, true, func(batch []string) error {
		args := make([]string, 0, len(prefix)+len(batch))
		args = append(args, prefix...)
		args = append(args, batch...)
		b, err := c.Run(ctx, args...)
		out.Write(b)
		return err
	})
	return out.Bytes(), err
}

// RunInput is Run with stdin.
func (c *Client) RunInput(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if err := c.RunStreams(ctx, stdin, &stdout, args...); err != nil {
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

// RunStreams executes git streaming stdout to w (bounded memory for large
// blobs). Stderr is always captured.
func (c *Client) RunStreams(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	if _, err := exec.LookPath(c.gitPath()); err != nil {
		return fmt.Errorf("%w: %v", ErrGitNotFound, err)
	}
	cmd := exec.CommandContext(ctx, c.gitPath(), args...)
	cmd.Dir = c.Dir
	cmd.Env = c.baseEnv()
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	gerr := &Error{Args: args, ExitCode: -1, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		gerr.ExitCode = ee.ExitCode()
	}
	if strings.Contains(gerr.Stderr, "not a git repository") {
		gerr.Err = ErrNotRepository
	}
	return gerr
}

// RunString is Run returning trimmed stdout as a string.
func (c *Client) RunString(ctx context.Context, args ...string) (string, error) {
	out, err := c.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// Version returns the `git version` string.
func (c *Client) Version(ctx context.Context) (string, error) {
	return c.RunString(ctx, "version")
}

// ---------------------------------------------------------------------------
// rev-parse helpers
// ---------------------------------------------------------------------------

// RevParse runs `git rev-parse <arg>` and returns the single-line result.
func (c *Client) RevParse(ctx context.Context, arg string) (string, error) {
	return c.RunString(ctx, "rev-parse", arg)
}

// TopLevel returns the absolute working-tree root.
func (c *Client) TopLevel(ctx context.Context) (string, error) {
	return c.RevParse(ctx, "--show-toplevel")
}

// GitDir returns $GIT_DIR (absolute).
func (c *Client) GitDir(ctx context.Context) (string, error) {
	return c.RevParse(ctx, "--absolute-git-dir")
}

// GitCommonDir returns $GIT_COMMON_DIR (absolute; equals GitDir outside
// linked worktrees).
func (c *Client) GitCommonDir(ctx context.Context) (string, error) {
	p, err := c.RevParse(ctx, "--git-common-dir")
	if err != nil {
		return "", err
	}
	return c.absolutize(ctx, p)
}

// Locations resolves the working-tree root, $GIT_DIR and $GIT_COMMON_DIR in a
// single invocation.
//
// It exists because opening a repository needed all three and asked for them
// one at a time. Every Git question costs a process, and on an interactive
// caller that opens a repository per request those three spawns are paid
// before any work begins — on macOS and Windows that is most of what a small
// query costs. rev-parse answers all three in argument order for the price of
// one.
func (c *Client) Locations(ctx context.Context) (top, gitDir, commonDir string, err error) {
	out, err := c.RunString(ctx, "rev-parse", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return "", "", "", err
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		return "", "", "", fmt.Errorf("gitclient: rev-parse returned %d lines, want 3: %q", len(lines), out)
	}
	commonDir, err = c.absolutize(ctx, strings.TrimSpace(lines[2]))
	if err != nil {
		return "", "", "", err
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), commonDir, nil
}

// GitPath resolves `git rev-parse --git-path <name>` (e.g. "fxvcs", "hooks",
// "info/exclude") to an absolute path, honoring worktrees and core.hooksPath.
func (c *Client) GitPath(ctx context.Context, name string) (string, error) {
	p, err := c.RunString(ctx, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	return c.absolutize(ctx, p)
}

// absolutize makes a rev-parse relative result absolute against Dir.
func (c *Client) absolutize(ctx context.Context, p string) (string, error) {
	if isAbs(p) {
		return p, nil
	}
	dir := c.Dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	return joinAbs(dir, p), nil
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

// ConfigSet sets a repository-local (--local) key.
func (c *Client) ConfigSet(ctx context.Context, key, value string) error {
	_, err := c.Run(ctx, "config", "--local", key, value)
	return err
}

// ConfigGet reads a key using git's normal precedence. ok is false when unset.
func (c *Client) ConfigGet(ctx context.Context, key string) (value string, ok bool, err error) {
	out, err := c.RunString(ctx, "config", "--get", key)
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.ExitCode == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

// ConfigUnset removes a repository-local key (no error if absent).
func (c *Client) ConfigUnset(ctx context.Context, key string) error {
	_, err := c.Run(ctx, "config", "--local", "--unset", key)
	var ge *Error
	if errors.As(err, &ge) && ge.ExitCode == 5 {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// attributes
// ---------------------------------------------------------------------------

// Attrs holds the effective FXVCS-relevant attributes of one path. Each value
// is one of: the attribute value, "set", "unset", or "unspecified", exactly as
// git check-attr reports them.
type Attrs struct {
	Filter string
	Diff   string
	Merge  string
	Text   string
}

// IsManaged reports whether the path is routed through the fxvcs filter.
func (a Attrs) IsManaged() bool { return a.Filter == "fxvcs" }

// CheckAttr returns filter/diff/merge/text attributes for a repo-relative path.
func (c *Client) CheckAttr(ctx context.Context, path string) (Attrs, error) {
	m, err := c.CheckAttrs(ctx, []string{path})
	if err != nil {
		return Attrs{}, err
	}
	return m[path], nil
}

// CheckAttrs is CheckAttr for many paths (one git process, -z --stdin).
func (c *Client) CheckAttrs(ctx context.Context, paths []string) (map[string]Attrs, error) {
	res := make(map[string]Attrs, len(paths))
	if len(paths) == 0 {
		return res, nil
	}
	var in bytes.Buffer
	for _, p := range paths {
		in.WriteString(p)
		in.WriteByte(0)
	}
	out, err := c.RunInput(ctx, &in, "check-attr", "-z", "--stdin", "filter", "diff", "merge", "text")
	if err != nil {
		return nil, err
	}
	fields := bytes.Split(out, []byte{0})
	// Layout: path\0attr\0value\0 repeated, trailing empty field.
	for i := 0; i+2 < len(fields); i += 3 {
		p := string(fields[i])
		a := res[p]
		v := string(fields[i+2])
		switch string(fields[i+1]) {
		case "filter":
			a.Filter = v
		case "diff":
			a.Diff = v
		case "merge":
			a.Merge = v
		case "text":
			a.Text = v
		}
		res[p] = a
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// objects
// ---------------------------------------------------------------------------

// HashObject returns the Git blob id of r; write=true also stores it.
func (c *Client) HashObject(ctx context.Context, r io.Reader, write bool) (string, error) {
	args := []string{"hash-object", "--stdin"}
	if write {
		args = append(args, "-w")
	}
	out, err := c.RunInput(ctx, r, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// StageEntry is one `ls-files --stage` row.
type StageEntry struct {
	Mode  string
	OID   string
	Stage int
	Path  string
}

// LsFilesStage returns the index entries for a path (0 = merged; 1/2/3 during
// a conflict). ok is false when the path is not in the index.
func (c *Client) LsFilesStage(ctx context.Context, path string) ([]StageEntry, error) {
	out, err := c.Run(ctx, "ls-files", "--stage", "-z", "--", path)
	if err != nil {
		return nil, err
	}
	var entries []StageEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		// "<mode> <oid> <stage>\t<path>"
		meta, p, ok := strings.Cut(string(rec), "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 3 {
			continue
		}
		stage, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, fmt.Errorf("gitclient: parse stage %q: %w", f[2], err)
		}
		entries = append(entries, StageEntry{Mode: f[0], OID: f[1], Stage: stage, Path: p})
	}
	return entries, nil
}

// CatFileBlob returns the raw bytes of a blob given any object name
// (oid, ":path" for the index, "<rev>:path").
func (c *Client) CatFileBlob(ctx context.Context, name string) ([]byte, error) {
	return c.Run(ctx, "cat-file", "blob", name)
}

// CatFileBlobTo streams a blob to w.
func (c *Client) CatFileBlobTo(ctx context.Context, name string, w io.Writer) error {
	return c.RunStreams(ctx, nil, w, "cat-file", "blob", name)
}

// RevList returns commit ids for the given rev-list arguments.
func (c *Client) RevList(ctx context.Context, args ...string) ([]string, error) {
	out, err := c.RunString(ctx, append([]string{"rev-list"}, args...)...)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// DiffTreePaths returns the paths added or modified by commit relative to its
// first parent (or everything for a root commit).
func (c *Client) DiffTreePaths(ctx context.Context, commit string) ([]string, error) {
	out, err := c.Run(ctx, "diff-tree", "-r", "-z", "--root", "--no-commit-id", "--name-only", "--diff-filter=AM", commit)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			paths = append(paths, string(p))
		}
	}
	return paths, nil
}

// ---------------------------------------------------------------------------
// Lock contention
// ---------------------------------------------------------------------------

// ErrLockContention is the transient class: another process held Git's index
// or a ref lock for the moment we asked.
//
// FXVCS has no daemon. The CLI, the desktop application, the editor's Git
// integration and whatever else the operator runs all drive `git` against one
// worktree at the same time, so a collision on $GIT_DIR/index.lock is an
// ordinary event on a healthy machine rather than a failure worth reporting.
// The holder releases it in milliseconds; waiting is the whole remedy.
var ErrLockContention = errors.New("gitclient: git index or ref lock is held by another process")

// IsLockContention reports whether err is a failed invocation that could
// succeed on a retry because someone else held a Git lock.
//
// It matches on Git's own message because Git offers nothing better: every
// one of these exits 128 with a message naming the lock file, and exit codes
// alone cannot tell "someone else is mid-commit" from "your pathspec is
// wrong". The match is deliberately narrow — a lock file by name — so that a
// real failure is never retried into a delay.
func IsLockContention(err error) bool {
	var gerr *Error
	if !errors.As(err, &gerr) {
		return errors.Is(err, ErrLockContention)
	}
	s := strings.ToLower(gerr.Stderr)
	switch {
	case strings.Contains(s, "index.lock"):
		return true
	case strings.Contains(s, "cannot lock ref"), strings.Contains(s, "unable to lock"):
		return true
	case strings.Contains(s, ".lock") && strings.Contains(s, "file exists"):
		return true
	}
	return false
}

// Retry bounds a wait for a busy Git lock.
type Retry struct {
	// Attempts is the total number of invocations (0 or 1 = no retry).
	Attempts int
	// Backoff is the wait before the second attempt; it doubles thereafter.
	Backoff time.Duration
}

// DefaultRetry waits out an ordinary collision — roughly 1.5s across five
// attempts — without turning a genuinely stuck lock into a long stall.
var DefaultRetry = Retry{Attempts: 5, Backoff: 100 * time.Millisecond}

// wait sleeps the backoff for attempt n (1-based), jittered, and reports
// whether the context survived it. Jitter matters because the processes that
// collide here are usually reacting to the same event — a file written, a
// command finished — and would otherwise retry in lockstep forever.
func (rt Retry) wait(ctx context.Context, n int) bool {
	d := rt.Backoff << (n - 1)
	d += time.Duration(rand.Int64N(int64(d)/2 + 1)) //nolint:gosec // G404: backoff jitter, not a secret
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// RunRetry is Run for a command that may be run again unchanged, retrying
// while another process holds the Git index or a ref lock.
//
// Only idempotent commands may use it: it cannot tell a command that did
// nothing from one that did its work and then failed to write the index.
func (c *Client) RunRetry(ctx context.Context, rt Retry, args ...string) ([]byte, error) {
	attempts := rt.Attempts
	if attempts < 1 {
		attempts = 1
	}
	for n := 1; ; n++ {
		out, err := c.Run(ctx, args...)
		if err == nil || n >= attempts || !IsLockContention(err) {
			return out, err
		}
		if !rt.wait(ctx, n) {
			return out, err
		}
	}
}
