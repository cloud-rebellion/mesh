// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func migrationFixture(t *testing.T, root, id string) (MigrationRequest, string) {
	t.Helper()
	rel := filepath.Join("decisions", id+".md")
	content := "---\nid: " + id + "\ntype: decision\ntitle: Historical " + id + "\nwhen: \"2026-02-01\"\ncreated: \"2026-02-01\"\nupdated: \"2026-02-02\"\nverified_at: \"2026-02-01\"\nauthor: historical-human\nagent: historical-agent\nsource: historical-observation\nconfidence: scoped\nreview_by: \"2026-03-01\"\nscope: [dev]\nrelated: [previous-note]\nsupersedes: [older-note]\nexpect_dead_ref_paths: [retired/example.go]\ncustom_metadata:\n  retain: exact\n  count: 2\ndo: Check [[fixture-reader]] before acting.\ndont: Never stop the fixture owner without authorization.\nwhy: Exclusive ownership protects fixture state.\n---\n\n# Historical " + id + "\n\nOriginal narrative and unresolved caveat, retained only in the exact archive.\n\n## Related\n- [[additional-evidence]]\n"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	spec := completeSpec(t, TypeDecision, "Historical "+id)
	spec.Sections["context"] = "The historical fixture is being reformatted; the original unresolved caveat remains open."
	spec.Sections["decision"] = "Preserve the recorded decision; this fixture makes no new operational choice."
	spec.Sections["rationale"] = "This is a synthetic formatting test with no new live verification."
	return MigrationRequest{Path: rel, ID: id, Spec: spec}, content
}

func TestAuthoringMigrationPreviewApplyArchivesAndPreserves(t *testing.T) {
	fixedNow(t)
	root := t.TempDir()
	request, original := migrationFixture(t, root, "first-decision")
	preview, err := PreviewAuthoringMigration(context.Background(), root, []MigrationRequest{request})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, _ := os.ReadFile(filepath.Join(root, request.Path))
	if string(unchanged) != original {
		t.Fatal("preview changed source")
	}
	if _, err := os.Stat(filepath.Join(root, ".mesh")); !os.IsNotExist(err) {
		t.Fatal("preview created archival state")
	}
	entry := preview.Entries[0]
	if !entry.CaveatsRequireReview || len(entry.Issues) == 0 {
		t.Fatal("migration hides caveat review")
	}
	if strings.Contains(entry.Content, "Original narrative and unresolved caveat, retained only in the exact archive.") {
		t.Fatal("current page duplicates full historical narrative")
	}
	if !strings.Contains(entry.Content, "Never stop the fixture owner without authorization.") {
		t.Fatal("historical prohibition dropped")
	}
	yamlText, body, _ := SplitFrontmatter(entry.Content)
	fm, raw, err := ParseFrontmatter([]byte(yamlText))
	if err != nil {
		t.Fatal(err)
	}
	if fm.ID != request.ID || fm.When != "2026-02-01" || fm.Created != "2026-02-01" || fm.VerifiedAt != "2026-02-01" {
		t.Fatalf("identity/date changed: %+v", fm)
	}
	if fm.Author != "historical-human" || fm.Agent != "historical-agent" || fm.Source != "historical-observation" || fm.ReviewBy != "2026-03-01" {
		t.Fatal("provenance changed")
	}
	if raw["custom_metadata"] == nil || len(fm.ExpectDeadRefPaths) != 1 {
		t.Fatal("unknown metadata/caveat metadata dropped")
	}
	if SectionText(body, "context") != request.Spec.Sections["context"] {
		t.Fatal("explicit reconstruction changed")
	}
	approval := MigrationApproval{PreviewHash: preview.Hash, IDs: []string{request.ID}}
	encoded, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded MigrationPreview
	if err := json.Unmarshal(encoded, &reloaded); err != nil {
		t.Fatal(err)
	}
	receipts, err := ApplyAuthoringMigration(context.Background(), root, &reloaded, approval)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].State != "applied" {
		t.Fatalf("receipt: %+v", receipts)
	}
	backup, err := os.ReadFile(filepath.Join(root, receipts[0].OriginalPath))
	if err != nil || string(backup) != original {
		t.Fatalf("original backup is not exact: %v", err)
	}
	applied, _ := os.ReadFile(filepath.Join(root, request.Path))
	if string(applied) != entry.Content {
		t.Fatal("applied content differs from reviewed preview")
	}
	info, _ := os.Stat(filepath.Join(root, request.Path))
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("source permission changed: %v", info.Mode())
	}
	var receipt MigrationReceipt
	receiptData, _ := os.ReadFile(filepath.Join(root, receipts[0].ReceiptPath))
	if err := json.Unmarshal(receiptData, &receipt); err != nil || receipt.State != "applied" {
		t.Fatalf("durable receipt missing: %v", err)
	}
	files, err := Walk(root)
	if err != nil || len(files) != 1 {
		t.Fatalf("archive entered active note graph: %v %v", files, err)
	}
	again, err := ApplyAuthoringMigration(context.Background(), root, preview, approval)
	if err != nil || len(again) != 1 || !again[0].AlreadyApplied {
		t.Fatalf("idempotent recovery failed: %+v %v", again, err)
	}
}

func TestAuthoringMigrationRejectsUnreviewedDriftTamperingAndRelabeling(t *testing.T) {
	root := t.TempDir()
	first, original1 := migrationFixture(t, root, "first")
	second, original2 := migrationFixture(t, root, "second")
	preview, err := PreviewAuthoringMigration(context.Background(), root, []MigrationRequest{first, second})
	if err != nil {
		t.Fatal(err)
	}
	for _, approval := range []MigrationApproval{
		{},
		{PreviewHash: preview.Hash},
		{PreviewHash: "wrong", IDs: []string{first.ID}},
		{PreviewHash: preview.Hash, IDs: []string{"unlisted"}},
	} {
		if _, err := ApplyAuthoringMigration(context.Background(), root, preview, approval); err == nil {
			t.Fatal("unreviewed migration accepted")
		}
	}
	if err := os.WriteFile(filepath.Join(root, second.Path), []byte(original2+"\nNew evidence.\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyAuthoringMigration(context.Background(), root, preview, MigrationApproval{PreviewHash: preview.Hash, IDs: []string{first.ID, second.ID}}); err == nil {
		t.Fatal("stale batch accepted")
	}
	firstData, _ := os.ReadFile(filepath.Join(root, first.Path))
	if string(firstData) != original1 {
		t.Fatal("stale batch partially mutated first source during preflight")
	}
	if _, err := os.Stat(filepath.Join(root, ".mesh")); !os.IsNotExist(err) {
		t.Fatal("rejected migration mutated state")
	}
	relabel := first
	relabel.Spec.Type = TypePostMortem
	relabel.Spec.Template = "post-mortem"
	if _, err := PreviewAuthoringMigration(context.Background(), root, []MigrationRequest{relabel}); err == nil {
		t.Fatal("semantic relabel accepted")
	}
	bump := first
	bump.Spec.VerifiedAt = "2026-10-06"
	if _, err := PreviewAuthoringMigration(context.Background(), root, []MigrationRequest{bump}); err == nil {
		t.Fatal("verification bump accepted")
	}
	tampered := *preview
	tampered.Entries = append([]MigrationEntry(nil), preview.Entries...)
	tampered.Entries[0].Content = strings.Replace(tampered.Entries[0].Content, "historical-observation", "fictional-verification", 1)
	tampered.Entries[0].ResultHash = contentHash([]byte(tampered.Entries[0].Content))
	tampered.Hash, _ = previewHash(&tampered)
	if _, err := ApplyAuthoringMigration(context.Background(), root, &tampered, MigrationApproval{PreviewHash: tampered.Hash, IDs: []string{first.ID}}); err == nil {
		t.Fatal("forged preserved metadata accepted")
	}
}

func TestAuthoringMigrationSelectionAndConfinedPaths(t *testing.T) {
	root := t.TempDir()
	first, _ := migrationFixture(t, root, "first")
	second, secondOriginal := migrationFixture(t, root, "second")
	preview, err := PreviewAuthoringMigration(context.Background(), root, []MigrationRequest{first, second})
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := ApplyAuthoringMigration(context.Background(), root, preview, MigrationApproval{PreviewHash: preview.Hash, IDs: []string{first.ID}})
	if err != nil || len(receipts) != 1 || receipts[0].ID != first.ID {
		t.Fatalf("explicit selection failed: %+v %v", receipts, err)
	}
	data, _ := os.ReadFile(filepath.Join(root, second.Path))
	if string(data) != secondOriginal {
		t.Fatal("unselected note changed")
	}
	for _, path := range []string{"../outside.md", "/tmp/outside.md", ".mesh/hidden.md"} {
		bad := first
		bad.Path = path
		if _, err := PreviewAuthoringMigration(context.Background(), root, []MigrationRequest{bad}); err == nil {
			t.Fatalf("unsafe path accepted: %s", path)
		}
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.md")
	if err := os.WriteFile(outsideFile, []byte(secondOriginal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "linked.md")); err != nil {
		t.Fatal(err)
	}
	bad := second
	bad.Path = "linked.md"
	if _, err := PreviewAuthoringMigration(context.Background(), root, []MigrationRequest{bad}); err == nil {
		t.Fatal("symlink source accepted")
	}
}

func TestAuthoringMigrationRejectsAddingOmittedAudienceAndLockedSource(t *testing.T) {
	root := t.TempDir()
	request, original := migrationFixture(t, root, "omitted-scope")
	original = strings.Replace(original, "scope: [dev]\n", "", 1)
	if err := os.WriteFile(filepath.Join(root, request.Path), []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewAuthoringMigration(context.Background(), root, []MigrationRequest{request})
	if err != nil {
		t.Fatal(err)
	}
	tampered := *preview
	tampered.Entries = append([]MigrationEntry(nil), preview.Entries...)
	tampered.Entries[0].Content = strings.Replace(tampered.Entries[0].Content, "template: decision", "scope: [public]\ntemplate: decision", 1)
	tampered.Entries[0].ResultHash = contentHash([]byte(tampered.Entries[0].Content))
	tampered.Hash, _ = previewHash(&tampered)
	if _, err := ApplyAuthoringMigration(context.Background(), root, &tampered, MigrationApproval{PreviewHash: tampered.Hash, IDs: []string{request.ID}}); err == nil {
		t.Fatal("migration added an omitted audience")
	}
	if err := os.Chmod(filepath.Join(root, request.Path), 0o440); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyAuthoringMigration(context.Background(), root, preview, MigrationApproval{PreviewHash: preview.Hash, IDs: []string{request.ID}}); err == nil {
		t.Fatal("migration bypassed locked source")
	}
	if _, err := os.Stat(filepath.Join(root, ".mesh")); !os.IsNotExist(err) {
		t.Fatal("rejected preflight created archival state")
	}
}

func TestAuthoringMigrationPreservesWrappedLegacyReferences(t *testing.T) {
	root := t.TempDir()
	request, original := migrationFixture(t, root, "wrapped-references")
	original = strings.Replace(original, "related: [previous-note]", "related: ['[[previous-note]]']", 1)
	if err := os.WriteFile(filepath.Join(root, request.Path), []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewAuthoringMigration(context.Background(), root, []MigrationRequest{request})
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := ApplyAuthoringMigration(context.Background(), root, preview, MigrationApproval{PreviewHash: preview.Hash, IDs: []string{request.ID}})
	if err != nil || len(receipts) != 1 {
		t.Fatalf("supported legacy references could not migrate: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, request.Path))
	header, _, _ := SplitFrontmatter(string(data))
	fm, _, err := ParseFrontmatter([]byte(header))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range fm.Related {
		if id == "previous-note" {
			found = true
		}
	}
	if !found {
		t.Fatal("wrapped reference target lost")
	}
	backup, _ := os.ReadFile(filepath.Join(root, receipts[0].OriginalPath))
	if string(backup) != original {
		t.Fatal("original wrapper syntax was not preserved in archive")
	}
}
