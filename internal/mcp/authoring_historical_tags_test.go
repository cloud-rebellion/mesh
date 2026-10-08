// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoricalTagsMCPPrepareValidatePublishAndAccess(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	first := publishUpdateFixture(t, s, "Historical OAuth finding")
	path := filepath.Join(s.vaultRoot, first["path"].(string))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	original = []byte(strings.Replace(string(original), "    - schema", "    - oauth-2.1", 1))
	if !strings.Contains(string(original), "oauth-2.1") {
		t.Fatal("fixture tag substitution failed")
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	ctx := WithLocalOperator(context.Background())
	prepared, rerr := s.toolPrepareUpdate(ctx, mustJSON(map[string]any{"id": first["id"]}))
	if rerr != nil {
		t.Fatal(rerr)
	}
	note := toolJSON(t, prepared)["note"].(map[string]any)
	note["summary"] = "The historical OAuth finding has a clearer explanation; no live verification occurred."
	for _, action := range []string{"prepare", "validate"} {
		out, rerr := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": action, "note": note}))
		if rerr != nil {
			t.Fatalf("%s: %v", action, rerr)
		}
		if action == "validate" && toolJSON(t, out)["valid"] != true {
			t.Fatal("unchanged historical tag failed validation")
		}
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != string(original) {
		t.Fatal("prepare/validate wrote source")
	}
	for _, tags := range [][]string{{"oauth-2.2"}, {"oauth-2.1", "added-tag"}, {"[[oauth-2.1]]"}, {"notes/oauth-2.1"}} {
		changed := map[string]any{}
		for k, v := range note {
			changed[k] = v
		}
		changed["tags"] = tags
		if _, rerr := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": changed})); rerr == nil {
			t.Fatal("changed invalid tags published")
		}
	}
	for _, ref := range []string{"../outside", "notes/other.md", "[[other-note]]", "missing-note#evidence"} {
		changed := map[string]any{}
		for k, v := range note {
			changed[k] = v
		}
		changed["related"] = []string{ref}
		if _, rerr := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": changed})); rerr == nil {
			t.Fatalf("historical tag compatibility admitted malformed/missing reference %q", ref)
		}
	}
	forged := map[string]any{}
	for k, v := range note {
		forged[k] = v
	}
	forged["historical_tags"] = []string{"oauth-2.1"}
	if _, rerr := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": forged})); rerr == nil {
		t.Fatal("client provided private compatibility state")
	}
	viewer := WithWriteCapability(ctx, false)
	if _, rerr := s.toolAuthorNote(viewer, mustJSON(map[string]any{"action": "publish", "note": note})); rerr == nil {
		t.Fatal("tag compatibility bypassed viewer refusal")
	}
	if _, rerr := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": note})); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": note})); rerr == nil {
		t.Fatal("stale dotted-tag edit accepted")
	}
	prepared, rerr = s.toolPrepareUpdate(ctx, mustJSON(map[string]any{"id": first["id"]}))
	if rerr != nil {
		t.Fatal(rerr)
	}
	note = toolJSON(t, prepared)["note"].(map[string]any)
	current, _ := os.ReadFile(path)
	current = []byte(strings.Replace(string(current), "---\n", "---\nscope: [finance]\n", 1))
	if err := os.WriteFile(path, current, 0600); err != nil {
		t.Fatal(err)
	}
	limited := WithScopeFilter(WithWriteCapability(context.Background(), true), &ScopeFilter{AllowedRead: map[string]bool{"dev": true}, WriteScope: "dev", CanWrite: func(scope string) bool { return scope == "dev" }})
	if _, rerr := s.toolAuthorNote(limited, mustJSON(map[string]any{"action": "publish", "note": note})); rerr == nil {
		t.Fatal("tag compatibility bypassed fresh source audience")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(current) {
		t.Fatal("refused source audience edit mutated bytes")
	}
}
