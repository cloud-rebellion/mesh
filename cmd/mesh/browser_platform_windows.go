// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
//go:build windows

package main

import (
	"golang.org/x/sys/windows"
	"path/filepath"
)

func nativeBrowserCommand(url string) (string, []string, error) {
	directory, err := windows.GetSystemDirectory()
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(directory, "rundll32.exe"), []string{"url.dll,FileProtocolHandler", url}, nil
}
