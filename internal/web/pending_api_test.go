// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/vault"
)

func TestPendingPromoteFinishesIndexingAfterClientCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.md"), []byte("---\nid: seed\ntype: note\nwhen: 2026-01-01\n---\n# Seed\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	s, err := NewOwningServerContext(lifetime, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := completePending("Finish after disconnect")
	if err := s.store.AddPending(p); err != nil {
		t.Fatal(err)
	}
	id := index.PendingID(p.Type, p.Title)

	requestCtx, disconnect := context.WithCancel(context.Background())
	disconnect()
	req := httptest.NewRequest(http.MethodPost, "/api/pending/promote",
		strings.NewReader(`{"id":"`+id+`"}`)).WithContext(requestCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("promote after client cancellation = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("promote response: id=%q err=%v body=%s", out.ID, err, rec.Body.String())
	}
	if _, err := s.store.NotePath(out.ID); err != nil {
		t.Fatalf("durable note was not indexed after disconnect: %v", err)
	}
	s.mu.RLock()
	_, inLiveGraph := s.graph.Node("note:" + out.ID)
	s.mu.RUnlock()
	if !inLiveGraph {
		t.Fatal("durable note reached SQLite but not the owning UI's live graph")
	}
	if _, err := s.store.GetPending(id); err == nil {
		t.Fatal("promoted candidate remained in the queue after disconnect")
	}
}

func TestPendingPromoteQueuesCleanupWhenShutdownFollowsFileCreation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.md"), []byte("---\nid: seed\ntype: note\nwhen: 2026-01-01\n---\n# Seed\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lifetime, stop := context.WithCancel(context.Background())
	s, err := NewOwningServerContext(lifetime, dir)
	if err != nil {
		t.Fatal(err)
	}
	p := completePending("Clean up after shutdown")
	if err := s.store.AddPending(p); err != nil {
		t.Fatal(err)
	}
	pendingID := index.PendingID(p.Type, p.Title)
	// Cancel at the exact durable boundary: the note file exists, but none of its
	// SQLite bookkeeping or the owning reindex has begun. The replacement must drain
	// the queued compensation and index that published file.
	s.afterPendingFilePublished = stop
	req := httptest.NewRequest(http.MethodPost, "/api/pending/promote",
		strings.NewReader(`{"id":"`+pendingID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("promote during shutdown = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("promote response: id=%q err=%v body=%s", out.ID, err, rec.Body.String())
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close old owner: %v", err)
	}

	replacement, err := NewOwningServer(dir)
	if err != nil {
		t.Fatalf("start replacement owner: %v", err)
	}
	defer replacement.Close()
	if _, err := replacement.store.GetPending(pendingID); err == nil {
		t.Fatal("replacement left the promoted candidate queued for duplicate promotion")
	}
	if _, err := replacement.store.NotePath(out.ID); err != nil {
		t.Fatalf("replacement did not index the note published during shutdown: %v", err)
	}
}

func postJSON(t *testing.T, ts *httptest.Server, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, 1<<16)
	n, _ := resp.Body.Read(b)
	return resp.StatusCode, string(b[:n])
}

// The review queue round-trip: a pending candidate lists, promotes into a real note
// (and leaves the queue), and another discards (leaves the queue, no note).
func TestPendingPromoteAndDiscard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.md"), []byte("---\nid: seed\ntype: note\nwhen: 2026-01-01\n---\n# Seed\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	// The queue is filled by the extractor (a writer) and resolved through the owning
	// writer, so this exercises the production shape: a read-only viewer in front of a
	// live owner.
	seedPending(t, dir,
		completePending("Keep me"),
		index.PendingNote{Type: "decision", Title: "Toss me"},
	)
	runOwner(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	st, body, _ := get(t, ts, "/api/pending")
	if st != 200 || !strings.Contains(body, "Keep me") || !strings.Contains(body, "Toss me") {
		t.Fatalf("list = %d %s", st, body)
	}

	keepID := index.PendingID("gotcha", "Keep me")
	tossID := index.PendingID("decision", "Toss me")

	if st, body := postJSON(t, ts, "/api/pending/promote", `{"id":"`+keepID+`"}`); st != 200 || !strings.Contains(body, "promoted") {
		t.Fatalf("promote = %d %s", st, body)
	}
	if _, err := s.store.GetPending(keepID); err == nil {
		t.Fatal("promoted note is still in the pending queue")
	}
	// The promoted candidate is now a real gotcha note in the vault.
	if _, err := os.Stat(filepath.Join(dir, "gotchas")); err != nil {
		t.Fatalf("promoted note dir not created: %v", err)
	}
	// Promoting must stamp the note in the flywheel (authored count), like a direct
	// mesh_append_note. Startup backfill saw only the non-agent seed note (authored 0),
	// so an authored count here proves the promote-time RecordWriteback fired.
	if fw, err := s.store.FlywheelStats(); err != nil || fw.Authored < 1 {
		t.Errorf("promoted candidate should count as an authored write-back, authored=%d err=%v", fw.Authored, err)
	}

	if st, _ := postJSON(t, ts, "/api/pending/discard", `{"id":"`+tossID+`"}`); st != 200 {
		t.Fatalf("discard = %d", st)
	}
	st, body, _ = get(t, ts, "/api/pending")
	if strings.Contains(body, "Keep me") || strings.Contains(body, "Toss me") {
		t.Fatalf("queue should be empty: %s", body)
	}

	// Unknown id is a clean 404, not a 500.
	if st, _ := postJSON(t, ts, "/api/pending/promote", `{"id":"nope"}`); st != 404 {
		t.Fatalf("promote unknown = %d, want 404", st)
	}
}

func completePending(title string) index.PendingNote {
	template, _ := vault.TemplateFor("troubleshooting", 1)
	sections := map[string]string{}
	for _, s := range template.Sections {
		sections[s.Key] = "Fixture observation and bounded verification, explicitly synthetic."
	}
	return index.PendingNote{Type: "gotcha", Title: title, Template: "troubleshooting", TemplateVersion: 1, Summary: "The fixture verifies pending publication bookkeeping.", Sections: sections}
}

func TestPendingPromotionRejectsIncompleteAndHistoricalDrafts(t *testing.T) {
	dir := t.TempDir()
	s, err := NewOwningServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, p := range []index.PendingNote{
		{Type: "note", Title: "Incomplete modern draft", Template: "finding", TemplateVersion: 1, Summary: "A bounded observation."},
		{Type: "gotcha", Title: "Historical shorthand", Do: "original words", Why: "original evidence"},
	} {
		if err := s.store.AddPending(p); err != nil {
			t.Fatal(err)
		}
		id := index.PendingID(p.Type, p.Title)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/pending/promote", strings.NewReader(`{"id":"`+id+`"}`)))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("incomplete promote=%d %s", rec.Code, rec.Body.String())
		}
		if _, err := s.store.GetPending(id); err != nil {
			t.Fatalf("draft lost after rejection: %v", err)
		}
		files, _ := vault.Walk(dir)
		if len(files) != 0 {
			t.Fatalf("incomplete draft published: %v", files)
		}
	}
}

func TestPendingPromotionRechecksReferenceAudienceAndCurrentScope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.md")
	if err := os.WriteFile(path, []byte("---\nid: target\ntype: entity\nwhen: 2026-01-01\nscope: [dev]\n---\n# Target\nVisible baseline.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewOwningServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := completePending("Scope drift must block publication")
	p.Related = []string{"target"}
	if err := s.store.AddPending(p); err != nil {
		t.Fatal(err)
	}
	// Change only the file, leaving a readable dev target in the stale index.
	if err := os.WriteFile(path, []byte("---\nid: target\ntype: entity\nwhen: 2026-01-01\nscope: [ops]\n---\n# Target\nPrivate operational content.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := index.PendingID(p.Type, p.Title)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/pending/promote", strings.NewReader(`{"id":"`+id+`"}`)))
	if rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), "target") {
		t.Fatalf("scope drift=%d %s", rec.Code, rec.Body.String())
	}
	if _, err := s.store.GetPending(id); err != nil {
		t.Fatalf("rejected queue item lost: %v", err)
	}
}

func TestPendingPromotionCannotBroadenFolderAudienceForAdmin(t *testing.T) {
	dir := t.TempDir()
	s, err := NewOwningServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := completePending("Folder audience must remain bounded")
	if err := s.store.AddPending(p); err != nil {
		t.Fatal(err)
	}
	s.SetMemberAuth(
		func(token string) (int64, string, bool) { return 1, "admin", token == "admin" },
		func(int64) map[string]bool { return map[string]bool{"dev": true} },
		func(id int64) func(string) bool {
			if id == 1 {
				return func(string) bool { return true }
			}
			return func(path string) bool { return !strings.HasPrefix(path, "private/") }
		},
		func(int64) (string, int64, bool) { return "admin", 1, true },
	)
	id := index.PendingID(p.Type, p.Title)
	request := httptest.NewRequest(http.MethodPost, "/api/pending/promote", strings.NewReader(`{"id":"`+id+`"}`))
	request.Header.Set("Authorization", "Bearer admin")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unproven admin publication=%d %s", response.Code, response.Body.String())
	}
	if _, err := s.store.GetPending(id); err != nil {
		t.Fatalf("guard lost pending work: %v", err)
	}
	files, _ := vault.Walk(dir)
	if len(files) != 0 {
		t.Fatal("folder guard published a note")
	}
}

func TestPendingPromotionChecksWikiReferencesInAllAuthoredProse(t *testing.T) {
	for _, where := range []string{"summary", "section", "block"} {
		t.Run(where, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "private-target.md"), []byte("---\nid: private-target\ntype: entity\nwhen: 2026-01-01\nscope: [ops]\n---\n# Private target\nPrivate operational material.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			s, err := NewOwningServer(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			p := completePending("Wiki reference audience " + where)
			switch where {
			case "summary":
				p.Summary = "A reference to [[private-target]] is private."
			case "section":
				p.Sections["cause"] = "The context refers to [[private-target]]."
			case "block":
				template, _ := vault.BlockTemplateFor("evidence", 1)
				fields := map[string]string{}
				for _, field := range template.Fields {
					fields[field.Key] = "Synthetic fixture context, explicitly unverified."
				}
				fields["claim"] = "This claim references [[private-target]]."
				p.Blocks = []vault.BlockSpec{{Template: "evidence", Version: 1, ID: "scope-evidence", Fields: fields}}
			}
			spec, err := p.AuthoringSpec()
			if err != nil {
				t.Fatal(err)
			}
			if err := vault.ValidateSpec(spec); err != nil {
				t.Fatalf("fixture incomplete before reference check: %v", err)
			}
			if err := s.store.AddPending(p); err != nil {
				t.Fatal(err)
			}
			id := index.PendingID(p.Type, p.Title)
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/pending/promote", strings.NewReader(`{"id":"`+id+`"}`)))
			if response.Code != http.StatusUnprocessableEntity || strings.Contains(response.Body.String(), "private-target") {
				t.Fatalf("wiki audience=%d %s", response.Code, response.Body.String())
			}
			if _, err := s.store.GetPending(id); err != nil {
				t.Fatalf("rejected item lost: %v", err)
			}
		})
	}
}

// A hub installs its path provider for all teams; the provider returns nil when
// no folder ACL rules exist. Provider presence alone must not disable publication.
func TestPendingPromotionWithMemberProviderAndNoFolderRules(t *testing.T) {
	dir := t.TempDir()
	s, err := NewOwningServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := completePending("Team without folder rules can publish")
	if err := s.store.AddPending(p); err != nil {
		t.Fatal(err)
	}
	s.SetMemberAuth(func(token string) (int64, string, bool) { return 1, "admin", token == "admin" }, func(int64) map[string]bool { return map[string]bool{"dev": true} }, func(int64) func(string) bool { return nil }, func(int64) (string, int64, bool) { return "admin", 1, true })
	id := index.PendingID(p.Type, p.Title)
	req := httptest.NewRequest(http.MethodPost, "/api/pending/promote", strings.NewReader(`{"id":"`+id+`"}`))
	req.Header.Set("Authorization", "Bearer admin")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("publication rejected without folder rules: %d %s", w.Code, w.Body.String())
	}
	if _, err := s.store.GetPending(id); err == nil {
		t.Fatal("published candidate retained in pending queue")
	}
	files, _ := vault.Walk(dir)
	if len(files) != 1 {
		t.Fatalf("publication created %d files", len(files))
	}
}

func TestPendingPromotionDoesNotInferBreakGlassAudienceFromNilFilter(t *testing.T) {
	dir := t.TempDir()
	s, err := NewOwningServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := completePending("Break-glass cannot bypass folder audience checks")
	if err := s.store.AddPending(p); err != nil {
		t.Fatal(err)
	}
	s.SetMemberAuth(func(token string) (int64, string, bool) { return -1, "admin", token == "shared" }, func(int64) map[string]bool { return nil }, func(int64) func(string) bool { return nil }, func(int64) (string, int64, bool) { return "admin", 0, true })
	id := index.PendingID(p.Type, p.Title)
	req := httptest.NewRequest(http.MethodPost, "/api/pending/promote", strings.NewReader(`{"id":"`+id+`"}`))
	req.Header.Set("Authorization", "Bearer shared")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("break-glass published without audience proof: %d %s", w.Code, w.Body.String())
	}
	if _, err := s.store.GetPending(id); err != nil {
		t.Fatal("guard lost pending work")
	}
}
