// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package desktop

import (
	"runtime"
	"runtime/debug"
)

// Releases stamp these from the exact reviewed source/build receipt. Neither is
// overridden by ambient MESH_VERSION; native package identity must be stable.
var Version = "dev"
var Source = ""

type BuildIdentity struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	Source   string `json:"source"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
}

func IdentityInfo() BuildIdentity {
	source := Source
	if source == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					source = setting.Value
				}
			}
		}
	}
	return BuildIdentity{Protocol: Protocol, Version: Version, Source: source, Platform: runtime.GOOS, Arch: runtime.GOARCH}
}
