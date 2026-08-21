//go:build !unix

package state

import "os"

// Windows exposes a file index only through an open handle; the stat cache
// relies on size+mtime there and treats every fingerprint match as advisory.
func fileID(os.FileInfo) string { return "" }
