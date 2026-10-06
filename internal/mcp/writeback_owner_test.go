// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
)

// startOwner runs the single owning writer against dir's index for the duration of the
// test: a writable server plus its watcher, which is exactly the production shape
// (`mesh watch` / `mesh sync --watch`, and the hub's own indexing server). The server
// under test opens the SAME index read-only, so these tests exercise the real two-process
// split rather than a stand-in for it.
func startOwner(t *testing.T, dir string) {
	t.Helper()
	startOwnerWith(t, dir, 50*time.Millisecond, 500*time.Millisecond)
}

// startOwnerWith is startOwner with explicit watcher cadence, so a test can run the
// owner at PRODUCTION settings (300ms debounce, 30s periodic reconcile) and tell apart
// "fsnotify delivered the new note" from "the periodic safety net eventually swept it up".
func startOwnerWith(t *testing.T, dir string, debounce, reconcile time.Duration) {
	t.Helper()
	owner, err := NewServerAt(dir, filepath.Join(dir, ".mesh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.WaitReady(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A full-reconcile interval far longer than the test proves the point the
		// write-back bound actually rests on: the CHEAP mtime sweep is what has to pick
		// up a note that missed its file event. If indexing here depended on the
		// authoritative content-hash pass, this would hang.
		_ = owner.Watch(ctx, debounce, reconcile, time.Hour, func(string, ...any) {})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		owner.Close()
	})
	awaitWatcherLive(t, owner, dir)
}

// awaitWatcherLive blocks until the owner's fsnotify watches are actually registered, by
// dropping a probe note and waiting for the owner to index it.
//
// Without this the harness is racy in a way that hides real results: Watch registers its
// watches asynchronously, so a note created in that window generates no event at all and
// waits for the PERIODIC sweep instead. With production's 30s sweep that read as
// "sample 0 never became queryable" and failed only under -v, which is exactly the kind
// of flake that gets rerun until green and believed.
//
// The same window exists in production: a note written in the moment after the owning
// writer starts is missed by fsnotify and picked up only by the sweep. That is survivable
// because the note is durable either way, but it is the reason the periodic sweep matters
// and should not be set far above the write-back bound.
func awaitWatcherLive(t *testing.T, owner *Server, dir string) {
	t.Helper()
	const probeID = "owner-watcher-probe"
	probe := filepath.Join(dir, probeID+".md")
	if err := os.WriteFile(probe,
		[]byte("---\nid: "+probeID+"\ntype: note\nwhen: 2026-01-01\n---\n# Probe\nwatcher liveness probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitProbe(t, owner, probeID, true, "the owner never indexed the watcher probe, so its watcher never came up")
	// Take the probe out again and wait for THAT to land too, so it leaves no trace in the
	// index. A test that counts what a refresh brought into view (mesh_reindex) would
	// otherwise be measuring the harness: the probe is one more added note, indexed at
	// exactly the moment the test is about to write the note it actually cares about.
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	awaitProbe(t, owner, probeID, false, "the owner never dropped the watcher probe from its index")
}

func awaitProbe(t *testing.T, owner *Server, probeID string, want bool, failure string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := owner.store.NotePath(probeID)
		if (err == nil) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(failure)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeNoteVia calls mesh_append_note on srv and returns the decoded tool payload.
func writeNoteVia(t *testing.T, srv *Server, title string) map[string]any {
	t.Helper()
	params, err := json.Marshal(map[string]any{
		"name": "mesh_append_note",
		"arguments": map[string]any{
			"type": "gotcha", "title": title,
			"summary": "Exercise owner acknowledgement using a complete synthetic troubleshooting note.", "sections": fixtureSections("gotcha"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, rerr := srv.handleToolsCall(WithLocalOperator(context.Background()), params)
	if rerr != nil {
		t.Fatalf("mesh_append_note returned an RPC error, but the note is written to disk before any indexing happens, so a write must never fail here: %+v", rerr)
	}
	return decodeToolPayload(t, res)
}

func decodeToolPayload(t *testing.T, res any) map[string]any {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var wrapper struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		t.Fatal(err)
	}
	if len(wrapper.Content) == 0 {
		t.Fatalf("tool result carried no content: %s", b)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(wrapper.Content[0].Text), &out); err != nil {
		t.Fatalf("tool payload was not JSON: %q", wrapper.Content[0].Text)
	}
	return out
}

// With the owner running, a write-back through a READ-ONLY server becomes queryable
// without that server ever taking the write lock. This is the whole point of the split:
// the note reaches the index through the owner's fsnotify, not through this process.
func TestWriteBackThroughTheOwnerBecomesQueryable(t *testing.T) {
	srv := newTestServer(t)
	startOwner(t, srv.vaultRoot)

	out := writeNoteVia(t, srv, "polling beats a second writer")

	if out["index_stale"] == true {
		t.Fatalf("write-back reported a stale index with the owner running: %v", out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no id in write-back result: %v", out)
	}
	if _, err := srv.store.NotePath(id); err != nil {
		t.Fatalf("note %q is not in the index after a successful write-back: %v", id, err)
	}
}

// The owner-down case the design note insisted on: the note must SAVE durably and fail
// LOUDLY on searchability, never hang forever and never report a failed write (a retry
// mints a duplicate note, which has happened three times in production).
func TestWriteBackWithNoOwnerSavesDurablyAndFailsLoudly(t *testing.T) {
	srv := newTestServer(t)
	srv.ownerIndexTimeout = 300 * time.Millisecond // no owner will ever land it; do not wait 10s

	start := time.Now()
	out := writeNoteVia(t, srv, "the owner is not running")
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("write-back hung for %s with no owner; it must fail on a bound", elapsed)
	}
	// Durable: the file is on disk regardless of any indexing.
	rel, _ := out["path"].(string)
	if rel == "" {
		t.Fatalf("no path in write-back result: %v", out)
	}
	if _, err := os.Stat(filepath.Join(srv.vaultRoot, rel)); err != nil {
		t.Fatalf("the note must be durable on disk even with no owner: %v", err)
	}
	// Loud: the caller is told it is not queryable, and told the real reason.
	if out["index_stale"] != true || out["owner_down"] != true {
		t.Fatalf("a note that never got indexed must report index_stale AND owner_down, got: %v", out)
	}
	warning, _ := out["warning"].(string)
	if warning == "" {
		t.Fatal("owner-down write-back carried no warning")
	}
	// It must NOT send the agent to mesh_reindex: on a read-only server that only
	// re-reads what the owner already persisted, so it cannot fix this.
	if want := "Do NOT call mesh_reindex"; !containsStr(warning, want) {
		t.Fatalf("owner-down warning must steer away from mesh_reindex, got: %q", warning)
	}
}

// A note id is not a publication receipt by itself. An editor can delete an indexed
// note and a writer can reuse its slug before the owner has reconciled the deletion.
// In that window NotePath(id) still names the old indexed version. Write-back must wait
// for the bytes it just wrote, rather than treating that stale row as success.
func TestWriteBackDoesNotMistakeAStaleReusedIDForTheNewNote(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "gotchas", "reused-id.md")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("---\nid: reused-id\ntype: gotcha\nwhen: 2026-01-01\ndo: old indexed bytes\ndont: x\nwhy: old\n---\n# Reused ID\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}

	srv, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	srv.ownerIndexTimeout = 150 * time.Millisecond // deliberately no owner

	out := writeNoteVia(t, srv, "reused id")
	if out["index_stale"] != true || out["owner_down"] != true {
		t.Fatalf("the old row for a reused id was accepted as publication of new bytes: %v", out)
	}
}

func TestAwaitOwnerIndexedDoesNotRefreshPastTheValidatedVersion(t *testing.T) {
	dir := t.TempDir()
	notePath := filepath.Join(dir, "published.md")
	if err := os.WriteFile(notePath, []byte("---\nid: published\ntype: note\n---\n# Published\nexpected bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	srv, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	owner, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	srv.ownerIndexTimeout = 150 * time.Millisecond
	var once sync.Once
	srv.beforeOwnerVersionRefresh = func() {
		once.Do(func() {
			if werr := owner.Write(func(tx *sql.Tx) error {
				if _, err := tx.Exec(`DELETE FROM edges WHERE source = 'note:published' OR target = 'note:published'`); err != nil {
					return err
				}
				if _, err := tx.Exec(`DELETE FROM nodes WHERE id = 'note:published' OR note_id = 'published'`); err != nil {
					return err
				}
				_, err := tx.Exec(`DELETE FROM notes WHERE id = 'published'`)
				return err
			}); werr != nil {
				t.Fatal(werr)
			}
		})
	}

	err = srv.awaitOwnerIndexed(context.Background(), "published", notePath)
	if !errors.Is(err, ErrOwnerNotIndexing) {
		t.Fatalf("row removed at refresh boundary was acknowledged as queryable: %v", err)
	}
}

func TestAwaitOwnerIndexedDoesNotAcknowledgeDiskThatAdvancedDuringRefresh(t *testing.T) {
	dir := t.TempDir()
	notePath := filepath.Join(dir, "published.md")
	oldBytes := []byte("---\nid: published\ntype: note\n---\n# Published\nindexed bytes\n")
	newBytes := []byte("---\nid: published\ntype: note\n---\n# Published\neditor advanced the disk version\n")
	if err := os.WriteFile(notePath, oldBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	srv, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.ownerIndexTimeout = 150 * time.Millisecond // deliberately no owner update
	var once sync.Once
	srv.beforeOwnerVersionRefresh = func() {
		once.Do(func() {
			if werr := os.WriteFile(notePath, newBytes, 0o644); werr != nil {
				t.Fatal(werr)
			}
		})
	}

	err = srv.awaitOwnerIndexed(context.Background(), "published", notePath)
	if !errors.Is(err, ErrOwnerNotIndexing) {
		t.Fatalf("disk advanced beyond the installed index snapshot but was acknowledged: %v", err)
	}
}

func TestCancelledWriteDoesNotCreateANote(t *testing.T) {
	srv := newTestServer(t)
	ctx, cancel := context.WithCancel(WithLocalOperator(context.Background()))
	cancel()

	raw := json.RawMessage(`{"type":"gotcha","title":"cancelled before write","do":"x","dont":"y","why":"z"}`)
	if _, rerr := srv.toolWrite(ctx, raw, ""); rerr == nil {
		t.Fatal("a request cancelled before dispatch still reported a successful durable write")
	}
	if _, err := os.Stat(filepath.Join(srv.vaultRoot, "gotchas", "cancelled-before-write.md")); !os.IsNotExist(err) {
		t.Fatalf("a request cancelled before dispatch created a note anyway: %v", err)
	}
}

type cancelAfterFirstErrContext struct {
	context.Context
	errCalls int
}

func (c *cancelAfterFirstErrContext) Err() error {
	c.errCalls++
	if c.errCalls == 1 {
		return nil
	}
	return context.Canceled
}

func TestCancellationDuringPreparationDoesNotCrossTheCreateBoundary(t *testing.T) {
	srv := newTestServer(t)
	ctx := &cancelAfterFirstErrContext{Context: WithLocalOperator(context.Background())}
	raw := json.RawMessage(`{"type":"gotcha","title":"cancelled during preparation","do":"x","dont":"y","why":"z"}`)

	if _, rerr := srv.toolWrite(ctx, raw, ""); rerr == nil {
		t.Fatal("cancellation during reversible preparation still crossed the durable-write boundary")
	}
	if _, err := os.Stat(filepath.Join(srv.vaultRoot, "gotchas", "cancelled-during-preparation.md")); !os.IsNotExist(err) {
		t.Fatalf("cancellation during preparation created a note anyway: %v", err)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// requireTimingProbe keeps wall-clock measurements off shared race-enabled CI runners.
// Those runners deliberately execute service packages concurrently, so scheduler delay
// is part of the observed duration and can exceed a production latency bound even when
// the timer and owner hand-off behave correctly. The deterministic owner/read-only tests
// remain in every run; these probes are for an idle or exclusive measurement host.
func requireTimingProbe(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("timing measurement")
	}
	if os.Getenv("MESH_RUN_TIMING_PROBES") != "1" {
		t.Skip("timing measurement; set MESH_RUN_TIMING_PROBES=1 on a controlled runner")
	}
}

// TestWriteBackLatencyDistribution is the MEASUREMENT the design note required: it
// reports write-to-queryable latency as a distribution against a live owner, so
// ownerIndexTimeout comes from data instead of a guess. It asserts only a generous
// ceiling; the numbers it logs are the artifact.
func TestWriteBackLatencyDistribution(t *testing.T) {
	requireTimingProbe(t)
	srv := newTestServer(t)
	// PRODUCTION cadence deliberately. An owner with a fast periodic sweep hides the
	// case that actually sets the bound: under a burst, some notes miss the fsnotify
	// window and wait for the sweep, and in production that sweep is 30s.
	startOwnerWith(t, srv.vaultRoot, 300*time.Millisecond, 30*time.Second)

	const n = 12
	var samples []time.Duration
	for i := 0; i < n; i++ {
		start := time.Now()
		out := writeNoteVia(t, srv, "latency sample "+string(rune('a'+i)))
		d := time.Since(start)
		if out["index_stale"] == true {
			t.Fatalf("sample %d did not become queryable inside the bound: %v", i, out)
		}
		samples = append(samples, d)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p := func(q float64) time.Duration { return samples[int(float64(len(samples)-1)*q)] }
	t.Logf("write-back to queryable over %d samples: min=%s p50=%s p90=%s max=%s (bound is %s)",
		len(samples), samples[0], p(0.5), p(0.9), samples[len(samples)-1], srv.ownerIndexTimeout)

	if samples[len(samples)-1] > srv.ownerIndexTimeout {
		t.Fatalf("max latency %s exceeded the bound %s", samples[len(samples)-1], srv.ownerIndexTimeout)
	}
}

// The bound is only defensible if fsnotify is what delivers the note. If write-back
// actually rode the owner's PERIODIC reconcile, latency would track that interval, and
// production runs it at 30s, which no sane write-back bound can cover. So: run the owner
// at production cadence with the safety net far out of reach, and require the note to
// land in a small multiple of the debounce. Failing here means the split is relying on
// the sweep rather than on events, and the bound is a fiction.
func TestWriteBackRidesFsnotifyNotThePeriodicSweep(t *testing.T) {
	requireTimingProbe(t)
	srv := newTestServer(t)
	const debounce = 300 * time.Millisecond
	startOwnerWith(t, srv.vaultRoot, debounce, 5*time.Minute) // sweep effectively disabled

	start := time.Now()
	out := writeNoteVia(t, srv, "fsnotify must be the trigger")
	elapsed := time.Since(start)

	if out["index_stale"] == true {
		t.Fatalf("note never became queryable with the periodic sweep out of reach, so fsnotify did not deliver it: %v", out)
	}
	t.Logf("write-back to queryable at production cadence (debounce=%s, sweep=5m): %s", debounce, elapsed)
	if elapsed > 10*debounce {
		t.Fatalf("write-back took %s, far past the %s debounce: it is not event-driven", elapsed, debounce)
	}
}
