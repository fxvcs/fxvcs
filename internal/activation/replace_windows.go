//go:build windows

package activation

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

type fileRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

func replaceCurrent(from, to string) error {
	if filepath.Dir(from) != filepath.Dir(to) {
		return fmt.Errorf("activation: Windows pointer replacement must stay in one directory")
	}
	fromp, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(fromp, windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)

	// MoveFileEx cannot replace a directory symlink. The NT rename operation's
	// POSIX semantics can atomically replace the reparse point itself rather
	// than treating it as a non-replaceable directory.
	name, err := windows.UTF16FromString(filepath.Base(to))
	if err != nil {
		return err
	}
	nameBytes := (len(name) - 1) * 2 // exclude UTF-16 terminator
	var dummy fileRenameInformation
	buffer := make([]byte, int(unsafe.Offsetof(dummy.FileName))+nameBytes)
	info := (*fileRenameInformation)(unsafe.Pointer(&buffer[0]))
	info.ReplaceIfExists = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32(nameBytes)
	copy((*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&info.FileName[0]))[:nameBytes/2:nameBytes/2], name)
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtSetInformationFile(handle, &status, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation); err != nil {
		return fmt.Errorf("activation: replace current pointer: %w", err)
	}
	return nil
}
