//go:build windows

package diskspace

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Available returns bytes available to the current process on the volume that
// will contain path. Missing paths are resolved to their nearest existing
// ancestor without mutating the filesystem during planning.
func Available(path string) (int64, error) {
	probe := filepath.Clean(path)
	for {
		if _, err := os.Stat(probe); err == nil {
			name, err := windows.UTF16PtrFromString(probe)
			if err != nil {
				return 0, err
			}
			var available uint64
			if err := windows.GetDiskFreeSpaceEx(name, &available, nil, nil); err != nil {
				return 0, err
			}
			const maxInt64 = int64(^uint64(0) >> 1)
			if available > uint64(maxInt64) {
				return maxInt64, nil
			}
			return int64(available), nil
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
