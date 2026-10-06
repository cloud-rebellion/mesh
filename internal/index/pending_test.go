// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

func TestPendingQueueCapPreservesUnreviewedWork(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < pendingQueueCap; i++ {
		if err := store.AddPending(PendingNote{Type: "gotcha", Title: fmt.Sprintf("candidate number %d", i), Why: "original unreviewed evidence", CreatedAt: int64(1000 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddPending(PendingNote{Type: "note", Title: "overflow"}); !errors.Is(err, ErrPendingQueueFull) {
		t.Fatalf("overflow=%v", err)
	}
	oldest, err := store.GetPending(PendingID("gotcha", "candidate number 0"))
	if err != nil || oldest.Why != "original unreviewed evidence" {
		t.Fatalf("old work lost: %+v %v", oldest, err)
	}
	oldest.Why = "reviewed correction"
	if err := store.AddPending(oldest); err != nil {
		t.Fatalf("update at capacity: %v", err)
	}
	count, _ := store.PendingCount()
	if count != pendingQueueCap {
		t.Fatalf("count=%d", count)
	}
}

func TestPendingStructuredPayloadRoundTrip(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := PendingNote{Type: "note", Title: "Bounded lookup preserves evidence", Template: "finding", TemplateVersion: 1, Summary: "Observed current lookup behavior.", Sections: map[string]string{"question": "What was checked?", "findings": "Only the bounded path was checked."}, Tags: []string{"retrieval"}, Collections: []string{"mesh"}, Related: []string{"mesh"}, Supersedes: []string{"old-result"}, Blocks: []vault.BlockSpec{{Template: "evidence", Version: 1, ID: "probe", Fields: map[string]string{"claim": "A bounded probe succeeded."}}}}
	if err := store.AddPending(p); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetPending(PendingID(p.Type, p.Title))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.payload(), p.payload()) {
		t.Fatalf("payload lost fields: %+v", got)
	}
	if _, err := got.AuthoringSpec(); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListPending()
	if err != nil || len(items) != 1 || !reflect.DeepEqual(items[0].payload(), p.payload()) {
		t.Fatalf("list=%+v %v", items, err)
	}
}

func TestPendingHistoricalRowsRequireExplicitRewrite(t *testing.T) {
	p := PendingNote{Type: "gotcha", Title: "Historical draft", Do: "original action", Dont: "original risk", Why: "original evidence"}
	if _, err := p.AuthoringSpec(); err == nil {
		t.Fatal("historical shorthand silently promoted")
	}
	p.Template = "troubleshooting"
	p.TemplateVersion = 1
	if _, err := p.AuthoringSpec(); err == nil {
		t.Fatal("template stamp silently discarded historical shorthand")
	}
}

func TestPendingSchemaMigrationPreservesHistoricalRows(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	oldSchema := strings.Replace(SchemaSQL, "  created_at INTEGER NOT NULL,\n  authoring_json TEXT NOT NULL DEFAULT '' -- structured draft; empty retains historical fields", "  created_at INTEGER NOT NULL", 1)
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES('schema_version','7'),('keep_shape_version','1'); INSERT INTO pending_notes(id,type,title,do_text,dont_text,why,confidence,source,created_at) VALUES('historical','gotcha','Original draft','act','risk','evidence','high','session',1)`); err != nil {
		t.Fatal(err)
	}
	if err := ensureSchemaContext(context.Background(), db, true, true); err != nil {
		t.Fatal(err)
	}
	var action, risk, evidence, payload string
	if err := db.QueryRow(`SELECT do_text,dont_text,why,authoring_json FROM pending_notes WHERE id='historical'`).Scan(&action, &risk, &evidence, &payload); err != nil {
		t.Fatal(err)
	}
	if action != "act" || risk != "risk" || evidence != "evidence" || payload != "" {
		t.Fatalf("migration rewrote history: %q %q %q %q", action, risk, evidence, payload)
	}
	if err := ensureSchemaContext(context.Background(), db, true, true); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
}
