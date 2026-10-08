// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func historicalTagFixture(t *testing.T, root string) (MigrationRequest, string) {
	t.Helper()
	request, original := migrationFixture(t, root, "historical-oauth")
	original = strings.Replace(original, "confidence: scoped\n", "confidence: scoped\ntags: [oauth-2.1, oauth, oauth-2.1]\n", 1)
	if err := os.WriteFile(filepath.Join(root, request.Path), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	return request, original
}

func TestHistoricalTagsMigrationAndJSONUpdatePreserveEvidence(t *testing.T) {
	root := t.TempDir()
	request, original := historicalTagFixture(t, root)
	preview, err := PreviewAuthoringMigration(t.Context(), root, []MigrationRequest{request})
	if err != nil {
		t.Fatal(err)
	}
	approval := MigrationApproval{PreviewHash: preview.Hash, IDs: []string{request.ID}}
	// Current source equality still fences a reviewed preview, even with compatible tags.
	path := filepath.Join(root, request.Path)
	if err := os.WriteFile(path, []byte(original+"\nLater evidence.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyAuthoringMigration(t.Context(), root, preview, approval); err == nil {
		t.Fatal("stale original accepted")
	}
	if _, err := os.Stat(filepath.Join(root, ".mesh")); !os.IsNotExist(err) {
		t.Fatal("stale preview created history")
	}
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	receipts, err := ApplyAuthoringMigration(t.Context(), root, preview, approval)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := os.ReadFile(filepath.Join(root, receipts[0].OriginalPath))
	if err != nil || string(archived) != original {
		t.Fatal("migration archive changed original bytes")
	}
	before, err := NoteSnapshotContext(t.Context(), root, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(*NoteSnapshot){
		func(s *NoteSnapshot) { s.Revision = "stale" },
		func(s *NoteSnapshot) { s.Content = append(append([]byte{}, s.Content...), '\n') },
		func(s *NoteSnapshot) { fm := *s.Frontmatter; fm.Tags = []string{"injected-2.1"}; s.Frontmatter = &fm },
	} {
		changed := *before
		mutation(&changed)
		if _, err := NormalizeUpdateSpec(before.Spec, &changed); err == nil {
			t.Fatal("snapshot bytes/metadata drift retained compatibility")
		}
	}
	want := []string{"oauth-2.1", "oauth", "oauth-2.1"}
	if !reflect.DeepEqual(before.Spec.Tags, want) || before.Frontmatter.VerifiedAt != "2026-02-01" || before.Spec.VerifiedAt != "" {
		t.Fatal("historical tags or verification changed")
	}
	// A normal wire round trip loses private normalization state. The publisher
	// must recover compatibility from fresh stored bytes, not an input flag.
	raw, _ := json.Marshal(before.Spec)
	var edit NewNoteSpec
	if err := json.Unmarshal(raw, &edit); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSpec(edit); err == nil {
		t.Fatal("general writer accepted dotted tag without a snapshot")
	}
	edit.Summary = "The historical fixture is described more precisely; no new check was performed."
	edit.Author, edit.Agent, edit.Source = "later-human", "later-agent", "forged-source"
	edit, err = NormalizeUpdateSpec(edit, before)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSpec(edit); err != nil {
		t.Fatal(err)
	}
	result, err := CreateNoteContext(t.Context(), root, edit)
	if err != nil {
		t.Fatal(err)
	}
	after, err := NoteSnapshotContext(t.Context(), root, result.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Spec.Tags, want) || after.Frontmatter.Author != "historical-human" || after.Frontmatter.Source != "historical-observation" || after.Frontmatter.UpdatedBy != "later-human" {
		t.Fatal("edit changed tags or original provenance")
	}
	if after.Frontmatter.VerifiedAt != "" || !strings.Contains(string(after.Content), "Original dont") {
		t.Fatal("edit freshly verified or dropped historical evidence")
	}
	history, err := os.ReadFile(filepath.Join(root, ".mesh", "note-history", request.ID, before.Revision+".md"))
	if err != nil || string(history) != string(before.Content) {
		t.Fatal("edit archive changed historical revision")
	}
	if _, err := CreateNoteContext(t.Context(), root, edit); err == nil {
		t.Fatal("stale edit accepted")
	}
}

func TestHistoricalTagsRejectChangesMalformedLabelsAndNewAuthoring(t *testing.T) {
	root := t.TempDir()
	request, original := historicalTagFixture(t, root)
	preview, err := PreviewAuthoringMigration(t.Context(), root, []MigrationRequest{request})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct{ from, to string }{
		{"tags: [oauth-2.1, oauth, oauth-2.1]", "tags: [oauth-2.2, oauth]"},
		{"tags: [oauth-2.1, oauth, oauth-2.1]", "tags: [oauth-2.1, oauth, new-tag]"},
		{"scope: [dev]", "scope: [public]"},
		{"source: historical-observation", "source: invented-observation"},
	} {
		candidate := strings.Replace(preview.Entries[0].Content, change.from, change.to, 1)
		if candidate == preview.Entries[0].Content {
			t.Fatalf("probe did not mutate %q", change.from)
		}
		if err := validateMigrationInvariant(original, candidate); err == nil {
			t.Fatalf("metadata change accepted: %s", change.to)
		}
	}
	for _, tags := range [][]string{{"oauth-2.2"}, {"oauth-2.1", "new-tag"}} {
		changed := request
		changed.Spec.Tags = tags
		if _, err := PreviewAuthoringMigration(t.Context(), root, []MigrationRequest{changed}); err == nil {
			t.Fatal("changed request tags accepted")
		}
	}
	if _, err := ApplyAuthoringMigration(t.Context(), root, preview, MigrationApproval{PreviewHash: preview.Hash, IDs: []string{request.ID}}); err != nil {
		t.Fatal(err)
	}
	before, err := NoteSnapshotContext(t.Context(), root, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tags := range [][]string{{"oauth-2.2"}, {"oauth-2.1", "oauth", "new-tag"}, {"[[oauth-2.1]]"}, {"notes/oauth-2.1"}} {
		edit := before.Spec
		edit.Tags = tags
		if _, err := NormalizeUpdateSpec(edit, before); err == nil {
			t.Fatalf("changed invalid tags accepted: %v", tags)
		}
		if _, err := CreateNoteContext(t.Context(), root, edit); err == nil {
			t.Fatal("publication accepted changed tags")
		}
	}
	newSpec := before.Spec
	newSpec.UpdateID, newSpec.UpdateRevision = "", ""
	if _, err := CreateNoteContext(t.Context(), t.TempDir(), newSpec); err == nil {
		t.Fatal("old in-memory fence authorized a new note")
	}
	for _, id := range []string{"oauth-2.1", "[[oauth-2.1]]", "notes/oauth-2.1", "../oauth-2.1", "oauth\\2.1", "oauth#2.1", "oauth 2.1"} {
		fresh := completeSpec(t, TypeNote, "New strict note")
		fresh.Tags = []string{id}
		if _, err := NormalizeSpec(fresh); err == nil {
			t.Fatalf("new authoring accepted %q", id)
		}
		if id == "oauth-2.1" {
			continue
		}
		badRoot := t.TempDir()
		badRequest, badOriginal := migrationFixture(t, badRoot, "malformed-tag")
		badOriginal = strings.Replace(badOriginal, "confidence: scoped\n", "confidence: scoped\ntags: ["+strconv.Quote(id)+"]\n", 1)
		if err := os.WriteFile(filepath.Join(badRoot, badRequest.Path), []byte(badOriginal), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := PreviewAuthoringMigration(t.Context(), badRoot, []MigrationRequest{badRequest}); err == nil {
			t.Fatalf("historical malformed tag accepted %q", id)
		}
	}
	canonical := completeSpec(t, TypeNote, "Canonical new note")
	canonical.Tags = []string{"oauth", "oauth-2-1"}
	if _, err := CreateNoteContext(t.Context(), t.TempDir(), canonical); err != nil {
		t.Fatal(err)
	}
}
