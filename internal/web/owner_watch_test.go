// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/bright-interaction/mesh/internal/watch"
)

func approvedPeerNote(t *testing.T, root, title, needle string, scopes []string) *vault.CreateResult {
	t.Helper()
	template, err := vault.TemplateFor("review", 1)
	if err != nil {
		t.Fatal(err)
	}
	sections := map[string]string{}
	for _, section := range template.Sections {
		sections[section.Key] = "This fixture assesses an external publication; real production results remain unverified."
	}
	note, err := vault.CreateNoteContext(context.Background(), root, vault.NewNoteSpec{
		Type: vault.TypeNote, Template: "review", TemplateVersion: 1, Title: title,
		Summary: needle + " describes the externally published fixture.", Sections: sections, Scope: scopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return note
}

func waitOwnerResponse(t *testing.T, h http.Handler, route string, accept func(*httptest.ResponseRecorder) bool) *httptest.ResponseRecorder {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	var last *httptest.ResponseRecorder
	for time.Now().Before(deadline) {
		last = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, route, nil)
		req.Header.Set("Authorization", "Bearer fixture-member")
		h.ServeHTTP(last, req)
		if accept(last) {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("automatic owner convergence failed for %s: %d %s", route, last.Code, last.Body.String())
	return nil
}

func searchHasID(w *httptest.ResponseRecorder, id string) bool {
	var result struct {
		Cards []struct {
			ID string `json:"NodeID"`
		} `json:"cards"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		return false
	}
	for _, card := range result.Cards {
		if card.ID == "note:"+id {
			return true
		}
	}
	return false
}

// A peer/hub publishes Markdown while the sole owning viewer stays alive. No
// test owner, reindex route or restart helps it: actual search/fetch/graph routes
// must converge, and scoped users must still be unable to see hidden notes.
func TestOwningViewerAutomaticallyObservesApprovedPeerLifecycle(t *testing.T) {
	t.Setenv("MESH_AUTHORING_MODE", "enabled")
	dir := t.TempDir()
	seed := approvedPeerNote(t, dir, "Initial peer review", "oldzeppelinsignal", []string{"engineering"})
	s, err := NewOwningServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	s.updateCheck = nil
	s.buildRetriever = func(ctx context.Context, st *index.Store, g *graph.Graph) (*retrieve.Retriever, error) {
		return retrieve.NewContext(ctx, st, g) // never call a configured model/provider
	}
	s.SetMemberAuth(func(token string) (int64, string, bool) { return 7, "fixture", token == "fixture-member" },
		func(int64) map[string]bool { return map[string]bool{"engineering": true} },
		func(int64) func(string) bool {
			return func(path string) bool { return !strings.HasPrefix(path, "private/") }
		},
		func(int64) (string, int64, bool) { return "member", 123, true })
	h := s.Handler()
	waitOwnerResponse(t, h, "/api/search?q=oldzeppelinsignal", func(w *httptest.ResponseRecorder) bool { return searchHasID(w, seed.ID) })
	oldRetriever := s.cachedRetriever.Load()
	if oldRetriever == nil {
		t.Fatal("fixture did not warm the existing search cache")
	}
	if _, err := index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "another viewer", false); !errors.Is(err, index.ErrOwnerHeld) {
		t.Fatalf("automatic watch admitted another owner: %v", err)
	}

	added := approvedPeerNote(t, dir, "New peer review", "freshorbitsignal", []string{"engineering"})
	hiddenScope := approvedPeerNote(t, dir, "Hidden scope review", "freshorbitsignal", []string{"marketing"})
	hiddenPath := approvedPeerNote(t, dir, "Hidden folder review", "freshorbitsignal", []string{"engineering"})
	if err := os.MkdirAll(filepath.Join(dir, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(hiddenPath.Path, filepath.Join(dir, "private", filepath.Base(hiddenPath.Path))); err != nil {
		t.Fatal(err)
	}
	waitOwnerResponse(t, h, "/api/search?q=freshorbitsignal", func(w *httptest.ResponseRecorder) bool { return searchHasID(w, added.ID) })
	if s.cachedRetriever.Load() == oldRetriever {
		t.Fatal("peer publication reused the stale retriever")
	}
	waitOwnerResponse(t, h, "/api/note/"+added.ID, func(w *httptest.ResponseRecorder) bool {
		return w.Code == 200 && strings.Contains(w.Body.String(), "freshorbitsignal")
	})
	waitOwnerResponse(t, h, "/graph.json", func(w *httptest.ResponseRecorder) bool {
		return w.Code == 200 && strings.Contains(w.Body.String(), added.ID)
	})
	for _, id := range []string{hiddenScope.ID, hiddenPath.ID} {
		w := waitOwnerResponse(t, h, "/api/note/"+id, func(w *httptest.ResponseRecorder) bool { return w.Code == http.StatusNotFound })
		if strings.Contains(w.Body.String(), "freshorbitsignal") {
			t.Fatal("hidden note content escaped")
		}
		w = waitOwnerResponse(t, h, "/api/search?q=freshorbitsignal", func(w *httptest.ResponseRecorder) bool { return w.Code == 200 })
		if searchHasID(w, id) {
			t.Fatalf("automatic reconcile widened visibility for %s", id)
		}
	}

	snapshot, err := vault.NoteSnapshotContext(context.Background(), dir, added.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Spec.Summary = "updatedorbitsignal replaces the earlier finding without making a verification claim."
	if _, err := vault.CreateNoteContext(context.Background(), dir, snapshot.Spec); err != nil {
		t.Fatal(err)
	}
	waitOwnerResponse(t, h, "/api/search?q=updatedorbitsignal", func(w *httptest.ResponseRecorder) bool { return searchHasID(w, added.ID) })
	waitOwnerResponse(t, h, "/api/note/"+added.ID, func(w *httptest.ResponseRecorder) bool {
		return w.Code == 200 && strings.Contains(w.Body.String(), "updatedorbitsignal") && !strings.Contains(w.Body.String(), "freshorbitsignal")
	})
	if err := os.Remove(added.Path); err != nil {
		t.Fatal(err)
	}
	waitOwnerResponse(t, h, "/api/note/"+added.ID, func(w *httptest.ResponseRecorder) bool { return w.Code == http.StatusNotFound })
	waitOwnerResponse(t, h, "/api/search?q=updatedorbitsignal", func(w *httptest.ResponseRecorder) bool { return w.Code == 200 && !searchHasID(w, added.ID) })
	waitOwnerResponse(t, h, "/graph.json", func(w *httptest.ResponseRecorder) bool {
		return w.Code == 200 && !strings.Contains(w.Body.String(), added.ID)
	})
	if s.store.ReadOnly() || s.owner == nil {
		t.Fatal("watch lost the one retained owner")
	}
}

func TestOwningViewerCloseJoinsAutomaticWatchBeforeReplacement(t *testing.T) {
	dir := t.TempDir()
	s, err := NewOwningServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeNote(t, dir, "last.md", "---\nid: last\ntype: note\nwhen: 2026-01-01\n---\n# Last\nlast peer write\n")
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("automatic owner watch delayed shutdown")
	}
	replacement, err := NewOwningServer(dir)
	if err != nil {
		t.Fatalf("clean close did not release ownership: %v", err)
	}
	defer replacement.Close()
	if path, err := replacement.store.NotePath("last"); err != nil || path != "last.md" {
		t.Fatalf("replacement did not read preserved final file: %s %v", path, err)
	}
}

func TestOwningViewerPeriodicRecoveryWithoutFilesystemEvents(t *testing.T) {
	t.Setenv("MESH_AUTHORING_MODE", "enabled")
	dir := t.TempDir()
	s, err := NewOwningServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	s.updateCheck = nil
	s.buildRetriever = func(ctx context.Context, st *index.Store, g *graph.Graph) (*retrieve.Retriever, error) {
		return retrieve.NewContext(ctx, st, g)
	}
	// Simulate the actual watch failure boundary. No fsnotify event can help
	// this owner, so all externally published changes depend on safety ticks.
	s.ownerWatchCancel()
	<-s.ownerWatchDone
	s.startOwnerVaultWatchPolicy(context.Background(), ownerVaultWatchPolicy{
		reconcile: 40 * time.Millisecond, full: 80 * time.Millisecond,
		run: func(context.Context, watch.Options) error {
			return errors.New("fixture filesystem event stream unavailable")
		},
	})
	h := s.Handler()
	added := approvedPeerNote(t, dir, "Periodic peer review", "periodiczeppelinsignal", nil)
	waitOwnerResponse(t, h, "/api/search?q=periodiczeppelinsignal", func(w *httptest.ResponseRecorder) bool { return searchHasID(w, added.ID) })
	before, err := os.Stat(added.Path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := vault.NoteSnapshotContext(context.Background(), dir, added.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Spec.Summary = "preservedmtimesignal describes the updated fixture."
	if _, err := vault.CreateNoteContext(context.Background(), dir, snapshot.Spec); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(added.Path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	waitOwnerResponse(t, h, "/api/search?q=preservedmtimesignal", func(w *httptest.ResponseRecorder) bool { return searchHasID(w, added.ID) })
	waitOwnerResponse(t, h, "/api/note/"+added.ID, func(w *httptest.ResponseRecorder) bool {
		return w.Code == 200 && strings.Contains(w.Body.String(), "preservedmtimesignal")
	})
	if err := os.Remove(added.Path); err != nil {
		t.Fatal(err)
	}
	waitOwnerResponse(t, h, "/api/note/"+added.ID, func(w *httptest.ResponseRecorder) bool { return w.Code == 404 })
	waitOwnerResponse(t, h, "/api/search?q=preservedmtimesignal", func(w *httptest.ResponseRecorder) bool { return w.Code == 200 && !searchHasID(w, added.ID) })
}
