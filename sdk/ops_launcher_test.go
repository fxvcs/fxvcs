package sdk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLauncherEnsureUsesEmbeddingHostSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "desktop-bundle", "fxvcs")
	target := filepath.Join(dir, "user-data", "fxvcs", "bin", "fxvcs")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("bundled-headless-fxvcs"), 0o755); err != nil { //nolint:gosec // executable fixture
		t.Fatal(err)
	}
	c, err := New(Options{InstalledVersion: "2.0.0", LauncherSourcePath: source})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.LauncherEnsure(context.Background(), LauncherEnsureRequest{TargetPath: target})
	if err != nil {
		t.Fatal(err)
	}
	if res.Launcher == nil || !res.Launcher.Changed || res.Launcher.Outcome != "installed" || res.Launcher.Path != target {
		t.Fatalf("result: %+v", res)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "bundled-headless-fxvcs" {
		t.Fatalf("installed helper: %q %v", got, err)
	}
	again, err := c.LauncherEnsure(context.Background(), LauncherEnsureRequest{TargetPath: target})
	if err != nil || again.Launcher == nil || again.Launcher.Outcome != "already-current" || again.Launcher.Changed {
		t.Fatalf("idempotent result: %+v %v", again, err)
	}
}
