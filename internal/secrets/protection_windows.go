//go:build windows

package secrets

import (
	"os"
	"unsafe"

	"fxvcs.dev/fxvcs/internal/fileprotect"
	"golang.org/x/sys/windows"
)

var getAce = windows.NewLazySystemDLL("advapi32.dll").NewProc("GetAce")

func protectCreatedFile(path string) error {
	return fileprotect.Protect(path)
}

// protectedFile applies the owner-only contract to Windows ACLs instead of
// POSIX mode bits, which os.Chmod cannot represent on Windows. The owner, the
// local SYSTEM account, and built-in Administrators are the only principals
// permitted an allow ACE; everything else fails closed.
func protectedFile(path string, _ os.FileInfo) bool {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return false
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !owner.Equals(user.User.Sid) {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return false
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var raw unsafe.Pointer
		ok, _, _ := getAce.Call(uintptr(unsafe.Pointer(dacl)), uintptr(i), uintptr(unsafe.Pointer(&raw)))
		if ok == 0 || raw == nil {
			return false
		}
		header := (*windows.ACE_HEADER)(raw)
		if header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false
		}
		ace := (*windows.ACCESS_ALLOWED_ACE)(raw)
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(user.User.Sid) && !sid.Equals(system) && !sid.Equals(admins) {
			return false
		}
	}
	return true
}
