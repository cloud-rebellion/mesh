// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bright-interaction/mesh/internal/rerank"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
)

type unavailableAuthoringReranker struct{ calls int }

func (r *unavailableAuthoringReranker) Model() string { return "fixture-offline" }
func (r *unavailableAuthoringReranker) Rerank(context.Context, string, []string) ([]rerank.Result, error) {
	r.calls++
	return nil, errors.New("fixture provider is unavailable")
}

func TestAuthoringDerivesCorroboratedLinksDuringRerankerOutage(t *testing.T) {
	root := t.TempDir()
	seedVaultFiles(t, root)
	for id, scope := range map[string]string{"public-recovery": "dev", "private-recovery": "sales", "unrelated-recovery": "dev"} {
		tags := "[mesh]"
		if id == "unrelated-recovery" {
			tags = "[other]"
		}
		body := "---\nid: " + id + "\ntype: note\nscope: [" + scope + "]\ntags: " + tags + "\n---\n# Mesh authoring recovery\nMesh authoring recovery engine evidence.\n"
		if err := os.WriteFile(filepath.Join(root, id+".md"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	seedIndex(t, root)
	s, err := NewServerWithRetrievalOptions(context.Background(), root, retrieve.ConstructionOptions{LocalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.WaitReady(); err != nil {
		t.Fatal(err)
	}
	_, rt := s.snapshot()
	offline := &unavailableAuthoringReranker{}
	if !rt.EnableRerank(offline) {
		t.Fatal("fixture reranker not configured")
	}
	ctx := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"dev": true}})
	spec := vault.NewNoteSpec{Title: "Mesh authoring recovery", Tags: []string{"mesh"}, Scope: []string{"dev"}}
	s.deriveAuthoringRelated(ctx, &spec)
	if offline.calls == 0 {
		t.Fatal("test did not exercise the configured provider failure")
	}
	if !reflect.DeepEqual(spec.Related, []string{"public-recovery"}) {
		t.Fatalf("outage fallback lost evidence/audience filtering: %v", spec.Related)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	stopped := vault.NewNoteSpec{Title: "Mesh authoring recovery", Tags: []string{"mesh"}, Scope: []string{"dev"}}
	s.deriveAuthoringRelated(cancelled, &stopped)
	if len(stopped.Related) != 0 {
		t.Fatalf("cancelled publication derived links: %v", stopped.Related)
	}
}
