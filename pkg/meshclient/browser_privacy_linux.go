// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
//go:build linux

package meshclient

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"strings"
)

func browserPrivateACL(file *os.File) error {
	size, err := unix.Flistxattr(int(file.Fd()), nil)
	if err != nil {
		return errBrowserLogin
	}
	if size > 65536 {
		return errBrowserLogin
	}
	data := make([]byte, size)
	n, err := unix.Flistxattr(int(file.Fd()), data)
	if err != nil {
		return errBrowserLogin
	}
	for _, name := range strings.Split(string(data[:n]), "\x00") {
		if strings.HasPrefix(name, "system.posix_acl_") {
			return errors.New("POSIX ACL requires review")
		}
	}
	return nil
}
