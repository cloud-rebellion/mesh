// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

func TestSearchMissingGuidanceAPI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("---\nid: n\ntype: note\ntemplate: finding\ntemplate_version: 1\nwhen: 2026-09-23\n---\n# Guidanceneedle\nHistorical context\n"), 0600); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	code, got := doJSON(t, s.Handler(), "GET", "/api/search?q=guidanceneedle&budget=400", "")
	if code != http.StatusOK {
		t.Fatalf("search = %d, %v", code, got)
	}
	cards, _ := got["cards"].([]any)
	if len(cards) != 1 {
		t.Fatalf("expected one card: %v", got)
	}
	if missing := cards[0].(map[string]any)["MissingGuidance"]; !reflect.DeepEqual(missing, []any{"summary", "question", "findings", "evidence", "limitations", "next_steps"}) {
		t.Fatalf("web API lost warning: %v", cards)
	}
	b, _ := json.Marshal(cards)
	if tokens := retrieve.EstimateTokens(string(b)); tokens > 400 || float64(tokens) > got["tokens"].(float64) {
		t.Fatalf("web API budget understated: %d, %v", tokens, got)
	}
}

func TestSearchAndNote(t *testing.T) {
	s, _ := cfgServer(t) // a vault with one note id "n", body "# N\nbody"
	h := s.Handler()

	// search requires q
	if code, _ := doJSON(t, h, "GET", "/api/search", ""); code != http.StatusBadRequest {
		t.Errorf("search without q = %d, want 400", code)
	}
	// search returns cards including note:n
	code, got := doJSON(t, h, "GET", "/api/search?q=body", "")
	if code != 200 {
		t.Fatalf("search = %d", code)
	}
	cards, _ := got["cards"].([]any)
	found := false
	for _, ci := range cards {
		if ci.(map[string]any)["NoteID"] == "n" {
			found = true
		}
	}
	if !found {
		t.Fatalf("search did not return note n: %+v", cards)
	}
	// fetch the note's markdown by id
	code, note := doJSON(t, h, "GET", "/api/note/n", "")
	if code != 200 || !strings.Contains(note["markdown"].(string), "# N") {
		t.Errorf("note fetch = %d %+v", code, note)
	}
	// unknown id -> 404
	if code, _ := doJSON(t, h, "GET", "/api/note/nope", ""); code != http.StatusNotFound {
		t.Errorf("unknown note = %d, want 404", code)
	}
}

// TestNoteBodyStripsFrontmatter guards the drawer bug: the client renders "body" (and
// the server-rendered "html" derived from it) as the note's prose. Neither may still
// carry the YAML frontmatter block, or a reader sees "id: n\ntype: note\n..." dumped
// above the actual content. "markdown" is exempt on purpose: it stays the raw file,
// frontmatter included, for any existing caller that relied on the old shape.
func TestNoteBodyStripsFrontmatter(t *testing.T) {
	s, _ := cfgServer(t) // note "n": "---\nid: n\ntype: note\nwhen: 2026-01-01\n---\n# N\nbody\n"
	h := s.Handler()

	code, note := doJSON(t, h, "GET", "/api/note/n", "")
	if code != 200 {
		t.Fatalf("note fetch = %d %+v", code, note)
	}
	body, _ := note["body"].(string)
	if strings.HasPrefix(strings.TrimSpace(body), "---") {
		t.Errorf("body still starts with a frontmatter fence:\n%s", body)
	}
	if strings.Contains(body, "id: n") || strings.Contains(body, "type: note") {
		t.Errorf("body still carries frontmatter keys:\n%s", body)
	}
	if !strings.Contains(body, "# N") {
		t.Errorf("body dropped the note's markdown heading:\n%s", body)
	}
	meta, _ := note["meta"].(map[string]any)
	if meta == nil || meta["title"] != "" || meta["type"] != "note" || meta["when"] != "2026-01-01" {
		t.Errorf("meta did not parse frontmatter fields: %+v", meta)
	}
	// markdown stays the full raw file for backward compatibility.
	md, _ := note["markdown"].(string)
	if !strings.Contains(md, "id: n") {
		t.Errorf("markdown should still carry the raw frontmatter for old callers: %+v", md)
	}
}
