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

	"github.com/bright-interaction/mesh/internal/index"
)

// The review queue's promote handler leaked the server's absolute vault path twice: once
// in the error text, and once in the "path" field of EVERY successful promote. The MCP
// write path fixed the same two leaks earlier and this sibling kept them, which is why
// both surfaces now go through one scrubber and one relativization.
//
// The fixture's vault root CONTAINS A SPACE ("Automation HQ"), which is the real shape
// here and the case the scrubber used to only half-handle.
func spacedMemberServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Automation HQ", "vault")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.md"),
		[]byte("---\nid: seed\ntype: note\nwhen: 2026-01-01\n---\n# Seed\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	// Member mode: the caller is a remote teammate on a hosted hub, so the server's
	// filesystem layout is pure reconnaissance for them.
	s.SetMemberAuth(
		func(tok string) (int64, string, bool) {
			if tok == "admintok" {
				return 1, "admin", true
			}
			return 0, "", false
		},
		func(id int64) map[string]bool { return nil },
		nil, // no folder permission provider configured in this fixture
		func(id int64) (string, int64, bool) { return "admin", 1000, true },
	)
	return s, dir
}

func promoteAsAdmin(t *testing.T, s *Server, id string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/pending/promote", strings.NewReader(`{"id":"`+id+`"}`))
	req.Header.Set("Authorization", "Bearer admintok")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestPromoteDoesNotLeakTheServerVaultPath(t *testing.T) {
	cases := []struct {
		name string
		// breakIt makes vault.CreateNote fail; nil means exercise the success path.
		breakIt    func(t *testing.T, vaultRoot string)
		wantStatus int
		// keep is the useful fragment that must survive the scrub, so this cannot be
		// passed by blanking the message.
		keep string
	}{
		{
			name:       "a successful promote returns a vault-relative path",
			wantStatus: http.StatusOK,
			keep:       "keep-me.md",
		},
		{
			name: "a failed promote does not echo the absolute path",
			breakIt: func(t *testing.T, vaultRoot string) {
				if os.Geteuid() == 0 {
					t.Skip("the fixture makes a directory unwritable, which root ignores")
				}
				dir := filepath.Join(vaultRoot, "gotchas")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0o555); err != nil { // readable, not writable
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			},
			wantStatus: http.StatusInternalServerError,
			keep:       "keep-me.md",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := spacedMemberServer(t)
			if tc.breakIt != nil {
				tc.breakIt(t, dir)
			}
			// The extractor fills the queue and the owner resolves it; the viewer only
			// reads. Seeding through s.store would need a writable viewer, which is the
			// shape the single-writer split removed.
			seedPending(t, dir, completePending("Keep me"))
			code, body := promoteAsAdmin(t, s, index.PendingID("gotcha", "Keep me"))
			if code != tc.wantStatus {
				t.Fatalf("promote = %d, want %d: %s", code, tc.wantStatus, body)
			}
			assertNoVaultPath(t, body, dir)
			if !strings.Contains(body, tc.keep) {
				t.Fatalf("response lost the useful part (%q), which is a different bug: %s", tc.keep, body)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			var out struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				t.Fatalf("promote response is not JSON (%v): %s", err, body)
			}
			if filepath.IsAbs(out.Path) {
				t.Fatalf("promote returned an absolute path: %q", out.Path)
			}
			// Relative, and still resolvable against the vault, so the field keeps
			// meaning something to a client.
			if _, err := os.Stat(filepath.Join(dir, out.Path)); err != nil {
				t.Fatalf("the returned path does not resolve inside the vault: %v", err)
			}
		})
	}
}

// assertNoVaultPath fails when body carries the server's vault root or any recognisable
// segment of it. The segment check is the point: a scrubber that tokenizes on whitespace
// removes only the first chunk of a root containing a space and leaves the rest, which
// still passes a plain strings.Contains(body, vaultRoot) assertion.
func assertNoVaultPath(t *testing.T, body, vaultRoot string) {
	t.Helper()
	if strings.Contains(body, vaultRoot) {
		t.Fatalf("response carries the whole vault root: %s", body)
	}
	for _, seg := range strings.Split(vaultRoot, string(filepath.Separator)) {
		if len(seg) < 4 { // short segments ("T", "001") collide with ordinary prose
			continue
		}
		if strings.Contains(body, seg) {
			t.Fatalf("response carries the vault-root segment %q, so the layout leaked anyway: %s", seg, body)
		}
	}
}
