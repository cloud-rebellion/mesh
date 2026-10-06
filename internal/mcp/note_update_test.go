// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

func publishUpdateFixture(t *testing.T, s *Server, title string) map[string]any {
	t.Helper()
	result, err := s.toolAuthorNote(WithLocalOperator(context.Background()), mustJSON(map[string]any{"action": "publish", "note": authoringArgs{Title: title, Template: "finding", Summary: "Synthetic schema guidance with explicit limits.", Sections: fixtureSections("note"), Tags: []string{"schema"}}}))
	if err != nil {
		t.Fatal(err)
	}
	return toolJSON(t, result)
}

func TestPublishedNoteUpdateWorkflowLinksDecisionAndRefreshesRetrieval(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	ctx := WithLocalOperator(context.Background())
	decision, err := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": authoringArgs{Title: "Schema compatibility decision", Template: "decision", Summary: "The fixture chooses a nullable field for compatibility.", Sections: fixtureSections("decision")}}))
	if err != nil {
		t.Fatal(err)
	}
	decisionID := toolJSON(t, decision)["id"].(string)
	first := publishUpdateFixture(t, s, "Schema before update")
	id, path := first["id"].(string), first["path"].(string)
	prepared, err := s.handleToolsCall(ctx, mustJSON(map[string]any{"name": "mesh_prepare_update", "arguments": map[string]any{"id": id}}))
	if err != nil {
		t.Fatal(err)
	}
	preview := toolJSON(t, prepared)
	note := preview["note"].(map[string]any)
	if note["update_revision"] != first["revision"] || note["update_id"] != id || preview["saved"] != false {
		t.Fatalf("missing safe update binding: %v", preview)
	}
	note["title"], note["summary"] = "Schema after update", "The synthetic schema adds a nullable field; production behavior remains unverified."
	note["sections"].(map[string]any)["findings"] = "The fixture adds a nullable field. [[" + decisionID + "]] explains the compatibility decision."
	note["related"] = []string{decisionID}
	validated, err := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "validate", "note": note}))
	if err != nil || toolJSON(t, validated)["valid"] != true {
		t.Fatalf("validation=%v / %v", validated, err)
	}
	saved, err := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": note}))
	if err != nil {
		t.Fatal(err)
	}
	updated := toolJSON(t, saved)
	if updated["id"] != id || updated["path"] != path || updated["revision"] == first["revision"] || updated["updated"] != true || updated["previous_revision"] != first["revision"] {
		t.Fatalf("bad receipt: %v", updated)
	}
	fetched, err := s.toolFetch(ctx, mustJSON(map[string]any{"id": id, "anchor": "findings"}))
	if err != nil || !strings.Contains(rawContentText(t, fetched), "nullable field") || !strings.Contains(rawContentText(t, fetched), decisionID) {
		t.Fatalf("updated section unavailable: %v %v", fetched, err)
	}
	search, err := s.toolSearch(ctx, mustJSON(map[string]any{"query": "Schema after update"}))
	if err != nil || !strings.Contains(rawContentText(t, search), "Schema after update") {
		t.Fatalf("updated title absent: %v %v", search, err)
	}
	if _, err := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": note})); err == nil {
		t.Fatal("stale retry accepted")
	}
}

func TestPublishedNoteUpdateEnforcesCurrentScopesAndWriteRole(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	first := publishUpdateFixture(t, s, "Protected schema fixture")
	id := first["id"].(string)
	ctx := WithScopeFilter(WithWriteCapability(context.Background(), true), &ScopeFilter{AllowedRead: map[string]bool{"dev": true}, WriteScope: "dev", CanWrite: func(scope string) bool { return scope == "dev" }})
	prepared, err := s.toolPrepareUpdate(ctx, mustJSON(map[string]any{"id": id}))
	if err != nil {
		t.Fatal(err)
	}
	note := toolJSON(t, prepared)["note"].(map[string]any)
	viewer := WithWriteCapability(ctx, false)
	if _, err := s.toolPrepareUpdate(viewer, mustJSON(map[string]any{"id": id})); err == nil {
		t.Fatal("viewer received a writable object")
	}
	if _, err := s.toolAuthorNote(viewer, mustJSON(map[string]any{"action": "publish", "note": note})); err == nil {
		t.Fatal("viewer changed publication")
	}
	path := filepath.Join(s.vaultRoot, first["path"].(string))
	data, _ := os.ReadFile(path)
	// Indexed permission remains dev. Current-source tightening must win before
	// another index refresh, including when the caller tries an already-read object.
	data = []byte(strings.Replace(string(data), "---\n", "---\nscope: [finance]\n", 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := s.toolPrepareUpdate(ctx, mustJSON(map[string]any{"id": id})); err == nil || out != nil {
		t.Fatal("tightened current scope leaked content")
	}
	if _, err := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": note})); err == nil {
		t.Fatal("old scoped object overwrote tightened source")
	}
	current, _ := os.ReadFile(path)
	if string(current) != string(data) {
		t.Fatal("permission refusal changed source")
	}
}

func TestPublishedNoteUpdateRejectsMissingLinksAndIncompleteEdits(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	first := publishUpdateFixture(t, s, "Intact schema fixture")
	ctx := WithLocalOperator(context.Background())
	prepared, err := s.toolPrepareUpdate(ctx, mustJSON(map[string]any{"id": first["id"]}))
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"draft", "publish"} {
		note := toolJSON(t, prepared)["note"].(map[string]any)
		note["sections"].(map[string]any)["findings"] = ""
		if _, err := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": action, "note": note})); err == nil {
			t.Fatalf("%s replaced intact note with incomplete work", action)
		}
	}
	note := toolJSON(t, prepared)["note"].(map[string]any)
	note["related"] = []string{"missing-decision"}
	if _, err := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": note})); err == nil {
		t.Fatal("invented decision link was published")
	}
	data, _ := os.ReadFile(filepath.Join(s.vaultRoot, first["path"].(string)))
	if len(data) == 0 || first["revision"] != vault.ContentRevision(data) {
		t.Fatal("refused update changed canonical note")
	}
}

func TestPrepareUpdateKeepsImportedContentBoundary(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	first := publishUpdateFixture(t, s, "Imported schema fixture")
	path := filepath.Join(s.vaultRoot, first["path"].(string))
	data, _ := os.ReadFile(path)
	data = []byte(strings.Replace(string(data), "source: agent", "source: import:fixture", 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := s.toolPrepareUpdate(context.Background(), mustJSON(map[string]any{"id": first["id"]}))
	if err != nil {
		t.Fatal(err)
	}
	text := rawContentText(t, result)
	if !strings.HasPrefix(text, untrustedOpenPrefix) || !strings.HasSuffix(text, untrustedClose) || strings.Count(text, untrustedOpenPrefix) != 1 || strings.Count(text, untrustedClose) != 1 {
		t.Fatal("imported update lost its data boundary")
	}
	if !strings.Contains(text, "import:fixture") || !strings.Contains(text, "update_revision") || !strings.Contains(text, "Synthetic schema guidance") {
		t.Fatal("update prefill lost provenance or content")
	}
	// Rendering the edited preview must carry the same boundary too.
	snapshot, err := s.authorizedUpdate(context.Background(), first["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	v := snapshot.Spec
	preview, err := s.toolPrepareNote(context.Background(), mustJSON(authoringArgs{UpdateID: v.UpdateID, UpdateRevision: v.UpdateRevision, Title: v.Title, Template: v.Template, TemplateVersion: v.TemplateVersion, Summary: v.Summary, Sections: v.Sections}), false)
	if err != nil {
		t.Fatal(err)
	}
	previewText := rawContentText(t, preview)
	if !strings.HasPrefix(previewText, untrustedOpenPrefix) || !strings.HasSuffix(previewText, untrustedClose) {
		t.Fatal("preview dropped imported data boundary")
	}
}

func TestPublishedUpdateRetainsEveryScopeWithoutDefaultingToOne(t *testing.T) {
	s := newTestServer(t)
	created, err := vault.CreateNote(s.vaultRoot, vault.NewNoteSpec{Title: "Shared schema fixture", Template: "finding", Summary: "A fixture shared by two audiences.", Sections: fixtureSections("note"), Scope: []string{"dev", "sales"}})
	if err != nil {
		t.Fatal(err)
	}
	seedIndex(t, s.vaultRoot)
	startOwner(t, s.vaultRoot)
	partial := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"dev": true}, WriteScope: "dev", CanWrite: func(scope string) bool { return scope == "dev" }})
	if _, err := s.toolPrepareUpdate(partial, mustJSON(map[string]any{"id": created.ID})); err == nil {
		t.Fatal("control of only one partition allowed shared-note editing")
	}
	full := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"dev": true, "sales": true}, WriteScope: "dev", CanWrite: func(scope string) bool { return scope == "dev" || scope == "sales" }})
	prepared, rerr := s.toolPrepareUpdate(full, mustJSON(map[string]any{"id": created.ID}))
	if rerr != nil {
		t.Fatal(rerr)
	}
	note := toolJSON(t, prepared)["note"].(map[string]any)
	note["summary"] = "The shared fixture changes without removing an audience."
	if _, err := s.toolAuthorNote(full, mustJSON(map[string]any{"action": "publish", "note": note})); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(created.Path)
	fm, _, err := vault.ParseFrontmatter(data)
	if err != nil || len(fm.EffectiveScopes()) != 2 {
		t.Fatalf("shared audience was changed: %+v %v", fm, err)
	}
}
