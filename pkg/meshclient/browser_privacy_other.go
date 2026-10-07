// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
//go:build !linux && !darwin && !windows

package meshclient

import "os"

func browserPrivateACL(*os.File) error { return errBrowserLogin }
