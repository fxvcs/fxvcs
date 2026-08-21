//go:build linux

package state

import (
	"os"
	"syscall"
)

// ctimeNS reports the inode change time, which a writer cannot set: utimes
// moves mtime, and nothing an unprivileged process can do moves ctime.
func ctimeNS(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Sec*1e9 + st.Ctim.Nsec
	}
	return 0
}
