//go:build windows

package app

import "os"

// Windows rename refuses an existing directory target.
func renameDirectoryNoReplace(from, to string) error { return os.Rename(from, to) }
