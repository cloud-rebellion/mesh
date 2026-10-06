// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"fmt"
	"os"
	"strings"
)

// RequireAuthoringWrites permits a reader-first deployment of this binary without
// enabling template publication yet. Catalog, validation, previews and retrieval
// stay available; durable publication and historical conversion share this gate.
// An unset mode preserves ordinary local authoring after reader rollout is complete.
func RequireAuthoringWrites() error {
	switch strings.TrimSpace(os.Getenv("MESH_AUTHORING_MODE")) {
	case "", "enabled":
		return nil
	case "readers-only":
		return fmt.Errorf("%w: template publication is disabled during reader rollout; enable MESH_AUTHORING_MODE after compatible readers are verified", ErrInvalidSpec)
	default:
		return fmt.Errorf("%w: MESH_AUTHORING_MODE must be enabled or readers-only", ErrInvalidSpec)
	}
}
