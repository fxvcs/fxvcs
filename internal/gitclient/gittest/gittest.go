// Package gittest creates throwaway Git repositories for tests. Every
// repository runs with an isolated environment (temporary HOME, no system or
// user config, fixed author/committer) so host settings such as core.autocrlf,
// hooksPath, or credential helpers cannot leak in.
package gittest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/gitclient"
)

// RequireGit skips the test when git is not on PATH.
func RequireGit(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

// Repo is a temporary repository.
type Repo struct {
	T      testing.TB
	Root   string
	Env    []string
	Client *gitclient.Client
}

// IsolatedEnv builds a git-safe environment rooted at home. Extra entries are
// appended (later entries win in git's environment handling for most
// variables, but callers should not duplicate keys).
func IsolatedEnv(home string, extra ...string) []string {
	cfg := filepath.Join(home, "gitconfig")
	env := []string{
		"HOME=" + home,
		"USERPROFILE=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + cfg,
		"GIT_AUTHOR_NAME=fxvcs test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=fxvcs test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	}
	// Keep PATH and platform essentials so git (and the driver binary) can run.
	for _, k := range []string{"PATH", "SYSTEMROOT", "SystemRoot", "TEMP", "TMP", "TMPDIR", "COMSPEC", "PATHEXT", "GOCOVERDIR", "GOTMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, extra...)
}

// NewEnvHome creates a fresh isolated environment in a temp dir with a global
// git config that pins deterministic defaults.
func NewEnvHome(t testing.TB, extra ...string) (home string, env []string) {
	t.Helper()
	home = t.TempDir()
	global := "[init]\n\tdefaultBranch = main\n[core]\n\tautocrlf = false\n[protocol \"file\"]\n\tallow = always\n"
	if err := os.WriteFile(filepath.Join(home, "gitconfig"), []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, IsolatedEnv(home, extra...)
}

// InitRepo creates an initialized non-bare repository with an isolated
// environment. extra environment entries (KEY=VALUE) are added for every git
// invocation and therefore inherited by filters, hooks, and drivers.
func InitRepo(t testing.TB, extra ...string) *Repo {
	t.Helper()
	RequireGit(t)
	_, env := NewEnvHome(t, extra...)
	root := t.TempDir()
	r := &Repo{T: t, Root: root, Env: env, Client: gitclient.New(root).WithEnv(env)}
	r.Git("init", "-q")
	return r
}

// BareRemote creates a bare repository and returns its path.
func BareRemote(t testing.TB) string {
	t.Helper()
	RequireGit(t)
	dir := t.TempDir()
	_, env := NewEnvHome(t)
	c := gitclient.New(dir).WithEnv(env)
	if _, err := c.Run(context.Background(), "init", "-q", "--bare"); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Clone clones url into a new temp dir with the same isolated env as r
// (so filter/driver configuration supplied through env or -c is inherited).
// extraArgs are passed to `git clone` before the URL (e.g. -c key=value).
func (r *Repo) Clone(url string, extraArgs ...string) *Repo {
	r.T.Helper()
	dest := r.T.TempDir()
	args := append([]string{"clone", "-q"}, extraArgs...)
	args = append(args, url, dest)
	if _, err := r.Client.WithDir(r.T.TempDir()).Run(context.Background(), args...); err != nil {
		r.T.Fatalf("clone: %v", err)
	}
	return &Repo{T: r.T, Root: dest, Env: r.Env, Client: gitclient.New(dest).WithEnv(r.Env)}
}

// Git runs git and fails the test on error. Returns trimmed stdout.
func (r *Repo) Git(args ...string) string {
	r.T.Helper()
	out, err := r.Client.RunString(context.Background(), args...)
	if err != nil {
		r.T.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// GitErr runs git and returns stdout, stderr and the error.
func (r *Repo) GitErr(args ...string) (stdout, stderr string, err error) {
	r.T.Helper()
	out, err := r.Client.Run(context.Background(), args...)
	if err != nil {
		var ge *gitclient.Error
		if asGitErr(err, &ge) {
			return string(out), ge.Stderr, err
		}
		return string(out), err.Error(), err
	}
	return string(out), "", nil
}

func asGitErr(err error, target **gitclient.Error) bool {
	e, ok := err.(*gitclient.Error)
	if ok {
		*target = e
	}
	return ok
}

// WriteFile writes rel under the working tree (creating parents).
func (r *Repo) WriteFile(rel string, data []byte) {
	r.T.Helper()
	p := filepath.Join(r.Root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.T.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		r.T.Fatal(err)
	}
}

// ReadFile reads rel from the working tree.
func (r *Repo) ReadFile(rel string) []byte {
	r.T.Helper()
	b, err := os.ReadFile(filepath.Join(r.Root, filepath.FromSlash(rel)))
	if err != nil {
		r.T.Fatal(err)
	}
	return b
}

// Commit stages everything and commits, returning the commit id.
func (r *Repo) Commit(msg string) string {
	r.T.Helper()
	r.Git("add", "-A")
	r.Git("commit", "-q", "--allow-empty", "-m", msg)
	return r.Git("rev-parse", "HEAD")
}

// SetEnv appends KEY=VALUE entries for subsequent git invocations.
func (r *Repo) SetEnv(kv ...string) {
	r.Env = append(r.Env, kv...)
	r.Client = r.Client.WithEnv(r.Env)
}
