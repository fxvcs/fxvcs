package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo creates an isolated Git repository and points the process
// environment at the same isolation so the engine's git inherits it.
func gitRepo(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".nogitconfig"))
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.invalid")
	t.Setenv("FXVCS_CACHE_DIR", t.TempDir())
	cmd := exec.Command(git, "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}

func TestLifecycleCommands(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.ytd"), []byte("asset"), 0o644); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(t.TempDir(), "fxvcs-launcher")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(exe) //nolint:gosec // test fixture copies its own executable
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, binary, 0o755); err != nil { //nolint:gosec // stand-in launcher
		t.Fatal(err)
	}
	code, out, errOut := run(t, dir, "--json", "init", "--name", "demo", "--launcher", launcher)
	if code != 0 {
		t.Fatalf("init: %d %s %s", code, out, errOut)
	}
	var initRes map[string]any
	if err := json.Unmarshal([]byte(out), &initRes); err != nil || initRes["profile"] != "generic" || initRes["repositoryID"] == "" {
		t.Fatalf("init json: %v %s", err, out)
	}
	if code, out, errOut := run(t, dir, "init"); code == 0 || !strings.Contains(errOut, "error: this repository is already set up for fxvcs") || !strings.Contains(errOut, "hint: fxvcs setup") {
		t.Fatalf("second init: %d %s %s", code, out, errOut)
	}
	if code, out, _ := run(t, dir, "track", "--glob", "*.ytd"); code != 0 || !strings.Contains(out, "tracking *.ytd from the next git add") || !strings.Contains(out, "added  *.ytd") {
		t.Fatalf("track: %d %s", code, out)
	}
	if code, _, errOut := run(t, dir, "track", "--glob", "*.lua"); code != ExitUsage || !strings.Contains(errOut, "--force") {
		t.Fatalf("track source glob: %d %s", code, errOut)
	}
	if code, out, _ := run(t, dir, "tracking", "check", "a.ytd"); code != 0 || !strings.Contains(out, "a.ytd is tracked by fxvcs (rule *.ytd from the fxvcs section of .gitattributes)") {
		t.Fatalf("tracking check: %d %s", code, out)
	}
	if code, out, _ := run(t, dir, "scan", "--min-bytes", "1"); code != 0 || !strings.Contains(out, "a.ytd") || !strings.Contains(out, "tracked") {
		t.Fatalf("scan: %d %s", code, out)
	}
	// Up to date (the stub launcher cannot report a version: a note only).
	if code, out, errOut := run(t, dir, "upgrade", "--check"); code != 0 || !strings.Contains(out, "up to date") {
		t.Fatalf("check: %d %s %s", code, out, errOut)
	}
	// Drift: --check exits 1, plan is a dry run, apply fixes, --check passes.
	if err := os.Remove(filepath.Join(dir, ".git", "hooks", "pre-push")); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run(t, dir, "upgrade", "--check"); code != ExitFailure || !strings.Contains(out, "pre-push check") || !strings.Contains(errOut, "error: this repository or clone needs an upgrade") || !strings.Contains(errOut, "hint: fxvcs upgrade --apply") {
		t.Fatalf("check drift: %d %s %s", code, out, errOut)
	}
	if code, out, _ := run(t, dir, "upgrade"); code != 0 || !strings.Contains(out, "dry run") {
		t.Fatalf("plan: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", "pre-push")); err == nil {
		t.Fatal("plan wrote the hook")
	}
	if code, out, _ := run(t, dir, "upgrade", "--apply"); code != 0 || !strings.Contains(out, "applied") {
		t.Fatalf("apply: %d %s", code, out)
	}
	if code, _, _ := run(t, dir, "upgrade", "--check"); code != 0 {
		t.Fatalf("check after apply: %d", code)
	}
	if code, out, _ := run(t, dir, "untrack", "--glob", "*.ytd"); code != 0 || !strings.Contains(out, "removed  *.ytd") {
		t.Fatalf("untrack: %d %s", code, out)
	}
	if code, _, errOut := run(t, dir, "clone"); code != ExitUsage || !strings.Contains(errOut, "error: clone requires a URL") || !strings.Contains(errOut, "hint: fxvcs clone <url>") {
		t.Fatalf("clone usage: %d %s", code, errOut)
	}
}
