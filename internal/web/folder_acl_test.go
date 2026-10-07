// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/llm"
)

// TestWebFolderACLFencesEveryReadSurface proves the per-member web app applies the
// team's FOLDER ACLs, not just note scopes, on every surface that returns vault
// content: /graph.json, /api/search, /api/note/{id} and /api/ask.
//
// The fixture is the shape that made this a hole rather than a corner case: the team
// fences clients/ with an ACL and has defined NO scope at all, so the scope resolver
// hands every member nil (unrestricted) and filters nothing whatsoever. Folder ACLs and
// scopes are independent partitions, and the sync path (internal/hub/sync.go) has always
// enforced both.
//
// The admin half of the table is the anti-vacuity control: an unrestricted caller must
// still SEE the fenced note on the same surface, so a test that passes because retrieval
// returned nothing cannot be mistaken for a test that passes because the fence works.
func TestWebFolderACLFencesEveryReadSurface(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("public/open.md", "---\nid: openinfo\ntype: note\ntitle: OpenInfo\n---\n# openbrief\n\nThe deployment runbook the whole team may read.\n")
	write("clients/fenced.md", "---\nid: fenced\ntype: note\ntitle: FencedClient\n---\n# quarterlyfence\n\nThe deployment terms only the client team may read.\n")

	seedIndex(t, dir)
	srv, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.SetMemberAuth(
		func(tok string) (int64, string, bool) {
			switch tok {
			case "admintok":
				return 1, "admin", true
			case "viewertok":
				return 2, "viewer", true
			}
			return 0, "", false
		},
		func(int64) map[string]bool { return nil }, // no scopes defined: scope filtering is off
		func(id int64) func(string) bool {
			if id == 2 {
				return func(p string) bool { return !strings.HasPrefix(p, "clients/") }
			}
			return nil // admin: no folder ACL governs them
		},
		func(id int64) (string, int64, bool) {
			switch id {
			case 1:
				return "admin", 1000, true
			case 2:
				return "viewer", 2000, true
			}
			return "", 0, false
		},
	)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// /api/ask forks the BYOAI LLM as a subprocess with the grounded prompt on stdin;
	// `cat` echoes it back, so the response body IS whatever context reached the model.
	// Filtering the citations alone would leave the fenced note's body in the answer.
	t.Setenv("MESH_CURATOR_AGENT", "cli")
	t.Setenv("MESH_CURATOR_CMD", "cat")
	t.Setenv("MESH_CURATOR_CLI_CONTRACT", llm.CompletionCLIContract)

	call := func(method, path, tok, body string) (int, string) {
		t.Helper()
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, ts.URL+path, rdr)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// Any of these in a response means the fenced note leaked: its title, its path, or a
	// term that appears only in its body.
	fenced := []string{"FencedClient", "clients/fenced.md", "quarterlyfence"}
	leaked := func(body string) string {
		for _, m := range fenced {
			if strings.Contains(body, m) {
				return m
			}
		}
		return ""
	}

	tests := []struct {
		name          string
		method, path  string
		body          string
		viewerStatus  int
		adminStatus   int
		viewerSeesOwn string // marker of the note the viewer IS allowed, "" to skip
	}{
		{"graph", "GET", "/graph.json", "", 200, 200, "OpenInfo"},
		{"search", "GET", "/api/search?q=deployment", "", 200, 200, "OpenInfo"},
		{"note", "GET", "/api/note/fenced", "", http.StatusNotFound, 200, ""},
		{"ask", "POST", "/api/ask", `{"question":"what are the deployment terms?"}`, 200, 200, "OpenInfo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, body := call(tc.method, tc.path, "viewertok", tc.body)
			if st != tc.viewerStatus {
				t.Errorf("viewer %s %s = %d, want %d", tc.method, tc.path, st, tc.viewerStatus)
			}
			if m := leaked(body); m != "" {
				t.Errorf("viewer %s %s leaked the fenced note (%q): %s", tc.method, tc.path, m, body)
			}
			if tc.viewerSeesOwn != "" && !strings.Contains(body, tc.viewerSeesOwn) {
				t.Errorf("viewer %s %s should still return %q: %s", tc.method, tc.path, tc.viewerSeesOwn, body)
			}
			st, body = call(tc.method, tc.path, "admintok", tc.body)
			if st != tc.adminStatus {
				t.Errorf("admin %s %s = %d, want %d", tc.method, tc.path, st, tc.adminStatus)
			}
			if leaked(body) == "" {
				t.Errorf("admin %s %s should see the fenced note (control): %s", tc.method, tc.path, body)
			}
		})
	}
}
