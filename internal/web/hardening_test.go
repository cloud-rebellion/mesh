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
	"time"
)

// memberServer builds a server in per-member mode with an admin (id 1) and a viewer
// (id 2), the shape the hosted /app actually runs in.
func memberServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("---\nid: n\ntype: note\ntitle: N\n---\n# n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	s.SetMemberAuth(
		func(tok string) (int64, string, bool) {
			switch tok {
			case "admintok":
				return 1, "admin", true
			case "viewertok":
				return 2, "viewer", true
			}
			return 0, "", false
		},
		func(id int64) map[string]bool { return nil },
		func(id int64) func(string) bool { return nil },
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
	return s
}

// A *.key_env field names an environment variable that the retrieval layer reads with
// os.Getenv and then sends as an Authorization: Bearer header to a caller-configured
// endpoint. Before the allow-list any identifier was accepted, so a config write was an
// arbitrary read of the process environment: pointing rerank.key_env at MESH_UI_TOKEN
// exfiltrated the break-glass admin login (and the member-cookie signing seed) to the
// attacker's rerank host on the next search. Only Mesh's own key slots are allowed.
func TestConfigKeyEnvAllowlist(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
		want  int
	}{
		{"rerank key_env cannot name the UI token", "rerank.key_env", "MESH_UI_TOKEN", http.StatusBadRequest},
		{"rerank key_env cannot name the cookie secret", "rerank.key_env", "MESH_UI_COOKIE_SECRET", http.StatusBadRequest},
		{"embedding key_env cannot name the hub token", "embedding.key_env", "MESH_HUB_TOKEN", http.StatusBadRequest},
		{"secret bridge key_env cannot name the UI token", "secret_bridge.key_env", "MESH_UI_TOKEN", http.StatusBadRequest},
		{"key_env cannot name an unrelated var", "rerank.key_env", "HOME", http.StatusBadRequest},
		{"key_env cannot name a Mesh-looking var outside the set", "rerank.key_env", "MESH_ANYTHING_KEY", http.StatusBadRequest},
		{"embedding key_env accepts its own slot", "embedding.key_env", "MESH_EMBED_KEY", http.StatusOK},
		{"rerank key_env accepts its own slot", "rerank.key_env", "MESH_RERANK_KEY", http.StatusOK},
		{"secret bridge key_env accepts its own slot", "secret_bridge.key_env", "MESH_SECRET_BRIDGE_KEY", http.StatusOK},
		{"blank clears back to the default", "rerank.key_env", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := cfgServer(t)
			h := s.Handler()
			code, _ := doJSON(t, h, "PUT", "/api/config", `{"updates":{"`+tc.key+`":"`+tc.value+`"}}`)
			if code != tc.want {
				t.Fatalf("PUT %s=%q = %d, want %d", tc.key, tc.value, code, tc.want)
			}
			if tc.want != http.StatusOK {
				// A rejected value must not reach config.toml either.
				b, _ := os.ReadFile(filepath.Join(dir, ".mesh", "config.toml"))
				if strings.Contains(string(b), tc.value) {
					t.Fatalf("rejected key_env %q was still persisted:\n%s", tc.value, b)
				}
			}
		})
	}
}

// A key_env that predates the allow-list (or was hand-edited into config.toml) must not
// be re-persisted the next time an admin saves an unrelated field.
func TestConfigKeyEnvScrubbedOnSave(t *testing.T) {
	s, dir := cfgServer(t)
	meshDir := filepath.Join(dir, ".mesh")
	if err := os.MkdirAll(meshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hostile := "[retrieval]\nrerank_key_env = \"MESH_UI_TOKEN\"\n"
	if err := os.WriteFile(filepath.Join(meshDir, "config.toml"), []byte(hostile), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := doJSON(t, s.Handler(), "PUT", "/api/config", `{"updates":{"rerank.blend":"0.5"}}`); code != http.StatusOK {
		t.Fatalf("PUT unrelated field = %d, want 200", code)
	}
	b, _ := os.ReadFile(filepath.Join(meshDir, "config.toml"))
	if strings.Contains(string(b), "MESH_UI_TOKEN") {
		t.Fatalf("a pre-existing hostile key_env survived the save:\n%s", b)
	}
}

// Members can open Settings, but the shared operator configuration remains read-only
// and sensitive endpoint/key-slot metadata is redacted. Writes remain admin-gated.
func TestGetConfigMemberViewIsRedactedAndReadOnly(t *testing.T) {
	h := memberServer(t).Handler()
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"viewer can read safe settings", "viewertok", http.StatusOK},
		{"admin can read settings", "admintok", http.StatusOK},
		{"unauthenticated is refused", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/config", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("GET /api/config as %q = %d, want %d", tc.token, rec.Code, tc.want)
			}
			if tc.token != "viewertok" {
				return
			}
			var got struct {
				Fields      []cfgField      `json:"fields"`
				Permissions map[string]bool `json:"permissions"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Permissions["admin"] {
				t.Fatal("viewer response reports admin permission")
			}
			for _, f := range got.Fields {
				if f.Editable {
					t.Fatalf("viewer field %s is editable", f.Key)
				}
				if strings.Contains(f.Key, "endpoint") || strings.HasSuffix(f.Key, "key_env") || f.Key == "secret_bridge.agent_id" {
					if f.Value != "" {
						t.Fatalf("viewer field %s leaked value %q", f.Key, f.Value)
					}
				}
			}
		})
	}
	req := httptest.NewRequest("PUT", "/api/config", strings.NewReader(`{"updates":{}}`))
	req.Header.Set("Authorization", "Bearer viewertok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer PUT /api/config = %d, want 403", rec.Code)
	}
}

// The absolute server-side vault path is operator-only. It stays in standalone mode
// (the browser is the operator, and the editor:// bridge needs it) but must not be
// handed to a remote teammate in member mode.
func TestVaultPathNotLeakedToMembers(t *testing.T) {
	s := memberServer(t)
	if strings.Contains(s.exposedVaultRoot(), string(filepath.Separator)) {
		t.Errorf("member mode exposed an absolute vault path: %q", s.exposedVaultRoot())
	}
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("Authorization", "Bearer viewertok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), s.vaultRoot) {
		t.Errorf("/api/status leaked the absolute vault root to a member:\n%s", rec.Body.String())
	}
	// Standalone mode keeps the absolute path.
	std, _ := cfgServer(t)
	if std.exposedVaultRoot() != std.vaultRoot {
		t.Errorf("standalone mode should keep the absolute vault path, got %q", std.exposedVaultRoot())
	}
}

// Every response carries a CSP that refuses inline script. This is the backstop behind
// renderMDSafe: the SPA assigns server-rendered note HTML to innerHTML, so a future
// sanitiser gap must still not be able to run an injected handler.
func TestContentSecurityPolicyHeader(t *testing.T) {
	ts := testServer(t)
	for _, path := range []string{"/", "/graph.json", "/api/status", "/assets/app.js"} {
		_, _, hdr := get(t, ts, path)
		csp := hdr.Get("Content-Security-Policy")
		if csp == "" {
			t.Fatalf("%s has no Content-Security-Policy header", path)
		}
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "object-src 'none'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s CSP missing %q: %s", path, want, csp)
			}
		}
		// script-src must not be relaxed; an injected inline handler has to stay dead.
		script := csp[strings.Index(csp, "script-src"):]
		if i := strings.Index(script, ";"); i >= 0 {
			script = script[:i]
		}
		if strings.Contains(script, "unsafe-inline") || strings.Contains(script, "unsafe-eval") {
			t.Errorf("%s script-src is relaxed, which defeats the point: %s", path, script)
		}
	}
}

// POST /api/login is the only unauthenticated route that validates a secret, so it is
// an unlimited guessing oracle for the shared admin token and every member client token
// unless it is throttled. Burst is 5 per peer, so the sixth attempt in a burst is 429.
func TestLoginRateLimited(t *testing.T) {
	s, _ := cfgServer(t)
	s.auth = authConfig{token: "s3cret", loopback: false}
	h := s.Handler()

	codes := make([]int, 0, 7)
	for i := 0; i < 7; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, jsonReq("POST", "/api/login", `{"key":"guess"}`))
		codes = append(codes, rec.Code)
	}
	for i := 0; i < 5; i++ {
		if codes[i] != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401 (the burst must be served)", i+1, codes[i])
		}
	}
	for i := 5; i < 7; i++ {
		if codes[i] != http.StatusTooManyRequests {
			t.Fatalf("attempt %d = %d, want 429 (the bucket must be empty)", i+1, codes[i])
		}
	}
	// The throttle is a rate, not a ban: once the bucket refills the correct key works.
	s.logins.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, jsonReq("POST", "/api/login", `{"key":"s3cret"}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login after the bucket refilled = %d, want 204", rec.Code)
	}
}

// peerKey must not be steerable by the caller: with no trusted proxy configured the
// socket address wins, and with a configured chain only the hop the trusted proxy
// appended is used. Otherwise an attacker rotates X-Forwarded-For and gets a fresh
// login bucket per request.
func TestPeerKeyTrustedProxyHops(t *testing.T) {
	cases := []struct {
		name, hops, xff, want string
	}{
		{"no proxy configured ignores the header", "", "1.2.3.4", "192.0.2.1"},
		{"zero hops ignores the header", "0", "1.2.3.4", "192.0.2.1"},
		{"one hop takes the last entry", "1", "1.2.3.4, 5.6.7.8", "5.6.7.8"},
		{"spoofed prefix does not shift the key", "1", "9.9.9.9, 8.8.8.8, 5.6.7.8", "5.6.7.8"},
		{"two hops takes the second from the right", "2", "1.2.3.4, 5.6.7.8", "1.2.3.4"},
		{"header shorter than the chain falls back to the socket", "2", "5.6.7.8", "192.0.2.1"},
		{"no header at all falls back to the socket", "1", "", "192.0.2.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(trustedProxyHopsEnv, tc.hops)
			req := httptest.NewRequest("POST", "/api/login", nil)
			req.RemoteAddr = "192.0.2.1:5555"
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := peerKey(req); got != tc.want {
				t.Errorf("peerKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// POST /api/ask forks an LLM subprocess per call and is reachable by any member,
// viewers included. Both guards must refuse before any LLM work starts: an empty token
// bucket and a full in-flight semaphore each return 429.
func TestAskRateLimitAndInFlightCap(t *testing.T) {
	s, _ := cfgServer(t)
	h := s.Handler()
	req := jsonReq("POST", "/api/ask", `{"question":"hi"}`)
	key := s.askKey(req)

	t.Run("empty token bucket refuses", func(t *testing.T) {
		for s.asks.allow(key) { // drain
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, jsonReq("POST", "/api/ask", `{"question":"hi"}`))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("ask with an empty bucket = %d, want 429", rec.Code)
		}
	})

	t.Run("full in-flight cap refuses", func(t *testing.T) {
		s.asks = newRateLimiter(1, 100) // plenty of tokens, so only the semaphore can refuse
		for i := 0; i < askMaxInFlight; i++ {
			s.askSlots <- struct{}{}
		}
		defer func() {
			for i := 0; i < askMaxInFlight; i++ {
				<-s.askSlots
			}
		}()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, jsonReq("POST", "/api/ask", `{"question":"hi"}`))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("ask with a full in-flight cap = %d, want 429", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "busy") {
			t.Errorf("expected the busy message, got %q", rec.Body.String())
		}
	})
}

// Building the retriever must not hold the exclusive graph lock: retrieve.NewFromEnv
// loads every stored vector, builds the ANN index and probes the embedding endpoint over
// the network, so the old `s.mu.Lock(); defer s.mu.Unlock()` around it froze /graph.json,
// every other search and the pending API for the whole probe. Holding a READ lock here
// and calling retriever() concurrently deadlocks against the old code and completes
// against the new one.
func TestRetrieverDoesNotHoldGraphWriteLock(t *testing.T) {
	s, _ := cfgServer(t)
	s.mu.RLock() // stands in for an in-flight /graph.json
	defer s.mu.RUnlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if rt := s.retriever(); rt == nil {
			t.Error("retriever() returned nil")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retriever() blocked while a reader held the graph lock: the build is still inside s.mu")
	}
}
