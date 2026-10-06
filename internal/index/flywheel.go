// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

// The flywheel measurement: prove (not assert) that agent write-backs get reused by a
// LATER session. RecordWriteback stamps each write-back at authoring time;
// RecordReuse counts a fetch as reuse only if it lands at least gapSec after authoring
// (the cross-session proxy: a re-read inside the same work burst is not "the next run
// inheriting it"). FlywheelStats turns the table into the three numbers that say
// whether the moat is real: reuse rate, time-to-reuse, and write-back input health.

// RecordWriteback stamps a note as a write-back at authoring time so its reuse can be
// measured. Idempotent: re-writing the same id keeps the original authored_at (the
// flywheel cares about when the knowledge first existed). Best-effort at call sites.
func (s *Store) RecordWriteback(noteID, source string) error {
	return s.RecordWritebackContext(context.Background(), noteID, source)
}

// RecordWritebackContext is RecordWriteback with a caller-owned transaction lifetime.
func (s *Store) RecordWritebackContext(ctx context.Context, noteID, source string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if noteID == "" {
		return nil
	}
	now := time.Now().Unix()
	return s.WriteContext(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO note_reuse(note_id, authored_at, source) SELECT ?,?,?
			 WHERE NOT EXISTS (SELECT 1 FROM notes n WHERE n.id=? AND
			 CASE WHEN json_valid(n.frontmatter) THEN lower(trim(COALESCE(json_extract(n.frontmatter, '$.Status'), ''))) ELSE '' END = 'draft')
			 ON CONFLICT(note_id) DO NOTHING`,
			noteID, now, source, noteID)
		return err
	})
}

// BackfillWritebacks seeds note_reuse from existing agent-authored notes, so the
// flywheel measurement reflects the whole accumulated corpus on day one instead of
// only notes written after this shipped. Idempotent (ON CONFLICT DO NOTHING); uses the
// note's mtime as authoring time, which makes any later fetch of an old note count as
// genuine cross-session reuse. Returns the number of notes newly tracked.
func (s *Store) BackfillWritebacks() (int, error) {
	return s.BackfillWritebacksContext(context.Background())
}

// BackfillWritebacksContext is BackfillWritebacks with a caller-owned transaction
// lifetime.
func (s *Store) BackfillWritebacksContext(ctx context.Context) (int, error) {
	var n int
	err := s.WriteContext(ctx, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(ctx,
			`INSERT INTO note_reuse(note_id, authored_at, source)
			   SELECT n.id, n.mtime, n.source FROM notes n WHERE n.source = 'agent'`+draftPredicate+`
			 ON CONFLICT(note_id) DO NOTHING`)
		if e != nil {
			return e
		}
		ra, _ := res.RowsAffected()
		n = int(ra)
		return nil
	})
	return n, err
}

// RecordReuse records a fetch of a tracked note as reuse, but only when it happens at
// least gapSec after the note was authored. A no-op for untracked notes (those not
// written back through Mesh) and for fetches inside the authoring burst. Best-effort and
// NON-BLOCKING: mesh_fetch calls it purely for telemetry after the note is already
// returned, so it is queued in memory with the fetch's own timestamp and applied by the
// writer's batched flush (see Store.drainTelemetry). Recording the time here, not at
// flush time, keeps the gap check honest. Always returns nil.
func (s *Store) RecordReuse(noteID string, gapSec int64) error {
	if noteID == "" {
		return nil
	}
	s.recordTelemetry("", 0, &reuseEvent{noteID: noteID, gapSec: gapSec, at: time.Now().Unix()})
	return nil
}

// IsAgentAuthoredNote reports whether noteID currently carries source: agent in the
// indexed vault. Team telemetry uses the note's durable provenance (rather than the
// legacy local flywheel table, which an owning sync daemon may not have backfilled)
// so ordinary reference-note fetches do not create outbox traffic the hub discards.
func (s *Store) IsAgentAuthoredNote(noteID string) bool {
	if noteID == "" {
		return false
	}
	var source string
	return s.readDB.QueryRow(`SELECT COALESCE(n.source,'') FROM notes n WHERE n.id=?`+draftPredicate, noteID).Scan(&source) == nil && source == "agent"
}

// ReusedNote is one note and how many later-session fetches it has drawn, for the
// most-reused list (the concrete evidence behind the reuse rate).
type ReusedNote struct {
	NoteID     string `json:"note_id"`
	ReuseCount int64  `json:"reuse_count"`
}

// TopReused returns the n most-reused notes (reuse_count desc). n<=0 uses a small default.
func (s *Store) TopReused(n int) []ReusedNote {
	if n <= 0 {
		n = 8
	}
	s.flushTelemetry() // reporting surface: include reuse events not yet flushed
	rows, err := s.readDB.Query(
		`SELECT note_id, reuse_count FROM note_reuse WHERE reuse_count > 0
		  ORDER BY reuse_count DESC, note_id LIMIT ?`, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ReusedNote
	for rows.Next() {
		var r ReusedNote
		if err := rows.Scan(&r.NoteID, &r.ReuseCount); err != nil {
			return out
		}
		out = append(out, r)
	}
	return out
}

// FlywheelStats is the headline answer to "does the flywheel compound?".
type FlywheelStats struct {
	Authored           int64   `json:"authored"`              // write-backs being tracked
	Reused             int64   `json:"reused"`                // how many got >=1 cross-session reuse
	ReuseRatePct       float64 `json:"reuse_rate_pct"`        // reused / authored
	TotalReuses        int64   `json:"total_reuses"`          // sum of qualifying fetches
	MedianHoursToReuse float64 `json:"median_hours_to_reuse"` // over reused notes (0 if none)
	WritesPer100Reads  float64 `json:"writes_per_100_reads"`  // input-health proxy (writes / (queries+fetches))
}

// FlywheelStats computes the reuse picture from note_reuse plus the usage counters.
func (s *Store) FlywheelStats() (FlywheelStats, error) {
	return s.FlywheelStatsContext(context.Background())
}

// FlywheelStatsContext is FlywheelStats with caller-controlled cancellation.
func (s *Store) FlywheelStatsContext(ctx context.Context) (FlywheelStats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var st FlywheelStats
	s.flushReportingTelemetryContext(ctx) // reporting surface: include reuse events not yet flushed
	if err := s.readDB.QueryRowContext(ctx,
		`SELECT count(*),
		        count(first_reuse),
		        COALESCE(sum(reuse_count),0)
		   FROM note_reuse`).Scan(&st.Authored, &st.Reused, &st.TotalReuses); err != nil {
		return st, err
	}
	if st.Authored > 0 {
		st.ReuseRatePct = float64(st.Reused) / float64(st.Authored) * 100
	}

	// Median time-to-first-reuse, in hours, over the notes that were reused.
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT (first_reuse - authored_at) FROM note_reuse
		  WHERE first_reuse IS NOT NULL AND first_reuse >= authored_at`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	var deltas []int64
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			return st, err
		}
		deltas = append(deltas, d)
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	if len(deltas) > 0 {
		sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
		mid := deltas[len(deltas)/2]
		if len(deltas)%2 == 0 {
			mid = (deltas[len(deltas)/2-1] + deltas[len(deltas)/2]) / 2
		}
		st.MedianHoursToReuse = float64(mid) / 3600
	}

	// Input health: write-backs per 100 reads (queries + fetches). A flywheel with a
	// healthy reuse rate but near-zero input is still starved.
	q, _ := s.MetricContext(ctx, "queries")
	if err := ctx.Err(); err != nil {
		return st, err
	}
	f, _ := s.MetricContext(ctx, "fetches")
	if err := ctx.Err(); err != nil {
		return st, err
	}
	wr, _ := s.MetricContext(ctx, "writes")
	if err := ctx.Err(); err != nil {
		return st, err
	}
	if reads := q + f; reads > 0 {
		st.WritesPer100Reads = float64(wr) / float64(reads) * 100
	}
	return st, nil
}
