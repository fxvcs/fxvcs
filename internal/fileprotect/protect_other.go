//go:build !linux && !darwin && !windows

// Package fileprotect applies the platform confidentiality boundary to files
// whose bytes contain resolved secret material.
package fileprotect

func Protect(string) error { return nil }
