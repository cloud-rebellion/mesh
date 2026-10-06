// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every in-place note rewrite used to be os.WriteFile, which opens with O_TRUNC: the note
// is EMPTY from the truncate until the write lands. That window is not hypothetical for a
// vault. It is the write path behind `mesh migrate --apply`, `mesh scope backfill
// --apply`, `mesh structure --fill-bodies --apply` and `--wire-orphans --apply`, all of
// which rewrite EVERY note in a vault in one pass with no backup, while the index daemon
// and `mesh sync --watch` are reading the same files.
//
// A power cut inside the window leaves an empty note; a concurrent reader inside it sees
// an empty or half-written note with no crash at all, and sync then pushes those bytes to
// the team. A rename cannot be observed half-done, so the fix is temp+rename (plus the two
// fsyncs, which are pinned by cmd/mesh's TestEveryNoteByteWriterFsyncs because a power cut
// cannot be reproduced in a unit test).
//
// The observable consequence of "replaced, not truncated in place" is file IDENTITY: a
// rename installs a NEW file over the old name, so os.SameFile is false afterwards, and a
// second hard link to the original still holds the ORIGINAL bytes instead of the truncated
// ones. os.WriteFile fails both. This test therefore fails on any regression back to a
// truncating writer, for every active entry point.
func TestNoteRewriteReplacesInsteadOfTruncating(t *testing.T) {
	cases := []struct {
		name    string
		content string
		rewrite func(path string) (*MigrateResult, error)
		want    string // substring the rewritten note must carry
	}{
		{
			name:    "MigrateFile",
			content: "---\ntitle: Untyped\n---\n\n# Untyped\n",
			rewrite: func(path string) (*MigrateResult, error) { return MigrateFile(filepath.Dir(path), path, false) },
			want:    "type: note",
		},
		{
			name:    "BackfillScopeFile",
			content: "---\nid: n1\ntype: note\ntitle: N\n---\n\n# N\n",
			rewrite: func(path string) (*MigrateResult, error) { return BackfillScopeFile(path, "team", false) },
			want:    "scope: team",
		},
		{
			name:    "BackfillRelatedFile",
			content: "---\nid: n2\ntype: note\ntitle: N\n---\n\n# N\n",
			rewrite: func(path string) (*MigrateResult, error) {
				return BackfillRelatedFile(path, []string{"other-note"}, false)
			},
			want: "- other-note",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "note.md")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			// A note the author restricted to 0600 must not come back world-readable
			// because the rewrite installed a fresh temp file over it.
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "observer.hardlink")
			linked := os.Link(path, link) == nil // not supported everywhere; only asserted when it is

			res, err := tc.rewrite(path)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if !res.Changed {
				t.Fatalf("rewrite reported no change, so this case never exercised the writer")
			}

			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if os.SameFile(before, after) {
				t.Errorf("the note was rewritten IN PLACE (same file identity), so the writer still " +
					"truncates the live note before writing it: a crash or a concurrent reader in " +
					"that window sees an empty note. Want a temp file renamed over it.")
			}
			if linked {
				old, err := os.ReadFile(link)
				if err != nil {
					t.Fatal(err)
				}
				if string(old) != tc.content {
					t.Errorf("the original file was mutated in place (a second link to it now reads %q), "+
						"so the old bytes were destroyed before the new ones landed", truncate(string(old)))
				}
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), tc.want) {
				t.Errorf("rewritten note is missing %q:\n%s", tc.want, got)
			}
			if _, _, had := SplitFrontmatter(string(got)); !had {
				t.Errorf("rewritten note lost its frontmatter block:\n%s", got)
			}
			if perm := after.Mode().Perm(); perm != 0o600 {
				t.Errorf("mode = %04o, want 0600: the rewrite must not widen a restricted note", perm)
			}
			assertNoTempLeftovers(t, dir)
		})
	}
}

// A rename installs a new file over the destination without ever opening it, so the move
// to temp+rename silently gained the power to overwrite a note the author had chmod'd
// read-only, which the os.WriteFile it replaced refused outright. That is the only
// protection an author has against a bulk `--apply` pass rewriting a note they locked, and
// `mesh structure --wire-orphans --apply` counts on the refusal to report a per-note
// failure at all. The durable writer asks the destination the same question a direct write
// would, so the refusal survives the fix.
func TestNoteRewriteRefusesAReadOnlyNote(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits, so there is nothing to refuse")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "locked.md")
	const content = "---\nid: locked\ntype: note\ntitle: Locked\n---\n\n# Locked\n"
	if err := os.WriteFile(path, []byte(content), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	if _, err := BackfillScopeFile(path, "team", false); err == nil {
		t.Error("rewriting a read-only note reported success; the rename overwrote a file the " +
			"author locked, and every caller that counts write failures now counts none")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("the read-only note was rewritten anyway:\n%s", got)
	}
	assertNoTempLeftovers(t, dir)
}

// CreateNote is the primary note-authoring path for the whole flywheel (`mesh new`, MCP
// mesh_append_note and mesh_write_entity, the hub's /mcp, internal/web/pending_api), and
// it claims its filename with O_EXCL. The durability fix added an fsync of the file and of
// the parent directory around that claim, and it had to leave the claim itself alone: the
// O_EXCL is what stops two concurrent write-backs from rendering the same id and having
// the second silently truncate the first while both callers get a success receipt.
//
// A temp+rename here would look like the other writers and quietly reintroduce exactly
// that bug, because a rename overwrites unconditionally. This pins the claim: an id that
// is already taken must be left BYTE-IDENTICAL and the new note must take the next
// suffix. TestCreateNoteConcurrentSameTitleLosesNothing covers the concurrent half; the
// two fsyncs are pinned by cmd/mesh's TestEveryNoteByteWriterFsyncs.
func TestCreateNoteKeepsExclusiveClaim(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, DirForType(TypeGotcha))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const squatter = "---\nid: taken-title\ntype: gotcha\ntitle: someone else's note\n---\n\n# not yours\n"
	taken := filepath.Join(dir, "taken-title.md")
	if err := os.WriteFile(taken, []byte(squatter), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := CreateNote(root, completeFixtureSpec(NewNoteSpec{Type: TypeGotcha, Title: "Taken title", Do: "d", Dont: "n", Why: "w"}))
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if res.ID != "taken-title-2" {
		t.Errorf("id = %q, want taken-title-2: the claim must step around a file that already exists", res.ID)
	}
	if got, err := os.ReadFile(taken); err != nil || string(got) != squatter {
		t.Errorf("the existing note at the base id was overwritten (err=%v, content=%q); the O_EXCL "+
			"claim is load-bearing and must not become a rename", err, truncate(string(got)))
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("the receipt names %s but reading it failed: %v", res.Path, err)
	}
	if !strings.Contains(string(got), "id: taken-title-2") {
		t.Errorf("new note does not carry its own id:\n%s", got)
	}
	assertNoTempLeftovers(t, dir)
}

// assertNoTempLeftovers fails when a write left a temp file in a note directory. The vault
// walker treats files it finds as notes, so a stranded temp file is not just litter: it
// can be indexed and synced as a duplicate of the note it was a copy of.
func assertNoTempLeftovers(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".mesh-") {
			t.Errorf("temp file %s left behind in %s", e.Name(), dir)
		}
	}
}

func truncate(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// BenchmarkCreateNote measures the interactive write-back path, which is where the two
// added fsyncs are paid. Run it with and without them to re-measure the cost on a given
// filesystem: an fsync is a device round trip, not free, and this path is synchronous
// under `mesh new` and every MCP write-back.
func BenchmarkCreateNote(b *testing.B) {
	root := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A distinct title per iteration, so the suffix search stays one attempt and the
		// number is the write, not a scan of everything the benchmark already wrote.
		spec := completeFixtureSpec(NewNoteSpec{Type: TypeGotcha, Title: "benchmark note " + strconv.Itoa(i), Do: "d", Dont: "n", Why: "w"})
		if _, err := CreateNote(root, spec); err != nil {
			b.Fatal(err)
		}
	}
}
