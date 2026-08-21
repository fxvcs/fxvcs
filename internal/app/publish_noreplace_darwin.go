//go:build darwin

package app

import "golang.org/x/sys/unix"

func renameDirectoryNoReplace(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
