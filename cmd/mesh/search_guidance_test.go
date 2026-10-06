// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"strings"
	"testing"
)

func TestSearchPrintsIncompleteGuidance(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "guidance.md", "---\nid: guidance\ntype: note\ntemplate: finding\ntemplate_version: 1\nwhen: 2026-09-23\n---\n# Guidanceneedle\nHistorical context\n")
	if out, err := runCLI(t, indexCmd(), dir); err != nil {
		t.Fatalf("index: %v: %s", err, out)
	}
	out, err := runCLI(t, searchCmd(), "--vault", dir, "--budget", "400", "guidanceneedle")
	if err != nil {
		t.Fatalf("search: %v: %s", err, out)
	}
	if !strings.Contains(out, "Incomplete authored content: summary, question, findings, evidence, limitations, next_steps; review the source before relying on this note.") {
		t.Fatalf("CLI lost guidance warning: %s", out)
	}
}
