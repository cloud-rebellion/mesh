// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
)

func TestLocalOnlyViewerConstructorKeepsOfflineSearchAcrossOwnerAndReader(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("MESH_RERANK_KEY", "")
	t.Setenv("MESH_NO_UPDATE_CHECK", "1")
	t.Setenv("MESH_RERANK_AGENT", "")
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer provider.Close()
	root := t.TempDir()
	note, err := vault.CreateNote(root, vault.NewNoteSpec{Title: "Local viewer OAuth", Template: "finding", Summary: "Synthetic local viewer fixture.", Sections: map[string]string{"question": "Offline?", "findings": "Fixture only.", "evidence": "Synthetic.", "limitations": "No production proof.", "next_steps": "Review."}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.CreateNote(root, vault.NewNoteSpec{Title: "Another local viewer OAuth", Template: "finding", Summary: "A second synthetic provider-control candidate.", Sections: map[string]string{"question": "Offline?", "findings": "Fixture only.", "evidence": "Synthetic.", "limitations": "No production proof.", "next_steps": "Review."}, Related: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	if os.MkdirAll(filepath.Join(root, ".mesh"), 0700) != nil {
		t.Fatal("fixture private directory")
	}
	cfg := []byte(fmt.Sprintf("[rerank]\nendpoint = %q\nmodel = 'configured-spy'\n", provider.URL))
	if os.WriteFile(filepath.Join(root, ".mesh", "config.toml"), cfg, 0600) != nil {
		t.Fatal("config fixture")
	}
	owner, err := NewOwningServerWithRetrievalOptions(t.Context(), root, retrieve.ConstructionOptions{LocalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	reader, err := NewServerWithRetrievalOptions(t.Context(), root, retrieve.ConstructionOptions{LocalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, s := range []*Server{owner, reader} {
		code, data := doJSON(t, s.Handler(), "GET", "/api/search?q=OAuth&limit=5", "")
		if code != 200 || !strings.Contains(fmt.Sprint(data), note.ID) {
			t.Fatalf("local viewer could not search: %d %v", code, data)
		}
	}
	// A revision-bracketed initial reader load can use the retrieval factory
	// before its graph is published. The native choice must already be installed,
	// rather than assigned by a wrapper after construction and initial refresh.
	startupReads := 0
	startup, err := newReadOnlyServerContextWithOptions(t.Context(), root, func(ctx context.Context, s *Server) (*graph.Graph, error) {
		startupReads++
		g, err := s.store.LoadGraphContext(ctx)
		if err != nil {
			return nil, err
		}
		rt, err := s.buildRetriever(ctx, s.store, g)
		if err != nil {
			return nil, err
		}
		cards, err := rt.Retrieve(ctx, "OAuth", retrieve.Options{Limit: 5})
		if err != nil || len(cards) == 0 || rt.RerankActive() || rt.VectorsActive() {
			return nil, fmt.Errorf("initial native reader retrieval failed: %v", err)
		}
		return g, nil
	}, retrieve.ConstructionOptions{LocalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer startup.Close()
	if startupReads == 0 || calls.Load() != 0 {
		t.Fatal("initial native reader discovered a configured provider")
	}
	// Exercise both live constructors while the actual owner watcher replaces
	// graph/retrieval caches. No mutable option installation races that worker or
	// converts an existing local-only reader back to configured inference.
	added := approvedPeerNote(t, root, "New offline peer", "localneworbitsignal", nil)
	var readers sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for _, s := range []*Server{owner, reader} {
				for n := 0; n < 4; n++ {
					w := httptest.NewRecorder()
					s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/search?q=OAuth&limit=5", nil))
					if w.Code != http.StatusOK {
						errs <- fmt.Errorf("local startup/reconcile read failed: %d", w.Code)
						return
					}
				}
			}
		}()
	}
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, s := range []*Server{owner, reader} {
		waitOwnerResponse(t, s.Handler(), "/api/search?q=localneworbitsignal", func(w *httptest.ResponseRecorder) bool { return searchHasID(w, added.ID) })
	}
	if calls.Load() != 0 {
		t.Fatal("native viewer contacted configured provider")
	}
	t.Setenv("MESH_RERANK_AGENT", "http")
	t.Setenv("MESH_RERANK_ENDPOINT", provider.URL)
	t.Setenv("MESH_RERANK_MODEL", "configured-spy")
	normal, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer normal.Close()
	code, _ := doJSON(t, normal.Handler(), "GET", "/api/search?q=OAuth", "")
	if code == 200 || calls.Load() == 0 {
		t.Fatal("normal viewer provider semantics changed")
	}
}
