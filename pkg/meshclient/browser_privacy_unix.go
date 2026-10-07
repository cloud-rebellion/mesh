// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
//go:build !windows

package meshclient

import (
	"errors"
	"os"
	"syscall"
)

func browserPrivateRoot(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return errBrowserLogin
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || owner.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0022 != 0 {
		return errors.New("browser sign-in requires an owned vault without group/world write access")
	}
	if err = browserPrivateACL(file); err != nil {
		return errors.New("browser sign-in refused an unsafe or unverifiable vault ACL")
	}
	return nil
}
func browserPrivateFile(file *os.File, directory bool) error {
	info, err := file.Stat()
	if err != nil {
		return errBrowserLogin
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	mode := os.FileMode(0600)
	if directory {
		mode = 0700
	}
	if !ok || owner.Uid != uint32(os.Getuid()) || info.Mode().Perm() != mode || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return errors.New("browser sign-in requires an owned private .mesh directory (0700) and files (0600)")
	}
	if err = browserPrivateACL(file); err != nil {
		return errors.New("browser sign-in refused an unsafe or unverifiable private-file ACL")
	}
	return nil
}
