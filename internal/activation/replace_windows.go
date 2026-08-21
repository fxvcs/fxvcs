//go:build windows

package activation

import "golang.org/x/sys/windows"

func replaceCurrent(from, to string) error {
	fromp, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	top, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromp, top, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
