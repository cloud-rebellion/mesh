// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/vault"
)

func TestLocalOnlyRetrievalIgnoresProviderDiscoveryAndRetainsAccessFilters(t *testing.T) {
	clearBYOAIEnv(t)
	t.Setenv("HOME", "")
	t.Setenv("MESH_EMBED_KEY", "")
	t.Setenv("MESH_RERANK_KEY", "")
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer provider.Close()
	root := t.TempDir()
	note, err := vault.CreateNote(root, vault.NewNoteSpec{Title: "Offline OAuth method", Template: "finding", Summary: "Synthetic local retrieval fixture.", Sections: map[string]string{"question": "Local search?", "findings": "Synthetic method only.", "evidence": "Private fixture.", "limitations": "No live proof.", "next_steps": "Review."}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.CreateNote(root, vault.NewNoteSpec{Title: "Second offline OAuth method", Template: "finding", Summary: "Another synthetic candidate for the rerank control.", Sections: map[string]string{"question": "Local search?", "findings": "Synthetic second method only.", "evidence": "Private fixture.", "limitations": "No live proof.", "next_steps": "Review."}, Related: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	store, err := index.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, _, err := index.ReindexFull(store, root)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := store.NoteRetrievalHash("note:" + note.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceVectors("local-spy", []index.VectorRow{{NodeID: "note:" + note.ID, NoteHash: hash, Vec: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	config := []byte(fmt.Sprintf("[embedding]\nendpoint = %q\nmodel = 'local-spy'\ndim = 2\n[rerank]\nendpoint = %q\nmodel = 'local-spy'\n", provider.URL, provider.URL))
	cfgPath := filepath.Join(store.MeshDir(), "config.toml")
	if os.WriteFile(cfgPath, config, 0600) != nil {
		t.Fatal("provider fixture config")
	}
	t.Setenv("MESH_EMBED_ENDPOINT", provider.URL)
	t.Setenv("MESH_EMBED_MODEL", "local-spy")
	t.Setenv("MESH_RERANK_ENDPOINT", provider.URL)
	t.Setenv("MESH_RERANK_MODEL", "local-spy")
	// A missing HOME would make automatic subscription discovery fail loudly. This
	// constructor must not discover either the profile or these configured models.
	local, err := NewFromEnvContextWithOptions(t.Context(), store, g, ConstructionOptions{LocalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	cards, err := local.Retrieve(t.Context(), "OAuth", Options{Limit: 5})
	if err != nil || len(cards) == 0 || local.VectorsActive() || local.RerankActive() || calls.Load() != 0 {
		t.Fatalf("local inference boundary failed: %v cards=%d calls=%d", err, len(cards), calls.Load())
	}
	denied, err := local.Retrieve(t.Context(), "OAuth", Options{AllowedScopes: map[string]bool{"unrelated": true}})
	if err != nil || len(denied) != 0 {
		t.Fatal("local option widened scope access")
	}
	denied, err = local.Retrieve(t.Context(), "OAuth", Options{AllowPath: func(string) bool { return false }})
	if err != nil || len(denied) != 0 {
		t.Fatal("local option widened folder access")
	}
	before, _ := os.ReadFile(cfgPath)
	if string(before) != string(config) {
		t.Fatal("local retrieval rewrote provider configuration")
	}
	// The regular constructor still enables explicitly configured inference and
	// preserves HTTP fail-loud behavior; this is not a shared fallback change.
	t.Setenv("MESH_RERANK_AGENT", "http")
	normal, err := NewFromEnvContext(t.Context(), store, g)
	if err != nil {
		t.Fatal(err)
	}
	if !normal.VectorsActive() || !normal.RerankActive() {
		t.Fatal("normal configured provider semantics changed")
	}
	if _, err := normal.Retrieve(t.Context(), "OAuth", Options{}); !errors.Is(err, ErrRerankUnavailable) || calls.Load() == 0 {
		t.Fatalf("normal HTTP failure was hidden: %v calls=%d", err, calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewFromEnvContextWithOptions(ctx, store, g, ConstructionOptions{LocalOnly: true}); !errors.Is(err, context.Canceled) {
		t.Fatal("local constructor ignored cancellation")
	}
}
