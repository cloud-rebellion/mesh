// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Cancel while an existing writer transaction is occupying the queue, after
// discovery/graph work. An uncommitted delta must not poison the next pass's
// cache, and retrying the same file must publish the correct graph and FTS.
func TestReconcileCancellationLeavesCommittedCacheRetryable(t *testing.T) {
	for _, targeted := range []bool{false, true} {
		name := "periodic"
		if targeted {
			name = "targeted"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "note.md")
			body := "---\nid: note\ntype: note\nwhen: 2026-01-01\n---\n# Note\noriginalzeppelinsignal\n"
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			_, notes, err := ReindexFull(store, dir)
			if err != nil {
				t.Fatal(err)
			}
			cache := NewNoteCache()
			cache.Seed(notes)
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "originalzeppelinsignal", "neworbitsignal")), 0600); err != nil {
				t.Fatal(err)
			}
			reconcile := func(ctx context.Context) (Reconciliation, error) {
				if targeted {
					return ReconcilePathsContext(ctx, store, dir, cache, []string{path})
				}
				return ReconcileIncrementalContext(ctx, store, dir, cache, false)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			writerDone := make(chan error, 1)
			go func() { writerDone <- store.Write(func(*sql.Tx) error { close(entered); <-release; return nil }) }()
			<-entered
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			_, err = reconcile(ctx)
			cancel()
			close(release)
			if blockerErr := <-writerDone; blockerErr != nil {
				t.Fatal(blockerErr)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("blocked reconcile did not cancel: %v", err)
			}
			if snapshot := cache.Snapshot(); len(snapshot) != 1 || !strings.Contains(snapshot[0].Body, "originalzeppelinsignal") {
				t.Fatal("uncommitted reconcile advanced the cache")
			}
			rec, err := reconcile(context.Background())
			if err != nil || !rec.Reindexed || rec.Changed != 1 {
				t.Fatalf("same-file retry did not commit: %+v %v", rec, err)
			}
			if snapshot := cache.Snapshot(); len(snapshot) != 1 || !strings.Contains(snapshot[0].Body, "neworbitsignal") {
				t.Fatal("committed retry failed to publish cache")
			}
			hits, err := store.Search(context.Background(), "neworbitsignal", 2)
			if err != nil || len(hits) == 0 {
				t.Fatalf("committed retry absent from FTS: %v %v", hits, err)
			}
		})
	}
}
