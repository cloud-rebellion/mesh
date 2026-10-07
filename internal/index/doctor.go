// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"os"
	"path/filepath"
	"sort"

	"github.com/bright-interaction/mesh/internal/vault"
)

// Drift describes how the vault on disk has diverged from the persisted index.
type Drift struct {
	Added   []string // files present on disk but not in the index
	Changed []string // files whose retrieval-critical content changed since indexing
	Removed []string // files in the index but gone from disk
}

// StaleIndexIssue is the operational health-finding kind for an index that no longer
// matches its Markdown source. A lifecycle pass over stale rows cannot truthfully call
// the vault clean: it may omit a newly-added note or inspect guidance that was removed.
const StaleIndexIssue = "stale-index"

// Any reports whether the index is stale in any way.
func (d Drift) Any() bool { return len(d.Added)+len(d.Changed)+len(d.Removed) > 0 }

// DriftReport compares the current vault files against the persisted notes using
// the same retrieval_hash the indexer stores, so `mesh doctor` can tell the
// operator exactly which files need a reindex instead of guessing.
// NoteHashes maps every indexed note's path to its retrieval hash, i.e. a fingerprint of
// what the index currently holds. DriftReport compares one of these against the vault on
// disk; a read-only server compares two of them across a refresh to say what its own view
// gained, lost or changed, which is the only honest way it can report counts for a pass
// it did not run itself.
func (s *Store) NoteHashes() (map[string]string, error) {
	return s.NoteHashesContext(context.Background())
}

// NoteHashesContext allows a refresh deadline to cover fingerprint loading too.
func (s *Store) NoteHashesContext(ctx context.Context) (map[string]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT path, retrieval_hash FROM notes`)
	if err != nil {
		return nil, err
	}
	// A leaked *sql.Rows pins a WAL read snapshot for the life of the process; see below.
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, h string
		if err := rows.Scan(&p, &h); err != nil {
			return nil, err
		}
		out[p] = h
	}
	return out, rows.Err()
}

func (s *Store) DriftReport(root string) (Drift, error) {
	return s.DriftReportContext(context.Background(), root)
}

// DriftReportContext is DriftReport with caller-controlled cancellation across the
// database snapshot, vault walk, and per-note reads. A cancelled report never returns a
// partial drift set that a caller could mistake for the owner's current backlog.
func (s *Store) DriftReportContext(ctx context.Context, root string) (Drift, error) {
	return s.driftReportContext(ctx, root, vault.WalkContext, ParseFileContext)
}

type walkVaultContextFunc func(context.Context, string) ([]string, error)
type parseNoteContextFunc func(context.Context, string) (*ParsedNote, error)

// driftReportContext keeps the filesystem phases injectable so cancellation during a
// walk or note read can be proved without depending on a particular FUSE/network mount.
func (s *Store) driftReportContext(ctx context.Context, root string, walk walkVaultContextFunc, parse parseNoteContextFunc) (Drift, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Drift{}, err
	}
	// id comes back alongside the hash so incumbent can be built from the SAME rows: the
	// drift check has to resolve a duplicate id exactly as the indexer does. Without the
	// incumbent, drift disagreed with the indexer about which of two colliding files
	// belongs in the index, and the file the indexer had deliberately quarantined came
	// back as Added on every single run, so `mesh doctor` said STALE and exited 1 forever
	// while `mesh index` kept producing a byte-identical index.
	rows, err := s.readDB.QueryContext(ctx, `SELECT path, id, retrieval_hash FROM notes`)
	if err != nil {
		return Drift{}, err
	}
	// A leaked *sql.Rows pins a WAL read snapshot for the life of the process, which
	// stops every checkpoint from reclaiming past it. In a long-running daemon that
	// grows the WAL without bound and starves other processes' writes into SQLITE_BUSY.
	defer rows.Close()
	dbHash := map[string]string{}
	incumbent := map[string]string{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return Drift{}, err
		}
		var p, id, h string
		if err := rows.Scan(&p, &id, &h); err != nil {
			rows.Close()
			return Drift{}, err
		}
		dbHash[p] = h
		incumbent[id] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Drift{}, err
	}
	if err := ctx.Err(); err != nil {
		return Drift{}, err
	}

	files, err := walk(ctx, root)
	if err != nil {
		return Drift{}, err
	}
	var d Drift
	seen := map[string]bool{}
	// Parse first, resolve id ownership, then classify: which file owns an id decides
	// whether another file is drift at all, and that cannot be known one file at a time.
	type parsedFile struct {
		rel string
		pn  *ParsedNote
	}
	parsedFiles := make([]parsedFile, 0, len(files))
	claims := make([]idClaim, 0, len(files))
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return Drift{}, err
		}
		rel, err := filepath.Rel(root, f)
		if err != nil {
			rel = f
		}
		seen[rel] = true
		pn, err := parse(ctx, f)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Drift{}, ctxErr
			}
			// A file that no longer parses but is in the index must be reported as
			// drift (a full reindex would drop it), not silently skipped.
			if _, ok := dbHash[rel]; ok {
				d.Removed = append(d.Removed, rel)
			}
			continue
		}
		pn.Path = rel
		parsedFiles = append(parsedFiles, parsedFile{rel: rel, pn: pn})
		claims = append(claims, idClaim{ID: effectiveID(pn), Path: rel})
	}
	if err := ctx.Err(); err != nil {
		return Drift{}, err
	}
	owner := resolveIDOwners(claims, incumbent)
	for _, pf := range parsedFiles {
		if err := ctx.Err(); err != nil {
			return Drift{}, err
		}
		if o, ok := owner[effectiveID(pf.pn)]; ok && o != pf.rel {
			// Quarantined: an index pass will not hold this file, so its absence is not
			// drift. If it still has a stale row (its id changed into a collision) that
			// row must go, exactly as the unparseable branch above does.
			if _, ok := dbHash[pf.rel]; ok {
				d.Removed = append(d.Removed, pf.rel)
			}
			continue
		}
		h, ok := dbHash[pf.rel]
		if !ok {
			d.Added = append(d.Added, pf.rel)
		} else if retrievalHash(pf.pn) != h {
			d.Changed = append(d.Changed, pf.rel)
		}
	}
	for p := range dbHash {
		if err := ctx.Err(); err != nil {
			return Drift{}, err
		}
		if !seen[p] {
			d.Removed = append(d.Removed, p)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Changed)
	sort.Strings(d.Removed)
	if err := ctx.Err(); err != nil {
		return Drift{}, err
	}
	return d, nil
}

// DriftDelta is DriftReport plus the parsed notes for Added+Changed (so an
// incremental reconcile does not re-parse them) and the resolved ids of removed
// notes (so it can target the DELETEs). RemovedIDs also carries the OLD id of a
// note whose frontmatter id changed under the same path.
type DriftDelta struct {
	Upserts    []*ParsedNote // Added + Changed, parsed, vault-relative Path
	RemovedIDs []string      // ids whose files are gone, plus retired old ids
	Drift      Drift         // the path lists, for logging/reporting
	// Dropped are files this delta could not index: unparseable frontmatter, or an
	// effective id already claimed by another file. They are reported, never fatal, so
	// the rest of the delta still applies and the invisible note is visible to the
	// operator (mesh_health, the reindex log) instead of vanishing without a trace.
	Dropped []FileError
}

// DriftDeltaReport is DriftReport that retains the parse work it must do anyway:
// it returns the parsed Added+Changed notes and the ids of removed notes, so the
// incremental reconcile path never parses a file twice. It uses the same
// authoritative retrieval_hash comparison, so a cosmetic edit (mtime moved, hash
// unchanged) yields no drift.
//
// mtimeFast trades a little safety for speed on the watcher's hot path: a file
// whose on-disk mtime equals the stored mtime is trusted unchanged and NOT parsed,
// so a single edit parses one file instead of the whole vault. The blind spot is
// an edit that does not move mtime (rare: a mtime-preserving write, or a second
// edit within the same one-second mtime resolution as the index); the watcher's
// periodic tick calls this with mtimeFast=false (parse-all, hash-authoritative),
// which catches any such case within one interval. Pass false for a correctness
// pass (the tick, `mesh doctor`, startup).
func (s *Store) DriftDeltaReport(root string, mtimeFast bool) (DriftDelta, error) {
	return s.DriftDeltaReportContext(context.Background(), root, mtimeFast)
}

// DriftDeltaReportContext keeps discovery, parsing and SQL reads within the owner's lifetime.
func (s *Store) DriftDeltaReportContext(ctx context.Context, root string, mtimeFast bool) (DriftDelta, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return DriftDelta{}, err
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT path, id, retrieval_hash, mtime FROM notes`)
	if err != nil {
		return DriftDelta{}, err
	}
	// A leaked *sql.Rows pins a WAL read snapshot for the life of the process, which
	// stops every checkpoint from reclaiming past it. In a long-running daemon that
	// grows the WAL without bound and starves other processes' writes into SQLITE_BUSY.
	defer rows.Close()
	type rec struct {
		id, hash string
		mtime    int64
	}
	dbByPath := map[string]rec{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return DriftDelta{}, err
		}
		var p, id, h string
		var mt int64
		if err := rows.Scan(&p, &id, &h, &mt); err != nil {
			rows.Close()
			return DriftDelta{}, err
		}
		dbByPath[p] = rec{id, h, mt}
	}
	if err := rows.Err(); err != nil {
		return DriftDelta{}, err
	}
	rows.Close()

	files, err := vault.WalkContext(ctx, root)
	if err != nil {
		return DriftDelta{}, err
	}
	var dd DriftDelta
	upsertIDs := map[string]bool{}
	removed := map[string]bool{}
	seen := map[string]bool{}
	// Two live files resolving to the same effectiveID is a data error the schema cannot
	// represent (notes.id is the PK), so exactly one of them can be indexed. This used to
	// abort the WHOLE delta, which froze the entire vault's index: after one duplicate
	// appeared, no edit to any other note was indexed until the process restarted, and a
	// restart only postponed it. Quarantine instead: one file keeps the id, the others
	// are dropped with a recorded reason, and the rest of the delta applies normally.
	//
	// The winner is chosen by the shared resolveIDOwners, against the id owners already
	// in the index, so this path and the full path (ClaimUniqueIDs in ReindexFull) agree
	// on WHICH file wins. While the choice was made here in walk order and there in
	// INSERT OR REPLACE order, the note you could actually retrieve flipped depending on
	// whether the watcher or a `mesh index` ran last.
	incumbent := make(map[string]string, len(dbByPath))
	for p, r := range dbByPath {
		if err := ctx.Err(); err != nil {
			return DriftDelta{}, err
		}
		incumbent[r.id] = p
	}
	// Gather every file's claim BEFORE classifying any of them: whether a file is drift
	// depends on whether it owns its id, and a single streaming pass can only see the
	// claims that came before it.
	type scannedFile struct {
		rel string
		id  string
		pn  *ParsedNote // nil when the mtime fast path skipped the parse
		err error       // parse failure
	}
	scanned := make([]scannedFile, 0, len(files))
	claims := make([]idClaim, 0, len(files))
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return DriftDelta{}, err
		}
		rel, err := filepath.Rel(root, f)
		if err != nil {
			rel = f
		}
		seen[rel] = true
		// Fast path: a file in the index whose mtime is unchanged is trusted as-is and
		// not parsed. It still owns its id (from the DB) for the collision check.
		if r, ok := dbByPath[rel]; mtimeFast && ok && r.mtime != 0 {
			if fi, statErr := os.Stat(f); statErr == nil && fi.ModTime().Unix() == r.mtime {
				scanned = append(scanned, scannedFile{rel: rel, id: r.id})
				claims = append(claims, idClaim{ID: r.id, Path: rel})
				continue
			}
		}
		pn, perr := ParseFileContext(ctx, f)
		if err := ctx.Err(); err != nil {
			return DriftDelta{}, err
		}
		if perr != nil {
			scanned = append(scanned, scannedFile{rel: rel, err: perr})
			continue
		}
		pn.Path = rel
		id := effectiveID(pn)
		scanned = append(scanned, scannedFile{rel: rel, id: id, pn: pn})
		claims = append(claims, idClaim{ID: id, Path: rel})
	}
	owner := resolveIDOwners(claims, incumbent)

	for _, sf := range scanned {
		if err := ctx.Err(); err != nil {
			return DriftDelta{}, err
		}
		if sf.err != nil {
			// A file that no longer parses but is in the index must be dropped (a full
			// reindex would), not silently kept stale by the incremental cache path.
			if r, ok := dbByPath[sf.rel]; ok {
				dd.Drift.Removed = append(dd.Drift.Removed, sf.rel)
				removed[r.id] = true
			}
			// Record it either way. A brand-new unparseable file is NOT in the index, so
			// it used to fall straight through to `continue`: no drift entry, no log
			// line, no dropped record, and mesh_health reported a clean vault while the
			// note was missing from search and the graph. That is the exact failure
			// recordDropped was added to prevent, and it was wired only into ReindexFull,
			// so the watcher and the hosted hub (the incremental paths) never saw it.
			dd.Dropped = append(dd.Dropped, FileError{Path: sf.rel, Err: sf.err})
			continue
		}
		if o, ok := owner[sf.id]; ok && o != sf.rel {
			dd.Dropped = append(dd.Dropped, FileError{Path: sf.rel, Err: duplicateIDErr(sf.id, o)})
			// A file Mesh REFUSES to index must not keep its previous index entry. This
			// used to `continue` before ever consulting dbByPath, so the row the file was
			// last indexed under was neither refreshed nor removed: search kept serving
			// the PRE-EDIT body, the graph kept the stale node, its tags and its edges,
			// and mesh_fetch resolved the id to a file that now declares a different id
			// and different content. Search and fetch disagreed about what the note was.
			//
			// No reconcile healed it, authoritative or not, because only ReindexFull
			// rebuilds from scratch, and a long-running MCP server does an incremental
			// reconcile rather than a full reload after startup. The warning even said
			// the note was "invisible to search and the graph until it is fixed", which
			// was the exact opposite of what happened.
			//
			// Do what the unparseable branch above does: drop the stale row too.
			if r, ok := dbByPath[sf.rel]; ok {
				dd.Drift.Removed = append(dd.Drift.Removed, sf.rel)
				removed[r.id] = true
			}
			continue
		}
		if sf.pn == nil {
			continue // mtime fast path: unchanged and still the owner, so nothing to do
		}
		r, ok := dbByPath[sf.rel]
		switch {
		case !ok:
			dd.Drift.Added = append(dd.Drift.Added, sf.rel)
			dd.Upserts = append(dd.Upserts, sf.pn)
			upsertIDs[sf.id] = true
		case retrievalHash(sf.pn) != r.hash:
			dd.Drift.Changed = append(dd.Drift.Changed, sf.rel)
			dd.Upserts = append(dd.Upserts, sf.pn)
			upsertIDs[sf.id] = true
			if r.id != sf.id { // frontmatter id changed under the same path: retire the old id
				removed[r.id] = true
			}
		}
	}
	for p, r := range dbByPath {
		if err := ctx.Err(); err != nil {
			return DriftDelta{}, err
		}
		if !seen[p] {
			dd.Drift.Removed = append(dd.Drift.Removed, p)
			removed[r.id] = true
		}
	}
	// A rename surfaces the old path as Removed and the new path as Added with the
	// SAME id; never delete an id we are upserting.
	for id := range removed {
		if err := ctx.Err(); err != nil {
			return DriftDelta{}, err
		}
		if !upsertIDs[id] {
			dd.RemovedIDs = append(dd.RemovedIDs, id)
		}
	}
	sort.Strings(dd.Drift.Added)
	sort.Strings(dd.Drift.Changed)
	sort.Strings(dd.Drift.Removed)
	sort.Strings(dd.RemovedIDs)
	// Stable order so the dropped set can be compared between reconciles (that
	// comparison is what stops the watcher re-logging the same warning every tick).
	sort.Slice(dd.Dropped, func(i, j int) bool { return dd.Dropped[i].Path < dd.Dropped[j].Path })
	return dd, nil
}
