//go:build windows

// Package durablefs contains platform-specific filesystem durability
// primitives shared by fxvcs' atomic writers.
package durablefs

// SyncDir is a no-op on Windows. os.File.Sync on a directory handle returns
// ERROR_ACCESS_DENIED there; the file itself is flushed before every rename.
func SyncDir(string) error { return nil }
