// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
//go:build windows

package meshclient

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"unsafe"
)

func browserPrivateRoot(file *os.File) error { return browserWindowsPrivate(file, true) }
func browserPrivateFile(file *os.File, directory bool) error {
	return browserWindowsPrivate(file, directory)
}
func browserWindowsPrivate(file *os.File, directory bool) error {
	info, err := file.Stat()
	if err != nil || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return errBrowserLogin
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return errBrowserLogin
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return errBrowserLogin
	}
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || !owner.Equals(current.User.Sid) {
		return errors.New("browser sign-in requires private files owned by the current Windows user")
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		return errBrowserLogin
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return errBrowserLogin
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return errBrowserLogin
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &ace) != nil || ace == nil {
			return errBrowserLogin
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errBrowserLogin
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Mask != 0 && !sid.Equals(owner) && !sid.Equals(system) && !sid.Equals(admins) {
			return errors.New("browser sign-in requires a Windows ACL restricted to this user, SYSTEM and Administrators")
		}
	}
	return nil
}
