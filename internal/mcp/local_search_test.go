// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

func TestLocalOnlyReaderPreservesChoiceAfterPublicationEditAndReuse(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("MESH_RERANK_AGENT", "")
	root := t.TempDir()
	seedIndex(t, root)
	startOwner(t, root)
	cfg := filepath.Join(root, ".mesh", "config.toml")
	if os.WriteFile(cfg, []byte("[rerank]\nendpoint='http://127.0.0.1:1'\nmodel='unavailable'\n"), 0600) != nil {
		t.Fatal("config fixture")
	}
	s, err := NewServerWithRetrievalOptions(t.Context(), root, retrieve.ConstructionOptions{LocalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.WaitReady(); err != nil {
		t.Fatal(err)
	}
	first := publishUpdateFixture(t, s, "Offline MCP initial finding")
	id := first["id"].(string)
	for _, summary := range []string{"First offline edit.", "Second offline edit with the same constructor."} {
		prepared, rerr := s.toolPrepareUpdate(t.Context(), mustJSON(map[string]any{"id": id}))
		if rerr != nil {
			t.Fatal(rerr)
		}
		note := toolJSON(t, prepared)["note"].(map[string]any)
		note["summary"] = summary
		if _, rerr := s.toolAuthorNote(WithLocalOperator(t.Context()), mustJSON(map[string]any{"action": "publish", "note": note})); rerr != nil {
			t.Fatal(rerr)
		}
		found, rerr := s.toolSearch(t.Context(), mustJSON(map[string]any{"query": "Offline MCP initial finding", "limit": 5, "budget": 2000}))
		if rerr != nil || !strings.Contains(rawContentText(t, found), id) {
			t.Fatal("reader refresh re-enabled providers", rerr)
		}
		_, r := s.snapshot()
		if r.RerankActive() || r.VectorsActive() {
			t.Fatal("reader acknowledged edit with inference enabled")
		}
	}
	// A normal constructor still surfaces the missing subscription profile instead
	// of silently taking this native-only local route.
	normal, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer normal.Close()
	if err := normal.WaitReady(); err != nil {
		t.Fatal(err)
	}
	if _, rerr := normal.toolSearch(t.Context(), mustJSON(map[string]any{"query": "Offline MCP initial finding"})); rerr == nil {
		t.Fatal("normal configured semantics silently changed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s, err := NewServerWithRetrievalOptions(ctx, root, retrieve.ConstructionOptions{LocalOnly: true}); err == nil {
		s.Close()
		t.Fatal("canceled native reader opened")
	}
}
