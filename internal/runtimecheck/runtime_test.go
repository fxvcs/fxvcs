package runtimecheck

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
)

func TestValidateSystemResourcesRecordsInventory(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	for _, name := range []string{"monitor", "webadmin"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	evidence, err := ValidateSystemResources(root, []domain.RuntimeResource{{ID: "runtime:system/monitor", Source: domain.ResourceSourceRuntime, Activation: domain.ActivationEnabled}})
	if err != nil || evidence.SystemResourcesDigest == "" || len(evidence.Inventory) != 2 || len(evidence.Verified) != 1 {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}
}

func TestValidateSystemResourcesRefusesMissingAndSymlinkedEntries(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	requirement := []domain.RuntimeResource{{ID: "runtime:system/monitor", Source: domain.ResourceSourceRuntime, Activation: domain.ActivationEnabled}}
	if _, err := ValidateSystemResources(root, requirement); !errors.Is(err, ErrRuntimeRequirement) {
		t.Fatalf("missing runtime accepted: %v", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "monitor")); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateSystemResources(root, requirement); !errors.Is(err, ErrRuntimeRequirement) {
		t.Fatalf("symlink runtime accepted: %v", err)
	}
}
