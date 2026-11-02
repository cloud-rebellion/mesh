// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateSnapshotPreparationUsesAuthorizedBytesWithoutChangingLiveVault(t *testing.T) {
	t.Setenv("MESH_AUTHORING_MODE", "enabled")
	root := t.TempDir()
	spec := completeSpec(t, TypeNote, "Private preparation fixture")
	spec.Author, spec.Agent, spec.Source = "original-author", "original-agent", "fixture"
	spec.VerifiedAt = "2026-06-17"
	spec.Blocks = []BlockSpec{{Template: "verification", ID: "synthetic-prior-check", Fields: map[string]string{
		"checks": "Synthetic fixture comparison only.", "context": "This private-preparation fixture, prior revision.", "results": "The synthetic fixture recorded a comparison result.", "gaps": "No live system or factual claim was verified.",
	}}}
	made, err := CreateNote(root, spec)
	if err != nil {
		t.Fatal(err)
	}
	before, err := NoteSnapshotContext(context.Background(), root, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	// This edit must never be used as a scratch winner, overwritten or archived by
	// private preparation. The eventual selected local refresh owns its separate CAS.
	concurrent := []byte("unsent editor bytes outside the authorized snapshot")
	if err := os.WriteFile(made.Path, concurrent, 0600); err != nil {
		t.Fatal(err)
	}
	edit := before.Spec
	edit.Summary = "A privately prepared update, with no live publication or factual verification."
	edit.Author, edit.Agent, edit.By = "mesh-curator", "mesh-curator", "mesh-curator"
	prepared, err := PrepareSnapshotUpdateContext(context.Background(), before, edit)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(made.Path)
	if err != nil || string(got) != string(concurrent) {
		t.Fatal("private preparation changed live editor bytes")
	}
	if _, err := os.Stat(filepath.Join(root, ".mesh", "note-history")); !os.IsNotExist(err) {
		t.Fatal("private preparation changed live history")
	}
	decoded, err := PublishedNoteSnapshot(before.Path, made.ID, prepared.Content)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Frontmatter.Author != "original-author" || decoded.Frontmatter.Agent != "original-agent" || decoded.Frontmatter.Source != "fixture" || decoded.Frontmatter.Created != before.Frontmatter.Created || decoded.Frontmatter.VerifiedAt != "" || decoded.Frontmatter.UpdatedBy != "mesh-curator" || !strings.Contains(string(prepared.Content), edit.Summary) {
		t.Fatalf("private writer lost provenance/content: %+v", decoded.Frontmatter)
	}
	if string(prepared.OriginalContent) != string(before.Content) || prepared.Result.Path != before.Path {
		t.Fatal("private preparation did not retain exact authorized history/path")
	}
}
