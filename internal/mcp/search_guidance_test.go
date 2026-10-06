// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

func TestSearchMissingGuidanceWireAndBudget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "guidance.md"), []byte("---\nid: guidance\ntype: decision\ntitle: Guidanceneedle\nwhen: 2026-09-23\ndo: TODO\ndont: Avoid guesses\n---\n# Guidanceneedle\nHistorical context only.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.WaitReady(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, budget := range []int{32, 100, 150, 250, 850} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			out := toolText(t, call(t, s, "tools/call", map[string]any{"name": "mesh_search", "arguments": map[string]any{"query": "guidanceneedle", "budget": budget}}))
			cards := out["cards"].([]any)
			b, err := json.Marshal(cards)
			if err != nil {
				t.Fatal(err)
			}
			actual := retrieve.EstimateTokens(string(b))
			reported := int(out["tokens"].(float64))
			if actual > budget || reported > budget || actual > reported {
				t.Fatalf("wire tokens %d, reported %d, budget %d: %s", actual, reported, budget, b)
			}
			for _, value := range cards {
				found = true
				card := value.(map[string]any)
				if !reflect.DeepEqual(card["MissingGuidance"], []any{"legacy do contains an unfilled placeholder"}) {
					t.Fatalf("agent received unqualified incomplete guidance: %s", b)
				}
				if _, exists := card["NodeID"]; exists {
					t.Fatal("warning reintroduced duplicate NodeID")
				}
			}
		})
	}
	if !found {
		t.Fatal("no budget returned a card")
	}
}
