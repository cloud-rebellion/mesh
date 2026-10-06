// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
package vault

import (
	"context"
	"os"
	"testing"
)

func TestReaderRolloutAllowsPreviewButNoPublication(t *testing.T) {
	root := t.TempDir()
	spec := completeSpec(t, TypeNote, "Reader-first rollout")
	t.Setenv("MESH_AUTHORING_MODE", "readers-only")
	preview, err := PrepareNoteContext(context.Background(), root, spec)
	if err != nil {
		t.Fatal(err)
	}
	header, body, _ := SplitFrontmatter(string(preview.Content))
	fm, _, _ := ParseFrontmatter([]byte(header))
	if _, err := ReadAuthoring(fm, body); err != nil {
		t.Fatalf("compatible reader failed: %v", err)
	}
	if _, err := CreateNote(root, spec); err == nil {
		t.Fatal("reader-only phase published")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("disabled publication mutated vault")
	}
	t.Setenv("MESH_AUTHORING_MODE", "enabled")
	if _, err := CreateNote(root, spec); err != nil {
		t.Fatalf("verified reader phase could not enable publication: %v", err)
	}
	t.Setenv("MESH_AUTHORING_MODE", "invalid")
	if err := RequireAuthoringWrites(); err == nil {
		t.Fatal("invalid rollout configuration silently enabled writes")
	}
}

func TestReaderRolloutStopsMigrationBeforeMutation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MESH_AUTHORING_MODE", "readers-only")
	if _, err := ApplyAuthoringMigration(context.Background(), root, nil, MigrationApproval{}); err == nil {
		t.Fatal("migration ignored reader-only gate")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("disabled migration created archive or receipt")
	}
}
