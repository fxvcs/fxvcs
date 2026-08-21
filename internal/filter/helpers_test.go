package filter

import (
	"crypto/sha256"
	"os"

	"fxvcs.dev/fxvcs/internal/cache"
)

// writeFileForce overwrites a read-only file (cached objects are 0444).
func writeFileForce(path string, data []byte) error {
	_ = os.Chmod(path, 0o644)
	return os.WriteFile(path, data, 0o644)
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return cache.Digest(sum[:])
}
