//go:build unix

package state

import (
	"fmt"
	"os"
	"syscall"
)

func fileID(fi os.FileInfo) string {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%x:%x", uint64(st.Dev), uint64(st.Ino))
	}
	return ""
}
