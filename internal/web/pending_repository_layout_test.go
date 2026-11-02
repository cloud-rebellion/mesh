// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

func TestPendingPromotionLinkedWorktreePreservesSoloBehaviorAndRefusesTeamAmbiguity(t *testing.T) {
	t.Setenv("MESH_AUTHORING_MODE", "enabled")
	for _, team := range []bool{false, true} {
		name := "solo"
		if team {
			name = "team"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			metadata := []byte("gitdir: /fixture-path-that-must-not-be-followed\n")
			if err := os.WriteFile(filepath.Join(dir, ".git"), metadata, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := NewOwningServer(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if team {
				s.SetMemberAuth(func(string) (int64, string, bool) { return 1, "fixture", true }, nil, nil,
					func(int64) (string, int64, bool) { return "admin", 1, true })
			}
			p := completePending("Linked-worktree layout fixture")
			if err := s.store.AddPending(p); err != nil {
				t.Fatal(err)
			}
			id := index.PendingID(p.Type, p.Title)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/pending/promote", strings.NewReader(`{"id":"`+id+`"}`))
			req.Header.Set("Authorization", "Bearer fixture")
			s.Handler().ServeHTTP(rec, req)
			want := http.StatusOK
			if team {
				want = http.StatusServiceUnavailable
			}
			if rec.Code != want {
				t.Fatalf("status=%d want=%d: %s", rec.Code, want, rec.Body.String())
			}
			if team {
				if _, err := s.store.GetPending(id); err != nil {
					t.Fatal("unsupported team layout lost pending source")
				}
			}
			current, err := os.ReadFile(filepath.Join(dir, ".git"))
			if err != nil || string(current) != string(metadata) {
				t.Fatal("publication altered Git metadata")
			}
		})
	}
}
