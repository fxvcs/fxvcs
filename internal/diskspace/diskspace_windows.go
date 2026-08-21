//go:build windows

package diskspace

import "errors"

// Available is intentionally unsupported until the Windows agent filesystem
// contract has an independently tested implementation.
func Available(string) (int64, error) {
	return 0, errors.New("diskspace: Windows capacity observation is not implemented")
}
