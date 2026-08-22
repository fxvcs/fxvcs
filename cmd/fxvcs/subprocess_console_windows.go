//go:build windows

package main

import "syscall"

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	user32           = syscall.NewLazyDLL("user32.dll")
	getConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	allocConsole     = kernel32.NewProc("AllocConsole")
	showWindow       = user32.NewProc("ShowWindow")
)

// prepareSubprocessConsole is a no-op for the normal console CLI, which already
// has a console. The GUI-subsystem helper embedded by Desktop has none, so it
// allocates and hides one before running any driver. Git processes and their
// own console-subsystem descendants then inherit it rather than flashing new
// consoles on screen.
func prepareSubprocessConsole() {
	if hwnd, _, _ := getConsoleWindow.Call(); hwnd != 0 {
		return
	}
	if ok, _, _ := allocConsole.Call(); ok == 0 {
		return
	}
	if hwnd, _, _ := getConsoleWindow.Call(); hwnd != 0 {
		const swHide = 0
		showWindow.Call(hwnd, swHide)
	}
}
