//go:build !linux && !darwin

package secrets

import "os"

func ownedByCurrentUser(os.FileInfo) bool { return true }
