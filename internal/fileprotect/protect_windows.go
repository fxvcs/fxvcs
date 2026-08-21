//go:build windows

// Package fileprotect applies the platform confidentiality boundary to files
// whose bytes contain resolved secret material.
package fileprotect

import (
	"runtime"

	"golang.org/x/sys/windows"
)

// Protect replaces inherited permissions with a protected DACL granting full
// control only to the current user, LocalSystem, and Administrators.
func Protect(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	var pinner runtime.Pinner
	pinner.Pin(user.User.Sid)
	pinner.Pin(system)
	pinner.Pin(admins)
	defer pinner.Unpin()
	entries := []windows.EXPLICIT_ACCESS{
		allowFullControl(user.User.Sid, windows.TRUSTEE_IS_USER),
		allowFullControl(system, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
		allowFullControl(admins, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
}

func allowFullControl(sid *windows.SID, trusteeType windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
