//go:build !windows

package gitclient

import "os/exec"

func configureCommand(_ *exec.Cmd) {}
