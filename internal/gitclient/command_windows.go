//go:build windows

package gitclient

import (
	"os/exec"
	"syscall"
)

func configureCommand(cmd *exec.Cmd) {
	// Give Git a hidden console rather than no console. Console-subsystem
	// descendants such as sh, ssh, credential helpers, and hooks inherit it
	// instead of allocating visible consoles that steal focus from Desktop.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
