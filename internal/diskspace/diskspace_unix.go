//go:build !windows

package diskspace

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Available returns bytes available to an unprivileged process on the
// filesystem that will contain path. If path does not exist, its nearest
// existing ancestor is observed without creating directories during planning.
func Available(path string) (int64, error) {
	probe := filepath.Clean(path)
	for {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(probe, &stat); err == nil {
			return int64(stat.Bavail) * int64(stat.Bsize), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return 0, os.ErrNotExist
		}
		probe = parent
	}
}
