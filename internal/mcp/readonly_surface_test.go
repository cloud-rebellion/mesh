// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
)

// TestNewServerOpensReadOnly is the regression guard for the whole refactor. The per-window
// server MUST NOT hold a writable store: that is the single fact steps 1 to 3 are built
// on, and it is one character away from being undone by a future "fix" for a test that
// fails because something wants to write. If this ever fails, the WAL contention comes
// back and every symptom in the multi-writer post-mortems comes back with it.
func TestNewServerOpensReadOnly(t *testing.T) {
	s := newTestServer(t)
	if !s.store.ReadOnly() {
		t.Fatal("mesh mcp opened a WRITABLE store: N windows can now fight each other and the owner for the SQLite write lock")
	}
	// The hub is the deliberate exception: it OWNS the index it serves.
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)
	hub, err := NewServerAt(dir, dir+"/.mesh")
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	if hub.store.ReadOnly() {
		t.Fatal("the hub lost its writable store; it owns its index and must keep indexing it")
	}
}

// TestEveryToolSurvivesOnAReadOnlyServer is the durable guard against the class of bug
// steps 1 to 3 kept finding one at a time: a tool that looks like a read but writes
// somewhere down its call stack, which on a read-only store fails the whole call with an
// opaque "internal error". Four surfaces did this (startup enrichment, write-back, the
// health pass, the code index), and each was found only when something exercised it.
//
// Every tool in ToolSpecs() must have an entry here, so a new one cannot ship without
// someone deciding what it does on the server every agent actually connects to.
func TestEveryToolSurvivesOnAReadOnlyServer(t *testing.T) {
	s := newTestServer(t)
	editable, err := vault.CreateNote(s.vaultRoot, vault.NewNoteSpec{Template: "finding", Title: "Existing published edit fixture", Summary: "A fixture for preparation from a read-only index window.", Sections: fixtureSections("note")})
	if err != nil {
		t.Fatal(err)
	}
	seedIndex(t, s.vaultRoot)
	// No owner is running, so the two tools that wait for one would otherwise sit on the
	// full 10s bound. They are still expected to SUCCEED (loudly, with owner_down).
	s.ownerIndexTimeout = 200 * time.Millisecond

	args := map[string]any{
		"mesh_prepare_update":   map[string]any{"id": editable.ID},
		"mesh_search":           map[string]any{"query": "storage"},
		"mesh_fetch":            map[string]any{"id": "sqlite"},
		"mesh_fetch_many":       map[string]any{"items": []map[string]any{{"id": "sqlite"}}},
		"mesh_god_nodes":        map[string]any{},
		"mesh_changed_since":    map[string]any{"since": 0},
		"mesh_neighbors":        map[string]any{"id": "sqlite"},
		"mesh_community":        map[string]any{},
		"mesh_append_note":      map[string]any{"type": "gotcha", "title": "a read only window still writes back", "summary": "A reader delegates indexing after a durable note write.", "sections": fixtureSections("gotcha")},
		"mesh_write_entity":     map[string]any{"title": "Read Only Surface Probe", "summary": "An entity write from a read-only index window.", "sections": fixtureSections("entity")},
		"mesh_templates":        map[string]any{},
		"mesh_note_template":    map[string]any{"template": "finding"},
		"mesh_block_template":   map[string]any{"template": "table"},
		"mesh_drafts":           map[string]any{},
		"mesh_author_note":      map[string]any{"action": "prepare", "note": fixtureNote("Read-only preparation")},
		"mesh_propose_template": map[string]any{"kind": "template", "title": "A proposal", "rationale": "Fixture proposal for review", "definition": "Proposed fixed headings", "example": "Illustrative example"},
		"mesh_reindex":          map[string]any{},
		"mesh_health":           map[string]any{},
		"mesh_code_search":      map[string]any{"query": "Store"},
		"mesh_code_neighbors":   map[string]any{"id": "code:x/x.go#Nope"},
		"mesh_code_context":     map[string]any{"query": "Store"},
		"mesh_secret_status":    map[string]any{},
		"mesh_secret_list":      map[string]any{},
		"mesh_secret_use":       map[string]any{"destination": "api.example.com/v1/thing"},
		// status only: action=install would write .claude/settings.json for real.
		"mesh_setup_hooks": map[string]any{"action": "status", "project_dir": t.TempDir()},
	}
	for _, sp := range ToolSpecs() {
		name, _ := sp["name"].(string)
		if _, ok := args[name]; !ok {
			t.Fatalf("tool %q has no entry here: decide what it does on a read-only server (the one every agent connects to) before shipping it", name)
		}
	}

	for name, a := range args {
		t.Run(name, func(t *testing.T) {
			params, _ := json.Marshal(map[string]any{"name": name, "arguments": a})
			_, rerr := s.handleToolsCall(WithLocalOperator(context.Background()), params)
			if rerr == nil {
				return
			}
			// codeInvalidParams is a deliberate, operator-authored refusal (a missing
			// credential, an unknown id). codeInternalError is what a swallowed write
			// failure looks like from the outside, and is exactly what must not happen.
			if rerr.Code == codeInternalError {
				t.Fatalf("%s failed with an internal error on a read-only server (the shape a write down the call stack takes): %+v", name, rerr)
			}
			t.Logf("%s refused with %d: %s (a deliberate refusal, not a write failure)", name, rerr.Code, rerr.Message)
		})
	}
}
