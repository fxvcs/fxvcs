//go:build !linux && !darwin && !windows

package secrets

import "os"

func protectCreatedFile(string) error { return nil }

func protectedFile(_ string, info os.FileInfo) bool {
	return info.Mode().Perm()&0o077 == 0 && info.Mode().Perm()&0o400 != 0 && ownedByCurrentUser(info)
}
