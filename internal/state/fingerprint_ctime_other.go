//go:build !linux && !darwin

package state

import "os"

// Platforms whose stat this package does not decode contribute no change
// time. The fingerprint is weaker there, exactly as it already is for file
// identity on Windows, and every match it reports stays advisory.
func ctimeNS(os.FileInfo) int64 { return 0 }
