//go:build !windows

package activation

import "os"

func replaceCurrent(from, to string) error {
	return os.Rename(from, to)
}
