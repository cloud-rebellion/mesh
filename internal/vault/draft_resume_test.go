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

func TestDraftCompletionPreservesIdentityAndMetadata(t *testing.T) {
	fixedNow(t)
	root := t.TempDir()
	original, err := CreateNote(root, NewNoteSpec{Title: "Unfinished review", Template: "finding", Status: "draft", Author: "original-author"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(original.Path)
	// Empty declared metadata must not override supplied values merely because
	// omitempty would remove it from a marshaled historical struct.
	data = []byte(strings.Replace(string(data), "---\n", "---\nconfidence: ''\ncustom_tracking: ticket-42\n", 1))
	if err := os.WriteFile(original.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := DraftSnapshotContext(context.Background(), root, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec := completeSpec(t, TypeNote, "Completed review")
	spec.DraftID, spec.DraftRevision, spec.DraftPath = original.ID, before.Revision, before.Path
	spec.Confidence, spec.Author = "med", "later-editor"
	preview, err := PrepareNoteContext(context.Background(), root, spec)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, _ := os.ReadFile(original.Path)
	if string(unchanged) != string(data) {
		t.Fatal("preparation wrote the draft")
	}
	published, err := CreateNote(root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if published.ID != original.ID || published.Path != original.Path || published.Revision != preview.Result.Revision {
		t.Fatal("identity, path or receipt changed unexpectedly")
	}
	after, _ := os.ReadFile(published.Path)
	header, _, _ := SplitFrontmatter(string(after))
	fm, raw, err := ParseFrontmatter([]byte(header))
	if err != nil {
		t.Fatal(err)
	}
	if fm.Created != before.Frontmatter.Created || fm.Author != "original-author" || fm.Confidence != "med" || IsDraft(fm) || raw["custom_tracking"] != "ticket-42" {
		t.Fatalf("metadata lost: %+v / %v", fm, raw)
	}
	if _, err := CreateNote(root, spec); err == nil {
		t.Fatal("stale completion accepted")
	}
}

func TestDraftCompletionBindsAuthorizedPathAndRevision(t *testing.T) {
	root := t.TempDir()
	original, err := CreateNote(root, NewNoteSpec{Title: "Draft path binding", Template: "finding", Status: "draft"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := DraftSnapshotContext(context.Background(), root, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec := completeSpec(t, TypeNote, "Complete")
	spec.DraftID, spec.DraftRevision = original.ID, before.Revision
	spec.DraftPath = filepath.Join("restricted", "duplicate.md")
	if _, err := CreateNote(root, spec); err == nil {
		t.Fatal("authorized path mismatch accepted")
	}
	spec.DraftPath = before.Path
	spec.DraftRevision = "stale"
	if _, err := CreateNote(root, spec); err == nil {
		t.Fatal("stale revision accepted")
	}
	got, _ := os.ReadFile(original.Path)
	if string(got) != string(before.Content) {
		t.Fatal("refused completion mutated source")
	}
}
