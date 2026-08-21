//go:build !windows

package app

import "os"

func replaceLauncherFile(from, to string) error {
	return os.Rename(from, to)
}
