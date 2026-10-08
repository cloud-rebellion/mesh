// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPublishedUpdateRetainsMoreThan32RelatedNotes(t *testing.T) {
	root := t.TempDir()
	spec := completeSpec(t, TypeEntity, "Connected product overview")
	for i := 0; i < 34; i++ {
		spec.Related = append(spec.Related, fmt.Sprintf("existing-note-%d", i))
	}
	created, err := CreateNote(root, spec)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(created.Path)
	prepared, err := NoteSnapshotContext(context.Background(), root, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared.Spec.Related, spec.Related) {
		t.Fatal("preparation dropped existing product connections")
	}
	edit := prepared.Spec
	edit.Related = append(edit.Related, "current-release")
	edit.Sections["current_state"] += " See the linked current release for its verification and limits."
	updated, err := CreateNote(root, edit)
	if err != nil {
		t.Fatal(err)
	}
	after, err := NoteSnapshotContext(context.Background(), root, updated.ID)
	if err != nil || !reflect.DeepEqual(after.Spec.Related, edit.Related) || updated.ID != created.ID || updated.Path != created.Path {
		t.Fatalf("update lost identity or product connections: %v", err)
	}
	archived, err := os.ReadFile(filepath.Join(root, ".mesh", "note-history", created.ID, prepared.Revision+".md"))
	if err != nil || string(archived) != string(original) {
		t.Fatal("update did not retain the exact original")
	}
	tooMany := spec
	tooMany.Related = make([]string, 257)
	for i := range tooMany.Related {
		tooMany.Related[i] = fmt.Sprintf("related-note-%d", i)
	}
	if _, err := NormalizeSpec(tooMany); err == nil {
		t.Fatal("unbounded related-note input accepted")
	}
}

func TestPublishedUpdatePreservesIdentityHistoryAndProvenance(t *testing.T) {
	fixedNow(t)
	root := t.TempDir()
	spec := completeSpec(t, TypeNote, "Current schema fixture")
	spec.Author, spec.Agent, spec.Source, spec.SourceURL = "original-author", "original-agent", "import:fixture", "https://example.invalid/schema"
	spec.Tags, spec.Related, spec.Collections = []string{"schema"}, []string{"prior-decision"}, []string{"product"}
	spec.VerifiedAt = "2026-06-16"
	spec.Blocks = []BlockSpec{{Template: "verification", ID: "historical-checks", Fields: map[string]string{"checks": "Fixture parser checks.", "context": "Synthetic schema revision one.", "results": "Fixture checks passed.", "gaps": "No production check was performed."}}}
	created, err := CreateNote(root, spec)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(created.Path)
	original = []byte(strings.Replace(string(original), "---\n", "---\ncustom_tracking: ticket-42\nrole: schema\nstack: [fixture]\nrepo_path: fixture/path\nexpect_dead_ref_paths: [fictional.sql]\n", 1))
	if err := os.WriteFile(created.Path, original, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := NoteSnapshotContext(context.Background(), root, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Spec.VerifiedAt != "" || len(before.Spec.Blocks) != 1 || before.Spec.Blocks[0].Fields["gaps"] != spec.Blocks[0].Fields["gaps"] {
		t.Fatal("verification was prefilled or historical evidence lost")
	}
	Now = func() time.Time { return time.Date(2026, 6, 17, 10, 0, 0, 0, time.UTC) }
	edit := before.Spec
	edit.Title = "Renamed schema fixture"
	edit.Sections["findings"] = "Fixture revision two adds a nullable field. The linked decision explains the change."
	edit.Related = append(edit.Related, "new-decision")
	edit.Author, edit.Agent = "later-editor", "editing-agent"
	preview, err := PrepareNoteContext(context.Background(), root, edit)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, _ := os.ReadFile(created.Path)
	if string(unchanged) != string(original) {
		t.Fatal("preview wrote source")
	}
	if _, err := os.Stat(filepath.Join(root, ".mesh", "note-history")); !os.IsNotExist(err) {
		t.Fatal("preview archived source")
	}
	result, err := CreateNote(root, edit)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != created.ID || result.Path != created.Path || result.When != created.When || result.Revision != preview.Result.Revision {
		t.Fatal("identity, path, event date or revision changed")
	}
	after, _ := os.ReadFile(result.Path)
	header, body, _ := SplitFrontmatter(string(after))
	fm, raw, err := ParseFrontmatter([]byte(header))
	if err != nil {
		t.Fatal(err)
	}
	if fm.Created != before.Frontmatter.Created || fm.Updated != "2026-06-17T10:00:00Z" || fm.Author != "original-author" || fm.Agent != "original-agent" || fm.Source != spec.Source || fm.SourceURL != spec.SourceURL || fm.UpdatedBy != "later-editor" || fm.UpdatedAgent != "editing-agent" {
		t.Fatalf("provenance changed: %+v", fm)
	}
	if fm.Template != before.Frontmatter.Template || fm.TemplateVersion != before.Frontmatter.TemplateVersion || fm.VerifiedAt != "" || raw["custom_tracking"] != "ticket-42" || fm.RepoPath != "fixture/path" || fm.Role != "schema" || len(fm.ExpectDeadRefPaths) != 1 {
		t.Fatalf("metadata changed: %+v / %+v", fm, raw)
	}
	if !strings.Contains(body, "[[prior-decision]]") || !strings.Contains(body, "[[new-decision]]") || !strings.Contains(body, spec.Blocks[0].Fields["gaps"]) {
		t.Fatal("links or historical caveat lost")
	}
	archive := filepath.Join(root, ".mesh", "note-history", result.ID, before.Revision+".md")
	archived, err := os.ReadFile(archive)
	if err != nil || string(archived) != string(original) {
		t.Fatalf("exact original not retained: %v", err)
	}
	info, _ := os.Stat(archive)
	if info.Mode().Perm() != 0600 {
		t.Fatal("history file permissions widened")
	}
	files, _ := Walk(root)
	if len(files) != 1 {
		t.Fatalf("history became an indexed note: %v", files)
	}
	if _, err := CreateNote(root, edit); err == nil {
		t.Fatal("stale update accepted")
	}
}

func TestPublishedUpdateRefusesInvalidReplacementWithoutChangingSource(t *testing.T) {
	root := t.TempDir()
	created, err := CreateNote(root, completeSpec(t, TypeNote, "Protected publication"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := NoteSnapshotContext(context.Background(), root, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*NewNoteSpec)
	}{
		{"missing revision", func(s *NewNoteSpec) { s.UpdateRevision = "" }},
		{"stale revision", func(s *NewNoteSpec) { s.UpdateRevision = "stale" }},
		{"path substitution", func(s *NewNoteSpec) { s.UpdatePath = "other.md" }},
		{"audience change", func(s *NewNoteSpec) { s.Scope = []string{"sales"} }},
		{"template change", func(s *NewNoteSpec) { s.Template = "review"; s.Sections = map[string]string{} }},
		{"template version change", func(s *NewNoteSpec) { s.TemplateVersion = 2 }},
		{"draft demotion", func(s *NewNoteSpec) { s.Status = "draft" }},
		{"incomplete content", func(s *NewNoteSpec) { delete(s.Sections, "findings") }},
		{"mixed identity", func(s *NewNoteSpec) { s.DraftID = created.ID }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := NormalizeSpec(before.Spec)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(&spec)
			if _, err := CreateNote(root, spec); err == nil {
				t.Fatal("invalid replacement accepted")
			}
			data, _ := os.ReadFile(created.Path)
			if string(data) != string(before.Content) {
				t.Fatal("refused update changed source")
			}
		})
	}
}

func TestPublishedUpdateRefusesUnrepresentedBodyAndLegacy(t *testing.T) {
	root := t.TempDir()
	created, err := CreateNote(root, completeSpec(t, TypeNote, "Extra prose"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(created.Path)
	for _, altered := range []string{
		strings.Replace(string(data), "## Summary", "Manual preamble with an essential warning.\n\n## Summary", 1),
		string(data) + "\n# Appendix\nAdditional historical evidence.\n",
		strings.Replace(string(data), "---\n", "---\nwhy: Historical rationale\n", 1),
	} {
		if _, err := PublishedNoteSnapshot("notes/extra-prose.md", created.ID, []byte(altered)); err == nil {
			t.Fatal("silently discarded authored material")
		}
	}
}

func TestPublishedUpdateConcurrencyAcceptsOneRevision(t *testing.T) {
	root := t.TempDir()
	created, err := CreateNote(root, completeSpec(t, TypeNote, "Concurrent fixture"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := NoteSnapshotContext(context.Background(), root, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	const attempts = 6
	errors := make(chan error, attempts)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			spec := before.Spec
			spec.Summary = "One concurrent edit changes the fixture."
			_, err := CreateNote(root, spec)
			errors <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errors)
	success := 0
	for err := range errors {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("accepted %d concurrent replacements of one revision", success)
	}
}

func TestPublishedUpdateRolloutCancellationAndConfinement(t *testing.T) {
	root := t.TempDir()
	created, err := CreateNote(root, completeSpec(t, TypeNote, "Confined fixture"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := NoteSnapshotContext(context.Background(), root, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_AUTHORING_MODE", "readers-only")
	if _, err := PrepareNoteContext(context.Background(), root, before.Spec); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateNote(root, before.Spec); err == nil {
		t.Fatal("reader-first mode accepted replacement")
	}
	t.Setenv("MESH_AUTHORING_MODE", "enabled")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CreateNoteContext(ctx, root, before.Spec); err == nil {
		t.Fatal("cancelled replacement succeeded")
	}
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".mesh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".mesh", "note-history")); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateNote(root, before.Spec); err == nil {
		t.Fatal("archived through an escaping symlink")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("wrote outside vault")
	}
	current, _ := os.ReadFile(created.Path)
	if string(current) != string(before.Content) {
		t.Fatal("refused update changed source")
	}
}

func TestDraftEditableSnapshotPreservesPartialSubstanceAndRefusesLoss(t *testing.T) {
	root := t.TempDir()
	created, err := CreateNote(root, NewNoteSpec{Status: "draft", Title: "Draft incident evidence", Template: "post-mortem", Summary: "The fixture has observations; cause is still unknown.", Sections: map[string]string{"what_happened": "A synthetic request failed. No production claim is made."}, Author: "original-author", Source: "import:fixture", Scope: []string{"dev", "sales"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(created.Path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := DraftNoteSnapshot("post-mortems/draft-incident-evidence.md", created.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Spec.DraftID != created.ID || snapshot.Spec.DraftRevision != ContentRevision(raw) || snapshot.Spec.UpdateID != "" || snapshot.Spec.VerifiedAt != "" || snapshot.Frontmatter.Author != "original-author" || len(snapshot.Spec.Scope) != 2 {
		t.Fatal("draft evidence/identity altered")
	}
	if _, err = PublishedNoteSnapshot(snapshot.Path, created.ID, raw); err == nil {
		t.Fatal("draft prepared as published update")
	}
	if _, err = DraftNoteSnapshot(snapshot.Path, created.ID, []byte(strings.Replace(string(raw), "# Draft incident evidence", "Unstructured preamble must not vanish.\n\n# Draft incident evidence", 1))); err == nil {
		t.Fatal("extra prose silently dropped")
	}
	if _, err = DraftNoteSnapshot(snapshot.Path, "other-id", raw); err == nil {
		t.Fatal("wrong stable draft identity accepted")
	}
}
