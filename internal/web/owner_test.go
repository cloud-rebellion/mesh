// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
)

// writeNote drops a note into a vault the way an editor (or the owner's peers) would.
func writeNote(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedIndex plays the part of the single owning writer: it builds the index once and
// closes, so a read-only NewServer has something to read. Production has the same
// ordering (`mesh watch` / `mesh sync --watch` owns the index; the viewer reads it),
// which is why the tests do it this way rather than handing the viewer a writable store
// it would only ever get in the mesh-ui container.
func seedIndex(t *testing.T, dir string) {
	t.Helper()
	owner, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, _, err := index.ReindexFull(owner, dir); err != nil {
		t.Fatal(err)
	}
}

// runOwner starts a writer that behaves like `mesh watch`: it reconciles the vault and
// drains the op queue on a tight loop until the test ends. Tests that exercise a write
// path through a read-only viewer need one, because the viewer's contract is that the
// owner applies what it queues.
func runOwner(t *testing.T, dir string) {
	t.Helper()
	owner, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	live := index.NewLiveIndexer(owner, dir)
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			if _, err := live.Reconcile(true); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done; owner.Close() })
}

// seedPending plays the part of `mesh extract`, which is a one-shot writer: it fills the
// review queue and closes. The viewer reads that queue and resolves items through the
// owner; it never populates it.
func seedPending(t *testing.T, dir string, items ...index.PendingNote) {
	t.Helper()
	owner, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	for _, p := range items {
		if err := owner.AddPending(p); err != nil {
			t.Fatalf("seeding the queue: %v", err)
		}
	}
}

// TestNewServerOpensReadOnly pins the one character everything above rests on. The
// viewer stays open for as long as the browser tab does, so a writable store here is a
// second long-lived writer against one mesh.db, which is the exact shape the
// single-writer split removed from `mesh mcp`. A change that flips this back must fail a
// test rather than be discovered later as WAL growth.
func TestNewServerOpensReadOnly(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "n.md", "---\nid: n\ntype: note\nwhen: 2026-01-01\n---\n# N\nbody\n")
	seedIndex(t, dir)

	s, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.store.ReadOnly() {
		t.Fatal("web.NewServer opened a WRITABLE store: the viewer is a second long-lived writer again")
	}
}

// TestEveryRouteSurvivesReadOnly is the durable guard against the class of bug the MCP
// split kept finding one instance at a time: a surface that looks like a read but writes
// somewhere down its call stack, which on a read-only store fails the whole request with
// an opaque 500. Every route in routes() must have an entry here, so a new one cannot
// ship without someone deciding what it does on the viewer people actually run.
//
// No owner runs here on purpose. The write routes are still expected to SUCCEED, loudly,
// saying the owner is down: a promote whose note is on disk must never report failure,
// or the reviewer promotes it again and the vault gets two.
func TestEveryRouteSurvivesReadOnly(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "n.md", "---\nid: n\ntype: note\nwhen: 2026-01-01\n---\n# N\nbody\n")
	seedIndex(t, dir)
	seedPending(t, dir, index.PendingNote{Type: "gotcha", Title: "Promote me", Do: "x", Dont: "y", Why: "z"})
	seedPending(t, dir, index.PendingNote{Type: "gotcha", Title: "Discard me", Do: "x", Dont: "y", Why: "z"})

	s, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// No owner will ever land these, so do not sit on the full 10s bound three times.
	s.ownerWait = 200 * time.Millisecond
	h := s.Handler()

	type probe struct {
		method, path, body string
	}
	probes := map[string]probe{
		"GET /":            {"GET", "/", ""},
		"GET /graph.json":  {"GET", "/graph.json", ""},
		"GET /assets/":     {"GET", "/assets/app.js", ""},
		"POST /api/login":  {"POST", "/api/login", `{"token":"nope"}`},
		"POST /api/logout": {"POST", "/api/logout", ""},
		// Connections are disabled on an unconfigured local viewer. Enabled flow is
		// exercised through Handler in connections_test.go without an index writer.
		"GET /connect":                 {"GET", "/connect", ""},
		"POST /api/connect/device":     {"POST", "/api/connect/device", "{}"},
		"POST /api/connect/token":      {"POST", "/api/connect/token", "{}"},
		"POST /api/connect/cancel":     {"POST", "/api/connect/cancel", "{}"},
		"POST /api/connect/revoke":     {"POST", "/api/connect/revoke", "{}"},
		"GET /api/connect/request":     {"GET", "/api/connect/request", ""},
		"POST /api/connect/decision":   {"POST", "/api/connect/decision", "{}"},
		"GET /api/connect/connections": {"GET", "/api/connect/connections", ""},
		"POST /api/connect/disconnect": {"POST", "/api/connect/disconnect", "{}"},
		"GET /api/status":              {"GET", "/api/status", ""},
		"GET /api/config":              {"GET", "/api/config", ""},
		"PUT /api/config":              {"PUT", "/api/config", `{"retrieval.budget_tokens":"4000"}`},
		"POST /api/reindex":            {"POST", "/api/reindex", ""},
		"GET /api/search":              {"GET", "/api/search?q=body", ""},
		"GET /api/note/{id}":           {"GET", "/api/note/n", ""},
		"GET /api/docs":                {"GET", "/api/docs", ""},
		"GET /api/docs/{slug}":         {"GET", "/api/docs/quickstart", ""},
		"GET /api/mcp-tools":           {"GET", "/api/mcp-tools", ""},
		"GET /api/dashboard":           {"GET", "/api/dashboard", ""},
		"GET /api/pending":             {"GET", "/api/pending", ""},
		"POST /api/pending/promote":    {"POST", "/api/pending/promote", `{"id":"` + index.PendingID("gotcha", "Promote me") + `"}`},
		"POST /api/pending/discard":    {"POST", "/api/pending/discard", `{"id":"` + index.PendingID("gotcha", "Discard me") + `"}`},
		"GET /openapi.json":            {"GET", "/openapi.json", ""},
		// /api/ask forks an LLM subprocess, so it is deliberately not driven here. It is
		// listed so the completeness check below still covers it: if it ever grows a write,
		// this entry is the reminder that nothing here would have caught it.
		"POST /api/ask": {},
	}
	for _, rt := range s.routes() {
		if _, ok := probes[rt.pattern]; !ok {
			t.Fatalf("route %q has no entry here: decide what it does on a read-only viewer (the one every laptop runs) before shipping it", rt.pattern)
		}
	}

	// The write routes wait for an owner that will never come, so shrink nothing and
	// instead accept the wall time once: this is the honest bound a real user would hit.
	for pattern, p := range probes {
		if p.method == "" {
			continue
		}
		t.Run(pattern, func(t *testing.T) {
			var body io.Reader
			if p.body != "" {
				body = strings.NewReader(p.body)
			}
			req := httptest.NewRequest(p.method, p.path, body)
			if p.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			// 401/403/400 are legitimate answers (auth, bad input). A 500 is not: that is
			// the ErrReadOnly shape this guard exists to catch.
			if rec.Code >= 500 {
				t.Fatalf("%s %s = %d on a read-only viewer: %s", p.method, p.path, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// TestReadOnlyWritesRouteThroughTheOwner is the positive half: the viewer's write
// features must actually take effect, not merely avoid erroring. It runs the production
// shape (a read-only viewer in front of a live owning writer) and asserts the effects
// land in the index the owner writes.
func TestReadOnlyWritesRouteThroughTheOwner(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "n.md", "---\nid: n\ntype: note\nwhen: 2026-01-01\n---\n# N\nbody\n")
	seedIndex(t, dir)
	seedPending(t, dir, completePending("Route me"))
	runOwner(t, dir)

	s, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := s.Handler()

	id := index.PendingID("gotcha", "Route me")
	req := httptest.NewRequest("POST", "/api/pending/promote", strings.NewReader(`{"id":"`+id+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("promote = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["owner_down"] == true {
		t.Fatalf("promote reported owner_down with an owner running: %s", rec.Body.String())
	}
	noteID, _ := out["id"].(string)
	if noteID == "" {
		t.Fatalf("promote returned no note id: %s", rec.Body.String())
	}

	// The queue item is gone, which is the op the viewer could not apply itself.
	if _, err := s.store.GetPending(id); err == nil {
		t.Fatal("the promoted candidate is still in the review queue: the owner never applied the delete")
	}
	// And the note the owner indexed is queryable through the viewer's own store.
	if _, err := s.store.NotePath(noteID); err != nil {
		t.Fatalf("the promoted note is not queryable: %v", err)
	}
	// The flywheel stamp landed too, which is the other queued op.
	stats, err := s.store.FlywheelStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Authored < 1 {
		t.Fatalf("the promote was not counted as a write-back (authored=%d)", stats.Authored)
	}
}

// TestNewOwningServerKeepsAWritableStore is the other half of the pin. The mesh-ui
// container is the only process that touches its vault's index, so if this one lost its
// writable store the deployed app would serve an index nothing ever updates.
func TestNewOwningServerKeepsAWritableStore(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "n.md", "---\nid: n\ntype: note\nwhen: 2026-01-01\n---\n# N\nbody\n")

	s, err := NewOwningServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.store.ReadOnly() {
		t.Fatal("web.NewOwningServer lost its writable store; it owns its index and must keep indexing it")
	}
	// It must also not need an owner to have run first: on a fresh volume it IS the one
	// that creates the index.
	if n, _ := s.store.Count("notes"); n == 0 {
		t.Fatal("the owning viewer did not index the vault at startup")
	}
}

func TestNewOwningServerPublishesAndReleasesItsOwnership(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "n.md", "---\nid: n\ntype: note\nwhen: 2026-01-01\n---\n# N\nbody\n")
	s, err := NewOwningServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, live := index.OwnerStatus(filepath.Join(dir, ".mesh"))
	if !live || info.Role != "mesh ui --own-index" {
		t.Fatalf("owning viewer did not publish its claim: info=%+v live=%v", info, live)
	}
	if _, err := index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "second viewer", false); !errors.Is(err, index.ErrOwnerHeld) {
		t.Fatalf("second declared writer was admitted beside owning viewer: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, live := index.OwnerStatus(filepath.Join(dir, ".mesh")); live {
		t.Fatal("owning viewer left its claim behind after Close")
	}
}

func TestNewOwningServerDrainsPendingOpsQueuedAfterStartup(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "n.md", "---\nid: n\ntype: note\nwhen: 2026-01-01\n---\n# N\nbody\n")
	s, err := NewOwningServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reader, err := index.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	pending := index.PendingNote{Type: "gotcha", Title: "UI owner drains extraction", Do: "poll owner ops after startup"}
	name, err := reader.EnqueueOp(index.Op{Kind: index.OpAddPending, Pending: &pending})
	if err != nil {
		t.Fatal(err)
	}
	s.ownerOpWake <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := reader.AwaitOpApplied(ctx, name, 2*time.Second); err != nil {
		t.Fatalf("mesh ui --own-index left a post-start extraction op queued: %v", err)
	}
	got, err := reader.GetPending(index.PendingID(pending.Type, pending.Title))
	if err != nil || got.Do != pending.Do {
		t.Fatalf("mesh ui --own-index did not persist the pending item: got=%+v err=%v", got, err)
	}
}
