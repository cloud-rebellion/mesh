// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMemberRoleGateAndRevocation proves two things the per-member web mode must do:
//  1. state-changing routes (config/reindex/review-queue) require an admin role, so a
//     viewer/member is refused with 403 while an admin succeeds;
//  2. a removed member's still-valid cookie/token stops working immediately, because
//     every request re-checks that the client still exists (roleFor ok=false).
func TestMemberRoleGateAndRevocation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("---\nid: n\ntype: note\ntitle: N\n---\n# n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	srv, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// exists is flipped to false to simulate the admin being removed mid-session.
	adminExists := true
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
		func(id int64) map[string]bool { return nil },   // unrestricted reads
		func(id int64) func(string) bool { return nil }, // no folder ACLs
		func(id int64) (string, int64, bool) {
			switch id {
			case 1:
				if !adminExists {
					return "", 0, false // revoked
				}
				return "admin", 1000, true
			case 2:
				return "viewer", 2000, true
			}
			return "", 0, false
		},
	)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	put := func(tok string) int {
		req, _ := http.NewRequest("PUT", ts.URL+"/api/config", strings.NewReader(`{"updates":{}}`))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if st := put("viewertok"); st != http.StatusForbidden {
		t.Fatalf("viewer PUT /api/config: want 403, got %d", st)
	}
	if st := put("admintok"); st != http.StatusOK {
		t.Fatalf("admin PUT /api/config: want 200, got %d", st)
	}
	// Remove the admin; the same token must now be refused everywhere (revocation).
	adminExists = false
	if st := put("admintok"); st == http.StatusOK {
		t.Fatal("revoked admin token still succeeded on PUT /api/config")
	}
	// A revoked member is also unauthenticated for reads (the guard denies them).
	req, _ := http.NewRequest("GET", ts.URL+"/graph.json", nil)
	req.Header.Set("Authorization", "Bearer admintok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked admin GET /graph.json: want 401, got %d", resp.StatusCode)
	}
}

// Curator is a known browser identity at member rank, without administrative
// permissions. Exercise the actual browser bridge and connection surfaces that
// reject an unrecognised role, not only the rank helper.
func TestCuratorBrowserIdentityRetainsMemberAccessWithoutAdministration(t *testing.T) {
	s, _, _ := connectionServer(t)
	alive := true
	s.SetMemberAuth(func(string) (int64, string, bool) { return 0, "", false }, func(int64) map[string]bool { return nil }, nil,
		func(id int64) (string, int64, bool) { return "curator", 123, alive && id == 7 })
	bridge := BrowserSignIn{LoginURL: connectionOrigin + "/auth/oidc/login", Resolve: func(r *http.Request) (int64, string, bool) {
		c, err := r.Cookie("fixture_team")
		return 7, "curator@example.test", err == nil && c.Value == "fixture-session"
	}, Clear: func(http.ResponseWriter) {}}
	if err := s.SetBrowserSignIn(bridge); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	d := connectionDevice(t, h, "full")
	cookie := &http.Cookie{Name: "fixture_team", Value: "fixture-session"}
	w := connectionCall(t, h, "GET", "/api/connect/request?user_code="+d.UserCode, nil, cookie, "", "")
	var details map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &details); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || details["account"] != "curator@example.test (curator)" {
		t.Fatalf("curator browser refused: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct{ method, path string }{{"PUT", "/api/config"}, {"POST", "/api/reindex"}, {"POST", "/api/pending/promote"}, {"POST", "/api/pending/discard"}} {
		w = connectionCall(t, h, tc.method, tc.path, map[string]any{"updates": map[string]any{}}, cookie, "", connectionOrigin)
		if w.Code != 403 {
			t.Fatalf("curator administration %s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	alive = false
	w = connectionCall(t, h, "GET", "/api/connect/request?user_code="+d.UserCode, nil, cookie, "", "")
	if w.Code != 401 {
		t.Fatalf("revoked curator retained browser access: %d", w.Code)
	}
}
