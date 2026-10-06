// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise wire dispatch with an indexed store but no graph/retriever. Unlike
// batchE2EReady, this helper must never close the startup gate for the caller.
func coldFetchHTTP(t *testing.T, s *Server, ctx context.Context, name string, args any) response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body))).WithContext(ctx)
	s.HandleHTTP(w, r)
	var reply response
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func coldFetchArgs(batch bool, anchor string) (string, any) {
	if batch {
		return "mesh_fetch_many", map[string]any{"items": []batchFetchItem{{ID: "n0", Anchor: anchor}}, "budget": 8000}
	}
	return "mesh_fetch", map[string]any{"id": "n0", "anchor": anchor}
}

func coldFetchText(t *testing.T, reply response, batch bool) string {
	t.Helper()
	if reply.Error != nil {
		t.Fatalf("indexed fetch waited for the unrelated graph: %v", reply.Error)
	}
	text := coldFetchWireText(t, reply.Result)
	if !batch {
		return text
	}
	var result batchFetchResponse
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Status != "ok" {
		t.Fatalf("batch did not return the known note: %s", text)
	}
	return result.Results[0].Text
}

func coldFetchWireText(t *testing.T, result any) string {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var content struct {
		Content []struct{ Type, Text string }
	}
	if err := json.Unmarshal(data, &content); err != nil {
		t.Fatal(err)
	}
	if len(content.Content) != 1 || content.Content[0].Type != "text" {
		t.Fatalf("invalid HTTP content envelope: %s", data)
	}
	return content.Content[0].Text
}

func TestKnownNoteFetchDoesNotAwaitGraph(t *testing.T) {
	for _, state := range []string{"loading", "failed"} {
		for _, batch := range []bool{false, true} {
			for _, anchor := range []string{"", "parent"} {
				name, args := coldFetchArgs(batch, anchor)
				t.Run(state+"/"+name+"/"+anchor, func(t *testing.T) {
					s := batchFixture(t, 1)
					s.ready = make(chan struct{})
					if state == "failed" {
						s.readyErr = errors.New("graph fixture failed")
						close(s.ready)
					}
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					ctx = WithScopeFilter(ctx, &ScopeFilter{AllowedRead: map[string]bool{"public": true}})
					text := coldFetchText(t, coldFetchHTTP(t, s, ctx, name, args), batch)
					for _, want := range []string{"parent answer", "retired", "Do not deploy."} {
						if !strings.Contains(text, want) {
							t.Fatalf("cold fetch omitted answer or safety context %q", want)
						}
					}
					if anchor != "" && strings.Contains(text, "other answer") {
						t.Fatal("section fetch widened its content")
					}
					if got, err := s.store.Metric("fetches"); err != nil || got != 1 {
						t.Fatalf("fetch attribution = %d, err = %v", got, err)
					}
				})
			}
		}
	}
}

func TestColdFetchKeepsCurrentScopeAndCancellation(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, failure := range []string{"restricted", "malformed", "cancelled"} {
			name, args := coldFetchArgs(batch, "parent")
			t.Run(name+"/"+failure, func(t *testing.T) {
				s := batchFixture(t, 1)
				s.ready = make(chan struct{})
				if failure != "cancelled" {
					scope := "[private]"
					if failure == "malformed" {
						scope = "[private"
					}
					body := "---\nid: n0\ntype: note\nscope: " + scope + "\n---\n## Parent\nrestricted fixture bytes\n"
					if err := os.WriteFile(filepath.Join(s.vaultRoot, "n0.md"), []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if failure == "cancelled" {
					cancel()
				}
				ctx = WithScopeFilter(ctx, &ScopeFilter{AllowedRead: map[string]bool{"public": true}})
				reply := coldFetchHTTP(t, s, ctx, name, args)
				if failure == "cancelled" {
					if reply.Error == nil || reply.Result != nil {
						t.Fatal("cancelled cold fetch returned a result")
					}
				} else if !batch {
					assertSingleFetchUnavailable(t, s, reply)
				} else {
					if reply.Error != nil {
						t.Fatalf("scope denial blocked on startup: %v", reply.Error)
					}
					var result batchFetchResponse
					if err := json.Unmarshal([]byte(coldFetchWireText(t, reply.Result)), &result); err != nil {
						t.Fatal(err)
					}
					if len(result.Results) != 1 || result.Results[0].Status != "unavailable" || result.Results[0].Text != "" {
						t.Fatal("cold batch leaked a restricted note")
					}
				}
				if got, err := s.store.Metric("fetches"); err != nil || got != 0 {
					t.Fatalf("denied/cancelled fetch attribution = %d, err = %v", got, err)
				}
			})
		}
	}
}

func TestOtherToolsStillAwaitGraph(t *testing.T) {
	// Derive coverage from the real tool catalogue, not a restated list of tools.
	for _, spec := range ToolSpecs() {
		name, _ := spec["name"].(string)
		if name == "mesh_fetch" || name == "mesh_fetch_many" || name == "mesh_templates" || name == "mesh_note_template" || name == "mesh_block_template" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			s := &Server{ready: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			reply := coldFetchHTTP(t, s, ctx, name, map[string]any{})
			if reply.Error == nil || !strings.Contains(reply.Error.Message, "index still loading") {
				t.Fatalf("tool bypassed graph readiness: %+v", reply)
			}
		})
	}
}

func TestUnknownAndMalformedCallsKeepGraphGate(t *testing.T) {
	for _, raw := range []string{
		`{"name":"mesh_future_tool"}`,
		`{"name":"MESH_FETCH"}`,
		`{"name":7}`,
		`{"name":"mesh_fetch",`,
		`null`,
		`[]`,
		`{}`,
	} {
		if !toolCallNeedsGraph(json.RawMessage(raw)) {
			t.Fatalf("unrecognized call bypasses graph readiness: %s", raw)
		}
	}
}
