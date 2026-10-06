// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// mustDropped is DroppedNotes for the tests that expect it to succeed. DroppedNotes
// reports a failed read rather than answering "nothing dropped", so every caller has to
// look at the error; a test that ignored it would be back to the defect this file covers.
func mustDropped(t *testing.T, s *Store) []FileError {
	t.Helper()
	d, err := s.DroppedNotes()
	if err != nil {
		t.Fatalf("DroppedNotes: %v", err)
	}
	return d
}

// seedIndexedVault writes one good note, indexes it with a writable store, and returns the
// vault root. That leaves an index stamped with the CURRENT SchemaVersion.
func seedIndexedVault(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "good.md"),
		[]byte("---\nid: good\ntype: note\nwhen: \"2026-01-01\"\n---\n# Good\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReindexFull(s, dir); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ageIndex rewrites an index into the shape a previously released Mesh left behind:
// an older schema_version stamp, minus the tables that version did not have.
func ageIndex(t *testing.T, vaultRoot string, version int, dropTables ...string) {
	t.Helper()
	dbPath := filepath.Join(vaultRoot, ".mesh", "mesh.db")
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tbl := range dropTables {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + tbl); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	if version == 0 {
		if _, err := db.Exec(`DELETE FROM meta WHERE key='schema_version'`); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := db.Exec(`UPDATE meta SET value=? WHERE key='schema_version'`, fmt.Sprint(version)); err != nil {
		t.Fatal(err)
	}
}

// TestReadOnlyOpenRefusesAnIndexFromAnOlderMesh is the read-only twin of the version
// comparison ensureSchema makes. Only the WRITABLE Open ever compared schema_version, and
// the shipped setup had no writable process: `mesh mcp`, `mesh doctor`, `mesh ui` and the
// TUI all opened read-only. So an upgraded user's index was never migrated and every one
// of those surfaces queried a schema its binary does not match. (`mesh mcp` has since been
// given the owning-writer election, so it opens writable and rebuilds instead of landing
// here; the other three still do.)
//
// It was not a hypothetical. dropped_notes landed in schema.sql on 2026-08-09 with no
// version bump, so a v5 index has no such table, and mesh_health answered
// {"counts":{},"findings":null} (a CLEAN vault) while quarantined notes stayed invisible.
// One version further back there is no note_health either, and mesh_health answered a bare
// "internal error" with the cause on stderr, which the agent client hides.
func TestReadOnlyOpenRefusesAnIndexFromAnOlderMesh(t *testing.T) {
	cases := []struct {
		name    string
		version int
		drop    []string
	}{
		{name: "v5 has no dropped_notes table", version: 5, drop: []string{"dropped_notes"}},
		{name: "an older index has no note_health table either", version: 4, drop: []string{"dropped_notes", "note_health"}},
		{name: "an index this Mesh never stamped", version: 0, drop: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedIndexedVault(t)
			ageIndex(t, dir, tc.version, tc.drop...)

			store, err := OpenReadOnly(dir)
			if err == nil {
				store.Close()
				t.Fatal("a read-only store opened an index written by a different Mesh; every read off it answers from a schema this binary does not match")
			}
			if !errors.Is(err, ErrSchemaMismatch) {
				t.Fatalf("open error is not ErrSchemaMismatch, so no caller can act on it: %v", err)
			}
			msg := err.Error()
			for _, want := range []string{"mesh index", "different version of Mesh"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal does not name the remedy (%q missing):\n%s", want, msg)
				}
			}
		})
	}
}

// A current index must still open, or the check above is a denial of service on every user.
func TestReadOnlyOpenAcceptsACurrentIndex(t *testing.T) {
	dir := seedIndexedVault(t)
	store, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("a freshly written index was refused: %v", err)
	}
	defer store.Close()
	if d := mustDropped(t, store); len(d) != 0 {
		t.Errorf("a clean vault reported dropped notes: %+v", d)
	}
}

// TestDroppedNotesReportsAFailedRead: "nothing was dropped" and "I could not find out"
// are opposite answers and only one of them is good news. droppedFromIndex used to return
// nil for a missing dropped_notes table, which is how mesh_health called an unmigrated
// vault clean. The read-only open now refuses a mismatched schema up front, so this drops
// the table underneath a live store to exercise the same swallow directly.
func TestDroppedNotesReportsAFailedRead(t *testing.T) {
	dir := seedIndexedVault(t)
	store, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	dbPath := filepath.Join(dir, ".mesh", "mesh.db")
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE dropped_notes`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	dropped, err := store.DroppedNotes()
	if err == nil {
		t.Fatalf("DroppedNotes answered %+v with no error although it could not read the record; that reads as a clean vault", dropped)
	}
	if !strings.Contains(err.Error(), "mesh index") {
		t.Errorf("the failure does not name the remedy:\n%v", err)
	}
}

// TestSchemaVersionCoversEveryTable is the guard for the ROOT cause: dropped_notes was
// added to schema.sql without bumping SchemaVersion, so nothing could tell an index that
// has the table from one that does not. The subject list is ENUMERATED from schema.sql, so
// a table added tomorrow is covered without anyone remembering to add it here.
//
// If this fails after a schema edit: bump SchemaVersion (an existing index cannot grow a
// table on a read-only surface, so it has to be rebuilt) and paste the printed fingerprint.
func TestSchemaVersionCoversEveryTable(t *testing.T) {
	const wantVersion = 8
	const wantFingerprint = "06050a26e2"

	if SchemaVersion != wantVersion {
		t.Fatalf("SchemaVersion = %d, this guard is pinned to %d; update both together", SchemaVersion, wantVersion)
	}
	got := schemaTablesFingerprint(t)
	if got != wantFingerprint {
		t.Errorf("schema.sql table shape changed (fingerprint %s, baked-in %s) while SchemaVersion stayed %d.\n"+
			"An index written before the change does not have the new shape and no read-only surface can migrate it.\n"+
			"Bump SchemaVersion, then set wantVersion/wantFingerprint in this test to %d/%s.",
			got, wantFingerprint, SchemaVersion, SchemaVersion+1, got)
	}
}

// schemaTablesFingerprint hashes every CREATE TABLE (and CREATE VIRTUAL TABLE) block in
// schema.sql, keyed by table name, with comments stripped and whitespace collapsed so a
// comment or formatting edit does not trip the guard. Index and trigger DDL is excluded:
// a missing index costs speed, never a wrong answer.
func schemaTablesFingerprint(t *testing.T) string {
	t.Helper()
	re := regexp.MustCompile(`(?i)CREATE\s+(?:VIRTUAL\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["` + "`" + `]?([A-Za-z_][A-Za-z0-9_]*)`)
	comments := regexp.MustCompile(`--[^\n]*`)
	spaces := regexp.MustCompile(`\s+`)
	blocks := map[string]string{}
	for _, loc := range re.FindAllStringSubmatchIndex(SchemaSQL, -1) {
		name := SchemaSQL[loc[2]:loc[3]]
		rest := SchemaSQL[loc[0]:]
		end := strings.Index(rest, ");")
		if end < 0 {
			t.Fatalf("could not find the end of CREATE TABLE %q in schema.sql", name)
		}
		block := comments.ReplaceAllString(rest[:end], "")
		blocks[name] = strings.TrimSpace(strings.ToLower(spaces.ReplaceAllString(block, " ")))
	}
	if len(blocks) == 0 {
		t.Fatal("parsed no CREATE TABLE blocks from schema.sql")
	}
	names := make([]string, 0, len(blocks))
	for name := range blocks {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		fmt.Fprintf(h, "%s\n%s\n", name, blocks[name])
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}
