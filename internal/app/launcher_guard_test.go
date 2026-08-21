package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A Go test binary must never be installed as a real launcher: only temp
// destinations are allowed while tests run.
func TestTestBinaryLauncherGuard(t *testing.T) {
	exe, _ := os.Executable()
	if !isTestBinary(exe) {
		t.Skip("not running as a test binary")
	}
	tmpDest := filepath.Join(t.TempDir(), "bin", "fxvcs")
	if err := installLauncherFrom(exe, tmpDest); err != nil {
		t.Fatalf("temp destination refused: %v", err)
	}
	home, _ := os.UserHomeDir()
	realDest := filepath.Join(home, ".local", "share", "fxvcs-guard-test", "bin", "fxvcs")
	err := installLauncherFrom(exe, realDest)
	if !errors.Is(err, ErrTestBinaryLauncher) {
		_ = os.RemoveAll(filepath.Dir(filepath.Dir(realDest)))
		t.Fatalf("real destination accepted: %v", err)
	}
	if _, err := os.Stat(realDest); err == nil {
		_ = os.RemoveAll(filepath.Dir(filepath.Dir(realDest)))
		t.Fatal("launcher written despite guard")
	}
}
