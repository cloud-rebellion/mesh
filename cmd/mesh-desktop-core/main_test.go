// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/desktop"
)

func TestDesktopCommandVersionAndPrivateStdioLifecycle(t *testing.T) {
	t.Setenv("MESH_NO_UPDATE_CHECK", "1")
	beforeVersion, beforeSource := desktop.Version, desktop.Source
	desktop.Version = "v0.42.13-test"
	desktop.Source = strings.Repeat("a", 40)
	defer func() { desktop.Version = beforeVersion; desktop.Source = beforeSource }()
	var version bytes.Buffer
	if err := run([]string{"--version", "--json"}, strings.NewReader(""), &version, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var identity map[string]any
	if json.Unmarshal(version.Bytes(), &identity) != nil || len(identity) != 5 || identity["source"] != desktop.Source || identity["platform"] != runtime.GOOS || identity["arch"] != runtime.GOARCH || identity["protocol"] != float64(1) {
		t.Fatal("compiled source/protocol identity changed")
	}
	root := filepath.Join(t.TempDir(), "new-vault")
	requests := "{\"protocol\":1,\"id\":1,\"method\":\"init\",\"params\":{\"name\":\"Command fixture\",\"for_join\":true}}\n{\"protocol\":1,\"id\":2,\"method\":\"close\",\"params\":{}}\n"
	var output, diagnostic bytes.Buffer
	if err := run([]string{"--stdio", "--vault", root}, strings.NewReader(requests), &output, &diagnostic); err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(rows) != 2 {
		t.Fatal("stdout mixed diagnostic output")
	}
	for i, row := range rows {
		var response desktop.Response
		if json.Unmarshal([]byte(row), &response) != nil || response.ID != int64(i+1) || response.Error != nil {
			t.Fatalf("normal application command failed: %s", row)
		}
	}
	if strings.Contains(output.String(), root) {
		t.Fatal("stdio exposed main-selected filesystem path")
	}
	for _, args := range [][]string{{"--vault", root}, {"--stdio", "--vault", "relative"}, {"--version", "--json", "--vault", root}, {"--stdio", "--vault", root, "--token", "synthetic-secret"}} {
		var rejected bytes.Buffer
		if err := run(args, strings.NewReader(""), &rejected, &bytes.Buffer{}); err == nil {
			t.Fatal("unsupported launch mode admitted")
		}
		if rejected.Len() != 0 {
			t.Fatal("rejected launch emitted protocol data")
		}
	}
}

func TestDesktopCommandFreshPublishSearchWithSanitizedNativeEnvironment(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("MESH_RERANK_AGENT", "")
	t.Setenv("MESH_RERANK_ENDPOINT", "")
	t.Setenv("MESH_RERANK_MODEL", "")
	t.Setenv("MESH_EMBED_ENDPOINT", "")
	t.Setenv("MESH_NO_UPDATE_CHECK", "1")
	root := filepath.Join(t.TempDir(), "native-offline-vault")
	sections := map[string]string{"what_happened": "Synthetic desktop fixture.", "impact": "No production effect.", "timeline": "Unknown.", "root_cause": "Unknown.", "resolution": "Unverified.", "follow_up": "This is an offline transport fixture."}
	requests := []map[string]any{
		{"protocol": 1, "id": 1, "method": "init", "params": map[string]any{"name": "Sanitized offline fixture"}},
		{"protocol": 1, "id": 2, "method": "tool", "params": map[string]any{"name": "mesh_author_note", "arguments": map[string]any{"action": "publish", "note": map[string]any{"title": "Desktop fixture engineering incident", "template": "post-mortem", "summary": "Illustrative offline integration fixture.", "sections": sections, "tags": []string{"fixture", "desktop"}}}}},
		{"protocol": 1, "id": 3, "method": "tool", "params": map[string]any{"name": "mesh_search", "arguments": map[string]any{"query": "Desktop fixture engineering incident", "limit": 5, "budget": 2000}}},
		{"protocol": 1, "id": 4, "method": "close", "params": map[string]any{}},
	}
	var input, output, diagnostics bytes.Buffer
	for _, r := range requests {
		if json.NewEncoder(&input).Encode(r) != nil {
			t.Fatal("fixture request")
		}
	}
	if err := run([]string{"--stdio", "--vault", root}, &input, &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(rows) != 4 {
		t.Fatal("framed lifecycle missing response")
	}
	for _, raw := range rows {
		var response desktop.Response
		if json.Unmarshal([]byte(raw), &response) != nil || response.Error != nil {
			t.Fatalf("fresh native search refused: %s", raw)
		}
	}
	var search struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(rows[2]), &search) != nil || len(search.Result.Content) != 1 || !strings.Contains(search.Result.Content[0].Text, `"NoteID":"desktop-fixture-engineering-incident"`) {
		t.Fatal("freshly published note is not searchable offline")
	}
	if strings.Contains(output.String(), root) || strings.Contains(diagnostics.String(), "$HOME") {
		t.Fatal("native profile isolation failed")
	}
	// Reopen an existing native vault with unusable prior provider configuration.
	// Both lexical search and structured editing must work without rewriting it.
	cfg := []byte("[embedding]\nendpoint='https://provider.invalid'\nmodel='offline-provider'\ndim=2\n[rerank]\nendpoint='https://provider.invalid'\nmodel='offline-provider'\n")
	cfgPath := filepath.Join(root, ".mesh", "config.toml")
	if os.WriteFile(cfgPath, cfg, 0600) != nil {
		t.Fatal("prior provider config")
	}
	input.Reset()
	output.Reset()
	diagnostics.Reset()
	for _, r := range []map[string]any{requests[2], {"protocol": 1, "id": 5, "method": "tool", "params": map[string]any{"name": "mesh_prepare_update", "arguments": map[string]any{"id": "desktop-fixture-engineering-incident"}}}, requests[3]} {
		if json.NewEncoder(&input).Encode(r) != nil {
			t.Fatal("reopen fixture")
		}
	}
	if err := run([]string{"--stdio", "--vault", root}, &input, &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "desktop-fixture-engineering-incident") || strings.Contains(output.String(), `"error"`) {
		t.Fatal("configured vault could not search/prepare offline")
	}
	if before, err := os.ReadFile(cfgPath); err != nil || !bytes.Equal(before, cfg) {
		t.Fatal("offline constructor rewrote provider config")
	}
}
