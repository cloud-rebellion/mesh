// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/onboarding"
	"github.com/bright-interaction/mesh/internal/rerank"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)
	srv, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// seedVaultFiles lays out the standard two-note fixture vault (markdown only, no index).
func seedVaultFiles(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("decisions/sqlite.md", "---\nid: sqlite\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: use modernc sqlite for storage\n---\n# Storage\n")
	write("note.md", "---\nid: note\ntype: note\nwhen: 2026-01-01\n---\n# Note\nmarketing copy\n")
}

// seedIndex plays the part of the single owning writer: it builds the index once and
// closes, so a read-only NewServer has something to read. Production has the same
// ordering (`mesh watch` / `mesh sync --watch` owns the index; every `mesh mcp` window
// reads it), which is why the tests do it this way rather than handing the server a
// writable store it would never get in production.
func seedIndex(t *testing.T, dir string) {
	t.Helper()
	owner, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, _, err := index.ReindexFull(owner, dir); err != nil {
		t.Fatal(err)
	}
}

// seedCodeIndex plays the part of whatever owns the SOURCE-CODE index in production (the
// post-commit git hook's `mesh code reindex`, or the owning writer): it opens the vault's
// index writable, indexes the given code roots, and closes. Indexing code is a write, so
// a read-only server can never do it for itself; it reads the result like any other
// reader. Tests that used to call index.ReindexCode on the server's own store were
// exercising a shape production no longer has.
func seedCodeIndex(t *testing.T, vaultRoot string, codeRoots ...string) {
	t.Helper()
	owner, err := index.Open(vaultRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := index.ReindexCode(owner, codeRoots, nil); err != nil {
		t.Fatal(err)
	}
}

// call dispatches as the LOCAL STDIO transport does (ServeStdio marks its context with
// WithLocalOperator), so these tests exercise the same trust level a `mesh mcp` session
// has: local-only tools are reachable and the remote reindex cooldown does not apply.
func call(t *testing.T, s *Server, method string, params any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(params)
	res, rerr := s.dispatch(WithLocalOperator(context.Background()), request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: method, Params: raw})
	if rerr != nil {
		t.Fatalf("%s: rpc error %d %s", method, rerr.Code, rerr.Message)
	}
	m, _ := res.(map[string]any)
	return m
}

// toolText pulls the JSON text out of an MCP tool content result.
func toolText(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	content, ok := res["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content in tool result: %v", res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(content[0]["text"].(string)), &out); err != nil {
		t.Fatalf("tool text not json: %v", err)
	}
	return out
}

func TestInitializeAndToolsList(t *testing.T) {
	s := newTestServer(t)
	init := call(t, s, "initialize", map[string]any{})
	if init["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v", init["protocolVersion"])
	}
	if init["instructions"] == "" {
		t.Error("expected instructions (the contract)")
	}
	list := call(t, s, "tools/list", map[string]any{})
	tools, _ := list["tools"].([]map[string]any)
	if len(tools) != len(ToolNames()) {
		t.Errorf("listed %d tools, registered %d", len(tools), len(ToolNames()))
	}
}

func TestInitializeDeliversMCPOnboardingOnceToLocalStdio(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newTestServer(t)
	if err := onboarding.SetPending(s.vaultRoot, "codex"); err != nil {
		t.Fatal(err)
	}
	params := mustJSON(map[string]any{"clientInfo": map[string]any{"name": "codex"}})

	// HTTP/hosted dispatches are not trusted local operators: they must neither see
	// nor consume a marker stored on the server's machine.
	remote, rerr := s.dispatch(context.Background(), request{Method: "initialize", Params: params})
	if rerr != nil {
		t.Fatal(rerr)
	}
	remoteInstructions := remote.(map[string]any)["instructions"].(string)
	if remoteInstructions != contractText {
		t.Fatalf("remote initialize received local onboarding:\n%s", remoteInstructions)
	}

	local, rerr := s.dispatch(WithLocalOperator(context.Background()), request{Method: "initialize", Params: params})
	if rerr != nil {
		t.Fatal(rerr)
	}
	instructions := local.(map[string]any)["instructions"].(string)
	for _, want := range []string{"FIRST MESH SESSION", "shared knowledge vault", "zero-model by default", "--client codex", "Luna, low effort", contractText} {
		if !strings.Contains(instructions, want) {
			t.Errorf("one-time instructions missing %q:\n%s", want, instructions)
		}
	}
	if len(instructions) > 2_000 {
		t.Errorf("one-time instructions = %d bytes, budget is 2000", len(instructions))
	}
	// Codex may emphasize only the beginning of server instructions. Keep the first
	// 512 bytes independently useful: identity, workflow, tour choice and no-delay
	// guard all land before optional provider detail.
	head := instructions
	if len(head) > 512 {
		head = head[:512]
	}
	for _, want := range []string{"FIRST MESH SESSION", "shared knowledge vault", "60-second tour", "Do not delay their task", "zero-model by default"} {
		if !strings.Contains(head, want) {
			t.Errorf("first 512 onboarding bytes are not self-contained; missing %q:\n%s", want, head)
		}
	}

	again, rerr := s.dispatch(WithLocalOperator(context.Background()), request{Method: "initialize", Params: params})
	if rerr != nil {
		t.Fatal(rerr)
	}
	if got := again.(map[string]any)["instructions"].(string); got != contractText {
		t.Fatalf("second initialize repeated onboarding:\n%s", got)
	}
}

func TestInitializeOnboardingReportsEnabledSubscriptionWithoutCallingIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newTestServer(t)
	if _, _, err := rerank.SaveLocalSubscription(s.vaultRoot, rerank.SubscriptionConfig{
		Agent: "codex", Model: "gpt-5.6-luna", Policy: "auto",
	}); err != nil {
		t.Fatal(err)
	}
	if err := onboarding.SetPending(s.vaultRoot, "vscode"); err != nil {
		t.Fatal(err)
	}
	init := call(t, s, "initialize", map[string]any{})
	instructions := init["instructions"].(string)
	if !strings.Contains(instructions, "already enabled with codex/gpt-5.6-luna at low effort") {
		t.Fatalf("enabled setup not reported accurately:\n%s", instructions)
	}
	if strings.Contains(instructions, "--agent codex") {
		t.Fatalf("enabled setup was presented as an opt-in:\n%s", instructions)
	}
}

// TestEveryToolIsScopeClassified is the durable guard against the recurring
// "a new read tool leaks across scopes by default" bug: every tool in ToolSpecs() must
// have a scope class in toolScopeClass, and every class entry must be a real tool. A new
// tool added without a class fails this test (and is refused at runtime), forcing a
// conscious scope decision before it can ship.
func TestEveryToolIsScopeClassified(t *testing.T) {
	specNames := map[string]bool{}
	for _, sp := range ToolSpecs() {
		name, _ := sp["name"].(string)
		if name == "" {
			t.Fatalf("a tool spec has no name: %v", sp)
		}
		specNames[name] = true
		if _, ok := toolScopeClass[name]; !ok {
			t.Errorf("tool %q is in ToolSpecs() but has no scope class in toolScopeClass; classify it (classFiltered/classCodeDev/classWrite/classOpen)", name)
		}
	}
	for name := range toolScopeClass {
		if !specNames[name] {
			t.Errorf("toolScopeClass has %q but ToolSpecs() does not; drop the stale classification", name)
		}
	}
}

func TestToolSearchFused(t *testing.T) {
	s := newTestServer(t)
	res, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_search", "arguments": map[string]any{"query": "sqlite storage"}}),
	})
	if rerr != nil {
		t.Fatalf("rpc error: %v", rerr)
	}
	out := toolText(t, res.(map[string]any))
	cards, _ := out["cards"].([]any)
	if len(cards) == 0 {
		t.Fatal("expected cards")
	}
	first, _ := cards[0].(map[string]any)
	if first["NoteID"] != "sqlite" {
		t.Errorf("top card = %v, want sqlite", first["NoteID"])
	}
}

func TestToolWriteBackReindexes(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot) // the owning writer is what indexes a write-back now
	res, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_append_note", "arguments": map[string]any{
			"type": "gotcha", "title": "Vec extensions unavailable",
			"summary": "Use flat cosine because modernc has no C extensions; vec0 is unavailable.", "sections": fixtureSections("gotcha"),
		}}),
	})
	if rerr != nil {
		t.Fatalf("write rpc error: %v", rerr)
	}
	w := toolText(t, res.(map[string]any))
	if w["id"] != "vec-extensions-unavailable" {
		t.Errorf("write id = %v", w["id"])
	}
	// The new gotcha must be immediately retrievable (reload worked).
	sr, _ := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_search", "arguments": map[string]any{"query": "vec extensions modernc"}}),
	})
	out := toolText(t, sr.(map[string]any))
	cards, _ := out["cards"].([]any)
	var found bool
	for _, c := range cards {
		if cm, _ := c.(map[string]any); cm["NoteID"] == "vec-extensions-unavailable" {
			found = true
		}
	}
	if !found {
		t.Errorf("written gotcha not retrievable after reload: %v", cards)
	}
}

func TestToolWriteRecordsProvenance(t *testing.T) {
	s := newTestServer(t)
	// initialize first so the server learns the calling agent's name.
	if _, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize",
		Params: mustJSON(map[string]any{"clientInfo": map[string]any{"name": "claude-code"}}),
	}); rerr != nil {
		t.Fatalf("initialize: %v", rerr)
	}
	res, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_append_note", "arguments": map[string]any{
			"type": "decision", "title": "Prov check note",
			"summary": "Stamp provenance so audit and lifecycle retain authorship.", "sections": fixtureSections("decision"),
			"confidence": "high", "review_by": "2027-01-01",
		}}),
	})
	if rerr != nil {
		t.Fatalf("write rpc error: %v", rerr)
	}
	w := toolText(t, res.(map[string]any))
	// path is now vault-relative (absolute paths must not leak to the agent),
	// so join it back onto the vault root to read the file.
	b, err := os.ReadFile(filepath.Join(s.vaultRoot, w["path"].(string)))
	if err != nil {
		t.Fatalf("read written note: %v", err)
	}
	body := string(b)
	for _, want := range []string{"agent: claude-code", "source: agent", "confidence: high", "review_by:", "2027-01-01"} {
		if !strings.Contains(body, want) {
			t.Errorf("written note missing %q:\n%s", want, body)
		}
	}
}

func TestHandleHTTPMatchesDispatch(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(http.HandlerFunc(s.HandleHTTP))
	defer ts.Close()

	body := mustJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
		Error *map[string]any `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error != nil {
		t.Fatalf("rpc error: %v", *out.Error)
	}
	if len(out.Result.Tools) != len(ToolNames()) {
		t.Fatalf("tools over HTTP = %d, want %d", len(out.Result.Tools), len(ToolNames()))
	}

	// A tools/call over HTTP returns the same shape as stdio.
	call := mustJSON(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "mesh_search", "arguments": map[string]any{"query": "sqlite"}}})
	r2, err := http.Post(ts.URL, "application/json", bytes.NewReader(call))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("tools/call over HTTP status = %d", r2.StatusCode)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// TestReindexPicksUpDirectFileEdit covers the IDE/CLI collaboration loop: an agent
// edits a note file directly (not via mesh_append_note), so the running server is
// stale until mesh_reindex forces a re-read. After reindex the new note is queryable.
//
// Under the single-writer split this is now an end-to-end test of the whole architecture,
// because this server cannot index anything: the file must reach the index through the
// owning writer, and mesh_reindex must wait for that rather than swapping in a snapshot
// that does not have it yet. The owner is deliberately started AFTER the staleness check,
// so "the server has not seen the file" is a fact about the index and not a race with the
// owner's debounce.
func TestReindexPicksUpDirectFileEdit(t *testing.T) {
	s := newTestServer(t)
	dir := s.vaultRoot

	// A brand-new note written straight to disk, as the Edit tool would.
	if err := os.WriteFile(filepath.Join(dir, "decisions", "hnsw.md"),
		[]byte("---\nid: hnsw\ntype: decision\nwhen: 2026-02-02\ndo: gate it\ndont: always build\nwhy: pure-go hnsw ann index for large vault retrieval\n---\n# HNSW\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	// Before reindex: the server has not seen the file.
	out := toolText(t, call(t, s, "tools/call", map[string]any{
		"name": "mesh_search", "arguments": map[string]any{"query": "hnsw ann index"}}))
	for _, c := range asCards(out) {
		if c["NoteID"] == "hnsw" {
			t.Fatal("server should be stale before mesh_reindex")
		}
	}

	// The owning writer comes up and takes the file, as `mesh watch` does in production.
	startOwner(t, dir)

	// Reindex, then it must be found.
	r := toolText(t, call(t, s, "tools/call", map[string]any{"name": "mesh_reindex", "arguments": map[string]any{}}))
	if r["index_stale"] == true {
		t.Fatalf("reindex reported a stale index with the owner running: %v", r)
	}
	if r["reindexed"] != true || r["added"].(float64) != 1 {
		t.Fatalf("reindex should report 1 added, got %v", r)
	}
	out = toolText(t, call(t, s, "tools/call", map[string]any{
		"name": "mesh_search", "arguments": map[string]any{"query": "hnsw ann index"}}))
	found := false
	for _, c := range asCards(out) {
		if c["NoteID"] == "hnsw" {
			found = true
		}
	}
	if !found {
		t.Fatal("note must be retrievable after mesh_reindex")
	}

	// Idempotent: a second reindex with no change reports nothing added.
	r2 := toolText(t, call(t, s, "tools/call", map[string]any{"name": "mesh_reindex", "arguments": map[string]any{}}))
	if r2["added"].(float64) != 0 || r2["changed"].(float64) != 0 || r2["removed"].(float64) != 0 {
		t.Fatalf("second reindex should be a no-op, got %v", r2)
	}
}

func asCards(out map[string]any) []map[string]any {
	raw, _ := out["cards"].([]any)
	var cards []map[string]any
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			cards = append(cards, m)
		}
	}
	return cards
}
