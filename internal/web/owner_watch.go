// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/watch"
)

const ownerVaultReconcileInterval = 8 * time.Second

// The existing UI owner also observes hub/peer filesystem writes. Never open a
// second Store or owner: publication shares the manual reindex/promotion gate.
type ownerVaultWatchPolicy struct {
	reconcile time.Duration
	full      time.Duration
	run       func(context.Context, watch.Options) error
}

func (s *Server) startOwnerVaultWatch(parent context.Context) {
	s.startOwnerVaultWatchPolicy(parent, ownerVaultWatchPolicy{
		reconcile: ownerVaultReconcileInterval, full: watch.DefaultFullReconcile, run: watch.Run,
	})
}

// The private policy seam makes lost-event and FD-failure recovery testable
// without a five-minute wait or exhausting the machine's descriptor limit.
func (s *Server) startOwnerVaultWatchPolicy(parent context.Context, policy ownerVaultWatchPolicy) {
	ctx, cancel := context.WithCancel(parent)
	s.ownerWatchCancel = cancel
	s.ownerWatchDone = make(chan struct{})
	go func() {
		defer close(s.ownerWatchDone)
		options := watch.Options{
			Root: s.vaultRoot, Reconcile: policy.reconcile,
			FullReconcile: policy.full,
			OnReindex: func(p watch.Pass) (watch.Result, error) {
				result, err := s.reconcileOwnerVault(ctx, p)
				if err != nil && ctx.Err() == nil {
					slog.Warn("mesh ui vault reconciliation failed", "reason", p.Reason, "error", err)
				}
				return result, err
			},
			Logf: func(format string, args ...any) {
				slog.Debug("mesh ui vault watch", "event", format, "args", args)
			},
		}
		err := policy.run(ctx, options)
		if ctx.Err() != nil {
			return
		}
		// FD exhaustion or a closed event stream must not silently disable
		// convergence. Retain the periodic safety net without fsnotify.
		slog.Warn("mesh ui filesystem watch stopped; using periodic reconciliation", "error", err)
		ticker := time.NewTicker(policy.reconcile)
		defer ticker.Stop()
		lastFull := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				authoritative := time.Since(lastFull) >= policy.full
				if _, err := options.OnReindex(watch.Pass{Reason: watch.ReasonTick, Authoritative: authoritative}); err != nil {
					if ctx.Err() == nil {
						slog.Warn("mesh ui periodic reconciliation failed", "error", err)
					}
				} else if authoritative {
					lastFull = time.Now()
				}
			}
		}
	}()
}

func (s *Server) reconcileOwnerVault(ctx context.Context, p watch.Pass) (watch.Result, error) {
	release, err := s.acquireGraphUpdate(ctx)
	if err != nil {
		return watch.Result{}, err
	}
	defer release()
	if s.viewClosed || s.owner == nil {
		return watch.Result{}, errors.New("vault reconciliation requires the current UI owner")
	}
	var rec index.Reconciliation
	if s.ownerNotes == nil {
		start := time.Now()
		g, notes, err := index.ReindexFullContext(ctx, s.store, s.vaultRoot)
		if err != nil {
			return watch.Result{}, err
		}
		s.ownerNotes = index.NewNoteCache()
		s.ownerNotes.Seed(notes)
		rec = index.Reconciliation{Reindexed: true, Graph: g, Dur: time.Since(start)}
	} else if len(p.Paths) > 0 {
		rec, err = index.ReconcilePathsContext(ctx, s.store, s.vaultRoot, s.ownerNotes, p.Paths)
	} else {
		// Directory creation can coalesce an early file event into a full discovery
		// pass. Check content for such observed changes: a later edit in that same
		// second may share the indexed mtime and have no individual file event.
		// Only ordinary safety ticks use the inexpensive mtime scan.
		mtimeFast := !p.Authoritative && p.Reason == watch.ReasonTick
		rec, err = index.ReconcileIncrementalContext(ctx, s.store, s.vaultRoot, s.ownerNotes, mtimeFast)
	}
	if err != nil {
		return watch.Result{}, err
	}
	if rec.Reindexed {
		s.publishGraph(rec.Graph) // includes retriever/vector cache invalidation
	}
	return watch.Result{Added: rec.Added, Changed: rec.Changed, Removed: rec.Removed, Reindexed: rec.Reindexed, Dur: rec.Dur}, nil
}
