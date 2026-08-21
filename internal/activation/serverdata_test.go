package activation

import (
	"os"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/layout"
)

func TestEnsureServerDataLinksIsExactAndNonDestructive(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roots := layout.Roots{DeployRoot: filepath.Join(base, "deploy"), ServerData: filepath.Join(base, "server")}
	if err := os.MkdirAll(roots.DeployRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(roots.ServerData, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureServerDataLinks(roots); err != nil {
		t.Fatal(err)
	}
	if err := EnsureServerDataLinks(roots); err != nil {
		t.Fatalf("idempotent ensure: %v", err)
	}
	for name, expected := range layout.ServerDataLinks(roots) {
		if err := validateServerDataLink(filepath.Join(roots.ServerData, name), expected); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	other := layout.Roots{DeployRoot: filepath.Join(base, "other-deploy"), ServerData: filepath.Join(base, "other-server")}
	if err := os.MkdirAll(other.DeployRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other.ServerData, 0o755); err != nil {
		t.Fatal(err)
	}
	operatorFile := filepath.Join(other.ServerData, layout.RuntimeCfg)
	if err := os.WriteFile(operatorFile, []byte("operator owned"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureServerDataLinks(other); err == nil {
		t.Fatal("operator-owned collision was replaced")
	}
	data, _ := os.ReadFile(operatorFile)
	if string(data) != "operator owned" {
		t.Fatalf("operator file changed: %q", data)
	}
	if _, err := os.Lstat(filepath.Join(other.ServerData, layout.ResourcesDir)); !os.IsNotExist(err) {
		t.Fatalf("preflight left partial links: %v", err)
	}
}
