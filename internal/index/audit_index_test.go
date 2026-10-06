// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeFixture writes one markdown file under dir and returns its absolute path.
func writeFixture(t *testing.T, dir, rel, frontmatter, body string) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("---\n"+frontmatter+"---\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// F1: retrievalHash is the ONLY drift signal, so every frontmatter field that lands in a
// persisted notes column or a graph node attr must be hashed. While scope/updated/when/
// review_by/source were omitted, an edit touching only one of them produced an identical
// hash: both drift gates reported nothing, Reconcile returned early, and the stale value
// stayed live. For scope that is an access-control failure: tightening a note's scope on
// a running hub did not revoke read access until an unrelated event forced a reindex.
func TestRetrievalHashCoversPersistedFrontmatterFields(t *testing.T) {
	const body = "# A\nunchanged body, deliberately identical across the edit\n"
	cases := []struct {
		name   string
		before string
		after  string
		verify func(t *testing.T, s *Store)
	}{
		{
			name:   "scope narrowed",
			before: "id: a\ntype: note\nwhen: \"2026-01-01\"\nscope:\n  - dev\n  - sales\n",
			after:  "id: a\ntype: note\nwhen: \"2026-01-01\"\nscope:\n  - dev\n",
			verify: func(t *testing.T, s *Store) {
				got, err := s.NoteScope("a")
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, []string{"dev"}) {
					t.Errorf("NoteScope after narrowing = %v, want [dev]; the revoked scope is still readable", got)
				}
			},
		},
		{
			name:   "updated bumped",
			before: "id: a\ntype: note\nwhen: \"2026-01-01\"\nupdated: \"2026-01-01\"\n",
			after:  "id: a\ntype: note\nwhen: \"2026-01-01\"\nupdated: \"2026-06-01\"\n",
			verify: func(t *testing.T, s *Store) {
				dates, err := s.NoteDates()
				if err != nil {
					t.Fatal(err)
				}
				if dates["a"].Updated != "2026-06-01" {
					t.Errorf("notes.updated = %q, want 2026-06-01; freshness decay is scoring a stale date", dates["a"].Updated)
				}
			},
		},
		{
			name:   "when bumped",
			before: "id: a\ntype: note\nwhen: \"2026-01-01\"\n",
			after:  "id: a\ntype: note\nwhen: \"2026-06-01\"\n",
			verify: func(t *testing.T, s *Store) {
				dates, _ := s.NoteDates()
				if dates["a"].Updated != "2026-06-01" {
					t.Errorf("notes.updated (from when) = %q, want 2026-06-01", dates["a"].Updated)
				}
			},
		},
		{
			name:   "review_by set",
			before: "id: a\ntype: note\nwhen: \"2026-01-01\"\n",
			after:  "id: a\ntype: note\nwhen: \"2026-01-01\"\nreview_by: \"2026-12-31\"\n",
			verify: func(t *testing.T, s *Store) {
				dates, _ := s.NoteDates()
				if dates["a"].ReviewBy != "2026-12-31" {
					t.Errorf("notes.review_by = %q, want 2026-12-31; lifecycle health is blind to it", dates["a"].ReviewBy)
				}
			},
		},
		{
			name:   "source changed",
			before: "id: a\ntype: note\nwhen: \"2026-01-01\"\nsource: human\n",
			after:  "id: a\ntype: note\nwhen: \"2026-01-01\"\nsource: agent\n",
			verify: func(t *testing.T, s *Store) {
				var src string
				if err := s.readDB.QueryRow(`SELECT source FROM notes WHERE id='a'`).Scan(&src); err != nil {
					t.Fatal(err)
				}
				if src != "agent" {
					t.Errorf("notes.source = %q, want agent", src)
				}
			},
		},
		{
			name:   "dead-ref path exemption added",
			before: "id: a\ntype: note\nwhen: \"2026-01-01\"\n",
			after:  "id: a\ntype: note\nwhen: \"2026-01-01\"\nexpect_dead_ref_paths:\n  - internal/handlers/synthetic.go\n",
			verify: func(t *testing.T, s *Store) {
				var fm string
				if err := s.readDB.QueryRow(`SELECT frontmatter FROM notes WHERE id='a'`).Scan(&fm); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(fm, "internal/handlers/synthetic.go") {
					t.Errorf("notes.frontmatter lost expect_dead_ref_paths after incremental reconcile: %s", fm)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFixture(t, dir, "a.md", tc.before, body)
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, _, err := ReindexFull(s, dir); err != nil {
				t.Fatal(err)
			}

			writeFixture(t, dir, "a.md", tc.after, body)

			// The authoritative full path (mesh doctor, startup, the watcher's tick).
			d, err := s.DriftReport(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(d.Changed) != 1 || d.Changed[0] != "a.md" {
				t.Fatalf("DriftReport saw no drift for a frontmatter-only edit: %+v", d)
			}
			// The incremental path the watcher and the hosted hub actually run.
			dd, err := s.DriftDeltaReport(dir, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(dd.Drift.Changed) != 1 {
				t.Fatalf("DriftDeltaReport saw no drift for a frontmatter-only edit: %+v", dd.Drift)
			}

			rec, err := Reconcile(s, dir)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Changed != 1 {
				t.Fatalf("Reconcile Changed = %d, want 1", rec.Changed)
			}
			tc.verify(t, s)
		})
	}
}

// F1 (graph side): the node's scope attr feeds the retriever's card filter, so it must
// follow the same edit, not just the notes column.
func TestScopeOnlyEditUpdatesGraphAttr(t *testing.T) {
	dir := t.TempDir()
	const body = "# A\nbody\n"
	writeFixture(t, dir, "a.md", "id: a\ntype: note\nwhen: \"2026-01-01\"\nscope:\n  - dev\n  - sales\n", body)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	live := NewLiveIndexer(s, dir)
	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}

	writeFixture(t, dir, "a.md", "id: a\ntype: note\nwhen: \"2026-01-01\"\nscope:\n  - dev\n", body)
	rec, err := live.Reconcile(true)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Reindexed {
		t.Fatal("a scope-only edit must reindex; it is the live access-control input")
	}
	n, ok := rec.Graph.Node("note:a")
	if !ok {
		t.Fatal("note:a missing from the rebuilt graph")
	}
	if got, _ := n.Attrs["scope"].(string); got != "dev" {
		t.Errorf("graph node scope attr = %q, want %q; a revoked scope stays readable", got, "dev")
	}
}

// F3: `vectors` is in schemaKeep so a schema-version bump never forces a paid re-embed,
// but vector_model/vector_dim live in `meta`, which IS dropped. Losing them left every
// kept row unreadable (LoadVectors/VectorStats/CachedVectors all filter on
// meta.vector_model) and killed the content-hash embed cache, which is exactly what
// keeping the table was supposed to prevent.
func TestSchemaVersionBumpKeepsVectorsReadable(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "a.md", "id: a\ntype: note\nwhen: \"2026-01-01\"\n", "# A\nbody\n")

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReindexFull(s, dir); err != nil {
		t.Fatal(err)
	}
	noteHash, err := s.NoteRetrievalHash("note:a")
	if err != nil {
		t.Fatal(err)
	}
	const model = "text-embedding-3-small"
	if err := s.ReplaceVectors(model, []VectorRow{{
		NodeID: "note:a", ChunkIx: 0, Vec: []float32{0.1, 0.2, 0.3},
		ContentHash: "chunk-hash", NoteHash: noteHash,
	}}); err != nil {
		t.Fatal(err)
	}
	// Simulate the next release bumping SchemaVersion.
	if err := s.Write(func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE meta SET value=? WHERE key='schema_version'`, "1")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	gotModel, gotDim := s2.VectorMeta()
	if gotModel != model || gotDim != 3 {
		t.Fatalf("after a version bump the kept vectors are orphaned: model=%q dim=%d, want %q/3", gotModel, gotDim, model)
	}
	cached, err := s2.CachedVectors(model)
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != 1 {
		t.Fatalf("embed cache is dead after a version bump: %d reusable chunks, want 1 (this forces a full paid re-embed)", len(cached))
	}
	// The notes table WAS dropped, so re-derive it and prove the rows come back live.
	if _, _, err := ReindexFull(s2, dir); err != nil {
		t.Fatal(err)
	}
	if err := s2.Write(func(tx *sql.Tx) error {
		// The rebuild re-hashed the note; restamp the vector as the embedder would.
		h, e := s2.NoteRetrievalHash("note:a")
		if e != nil {
			return e
		}
		_, e = tx.Exec(`UPDATE vectors SET note_hash=? WHERE node_id='note:a'`, h)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	m, d, byNode, err := s2.LoadVectors()
	if err != nil {
		t.Fatal(err)
	}
	if m != model || d != 3 || len(byNode["note:a"]) != 1 {
		t.Fatalf("LoadVectors after a version bump = model %q dim %d nodes %d, want %q/3/1", m, d, len(byNode), model)
	}
}

// F6: version 1 is the first stamped kept-table shape. Zero therefore means the index
// predates the stamp and must be adopted without erasing non-derivable rows. A greater
// value belongs to a future binary and is covered by the downgrade-refusal tests: it is
// never permission for this older binary to erase tables it does not understand.
func TestKeepShapeVersionAdoptsLegacyStampWithoutLosingRows(t *testing.T) {
	cases := []struct {
		name        string
		storedKeep  string // meta.keep_shape_version to plant before reopening
		wantMetrics int64  // surviving counter value
	}{
		{name: "unchanged shape keeps the rows", storedKeep: "", wantMetrics: 7},
		{name: "legacy unstamped shape adopts the current stamp and keeps the rows", storedKeep: "0", wantMetrics: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.IncrMetric("queries", 7); err != nil {
				t.Fatal(err)
			}
			if tc.storedKeep != "" {
				if err := s.Write(func(tx *sql.Tx) error {
					_, e := tx.Exec(`INSERT INTO meta(key,value) VALUES('keep_shape_version',?)
					                 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, tc.storedKeep)
					return e
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			s2, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s2.Close()
			got, err := s2.Metric("queries")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.wantMetrics {
				t.Errorf("metrics after reopen = %d, want %d (keep_shape_version was %q)", got, tc.wantMetrics, tc.storedKeep)
			}
			// Either way the stored version is left equal to the code's, so the next open
			// is a no-op instead of rebuilding forever.
			var stored string
			if err := s2.readDB.QueryRow(`SELECT value FROM meta WHERE key='keep_shape_version'`).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if want := strconv.Itoa(keepShapeVersion); stored != want {
				t.Errorf("keep_shape_version left at %q, want %q (a rebuild must converge)", stored, want)
			}
		})
	}
}

// F4: the drop-visibility record was wired only into ReindexFull. On the incremental path
// (mesh watch, mesh mcp --watch, the hosted hub) a NEW file with broken frontmatter was
// skipped with no drift entry, no log line and no dropped record, so mesh_health reported
// a clean vault while the note was missing from search and the graph.
func TestIncrementalRecordsUnparseableNewNote(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "good.md", "id: good\ntype: note\nwhen: \"2026-01-01\"\n", "# Good\nbody\n")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	live := NewLiveIndexer(s, dir)
	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}
	if d := mustDropped(t, s); len(d) != 0 {
		t.Fatalf("clean vault should have no dropped notes, got %+v", d)
	}

	// A colon-space in an unquoted value is invalid YAML: the documented cause that
	// silently dropped real notes for weeks.
	writeFixture(t, dir, "bad.md", "id: bad\ntype: gotcha\nupdated: 2026-06-18 (post-mortem: root cause)\n", "# Bad\nbody\n")

	for _, mode := range []struct {
		name          string
		authoritative bool
	}{
		{"mtime fast path", false},
		{"authoritative tick", true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			rec, err := live.Reconcile(mode.authoritative)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Dropped != 1 {
				t.Errorf("Reconciliation.Dropped = %d, want 1", rec.Dropped)
			}
			dropped := mustDropped(t, s)
			if len(dropped) != 1 || dropped[0].Path != "bad.md" {
				t.Fatalf("the unparseable new note must be recorded, got %+v", dropped)
			}
			if dropped[0].Err == nil {
				t.Error("dropped record must carry the parse error")
			}
		})
	}

	// It clears once the frontmatter is fixed, and the note becomes indexed.
	writeFixture(t, dir, "bad.md", "id: bad\ntype: gotcha\nwhen: \"2026-06-18\"\ndo: x\ndont: y\nwhy: z\n", "# Bad\nbody\n")
	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}
	if d := mustDropped(t, s); len(d) != 0 {
		t.Errorf("dropped record must clear once the note parses, got %+v", d)
	}
	var n int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM notes WHERE id='bad'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the fixed note must be indexed, got %d rows", n)
	}
}

// F5: two notes in different folders sharing a basename collide on the lowercased
// wikilink key. Last-wins handed the key to whichever note was walked last, which then
// absorbed every [[basename]] and related: edge in the vault while the other received
// none, with no Issue raised at all.
func TestAmbiguousWikilinkKeyIsReportedNotGuessed(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "decisions/deploy.md", "id: decision-deploy\ntype: note\nwhen: \"2026-01-01\"\n", "# Deploy decision\nbody\n")
	writeFixture(t, dir, "entities/deploy.md", "id: entity-deploy\ntype: note\nwhen: \"2026-01-01\"\n", "# Deploy entity\nbody\n")
	writeFixture(t, dir, "ref.md", "id: ref\ntype: note\nwhen: \"2026-01-01\"\n", "# Ref\ncites [[deploy]]\n")
	writeFixture(t, dir, "qualified.md", "id: qualified\ntype: note\nwhen: \"2026-01-01\"\n", "# Qualified\ncites [[decisions/deploy]]\n")

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g, notes, err := ReindexFull(s, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, issues := BuildGraph(notes)

	kinds := map[string]int{}
	for _, is := range issues {
		kinds[is.Kind]++
	}
	if kinds["ambiguous-link-key"] == 0 {
		t.Errorf("a basename shared by two distinct notes must raise ambiguous-link-key, got issues %+v", issues)
	}
	if kinds["ambiguous-link"] == 0 {
		t.Errorf("a link through an ambiguous key must be reported, got issues %+v", issues)
	}

	// No arbitrary winner: the bare [[deploy]] produces no edge at all rather than a
	// wrong one that steals the other note's backlinks.
	for _, target := range []string{"note:decision-deploy", "note:entity-deploy"} {
		for _, e := range g.Neighbors("note:ref") {
			if e.Source == "note:ref" && e.Target == target && e.Relation == "references" {
				t.Errorf("ambiguous [[deploy]] was guessed into an edge ref -> %s", target)
			}
		}
	}
	// The path-qualified link still resolves, and to the RIGHT note.
	var resolved bool
	for _, e := range g.Neighbors("note:qualified") {
		if e.Source == "note:qualified" && e.Relation == "references" {
			if e.Target != "note:decision-deploy" {
				t.Errorf("[[decisions/deploy]] resolved to %s, want note:decision-deploy", e.Target)
			}
			resolved = true
		}
	}
	if !resolved {
		t.Error("a path-qualified wikilink must resolve; it is the escape hatch from an ambiguous basename")
	}
}

// F5 (no false positive): two un-id'd files sharing a basename resolve to the SAME
// effective id, so the key still points at the one collapsed node. That is the
// duplicate-id case and must not be reported as ambiguous.
func TestSharedBasenameWithoutIDsIsNotAmbiguous(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "a/readme.md", "type: note\nwhen: \"2026-01-01\"\n", "# Readme A\nbody\n")
	writeFixture(t, dir, "b/readme.md", "type: note\nwhen: \"2026-01-01\"\n", "# Readme B\nbody\n")
	writeFixture(t, dir, "ref.md", "id: ref\ntype: note\nwhen: \"2026-01-01\"\n", "# Ref\ncites [[readme]]\n")

	notes, ferrs := ParseFiles(mustWalk(t, dir), 0)
	if len(ferrs) != 0 {
		t.Fatalf("unexpected parse errors: %+v", ferrs)
	}
	g, issues := BuildGraph(notes)
	for _, is := range issues {
		if strings.HasPrefix(is.Kind, "ambiguous") {
			t.Errorf("un-id'd files sharing a basename collapse to one node, not an ambiguity: %+v", is)
		}
	}
	var edges int
	for _, e := range g.Neighbors("note:ref") {
		if e.Source == "note:ref" && e.Relation == "references" {
			edges++
		}
	}
	if edges != 1 {
		t.Errorf("[[readme]] should still resolve to the collapsed node, got %d edges", edges)
	}
}

// F7: the read-only MCP/web tools call IncrMetric/RecordReuse purely for telemetry after
// the answer is already computed. Store.Write is a synchronous, context-free handoff to
// the single writer, so those calls used to inherit the latency of whatever transaction
// was running (a full reindex) with the DSN's 30s busy_timeout as the ceiling. They must
// return immediately and land later.
func TestTelemetryDoesNotBlockBehindTheWriter(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecordWriteback("note-a", "agent"); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE note_reuse SET authored_at=? WHERE note_id=?`, time.Now().Unix()-7200, "note-a")
		return e
	}); err != nil {
		t.Fatal(err)
	}

	// Occupy the single writer with a long transaction.
	busy := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = s.Write(func(*sql.Tx) error {
			close(busy)
			<-release
			return nil
		})
	}()
	<-busy

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.IncrMetric("queries", 1)
		_ = s.IncrMetric("fetch:note-a", 1)
		_ = s.RecordReuse("note-a", 600)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("read-path telemetry blocked on the busy writer; a search or fetch inherits the reindex latency")
	}
	close(release)

	// And it is not lost: the reporting surfaces flush it.
	if got, err := s.Metric("queries"); err != nil || got != 1 {
		t.Fatalf("queries counter = %d (err %v), want 1", got, err)
	}
	st, err := s.FlywheelStats()
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalReuses != 1 {
		t.Errorf("deferred reuse event was lost: TotalReuses = %d, want 1", st.TotalReuses)
	}
}

func mustWalk(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() && strings.HasSuffix(p, ".md") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
