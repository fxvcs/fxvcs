//go:build !windows

// Package durablefs contains platform-specific filesystem durability
// primitives shared by fxvcs' atomic writers.
package durablefs

import "os"

// SyncDir makes directory-entry changes durable where the host supports it.
func SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
