// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// L8: the hardcoded drop list in ensureSchema must stay in sync with schema.sql, or a
// future table added without updating it leaks stale rows on a version bump (and a
// rename orphans the old table). Assert dropOnVersionChange + schemaKeep == every
// table declared in schema.sql.
func TestSchemaDropListCoversSchema(t *testing.T) {
	re := regexp.MustCompile(`(?i)CREATE\s+(?:VIRTUAL\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["` + "`" + `]?([A-Za-z_][A-Za-z0-9_]*)`)
	declared := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(SchemaSQL, -1) {
		declared[m[1]] = true
	}
	if len(declared) == 0 {
		t.Fatal("parsed no CREATE TABLE names from schema.sql")
	}
	covered := map[string]bool{}
	for _, name := range dropOnVersionChange {
		covered[name] = true
	}
	for name := range schemaKeep {
		covered[name] = true
	}
	for name := range declared {
		if !covered[name] {
			t.Errorf("table %q is in schema.sql but neither dropped nor kept on a version bump", name)
		}
	}
	for name := range covered {
		if !declared[name] {
			t.Errorf("table %q is in the drop/keep list but not in schema.sql (stale entry)", name)
		}
	}
}

// A kept table's column shape cannot change silently: schema.sql runs with CREATE TABLE
// IF NOT EXISTS, so a column added to a schemaKeep table would never apply on a live hub
// DB. This fingerprints each kept table's DDL; a change fails the test until keepShapeVersion
// is bumped (drop+rebuild that release) or the rows are migrated and the fingerprint updated.
func TestKeptTableShapeGuard(t *testing.T) {
	// Baked-in fingerprints for keepShapeVersion. If this test fails after a schema edit,
	// see keepShapeVersion in store.go before updating these.
	want := map[string]string{
		"metrics":       "976162c7f6",
		"vectors":       "f7efe75545",
		"note_reuse":    "09ecef0eb3",
		"pending_notes": "136c212730",
	}
	for name := range schemaKeep {
		got := keptTableFingerprint(t, name)
		if want[name] != got {
			t.Errorf("kept table %q shape fingerprint = %q, baked-in = %q; if you changed its columns, bump keepShapeVersion (=%d) or migrate live rows, then set this fingerprint",
				name, got, want[name], keepShapeVersion)
		}
	}
}

// keptTableFingerprint extracts a table's CREATE block from schema.sql, strips comments
// and collapses whitespace, and returns a short hash of the normalized column list.
func keptTableFingerprint(t *testing.T, table string) string {
	t.Helper()
	idx := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?` + regexp.QuoteMeta(table) + `\s*\(`).FindStringIndex(SchemaSQL)
	if idx == nil {
		t.Fatalf("kept table %q not found in schema.sql", table)
	}
	rest := SchemaSQL[idx[0]:]
	end := strings.Index(rest, ");")
	if end < 0 {
		t.Fatalf("could not find end of CREATE TABLE %q", table)
	}
	block := rest[:end]
	// Strip -- line comments and collapse whitespace so a comment/format-only edit does
	// not trip the guard; only a real column change does.
	block = regexp.MustCompile(`--[^\n]*`).ReplaceAllString(block, "")
	block = regexp.MustCompile(`\s+`).ReplaceAllString(block, " ")
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(block))))
	return hex.EncodeToString(sum[:])[:10]
}

// H1: two files sharing a basename with no frontmatter id resolve to the same
// effectiveID. The full reindex path used a plain INSERT, so the duplicate hit the
// notes.id PRIMARY KEY and rolled back the WHOLE transaction, taking the entire
// index offline (0 notes) with an opaque SQLite error. It must instead tolerate the
// collision (last-wins, like the incremental path) and keep every other note indexed.
func TestReindexFullDuplicateBasenameDoesNotBrickIndex(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Two un-id'd files with the same basename in different folders.
	mustWrite("a/readme.md", "# Readme A\nalpha content\n")
	mustWrite("b/readme.md", "# Readme B\nbravo content\n")
	// A valid, unrelated note that MUST remain indexed.
	mustWrite("keep.md", "---\nid: keep\ntype: note\nwhen: 2026-01-01\n---\n# Keep\nimportant searchable knowledge\n")

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, _, err := ReindexFull(s, dir); err != nil {
		t.Fatalf("ReindexFull must not fail on a duplicate basename, got: %v", err)
	}
	n, err := s.Count("notes")
	if err != nil {
		t.Fatal(err)
	}
	// keep + one of the readmes (last-wins collapse) = 2 distinct notes; the point is
	// the index is NOT empty.
	if n < 2 {
		t.Fatalf("index bricked: notes=%d, want >=2 (the valid note must survive a dup basename)", n)
	}
	var keepRows int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM notes WHERE id='keep'`).Scan(&keepRows); err != nil {
		t.Fatal(err)
	}
	if keepRows != 1 {
		t.Fatalf("valid note 'keep' missing after dup-basename reindex (keepRows=%d)", keepRows)
	}
	// No duplicate FTS rows for the collapsed node.
	var ftsRows int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM search_index WHERE node_id='note:readme'`).Scan(&ftsRows); err != nil {
		t.Fatal(err)
	}
	if ftsRows > 1 {
		t.Fatalf("duplicate FTS rows for collapsed node: %d", ftsRows)
	}
}

// H2: BuildGraph (the in-memory graph the MCP serves) and LoadGraph (the DB-reloaded
// graph the CLI uses) must report identical node degrees. BuildGraph interleaves
// AddNode/AddEdge so an inbound edge to a not-yet-added later note used to be missed,
// making MCP and CLI disagree on degree (corrupting god-node ranking + hub-skip).
func TestBuildGraphLoadGraphDegreesAgree(t *testing.T) {
	dir := t.TempDir()
	wr := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// b is defined AFTER a but a references b, exercising the forward-reference case.
	wr("a.md", "---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# A\nlinks to [[b]] and [[c]]\n")
	wr("b.md", "---\nid: b\ntype: note\nwhen: 2026-01-01\n---\n# B\nlinks to [[c]]\n")
	wr("c.md", "---\nid: c\ntype: note\nwhen: 2026-01-01\n---\n# C\nno outbound links\n")

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	built, _, err := ReindexFull(s, dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadGraph()
	if err != nil {
		t.Fatal(err)
	}
	for _, nd := range built.Nodes() {
		ln, ok := loaded.Node(nd.ID)
		if !ok {
			t.Fatalf("node %s present in BuildGraph but not LoadGraph", nd.ID)
		}
		if nd.Degree != ln.Degree {
			t.Fatalf("degree mismatch for %s: BuildGraph=%d LoadGraph=%d", nd.ID, nd.Degree, ln.Degree)
		}
	}
	// Sanity: c is referenced by both a and b, so its (inbound) degree must be >= 2.
	if cNode, ok := built.Node("note:c"); !ok || cNode.Degree < 2 {
		got := -1
		if cNode != nil {
			got = cNode.Degree
		}
		t.Fatalf("note:c inbound degree = %d, want >= 2 (referenced by a and b)", got)
	}
}

// I3: a panic inside a write transaction must be recovered into an error, not kill
// the single writer goroutine. A dead writer would leave every future Write blocked
// forever on the jobs channel. The hub Store already guards this; the index Store
// must too.
func TestWriterSurvivesPanic(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	err = s.Write(func(*sql.Tx) error {
		panic("boom inside a write")
	})
	if err == nil {
		t.Fatal("a panicking write must return an error, not nil")
	}

	// The writer goroutine must still be alive: a subsequent write must complete and
	// not block forever.
	done := make(chan error, 1)
	go func() {
		done <- s.Write(func(tx *sql.Tx) error {
			_, e := tx.Exec(`DELETE FROM notes WHERE id='nonexistent'`)
			return e
		})
	}()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("post-panic write failed: %v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("writer deadlocked after a panic (the bug); subsequent Write never returned")
	}
}
