// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A note id is vault-GLOBAL: the indexer keys notes.id and the graph's note node on it,
// and neither has any notion of which folder the file came from. CreateNote proved an id
// free by claiming <dir>/<id>.md with O_EXCL, and dir is the TYPE directory, so a gotcha
// and a decision with the same title both got the same id, both got a success receipt,
// and one of the two was quarantined by the next index pass. Which one was retrievable
// flipped depending on whether the watcher or `mesh index` ran last.

// TestCreateNoteClaimsIDsAcrossTheWholeVault: same title, two types, two distinct ids.
func TestCreateNoteClaimsIDsAcrossTheWholeVault(t *testing.T) {
	dir := t.TempDir()
	first, err := CreateNote(dir, completeFixtureSpec(NewNoteSpec{Type: TypeGotcha, Title: "Deploy times out"}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := CreateNote(dir, completeFixtureSpec(NewNoteSpec{Type: TypeDecision, Title: "Deploy times out"}))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("two notes in different type directories were both given id %q (%s and %s); "+
			"one of them is invisible to search from the moment it is written", first.ID, first.Path, second.Path)
	}
	// Both receipts must name a file that exists, and the ids on disk must match them.
	for _, r := range []*CreateResult{first, second} {
		if _, err := os.Stat(r.Path); err != nil {
			t.Fatalf("the receipt names a file that is not there: %v", err)
		}
		if got := NoteIDForFile(r.Path); got != r.ID {
			t.Errorf("%s: receipt says id %q, the file says %q", r.Path, r.ID, got)
		}
	}
	claimed, err := ClaimedIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Errorf("the vault should hold 2 distinct ids, got %d: %v", len(claimed), claimed)
	}
}

// TestCreateNoteYieldsToAHandWrittenNoteInAnotherFolder: the collision does not need a
// second CreateNote. A hand-written or imported note anywhere in the vault owns its id,
// including one whose id comes from its filename rather than frontmatter.
func TestCreateNoteYieldsToAHandWrittenNoteInAnotherFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "imported"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No frontmatter at all, so its id is its basename: exactly what the slug of the
	// title below produces.
	if err := os.WriteFile(filepath.Join(dir, "imported", "deploy-times-out.md"),
		[]byte("# Deploy times out\nimported prose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// And one that declares an id unrelated to its filename, to prove the scan reads
	// frontmatter and not just names.
	if err := os.WriteFile(filepath.Join(dir, "imported", "unrelated-name.md"),
		[]byte("---\nid: deploy-times-out-2\ntype: note\nwhen: 2026-01-01\n---\n# Other\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := CreateNote(dir, completeFixtureSpec(NewNoteSpec{Type: TypeGotcha, Title: "Deploy times out"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.ID == "deploy-times-out" || res.ID == "deploy-times-out-2" {
		t.Fatalf("CreateNote took an id another note in the vault already holds: %q", res.ID)
	}
	if !strings.HasPrefix(res.ID, "deploy-times-out") {
		t.Errorf("the id should still be derived from the title, got %q", res.ID)
	}
	claimed, err := ClaimedIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 3 {
		t.Errorf("3 notes must hold 3 distinct ids, got %d: %v", len(claimed), claimed)
	}
}

// TestClaimedIDsSurvivesAnUnreadableFolder: an id scan is a nice-to-have, a write-back is
// not. Walk stops at the first unreadable directory, and failing CreateNote for that would
// lose the note the flywheel exists to capture. The scan skips what it cannot read.
func TestClaimedIDsSurvivesAnUnreadableFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory, so the permission cannot be tested")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "x.md"),
		[]byte("---\nid: x\ntype: note\nwhen: 2026-01-01\n---\n# X\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if _, err := ClaimedIDs(dir); err != nil {
		t.Errorf("an unreadable folder must not fail the id scan: %v", err)
	}
	if _, err := CreateNote(dir, completeFixtureSpec(NewNoteSpec{Type: TypeGotcha, Title: "Still writable"})); err != nil {
		t.Errorf("an unreadable folder elsewhere in the vault must not fail a note write: %v", err)
	}
}
