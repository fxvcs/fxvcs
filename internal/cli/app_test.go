package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func repoWithMinimum(t *testing.T, minimum string) string {
	dir := t.TempDir()
	if git, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command(git, "init", "-q", dir)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(dir, ".nogitconfig"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
	}
	_ = os.MkdirAll(filepath.Join(dir, ".fxvcs"), 0o755)
	cfg := "apiVersion: fxvcs.dev/v1alpha1\nkind: Repository\nmetadata:\n  repositoryID: repo_t\n  storageDomainID: domain_t\n  name: t\nspec:\n  profile: generic\n  minimumFXVCSVersion: \"" + minimum + "\"\n  gitIntegrationVersion: 1\n  storage:\n    remotes: []\n"
	_ = os.WriteFile(filepath.Join(dir, ".fxvcs", "repository.yaml"), []byte(cfg), 0o644)
	return dir
}

func run(t *testing.T, dir string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Getwd: func() (string, error) { return dir, nil }}
	code := Run(context.Background(), env, append([]string{"fxvcs"}, args...))
	return code, out.String(), errb.String()
}

func TestVersionAndCompat(t *testing.T) {
	dir := repoWithMinimum(t, "0.0.0-dev")
	if code, out, _ := run(t, dir, "version"); code != 0 || !bytes.Contains([]byte(out), []byte("fxvcs 0.0.0-dev")) || bytes.Contains([]byte(out), []byte("placeholder v")) {
		t.Fatalf("version: %d %s", code, out)
	}
	if code, out, _ := run(t, dir, "--verbose", "version"); code != 0 || !bytes.Contains([]byte(out), []byte("placeholder v1")) {
		t.Fatalf("verbose version: %d %s", code, out)
	}
	code, out, _ := run(t, dir, "--json", "compat")
	if code != ExitOK {
		t.Fatalf("compat: %d %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil || res["repositoryID"] != "repo_t" {
		t.Fatalf("bad json: %v %s", err, out)
	}
}

func TestUpgradeRequiredExitCodeAndOnlyCompatRuns(t *testing.T) {
	dir := repoWithMinimum(t, "99.0.0")
	code, out, _ := run(t, dir, "compat")
	if code != ExitUpgradeRequired || !bytes.Contains([]byte(out), []byte("UPGRADE REQUIRED")) {
		t.Fatalf("compat: %d %s", code, out)
	}
	// Human mode: plain cause plus the exact next step, no SDK code.
	if code, _, errOut := run(t, dir, "status"); code != ExitUpgradeRequired || !bytes.Contains([]byte(errOut), []byte("error: this repository needs fxvcs 99.0.0 or newer")) || !bytes.Contains([]byte(errOut), []byte("hint: install fxvcs 99.0.0")) || bytes.Contains([]byte(errOut), []byte("upgrade-required")) {
		t.Fatalf("status human: %d %s", code, errOut)
	}
	if _, _, errOut := run(t, dir, "--verbose", "status"); !bytes.Contains([]byte(errOut), []byte("code: upgrade-required")) {
		t.Fatalf("verbose should show the code: %s", errOut)
	}
	for _, args := range [][]string{{"load"}, {"hydrate"}, {"status"}, {"upgrade", "--check"}, {"setup"}, {"objects", "publish"}, {"resources", "sync"}, {"info"}} {
		code, _, errOut := run(t, dir, append([]string{"--json"}, args...)...)
		if code != ExitUpgradeRequired {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
		var payload map[string]map[string]any
		if err := json.Unmarshal([]byte(errOut), &payload); err != nil || payload["error"]["code"] != "upgrade-required" {
			t.Errorf("%v: bad error payload %s", args, errOut)
		}
	}
}

func TestUnknownPathIsNotFoundExit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := repoWithMinimum(t, "0.0.0-dev")
	// A path that is not a managed asset in the index yields a clean, typed
	// error (not a panic and not a stack trace); the exit code is a failure.
	code, out, errOut := run(t, dir, "load", "x.ytd")
	if code != ExitFailure || bytes.Contains([]byte(errOut), []byte("panic")) {
		t.Fatalf("got %d %s", code, errOut)
	}
	if !bytes.Contains([]byte(out), []byte("skipped")) || !bytes.Contains([]byte(out), []byte("not tracked by fxvcs (fxvcs track x.ytd")) {
		t.Fatalf("per-item outcome missing: %s", out)
	}
	if !bytes.Contains([]byte(errOut), []byte("error: nothing could be done")) || !bytes.Contains([]byte(errOut), []byte("hint: fxvcs track x.ytd")) {
		t.Fatalf("error shape: %s", errOut)
	}
}

func TestUnknownCommandsAndUsageExit2(t *testing.T) {
	dir := repoWithMinimum(t, "0.0.0-dev")
	code, _, errOut := run(t, dir, "doctr")
	if code != ExitUsage || !bytes.Contains([]byte(errOut), []byte("unknown command 'doctr'. Did you mean 'doctor'?")) || !bytes.Contains([]byte(errOut), []byte("hint: fxvcs --help")) {
		t.Fatalf("unknown: %d %s", code, errOut)
	}
	code, _, errOut = run(t, dir, "objects", "publsh")
	if code != ExitUsage || !bytes.Contains([]byte(errOut), []byte("Did you mean 'publish'?")) || !bytes.Contains([]byte(errOut), []byte("fxvcs objects --help")) {
		t.Fatalf("unknown sub: %d %s", code, errOut)
	}
	code, _, errOut = run(t, dir, "status", "--nope")
	if code != ExitUsage || !bytes.Contains([]byte(errOut), []byte("error: flag provided but not defined: -nope")) || !bytes.Contains([]byte(errOut), []byte("hint: fxvcs status --help")) {
		t.Fatalf("bad flag: %d %s", code, errOut)
	}
	code, out, _ := run(t, dir, "--help")
	for _, want := range []string{"Daily:", "Set up:", "Fix:", "Sharing:", "FiveM:", "load, download", "unload, undownload"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("root help lacks %q:\n%s", want, out)
		}
	}
	if code != ExitOK || bytes.Contains([]byte(out), []byte("hydrate")) {
		t.Fatalf("hidden aliases leaked into help: %d %s", code, out)
	}
	if _, out, _ := run(t, dir, "load", "--help"); !bytes.Contains([]byte(out), []byte("Examples:")) || !bytes.Contains([]byte(out), []byte("fxvcs load --glob '[maps]/**/*.ymap'")) {
		t.Fatalf("command help lacks examples: %s", out)
	}
	if code, _, errOut := run(t, t.TempDir(), "status"); code != ExitFailure || !bytes.Contains([]byte(errOut), []byte("error: this folder is not an fxvcs repository")) || !bytes.Contains([]byte(errOut), []byte("hint: run this inside your server resource repository, or set one up with: fxvcs init --profile fivem")) {
		t.Fatalf("not a repo: %d %s", code, errOut)
	}
}
