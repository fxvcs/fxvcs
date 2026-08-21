//go:build !linux && !darwin && !windows

package app

import "errors"

func renameDirectoryNoReplace(string, string) error {
	return errors.New("reconcile: atomic no-replace directory publication is unsupported on this platform")
}
