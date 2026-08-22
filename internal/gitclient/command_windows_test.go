//go:build windows

package gitclient

import (
	"os/exec"
	"testing"
)

func TestConfigureCommandKeepsAnInheritableHiddenConsole(t *testing.T) {
	cmd := exec.Command("git", "version")
	configureCommand(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Fatal("git command is not hidden")
	}
	if cmd.SysProcAttr.CreationFlags != 0 {
		t.Fatalf("creation flags = %#x; no-console flags leave Git descendants free to allocate visible consoles", cmd.SysProcAttr.CreationFlags)
	}
}
