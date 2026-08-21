//go:build !windows

package ui

import "io"

// enableVirtualTerminal is a no-op outside Windows: terminals speak ANSI.
func enableVirtualTerminal(io.Writer) bool { return true }
