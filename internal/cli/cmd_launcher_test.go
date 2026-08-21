package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLauncherEnsureCommand(t *testing.T) {
	dir := gitRepo(t)
	target := filepath.Join(t.TempDir(), "fxvcs", "bin", "fxvcs")
	code, out, errOut := run(t, dir, "--json", "launcher", "ensure", "--path", target)
	if code != 0 {
		t.Fatalf("launcher ensure: %d %s %s", code, out, errOut)
	}
	var got struct {
		Launcher struct {
			Path    string `json:"path"`
			Outcome string `json:"outcome"`
			Changed bool   `json:"changed"`
		} `json:"launcher"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Launcher.Path != target || got.Launcher.Outcome != "installed" || !got.Launcher.Changed {
		t.Fatalf("result: %+v", got)
	}
	if fi, err := os.Stat(target); err != nil || fi.Size() == 0 {
		t.Fatalf("launcher: %v %v", fi, err)
	}
	code, out, errOut = run(t, dir, "--json", "launcher", "ensure", "--path", target)
	if code != 0 || errOut != "" {
		t.Fatalf("idempotent ensure: %d %s %s", code, out, errOut)
	}
}
