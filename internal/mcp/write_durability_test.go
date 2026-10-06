// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A failed index refresh must not be reported as a failed write.
//
// mesh_append_note used to return -32603 when reload() failed, even though the
// note was already durably on disk. Agents read that as "nothing was written",
// retried, and Mesh minted a near-duplicate with a -2 id suffix. It happened
// three times (2026-07-26, 07-28, 07-30), the last two with a warning note
// already sitting in the vault, which is what settled that a gotcha note does
// not fix a tool that misreports its own outcome.
//
// reload() runs index.ReindexFull, which WRITES to the store, so it fails
// whenever a `mesh sync --watch` daemon holds the write lock. Closing the store
// reproduces that failure deterministically without needing a second process.
func TestWriteSurvivesAFailedIndexRefresh(t *testing.T) {
	s := newTestServer(t)

	// Break the index refresh, and only the index refresh. The vault directory
	// stays writable, so vault.CreateNote still lands on disk.
	if err := s.store.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	res, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_append_note", "arguments": map[string]any{
			"type": "gotcha", "title": "Durable under a broken index",
			"summary": "Report durable publication accurately because retries duplicate notes.", "sections": fixtureSections("gotcha"),
		}}),
	})

	// The whole point: no rpc error, because the write itself succeeded.
	if rerr != nil {
		t.Fatalf("mesh_append_note reported failure on a write that succeeded: %v\n"+
			"This is the bug: the agent retries and creates a duplicate note.", rerr)
	}

	w := toolText(t, res.(map[string]any))

	// The note has to actually be on disk, otherwise reporting success is the
	// opposite bug and strictly worse.
	rel, ok := w["path"].(string)
	if !ok || rel == "" {
		t.Fatalf("no path in the response: %#v", w)
	}
	if _, err := os.Stat(filepath.Join(s.vaultRoot, rel)); err != nil {
		t.Fatalf("reported success but the note is not on disk at %s: %v", rel, err)
	}

	// And the response must say the index is stale, or the agent has no way to
	// know the note is not queryable yet.
	if w["index_stale"] != true {
		t.Errorf("index_stale = %v, want true; without it the agent cannot tell "+
			"the note is unqueryable and will not call mesh_reindex", w["index_stale"])
	}
	if warn, _ := w["warning"].(string); !strings.Contains(warn, "mesh_reindex") {
		t.Errorf("warning does not point at mesh_reindex: %q", warn)
	}
}

// The healthy path must stay silent about staleness, so index_stale keeps
// meaning something when it does show up.
func TestWriteOnAHealthyIndexReportsNoStaleness(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot) // "healthy" now means an owning writer is running
	res, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_append_note", "arguments": map[string]any{
			"type": "gotcha", "title": "Healthy index stays quiet",
			"summary": "Warn only when the index is stale so the receipt remains useful.", "sections": fixtureSections("gotcha"),
		}}),
	})
	if rerr != nil {
		t.Fatalf("write rpc error: %v", rerr)
	}
	w := toolText(t, res.(map[string]any))
	if _, present := w["index_stale"]; present {
		t.Errorf("index_stale is set on a healthy write: %#v", w)
	}
	if _, present := w["warning"]; present {
		t.Errorf("warning is set on a healthy write: %#v", w)
	}
}
