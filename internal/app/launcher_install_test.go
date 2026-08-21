package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/format"
)

func TestEnsureLauncherNewestWins(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "candidate", "fxvcs")
	target := filepath.Join(dir, "installed", "bin", "fxvcs")
	writeLauncherTestFile(t, source, "candidate-v2")

	originalProbe := probeLauncherVersion
	installedVersion := "1.0.0"
	probeLauncherVersion = func(context.Context, string) (string, error) { return installedVersion, nil }
	t.Cleanup(func() { probeLauncherVersion = originalProbe })

	in := LauncherEnsureInput{SourcePath: source, TargetPath: target, CandidateVersion: format.MustParseVersion("2.0.0"), LockTimeout: time.Second}
	res, err := EnsureLauncher(context.Background(), in)
	if err != nil || res.Outcome != LauncherInstalled || !res.Changed {
		t.Fatalf("install: %+v %v", res, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "candidate-v2" {
		t.Fatalf("installed bytes: %q", got)
	}

	// Identical bytes are current without needing a version probe.
	probeLauncherVersion = func(context.Context, string) (string, error) { return "", errors.New("must not probe") }
	res, err = EnsureLauncher(context.Background(), in)
	if err != nil || res.Outcome != LauncherAlreadyCurrent || res.Changed {
		t.Fatalf("current: %+v %v", res, err)
	}

	// A newer installed build wins and must not be overwritten.
	writeLauncherTestFile(t, target, "installed-v3")
	installedVersion = "3.0.0"
	probeLauncherVersion = func(context.Context, string) (string, error) { return installedVersion, nil }
	res, err = EnsureLauncher(context.Background(), in)
	if err != nil || res.Outcome != LauncherNewerPresent || res.Changed {
		t.Fatalf("newer present: %+v %v", res, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "installed-v3" {
		t.Fatalf("newer launcher overwritten: %q", got)
	}

	// An older installed build is atomically updated.
	installedVersion = "1.0.0"
	res, err = EnsureLauncher(context.Background(), in)
	if err != nil || res.Outcome != LauncherUpdated || !res.Changed {
		t.Fatalf("update: %+v %v", res, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "candidate-v2" {
		t.Fatalf("updated bytes: %q", got)
	}
}

func TestEnsureLauncherEqualVersionConflictRequiresRepair(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "candidate", "fxvcs")
	target := filepath.Join(dir, "installed", "bin", "fxvcs")
	writeLauncherTestFile(t, source, "candidate")
	writeLauncherTestFile(t, target, "different")

	originalProbe := probeLauncherVersion
	probeLauncherVersion = func(context.Context, string) (string, error) { return "2.0.0", nil }
	t.Cleanup(func() { probeLauncherVersion = originalProbe })
	in := LauncherEnsureInput{SourcePath: source, TargetPath: target, CandidateVersion: format.MustParseVersion("2.0.0"), LockTimeout: time.Second}
	res, err := EnsureLauncher(context.Background(), in)
	if !errors.Is(err, ErrLauncherVersionConflict) || res.Outcome != LauncherVersionConflict {
		t.Fatalf("conflict: %+v %v", res, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "different" {
		t.Fatalf("conflicting launcher overwritten: %q", got)
	}
	in.RepairEqualVersion = true
	res, err = EnsureLauncher(context.Background(), in)
	if err != nil || res.Outcome != LauncherUpdated || !res.Changed {
		t.Fatalf("repair: %+v %v", res, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "candidate" {
		t.Fatalf("repair bytes: %q", got)
	}
}

func writeLauncherTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // executable fixture
		t.Fatal(err)
	}
}
