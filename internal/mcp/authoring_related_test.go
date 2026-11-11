// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

func TestDerivedAuthoringRelationsRespectEvidenceAndAudience(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	for id, scope := range map[string]string{"public": "dev", "private": "sales", "unrelated": "dev"} {
		tags := "[mesh]"
		if id == "unrelated" {
			tags = "[other]"
		}
		data := "---\nid: " + id + "\ntype: note\nscope: [" + scope + "]\ntags: " + tags + "\n---\n# Mesh connection engine\nMesh connection engine relationships.\n"
		if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
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
	for _, scoped := range []bool{false, true} {
		ctx := context.Background()
		if scoped {
			ctx = WithScopeFilter(ctx, &ScopeFilter{AllowedRead: map[string]bool{"dev": true}})
		}
		spec := vault.NewNoteSpec{Title: "Mesh connection engine", Tags: []string{"mesh"}, Scope: []string{"dev"}}
		s.deriveAuthoringRelated(ctx, &spec)
		if !reflect.DeepEqual(spec.Related, []string{"public"}) {
			t.Fatalf("scoped=%v: related=%v; want only corroborated audience-compatible reference", scoped, spec.Related)
		}
	}
	for _, spec := range []vault.NewNoteSpec{
		{Title: "Mesh connection engine", Tags: []string{"mesh"}, Status: "draft"},
		{Title: "Mesh connection engine", Tags: []string{"mesh"}, UpdateID: "existing"},
		{Title: "Mesh connection engine", Tags: []string{"mesh"}, Related: []string{}},
		{Title: "Mesh connection engine", Tags: []string{"mesh"}, Related: []string{"authored"}},
	} {
		before := spec.Related
		s.deriveAuthoringRelated(context.Background(), &spec)
		if !reflect.DeepEqual(spec.Related, before) {
			t.Fatalf("explicit authoring intent changed: %v -> %v", before, spec.Related)
		}
	}
}
