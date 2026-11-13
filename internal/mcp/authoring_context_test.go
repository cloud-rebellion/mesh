// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
)

func TestAuthoringContextFindsDeclaredPeersBeyondPrivateSearchResults(t *testing.T) {
	root := t.TempDir()
	seedVaultFiles(t, root)
	for i := range 48 {
		id := fmt.Sprintf("private-%02d", i)
		text := fmt.Sprintf("---\nid: %s\ntype: note\ntitle: Connection engine connection engine\nscope: [sales]\ntags: [context-topic]\n---\n# Connection engine\nConnection engine connection engine.\n", id)
		if err := os.WriteFile(filepath.Join(root, id+".md"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for id, text := range map[string]string{
		"eligible": "---\nid: eligible\ntype: note\ntitle: Persistence behavior\nscope: [dev]\ntags: [context-topic]\n---\nDurable storage semantics.\n",
		"mesh":     "---\nid: mesh\ntype: entity\ntitle: Mesh\nscope: [dev]\n---\nCanonical knowledge product.\n",
	} {
		if err := os.WriteFile(filepath.Join(root, id+".md"), []byte(text), 0600); err != nil {
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
	for _, scoped := range []bool{false, true} {
		ctx := context.Background()
		if scoped {
			ctx = WithScopeFilter(ctx, &ScopeFilter{AllowedRead: map[string]bool{"dev": true}})
		}
		peer := vault.NewNoteSpec{Title: "Connection engine", Tags: []string{"context-topic"}, Scope: []string{"dev"}}
		s.deriveAuthoringRelated(ctx, &peer)
		if !reflect.DeepEqual(peer.Related, []string{"eligible"}) {
			t.Fatalf("scoped=%v: private-ranked results hid declared peer: %v", scoped, peer.Related)
		}
		entity := vault.NewNoteSpec{Title: "Knowledge storage", Tags: []string{"mesh"}, Scope: []string{"dev"}}
		s.deriveAuthoringRelated(ctx, &entity)
		if !reflect.DeepEqual(entity.Related, []string{"mesh"}) {
			t.Fatalf("scoped=%v: explicit topic missed untagged canonical entity: %v", scoped, entity.Related)
		}
		ordinary := vault.NewNoteSpec{Title: "Mesh Hash Stage", Scope: []string{"dev"}}
		s.deriveAuthoringRelated(ctx, &ordinary)
		if len(ordinary.Related) != 0 {
			t.Fatalf("ordinary short words became durable evidence: %v", ordinary.Related)
		}
	}
}

func TestAuthoringContextRejectsCurrentTargetDrift(t *testing.T) {
	root := t.TempDir()
	seedVaultFiles(t, root)
	path := filepath.Join(root, "mesh.md")
	initial := "---\nid: mesh\ntype: entity\ntitle: Mesh\nscope: [dev]\n---\nCanonical entity.\n"
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
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
	ctx := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"dev": true}})
	for _, tc := range []struct{ name, current string }{
		{"scope", "---\nid: mesh\ntype: entity\ntitle: Mesh\nscope: [sales]\n---\nCanonical entity.\n"},
		{"identity", "---\nid: different\ntype: entity\ntitle: Mesh\nscope: [dev]\n---\nCanonical entity.\n"},
		{"type", "---\nid: mesh\ntype: note\ntitle: Mesh\nscope: [dev]\n---\nOrdinary note.\n"},
		{"title", "---\nid: mesh\ntype: entity\ntitle: Other\nscope: [dev]\n---\nChanged canonical declaration.\n"},
		{"draft", "---\nid: mesh\ntype: entity\ntitle: Mesh\nscope: [dev]\nstatus: draft\n---\nUnpublished.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.current), 0600); err != nil {
				t.Fatal(err)
			}
			spec := vault.NewNoteSpec{Title: "Storage", Tags: []string{"mesh"}, Scope: []string{"dev"}}
			s.deriveAuthoringRelated(ctx, &spec)
			if len(spec.Related) != 0 {
				t.Fatalf("stale indexed %s granted relation: %v", tc.name, spec.Related)
			}
		})
	}
}
