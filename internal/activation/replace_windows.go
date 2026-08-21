//go:build windows

package activation

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileRenameInformationEx is FILE_RENAME_INFORMATION for the
// FileRenameInformationEx class, where the leading field carries
// FILE_RENAME_* flag bits rather than a BOOLEAN.
type fileRenameInformationEx struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// fileRenameInformationExClass is not exported by x/sys/windows.
const fileRenameInformationExClass = 65

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
	dirp, err := windows.UTF16PtrFromString(filepath.Dir(to))
	if err != nil {
		return err
	}
	dirHandle, err := windows.CreateFile(dirp,
		windows.GENERIC_WRITE|windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(dirHandle)

	// MoveFileEx cannot replace a directory symlink. The FileRenameInformationEx
	// class with POSIX semantics can atomically replace the reparse point itself
	// rather than treating it as a non-replaceable directory; the plain
	// FileRenameInformation class ignores these flag bits and fails with
	// STATUS_ACCESS_DENIED on directory targets.
	name, err := windows.UTF16FromString(filepath.Base(to))
	if err != nil {
		return err
	}
	nameBytes := (len(name) - 1) * 2 // exclude UTF-16 terminator
	var dummy fileRenameInformationEx
	buffer := make([]byte, int(unsafe.Offsetof(dummy.FileName))+nameBytes)
	info := (*fileRenameInformationEx)(unsafe.Pointer(&buffer[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.RootDirectory = dirHandle
	info.FileNameLength = uint32(nameBytes)
	copy((*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&info.FileName[0]))[:nameBytes/2:nameBytes/2], name)
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtSetInformationFile(handle, &status, &buffer[0], uint32(len(buffer)), fileRenameInformationExClass); err != nil {
		return fmt.Errorf("activation: replace current pointer: %w", err)
	}
	return nil
}
