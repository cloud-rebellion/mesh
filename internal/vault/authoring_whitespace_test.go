// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEditableWhitespaceCreateUpdatePrepare(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"trailing newline", "Recorded fixture checks; deployment remains pending.\n"},
		{"blank lines", "\n\nObserved evidence remains bounded.\n\n"},
		{"indented code", "    printf 'synthetic only'\n    exit 0\n"},
		{"hard breaks", "First evidence line.  \nSecond evidence line.  "},
		{"fenced code", "\n```text\n  ## literal heading\n\tcode with trailing spaces  \n```\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			spec := completeSpec(t, TypeStatus, "Whitespace "+tc.name)
			spec.Related = []string{"synthetic-evidence"}
			spec.Sections["verification"] = tc.text
			spec.Author, spec.Source = "original-author", "import:synthetic"
			created, err := CreateNote(root, spec)
			if err != nil {
				t.Fatal(err)
			}
			before, err := NoteSnapshotContext(context.Background(), root, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.Spec.Sections["verification"] != tc.text {
				t.Fatalf("editable content changed: got %q, want %q", before.Spec.Sections["verification"], tc.text)
			}
			edited := before.Spec
			edited.Sections["changes"] += " Another synthetic change is recorded.\n"
			preview, err := PrepareNoteContext(context.Background(), root, edited)
			if err != nil {
				t.Fatal(err)
			}
			unchanged, _ := os.ReadFile(created.Path)
			if string(unchanged) != string(before.Content) {
				t.Fatal("preview wrote the source")
			}
			updated, err := CreateNote(root, edited)
			if err != nil {
				t.Fatal(err)
			}
			after, err := NoteSnapshotContext(context.Background(), root, updated.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Spec.Sections["verification"] != tc.text || after.Spec.Sections["changes"] != edited.Sections["changes"] {
				t.Fatal("unrelated edit changed whitespace or made a second prepare lossy")
			}
			if updated.ID != created.ID || updated.Path != created.Path || after.Revision != preview.Result.Revision || after.Frontmatter.Created != before.Frontmatter.Created || after.Frontmatter.Author != "original-author" || after.Frontmatter.Source != "import:synthetic" {
				t.Fatal("update changed identity or original provenance")
			}
			archived, err := os.ReadFile(filepath.Join(root, ".mesh", "note-history", created.ID, before.Revision+".md"))
			if err != nil || string(archived) != string(before.Content) {
				t.Fatal("exact prior bytes were not archived")
			}
			if _, err := CreateNote(root, edited); err == nil {
				t.Fatal("stale revision accepted")
			}
		})
	}
}

func TestEditableWhitespaceBlockAndCodeBytes(t *testing.T) {
	for _, withRelated := range []bool{false, true} {
		name := "note final block"
		if withRelated {
			name = "block before related"
		}
		t.Run(name, func(t *testing.T) {
			spec := completeSpec(t, TypeNote, "Code whitespace fixture")
			if withRelated {
				spec.Related = []string{"synthetic-code-reference"}
			}
			for _, id := range []string{"verification", "code"} {
				bt, err := BlockTemplateFor(id, 1)
				if err != nil {
					t.Fatal(err)
				}
				block := BlockSpec{Template: id, Version: 1, ID: id, Fields: map[string]string{}}
				for _, field := range bt.Fields {
					if field.Required {
						block.Fields[field.Key] = "  Explicit " + field.Key + ".  \n\n"
					}
				}
				if id == "code" {
					block.Fields["language"] = "text"
					// This is the final code-template field. The existing renderer
					// collapses note-final LF; a following Related heading retains it.
					block.Fields["illustrative"] = "true\n\n"
					block.Fields["code"] = "\n\tfirst line  \n    ## literal heading\n```nested\n\n"
				}
				spec.Blocks = append(spec.Blocks, block)
			}
			root := t.TempDir()
			created, err := CreateNote(root, spec)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := NoteSnapshotContext(context.Background(), root, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			for i, block := range spec.Blocks {
				want := make(map[string]string, len(block.Fields))
				for key, value := range block.Fields {
					want[key] = value
				}
				if !withRelated && i == len(spec.Blocks)-1 {
					// Do not invent LF bytes already absent from the saved file.
					want["illustrative"] = "true"
				}
				if !reflect.DeepEqual(snapshot.Spec.Blocks[i].Fields, want) {
					t.Fatalf("block %s encoded bytes changed: got %#v, want %#v", block.ID, snapshot.Spec.Blocks[i].Fields, want)
				}
			}
			edit := snapshot.Spec
			edit.Sections["findings"] += " Only this prose changed."
			updated, err := CreateNote(root, edit)
			if err != nil {
				t.Fatal(err)
			}
			after, err := NoteSnapshotContext(context.Background(), root, updated.ID)
			if err != nil || !reflect.DeepEqual(after.Spec.Blocks, snapshot.Spec.Blocks) {
				t.Fatalf("unrelated edit changed extracted code/evidence: %v", err)
			}
		})
	}
}

func TestEditableWhitespaceDraftKeepsGapsAndExactRevision(t *testing.T) {
	root := t.TempDir()
	text := "    synthetic observation  \n\nCause remains unknown.\n"
	spec := NewNoteSpec{Title: "Incomplete whitespace fixture", Template: "post-mortem", Status: "draft", Summary: "Only synthetic observations are recorded.", Sections: map[string]string{"what_happened": text}, Related: []string{"synthetic-observation"}, Scope: []string{"dev"}}
	created, err := CreateNote(root, spec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(created.Path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := DraftNoteSnapshot(filepath.Base(created.Path), created.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Spec.Sections["what_happened"] != text || snapshot.Spec.DraftRevision != ContentRevision(raw) || snapshot.Spec.DraftID != created.ID || snapshot.Spec.UpdateID != "" || len(MissingContent(snapshot.Spec)) == 0 {
		t.Fatal("draft bytes, revision, lifecycle or gaps changed")
	}
	if _, err := PublishedNoteSnapshot(snapshot.Path, created.ID, raw); err == nil {
		t.Fatal("draft became published")
	}
}

func TestEditableWhitespaceStillRefusesUnrepresentedProse(t *testing.T) {
	spec := completeSpec(t, TypeStatus, "Canonical refusal fixture")
	spec.Related = []string{"synthetic-evidence"}
	spec.Sections["verification"] += "\n"
	created, err := CreateNote(t.TempDir(), spec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(created.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, body string }{
		{"preamble", strings.Replace(string(raw), "## Summary", "Essential preamble must not vanish.\n\n## Summary", 1)},
		{"related annotation", string(raw) + "\nThe Related item has an essential qualification.\n"},
		{"appendix", string(raw) + "\n# Appendix\nAdditional evidence must not vanish.\n"},
		{"unmarked section", strings.Replace(string(raw), "<!-- mesh:section verification -->\n", "", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PublishedNoteSnapshot("statuses/canonical-refusal-fixture.md", created.ID, []byte(tc.body)); err == nil {
				t.Fatal("unrepresented content silently discarded")
			}
		})
	}
}
