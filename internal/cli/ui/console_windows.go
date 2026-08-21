//go:build windows

package ui

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// enableVirtualTerminal switches a Windows console to VT processing so ANSI
// colours and cursor movement render. It reports false when the console
// refuses (legacy conhost without VT support), in which case decoration and
// live redraw stay off.
func enableVirtualTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
