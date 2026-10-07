// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
//go:build !windows

package main

import "runtime"

func nativeBrowserCommand(url string) (string, []string, error) {
	return unixBrowserCommand(runtime.GOOS, url)
}
