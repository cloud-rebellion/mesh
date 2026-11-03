// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

func TestAuthoringReferencesRepeatedTargetsAndAnchors(t *testing.T) {
	spec := vault.NewNoteSpec{
		Related:  []string{"note", " note ", "note#evidence"},
		Summary:  strings.Repeat("[[note]] [[note#evidence|Evidence]] ", 100),
		Sections: map[string]string{"findings": "[[note#limitations]]\n\n```md\n[[literal-example]]\n```"},
		Blocks: []vault.BlockSpec{{Template: "code", Fields: map[string]string{
			"code": "[[literal-code]]", "context": "[[context-note]] [[note]]",
		}}},
	}
	refs, err := AuthoringReferences(spec)
	want := []string{"note", "note#evidence", "note#limitations", "context-note"}
	if err != nil || !reflect.DeepEqual(refs, want) {
		t.Fatalf("references = %v, %v; want %v", refs, err, want)
	}
	if spec.Related[1] != " note " {
		t.Fatal("reference scan mutated the authored metadata")
	}
}

func TestAuthoringReferencesCombinedBudget(t *testing.T) {
	var spec vault.NewNoteSpec
	for i := 0; i < vault.MaxAuthoringRelated; i++ {
		spec.Related = append(spec.Related, fmt.Sprintf("related-%03d", i))
	}
	for i := 0; i < vault.MaxAuthoringList; i++ {
		spec.Collections = append(spec.Collections, fmt.Sprintf("collection-%03d", i))
		spec.Supersedes = append(spec.Supersedes, fmt.Sprintf("superseded-%03d", i))
	}
	var body strings.Builder
	for i := 0; i < 128; i++ {
		fmt.Fprintf(&body, "[[body-%03d]] ", i)
	}
	// Repeated links in prose and metadata do not add lookup work.
	body.WriteString(strings.Repeat("[[related-000]] [[body-000]] ", 100))
	spec.Sections = map[string]string{"findings": body.String()}
	refs, err := AuthoringReferences(spec)
	if err != nil || len(refs) != 448 {
		t.Fatalf("documented metadata plus body allowance: got %d references, %v", len(refs), err)
	}
	spec.Sections["findings"] += " [[one-more-target]]"
	if refs, err := AuthoringReferences(spec); err == nil || refs != nil {
		t.Fatalf("over-budget scan must refuse the whole set, got %d references, %v", len(refs), err)
	}
}

func TestAuthoringConnectedEntityReferences(t *testing.T) {
	// A connected product overview can use the schema's full relationship list
	// and repeat those relationships in prose through prepare/validate/publish.
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	refs := make([]string, vault.MaxAuthoringRelated)
	var body strings.Builder
	for i := range refs {
		refs[i] = fmt.Sprintf("connection-%03d", i)
		data := fmt.Sprintf("---\nid: %s\ntype: note\nscope: [dev]\n---\n# Connection\n\n## Evidence\nSynthetic reference fixture.\n", refs[i])
		if err := os.WriteFile(filepath.Join(dir, refs[i]+".md"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&body, "[[%s]] ", refs[i])
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
	startOwner(t, dir)
	ctx := WithLocalOperator(context.Background())
	note := authoringArgs{Title: "Connected product overview", Template: "entity", Summary: "Synthetic overview exercising the supported relationship allowance.", Sections: fixtureSections("entity"), Related: refs}
	note.Sections["interfaces"] = body.String()
	for _, action := range []string{"prepare", "validate", "publish"} {
		out, rpcErr := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": action, "note": note}))
		if rpcErr != nil {
			t.Fatalf("%s: %v", action, rpcErr)
		}
		result := toolJSON(t, out)
		if action == "validate" && result["valid"] != true {
			t.Fatalf("connected overview invalid: %v", result)
		}
		if action == "publish" {
			fetched, rpcErr := s.toolFetch(ctx, mustJSON(map[string]any{"id": result["id"], "anchor": "responsibilities-and-interfaces"}))
			if rpcErr != nil || !strings.Contains(rawContentText(t, fetched), "[[connection-255]]") {
				t.Fatalf("last relationship not retrievable: %v", rpcErr)
			}
		}
	}

	// A repeated valid target must not hide a later invalid anchor or target.
	for _, ref := range []string{"connection-000#missing", "missing-note", "../outside", ""} {
		spec := vault.NewNoteSpec{Related: append(append([]string{}, refs...), ref), Scope: []string{"dev"}}
		if err := s.validateAuthoringLinks(ctx, spec); err == nil {
			t.Fatalf("accepted invalid trailing reference %q", ref)
		}
	}
	// An accessible note ID does not make a different anchor valid.
	spec := vault.NewNoteSpec{Related: []string{"connection-000", "connection-000#evidence", "connection-000#missing"}, Scope: []string{"dev"}}
	if err := s.validateAuthoringLinks(ctx, spec); err == nil {
		t.Fatal("anchor deduplication skipped an invalid anchor")
	}
	// Change only the current file: stale indexed visibility must not bypass
	// caller scope or publication audience checks, including after 255 targets.
	lastPath := filepath.Join(dir, refs[len(refs)-1]+".md")
	data, err := os.ReadFile(lastPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lastPath, []byte(strings.Replace(string(data), "scope: [dev]", "scope: [finance]", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	spec = vault.NewNoteSpec{Related: refs, Scope: []string{"dev"}}
	limited := WithScopeFilter(ctx, &ScopeFilter{AllowedRead: map[string]bool{"dev": true}})
	for name, caller := range map[string]context.Context{"reader scope": limited, "publication audience": ctx} {
		if err := s.validateAuthoringLinks(caller, spec); err == nil {
			t.Fatalf("%s check skipped the final distinct target", name)
		}
	}
}
