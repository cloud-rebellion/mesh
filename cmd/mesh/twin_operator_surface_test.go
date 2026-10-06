// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/pkg/meshclient"
)

// capture runs fn with both standard streams redirected into a pipe and returns
// what it wrote, alongside the error it returned. The operator judges these code
// paths by what they print AND by the exit code, so a test that checks only one of
// the two misses exactly the defects here: a run that printed every failure and then
// exited 0, and a count that was computed and printed nowhere.
func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	os.Stdout, os.Stderr = w, w
	runErr := fn()
	_ = w.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	b, _ := io.ReadAll(r)
	_ = r.Close()
	return string(b), runErr
}

// TestSyncSummaryTellsTheOperatorWorkRemains is the operator half of the bounded-push
// fix. SyncVault caps how many dirty notes one round pushes and defers the rest into
// Summary.Remaining, but nothing outside the tests ever read that field: the CLI
// printed only "synced: pushed %d, pulled %d, %d conflict(s) (HEAD %s)". A user with
// 12000 dirty notes saw "pushed 4000" and had no way to learn that 8000 edits were
// still local, invisible to the team, and waiting for another run. A number nobody
// prints is the same as no fix at all.
//
// Both the one-shot path and the --watch loop render through syncSummaryLines, so
// this covers both. That sharing is itself part of the fix: the two had drifted into
// separate wordings of the same four lines, which is how one of them ends up with a
// change the other never gets.
func TestSyncSummaryTellsTheOperatorWorkRemains(t *testing.T) {
	tests := []struct {
		name       string
		sum        meshclient.Summary
		wantLines  []string
		notWant    []string
		wantMoved  bool
		wantRemain bool
	}{
		{
			name:       "a bounded round names the deferred remainder and the next action",
			sum:        meshclient.Summary{Pushed: 4000, Remaining: 8000, Head: "abcdef1234"},
			wantLines:  []string{"pushed 4000", "8000 more changed note(s)", "mesh sync"},
			wantMoved:  true,
			wantRemain: true,
		},
		{
			name:      "a complete round says nothing about a remainder",
			sum:       meshclient.Summary{Pushed: 3, Pulled: 1, Head: "abcdef1234"},
			wantLines: []string{"pushed 3, pulled 1"},
			notWant:   []string{"still queued", "more changed note(s)"},
			wantMoved: true,
		},
		{
			name:      "an idle round is silent under watch",
			sum:       meshclient.Summary{Head: "abcdef1234"},
			wantLines: []string{"pushed 0"},
			notWant:   []string{"more changed note(s)"},
			wantMoved: false,
		},
		{
			name: "a remainder alongside conflicts keeps both lines",
			sum: meshclient.Summary{
				Pushed: 10, Remaining: 5, Conflicts: 1, Head: "abcdef1234",
				ConflictSiblings: []string{"a.sync-conflict-20260806-tom-0123456789abcdef.md"},
			},
			wantLines:  []string{"5 more changed note(s)", "conflict: hub version kept"},
			wantMoved:  true,
			wantRemain: true,
		},
		{
			name:      "a round that only deferred still logs under watch",
			sum:       meshclient.Summary{Remaining: 2, Head: "abcdef1234"},
			wantMoved: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := strings.Join(syncSummaryLines(tc.sum), "\n")
			for _, want := range tc.wantLines {
				if !strings.Contains(out, want) {
					t.Errorf("summary missing %q:\n%s", want, out)
				}
			}
			for _, no := range tc.notWant {
				if strings.Contains(out, no) {
					t.Errorf("summary must not contain %q:\n%s", no, out)
				}
			}
			if tc.wantRemain && !strings.Contains(out, strconv.Itoa(tc.sum.Remaining)) {
				t.Errorf("summary never prints the deferred count %d, so the operator cannot "+
					"tell a bounded round from a complete one:\n%s", tc.sum.Remaining, out)
			}
			if got := syncSummaryMoved(tc.sum); got != tc.wantMoved {
				t.Errorf("syncSummaryMoved = %v, want %v (an unlogged round is an invisible one)", got, tc.wantMoved)
			}
		})
	}
}

func TestRetiredFillBodiesCannotPublish(t *testing.T) {
	for _, apply := range []bool{false, true} {
		dir := t.TempDir()
		original := "---\nid: incident\ntype: post-mortem\nwhy: An unverified explanation.\ndont: Avoid premature conclusions.\n---\n# Incident\n"
		path := writeNote(t, dir, "incident.md", original)
		_, err := capture(t, func() error { return fillNoteBodies(dir, []string{path}, apply) })
		if err == nil || !strings.Contains(err.Error(), "migration-preview") {
			t.Fatalf("retired conversion not refused: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != original {
			t.Fatalf("retired conversion changed source: %q %v", got, err)
		}
	}
}

// linkedNote is a note that links to the two others, so it is not itself an orphan.
func linkedNote(id, title, body string) string {
	return "---\nid: " + id + "\ntype: note\ntitle: " + title +
		"\nwhen: \"2026-08-06\"\ntags: [postgres]\n---\n# " + title + "\n\n" + body + "\n"
}

// TestWireOrphansSplitsBenignSkipsFromWriteFailures covers both halves of the third
// finding at once, because one does not work without the other.
//
// wireOrphanNotes had ONE counter named `skipped` absorbing three unrelated outcomes:
// retrieval found no candidate links (benign), the note already declared `related`
// (benign, and the point of the idempotence), and the write failed (an error). While
// the third was buried inside a benign-sounding total there was nothing to base an
// exit code on, so the function printed every failure and returned nil. Splitting the
// counters is what makes the non-zero exit possible, so the test asserts the split
// (the benign line names its two reasons separately) and the exit code together.
func TestWireOrphansSplitsBenignSkipsFromWriteFailures(t *testing.T) {
	// Three interlinked notes give retrieval something to propose, and keep
	// themselves out of the orphan set. The orphan shares their vocabulary so a
	// candidate is found and the write is actually attempted.
	base := map[string]string{
		"alpha.md": linkedNote("alpha", "Postgres migration alpha", "Postgres migration notes. See [[beta]] and [[gamma]]."),
		"beta.md":  linkedNote("beta", "Postgres migration beta", "More Postgres migration notes. See [[alpha]]."),
		"gamma.md": linkedNote("gamma", "Postgres migration gamma", "Yet more Postgres migration notes. See [[alpha]]."),
	}
	orphan := linkedNote("postgres-migration-orphan", "Postgres migration orphan", "Postgres migration, unlinked.")

	tests := []struct {
		name      string
		readOnly  bool
		wantErr   bool
		wantOut   []string
		notWant   []string
		wantWrite bool
	}{
		{
			name:      "a writable orphan is wired and the run exits 0",
			wantErr:   false,
			wantOut:   []string{"wired 1 note(s)", "no corroborated candidate links", "already declaring related"},
			notWant:   []string{"failed on"},
			wantWrite: true,
		},
		{
			name:    "an unwritable orphan fails the run instead of counting as skipped",
			wantErr: true,
			wantOut: []string{"failed on 1 note(s)"},
			// The benign line must still be honest: the failure is NOT a skip.
			notWant:  []string{"skipped 1 ("},
			readOnly: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range base {
				writeNote(t, dir, name, body)
			}
			orphanPath := writeNote(t, dir, "postgres-migration-orphan.md", orphan)
			if _, err := runCLI(t, indexCmd(), dir); err != nil {
				t.Fatalf("mesh index: %v", err)
			}
			if tc.readOnly {
				// Root ignores the mode bits below, so the write this case exists to
				// FAIL simply succeeds and the assertions read as a real regression in
				// wire-orphans. That is not hypothetical: the public-mirror publish runs
				// `go test` inside mesh-publish.Dockerfile, which declares no USER and
				// therefore runs as root, so this case failed there while passing for
				// every developer. Skip rather than weaken it: as an unprivileged user
				// it still guards the benign-skip vs write-failure split.
				if os.Geteuid() == 0 {
					t.Skip("running as root: a 0444 file is still writable, so the write " +
						"failure this case needs cannot be provoked")
				}
				// Readable so it still indexes as an orphan and retrieval still runs;
				// unwritable so BackfillRelatedFile's os.WriteFile is what fails.
				if err := os.Chmod(orphanPath, 0o444); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(orphanPath, 0o644) })
			}
			out, runErr := runCLI(t, structureCmd(), dir, "--wire-orphans", "--apply")
			if tc.wantErr && runErr == nil {
				t.Errorf("wire-orphans exited 0 after failing to write\n%s", out)
			}
			if !tc.wantErr && runErr != nil {
				t.Errorf("wire-orphans failed unexpectedly: %v\n%s", runErr, out)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			for _, no := range tc.notWant {
				if strings.Contains(out, no) {
					t.Errorf("output must not contain %q:\n%s", no, out)
				}
			}
			if tc.wantWrite {
				got, err := os.ReadFile(orphanPath)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(got), "related:") {
					t.Errorf("orphan was reported wired but has no related block:\n%s", got)
				}
			}
		})
	}
}

// TestHealthReportsUnparseableNotes is the doctor twin. `mesh health` computed every
// finding from INDEX ROWS, and a note that will not parse never became a row, so a
// vault whose every note was unparseable had nothing to hold a dead ref, nothing to
// be overdue and nothing to contradict anything: health printed
//
//	vault healthy: no dead refs, overdue reviews, or contradictions   (exit 0)
//
// on a vault where the entire knowledge base was invisible to search. Doctor carried
// exactly this blind spot, doctor was fixed, and its twin was not.
func TestHealthReportsUnparseableNotes(t *testing.T) {
	tests := []struct {
		name       string
		notes      map[string]string
		wantErr    bool
		wantOut    string
		notWantOut string
	}{
		{
			name:       "a vault of only unparseable notes is BROKEN, not healthy",
			notes:      map[string]string{"bad.md": unparseableNote},
			wantErr:    true,
			wantOut:    "health: BROKEN",
			notWantOut: "vault healthy",
		},
		{
			name:       "one bad note among good ones still breaks the verdict",
			notes:      map[string]string{"good.md": goodNote, "bad.md": unparseableNote},
			wantErr:    true,
			wantOut:    "health: BROKEN",
			notWantOut: "vault healthy",
		},
		{
			name:       "the failing note is named so it can be fixed",
			notes:      map[string]string{"bad.md": unparseableNote},
			wantErr:    true,
			wantOut:    "bad.md",
			notWantOut: "vault healthy",
		},
		{
			name:       "a clean vault still reads healthy and exits 0",
			notes:      map[string]string{"good.md": goodNote},
			wantErr:    false,
			wantOut:    "vault healthy",
			notWantOut: "BROKEN",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.notes {
				writeNote(t, dir, name, body)
			}
			if _, err := runCLI(t, indexCmd(), dir); err != nil {
				t.Fatalf("mesh index: %v", err)
			}
			out, err := runCLI(t, healthCmd(), dir)
			if tc.wantErr && err == nil {
				t.Errorf("health exited 0 on a vault with notes invisible to search\n%s", out)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("health failed on a clean vault: %v\n%s", err, out)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("health output missing %q:\n%s", tc.wantOut, out)
			}
			if strings.Contains(out, tc.notWantOut) {
				t.Errorf("health output must not contain %q:\n%s", tc.notWantOut, out)
			}
		})
	}
}

// TestIngestReindexSurfacesParseErrors covers the `notes, _ := index.ParseFiles(...)`
// in reindexVault, in a function whose own comment claimed it mirrored `mesh index`
// while dropping the one thing `mesh index` prints loudest. Before this, an import
// whose every rendered note was unparseable printed "reindexed; imported notes are
// searchable" and exited 0, with the imported items in neither the index nor the graph.
//
// The two kinds of failure are not the same event and the test pins the difference:
// a broken note the connector JUST wrote is an import failure, and a broken note that
// predates the import is vault damage that must be reported without turning every
// scheduled pull red for something the pull did not cause.
func TestIngestReindexSurfacesParseErrors(t *testing.T) {
	const owned = "imported/github"
	tests := []struct {
		name    string
		notes   map[string]string
		folders []string
		wantErr bool
		wantOut []string
	}{
		{
			name:    "a just-imported note that will not parse fails the import",
			notes:   map[string]string{"imported/github/bad.md": unparseableNote, "good.md": goodNote},
			folders: []string{owned},
			wantErr: true,
			wantOut: []string{"just-imported note", "imported/github/bad.md", "invisible to search"},
		},
		{
			name:    "a pre-existing broken note is reported but does not fail the import",
			notes:   map[string]string{"imported/github/ok.md": goodNote, "legacy/bad.md": unparseableNote},
			folders: []string{owned},
			wantErr: false,
			wantOut: []string{"warning:", "legacy/bad.md", "predates this import"},
		},
		{
			name:    "a clean vault is silent and succeeds",
			notes:   map[string]string{"imported/github/ok.md": goodNote},
			folders: []string{owned},
			wantErr: false,
		},
		{
			name:    "a folder name that is only a prefix of another is not treated as owned",
			notes:   map[string]string{"imported/githubs/bad.md": unparseableNote},
			folders: []string{owned},
			wantErr: false,
			wantOut: []string{"predates this import"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.notes {
				p := filepath.Join(dir, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := capture(t, func() error { return reindexVault(dir, tc.folders...) })
			if tc.wantErr && err == nil {
				t.Errorf("reindex reported success while the notes it just imported are invisible to search\n%s", out)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("reindex failed for something the import did not cause: %v\n%s", err, out)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			if len(tc.wantOut) == 0 && strings.TrimSpace(out) != "" {
				t.Errorf("clean reindex printed diagnostics:\n%s", out)
			}
		})
	}
}

// TestTakeMineReceiptNamesADeferredRemainder covers the third caller of SyncVault.
// `mesh conflicts resolve --take-mine` restores the parked version, pushes it, deletes
// the sibling, and prints "took your version and pushed it". SyncVault pushes a bounded
// batch, and the restored note is an ordinary dirty path in that outbox, so on a vault
// with a backlog it can sit in the deferred tail: sibling gone, receipt claims the hub
// has it, hub does not. The bytes are safe at the note's own path and stay dirty, so
// this is a false receipt rather than data loss, but the user stops at the receipt.
func TestTakeMineReceiptNamesADeferredRemainder(t *testing.T) {
	tests := []struct {
		name    string
		sum     meshclient.Summary
		want    []string
		notWant []string
	}{
		{
			name: "a complete round may claim the push landed",
			sum:  meshclient.Summary{Pushed: 1, Head: "abcdef1234"},
			want: []string{"took your version and pushed it", "abcdef12"},
			// Nothing was deferred, so no caveat should appear.
			notWant: []string{"still queued"},
		},
		{
			name: "a bounded round says the note may still be queued",
			sum:  meshclient.Summary{Pushed: 4000, Remaining: 8000, Head: "abcdef1234"},
			want: []string{"8000 changed note(s) are still queued", "this note may be one of them", "mesh sync"},
		},
		{
			name:    "an exact deferred list proves this note was sent",
			sum:     meshclient.Summary{Pushed: 4000, Remaining: 1, Deferred: []string{"topics/other.md"}, Head: "abcdef1234"},
			want:    []string{"1 changed note(s) are still queued", "resolved note was sent", "named deferred paths were not"},
			notWant: []string{"may be one of them"},
		},
		{
			name: "a single deferred note is enough to caveat the receipt",
			sum:  meshclient.Summary{Pushed: 4000, Remaining: 1, Head: "abcdef1234"},
			want: []string{"1 changed note(s) are still queued"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := strings.Join(takeMineReceipt("topics/topic.md", tc.sum), "\n")
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("receipt missing %q:\n%s", want, out)
				}
			}
			for _, no := range tc.notWant {
				if strings.Contains(out, no) {
					t.Errorf("receipt must not contain %q:\n%s", no, out)
				}
			}
		})
	}
}
