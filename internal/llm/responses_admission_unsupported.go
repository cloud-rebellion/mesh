//go:build !linux && !darwin

// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package llm

import (
	"context"
	"os"
)

// No weaker accounting fallback on platforms without the reviewed local
// filesystem/kernel-lock implementation. Other existing backends are unchanged.
func admissionOpenLocked(context.Context, string) (*os.Root, func(), error) {
	return nil, nil, spendRefused()
}
func admissionInventory(*os.Root) error                     { return spendRefused() }
func admissionRead(*os.Root, string, int64) ([]byte, error) { return nil, spendRefused() }
func admissionCommit(*os.Root, responsesSpendState) error   { return spendRefused() }
