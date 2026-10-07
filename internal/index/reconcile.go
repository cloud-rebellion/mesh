// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/latency"
	"github.com/bright-interaction/mesh/internal/vault"
)

// Reconciliation reports what a Reconcile did: the drift it found and, when it
// rebuilt, the freshly loaded graph so a long-running server can swap it in.
type Reconciliation struct {
	Added     int
	Changed   int
	Removed   int
	Reindexed bool
	Graph     *graph.Graph // non-nil only when Reindexed
	Dur       time.Duration
	// Dropped is how many files this pass could not index (unparseable frontmatter, or
	// a duplicate effective id). They are invisible to search and the graph; the details
	// are on Store.DroppedNotes and feed mesh_health.
	Dropped int
}

// Any reports whether the vault had drifted from the index.
func (r Reconciliation) Any() bool { return r.Added+r.Changed+r.Removed > 0 }

// Reconcile brings the index up to date with the vault on disk, but only does
// the expensive rebuild when retrieval-relevant content actually changed. It
// runs the same content-hash DriftReport `mesh doctor` uses; if a file's mtime
// moved but its retrieval hash did not (a cosmetic edit, a touch), DriftReport
// reports no drift and Reconcile skips the reindex. This is the convergent,
// idempotent core the watcher calls on every file event and on its periodic
// safety tick: run it twice in a row with no edits in between and the second is
// a cheap no-op.
func Reconcile(s *Store, root string) (Reconciliation, error) {
	start := time.Now()
	d, err := s.DriftReport(root)
	if err != nil {
		return Reconciliation{}, err
	}
	r := Reconciliation{Added: len(d.Added), Changed: len(d.Changed), Removed: len(d.Removed)}
	if !d.Any() {
		r.Dur = time.Since(start)
		return r, nil
	}
	g, err := Reindex(s, root)
	if err != nil {
		return Reconciliation{}, err
	}
	r.Reindexed = true
	r.Graph = g
	dropped, err := s.DroppedNotes() // recorded by the ReindexFull inside Reindex
	if err != nil {
		return Reconciliation{}, err
	}
	r.Dropped = len(dropped)
	r.Dur = time.Since(start)
	return r, nil
}

// ReconcileIncremental is the incremental sibling of Reconcile for a long-running
// watcher that holds a NoteCache. It parses only the changed files (DriftDeltaReport
// retains that parse), rebuilds the graph in memory from the cache (CPU-only, no
// disk re-parse), and applies targeted note/FTS writes plus a full nodes/edges
// rewrite from the rebuilt graph. The DB is always left authoritative for a
// concurrent `mesh search` reader. The returned Graph is the in-memory one, so the
// caller can swap it directly without a LoadGraph round-trip.
func ReconcileIncremental(s *Store, root string, cache *NoteCache, mtimeFast bool) (Reconciliation, error) {
	return ReconcileIncrementalContext(context.Background(), s, root, cache, mtimeFast)
}

// ReconcileIncrementalContext cancels discovery and publication without advancing the cache before commit.
func ReconcileIncrementalContext(ctx context.Context, s *Store, root string, cache *NoteCache, mtimeFast bool) (Reconciliation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Reconciliation{}, err
	}
	trace := latency.Start("index_incremental", "discover_parse")
	defer trace.End()
	start := time.Now()
	dd, err := s.DriftDeltaReportContext(ctx, root, mtimeFast)
	if err != nil {
		return Reconciliation{}, err
	}
	r := Reconciliation{
		Added:   len(dd.Drift.Added),
		Changed: len(dd.Drift.Changed),
		Removed: len(dd.Drift.Removed),
		Dropped: len(dd.Dropped),
	}
	// Record the drops BEFORE the no-drift early return. A newly added file with broken
	// frontmatter (or a duplicate id) produces no drift at all, so returning first is
	// exactly how it stayed invisible: no log line, no DroppedNotes record, and
	// mesh_health reporting a clean vault while the note was missing from the index.
	trace.Phase("record_dropped")
	if !dd.Drift.Any() {
		if err := s.recordDroppedContext(ctx, root, dd.Dropped); err != nil {
			return Reconciliation{}, err
		}
		r.Dur = time.Since(start)
		return r, nil
	}
	trace.Phase("graph")
	nextCache := NewNoteCache()
	nextCache.Seed(cache.Snapshot())
	nextCache.Apply(dd.Upserts, dd.RemovedIDs)
	g, _, err := BuildGraphContext(ctx, nextCache.Snapshot())
	if err != nil {
		return Reconciliation{}, err
	}
	trace.Phase("communities")
	if _, err := g.DetectCommunitiesContext(ctx, 0); err != nil {
		return Reconciliation{}, err
	}
	trace.Phase("persist")
	if _, err := s.indexVaultDeltaContext(ctx, dd.Upserts, dd.RemovedIDs, g, dd.Dropped, true); err != nil {
		return Reconciliation{}, err
	}
	// Refresh only this delta's bridge links. Full/code-index rebuilds still refresh
	// every note because symbol changes can alter resolution for unchanged notes.
	// Commit is the publication boundary: cancellation after it must still return
	// the new graph/cache, while best-effort bridge work stays cancellable.
	cache.Seed(nextCache.Snapshot())
	s.publishDropped(root, dd.Dropped)
	trace.Phase("code_links")
	if ctx.Err() == nil {
		_, _ = s.linkChangedNotesToCodeContext(ctx, root, dd.Upserts, dd.RemovedIDs)
	}
	r.Reindexed = true
	r.Graph = g
	r.Dur = time.Since(start)
	return r, nil
}

// ReconcilePaths is the event-driven sibling of ReconcileIncremental. The
// fsnotify watcher already knows which markdown paths changed; rediscovering that
// fact with vault.Walk makes a one-note write-back pay one stat for every note in
// the vault before it can be acknowledged. This path parses only the reported
// files plus the previously quarantined files needed to preserve duplicate-id
// ownership, then performs the same atomic note/FTS/graph commit.
//
// Periodic, startup, directory, and remote-trigger passes must continue to use
// ReconcileIncremental because their job is precisely to discover unknown drift.
func ReconcilePaths(s *Store, root string, cache *NoteCache, paths []string) (Reconciliation, error) {
	return ReconcilePathsContext(context.Background(), s, root, cache, paths)
}

// ReconcilePathsContext cancels discovery and publication without advancing the cache before commit.
func ReconcilePathsContext(ctx context.Context, s *Store, root string, cache *NoteCache, paths []string) (Reconciliation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Reconciliation{}, err
	}
	trace := latency.Start("index_targeted", "candidates")
	defer trace.End()
	start := time.Now()
	if len(paths) == 0 {
		return Reconciliation{Dur: time.Since(start)}, nil
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return Reconciliation{}, err
	}
	candidates := map[string]string{} // vault-relative -> absolute
	addCandidate := func(path string) error {
		if strings.TrimSpace(path) == "" {
			return nil
		}
		abs := path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(rootAbs, abs)
		}
		abs = filepath.Clean(abs)
		rel, err := filepath.Rel(rootAbs, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("mesh: targeted reconcile path is outside the vault: %q", path)
		}
		if !strings.EqualFold(filepath.Ext(rel), ".md") || vault.IsConflictSibling(filepath.Base(rel)) {
			return nil
		}
		candidates[filepath.Clean(rel)] = abs
		return nil
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return Reconciliation{}, err
		}
		if err := addCandidate(path); err != nil {
			return Reconciliation{}, err
		}
	}

	// A targeted pass must not forget existing parse/duplicate findings. It also
	// needs to reconsider a quarantined duplicate when the incumbent is deleted or
	// changes id; that file can become the rightful owner without receiving a new
	// fsnotify event of its own.
	previousDropped, err := s.droppedFromIndexContext(ctx)
	if err != nil {
		return Reconciliation{}, err
	}
	for _, fe := range previousDropped {
		if err := ctx.Err(); err != nil {
			return Reconciliation{}, err
		}
		if err := addCandidate(fe.Path); err != nil {
			return Reconciliation{}, err
		}
	}
	if len(candidates) == 0 {
		return Reconciliation{Dur: time.Since(start)}, nil
	}

	trace.Phase("parse_claims")
	current := cache.Snapshot()
	byPath := make(map[string]*ParsedNote, len(current))
	incumbent := make(map[string]string, len(current))
	for _, pn := range current {
		if err := ctx.Err(); err != nil {
			return Reconciliation{}, err
		}
		p := filepath.Clean(pn.Path)
		byPath[p] = pn
		incumbent[effectiveID(pn)] = p
	}

	type scannedFile struct {
		rel string
		pn  *ParsedNote
		err error
	}
	rels := make([]string, 0, len(candidates))
	for rel := range candidates {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	scanned := make([]scannedFile, 0, len(rels))
	claims := make([]idClaim, 0, len(current)+len(rels))
	for _, pn := range current {
		if err := ctx.Err(); err != nil {
			return Reconciliation{}, err
		}
		p := filepath.Clean(pn.Path)
		if _, targeted := candidates[p]; targeted {
			continue
		}
		claims = append(claims, idClaim{ID: effectiveID(pn), Path: p})
	}
	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return Reconciliation{}, err
		}
		abs := candidates[rel]
		fi, perr := os.Lstat(abs)
		var pn *ParsedNote
		switch {
		case perr != nil:
		case !fi.Mode().IsRegular():
			// Match vault.Walk: a markdown-shaped symlink, FIFO, socket, or
			// device is not a note. In particular, never follow a symlink out
			// of the vault merely because fsnotify reported its name.
			perr = os.ErrNotExist
		default:
			pn, perr = ParseFileContext(ctx, abs)
		}
		if err := ctx.Err(); err != nil {
			return Reconciliation{}, err
		}
		if perr != nil {
			scanned = append(scanned, scannedFile{rel: rel, err: perr})
			continue
		}
		pn.Path = rel
		scanned = append(scanned, scannedFile{rel: rel, pn: pn})
		claims = append(claims, idClaim{ID: effectiveID(pn), Path: rel})
	}
	owners := resolveIDOwners(claims, incumbent)

	var dd DriftDelta
	removed := map[string]bool{}
	upserted := map[string]bool{}
	for _, sf := range scanned {
		if err := ctx.Err(); err != nil {
			return Reconciliation{}, err
		}
		old := byPath[sf.rel]
		switch {
		case sf.err != nil:
			if os.IsNotExist(sf.err) {
				if old != nil {
					dd.Drift.Removed = append(dd.Drift.Removed, sf.rel)
					removed[effectiveID(old)] = true
				}
				continue
			}
			if old != nil {
				dd.Drift.Removed = append(dd.Drift.Removed, sf.rel)
				removed[effectiveID(old)] = true
			}
			dd.Dropped = append(dd.Dropped, FileError{Path: sf.rel, Err: sf.err})
		case owners[effectiveID(sf.pn)] != sf.rel:
			dd.Dropped = append(dd.Dropped, FileError{Path: sf.rel, Err: duplicateIDErr(effectiveID(sf.pn), owners[effectiveID(sf.pn)])})
			if old != nil {
				dd.Drift.Removed = append(dd.Drift.Removed, sf.rel)
				removed[effectiveID(old)] = true
			}
		case old == nil:
			dd.Drift.Added = append(dd.Drift.Added, sf.rel)
			dd.Upserts = append(dd.Upserts, sf.pn)
			upserted[effectiveID(sf.pn)] = true
		case retrievalHash(sf.pn) != retrievalHash(old):
			dd.Drift.Changed = append(dd.Drift.Changed, sf.rel)
			dd.Upserts = append(dd.Upserts, sf.pn)
			upserted[effectiveID(sf.pn)] = true
			if effectiveID(old) != effectiveID(sf.pn) {
				removed[effectiveID(old)] = true
			}
		}
	}
	for id := range removed {
		if err := ctx.Err(); err != nil {
			return Reconciliation{}, err
		}
		if !upserted[id] {
			dd.RemovedIDs = append(dd.RemovedIDs, id)
		}
	}
	sort.Strings(dd.Drift.Added)
	sort.Strings(dd.Drift.Changed)
	sort.Strings(dd.Drift.Removed)
	sort.Strings(dd.RemovedIDs)
	sort.Slice(dd.Dropped, func(i, j int) bool { return dd.Dropped[i].Path < dd.Dropped[j].Path })

	r := Reconciliation{
		Added:   len(dd.Drift.Added),
		Changed: len(dd.Drift.Changed),
		Removed: len(dd.Drift.Removed),
		Dropped: len(dd.Dropped),
	}
	trace.Phase("record_dropped")
	if !dd.Drift.Any() {
		if err := s.recordDroppedContext(ctx, root, dd.Dropped); err != nil {
			return Reconciliation{}, err
		}
		r.Dur = time.Since(start)
		return r, nil
	}
	trace.Phase("graph")
	nextCache := NewNoteCache()
	nextCache.Seed(cache.Snapshot())
	nextCache.Apply(dd.Upserts, dd.RemovedIDs)
	g, _, err := BuildGraphContext(ctx, nextCache.Snapshot())
	if err != nil {
		return Reconciliation{}, err
	}
	trace.Phase("communities")
	if _, err := g.DetectCommunitiesContext(ctx, 0); err != nil {
		return Reconciliation{}, err
	}
	trace.Phase("persist")
	if _, err := s.indexVaultDeltaContext(ctx, dd.Upserts, dd.RemovedIDs, g, dd.Dropped, true); err != nil {
		return Reconciliation{}, err
	}
	// Commit is the publication boundary: cancellation after it must still return
	// the new graph/cache, while best-effort bridge work stays cancellable.
	cache.Seed(nextCache.Snapshot())
	s.publishDropped(root, dd.Dropped)
	trace.Phase("code_links")
	if ctx.Err() == nil {
		_, _ = s.linkChangedNotesToCodeContext(ctx, root, dd.Upserts, dd.RemovedIDs)
	}
	r.Reindexed = true
	r.Graph = g
	r.Dur = time.Since(start)
	return r, nil
}
