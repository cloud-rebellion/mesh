// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bright-interaction/mesh/internal/vault"
	"strings"
	"time"
)

// PendingNote is an extracted draft awaiting review. Structured authoring lives in
// one JSON payload; historical columns are retained by the legacy read adapter.
// Pending items never enter retrieval until publication succeeds.
type PendingNote struct {
	ID              string            `json:"id"`
	Type            string            `json:"type"`
	Title           string            `json:"title"`
	Template        string            `json:"template,omitempty"`
	TemplateVersion int               `json:"template_version,omitempty"`
	Summary         string            `json:"summary,omitempty"`
	Sections        map[string]string `json:"sections,omitempty"`
	Blocks          []vault.BlockSpec `json:"blocks,omitempty"`
	Collections     []string          `json:"collections,omitempty"`
	Tags            []string          `json:"tags,omitempty"`
	Related         []string          `json:"related,omitempty"`
	Supersedes      []string          `json:"supersedes,omitempty"`
	Confidence      string            `json:"confidence"`
	Source          string            `json:"source"`
	CreatedAt       int64             `json:"created_at"`
	// Deprecated: historical pending payloads only. Active producers never set
	// these fields, and promotion requires an explicit reviewed replacement.
	Do   string `json:"do,omitempty"`
	Dont string `json:"dont,omitempty"`
	Why  string `json:"why,omitempty"`
}

var ErrPendingQueueFull = errors.New("pending review queue is full; review existing drafts before extracting more")

// AuthoringSpec is the sole promotion adapter. It deliberately refuses to guess
// purpose-specific prose from historical shorthand.
func (p PendingNote) AuthoringSpec() (vault.NewNoteSpec, error) {
	if p.Template == "" || p.HasLegacy() {
		return vault.NewNoteSpec{}, fmt.Errorf("historical draft requires a reviewed template and authored sections")
	}
	return vault.NewNoteSpec{
		Type: vault.NoteType(p.Type), Title: p.Title, Template: p.Template,
		TemplateVersion: p.TemplateVersion, Summary: p.Summary, Sections: p.Sections,
		Blocks: p.Blocks, Collections: p.Collections, Tags: p.Tags, Related: p.Related,
		Supersedes: p.Supersedes, Confidence: p.Confidence,
		Source: "agent", Agent: "mesh-extract", By: "mesh-extract", Status: "active",
	}, nil
}

// pendingPayload decouples durable structured drafts from the historical columns.
type pendingPayload struct {
	Template        string            `json:"template"`
	TemplateVersion int               `json:"template_version"`
	Summary         string            `json:"summary"`
	Sections        map[string]string `json:"sections"`
	Blocks          []vault.BlockSpec `json:"blocks,omitempty"`
	Collections     []string          `json:"collections,omitempty"`
	Tags            []string          `json:"tags,omitempty"`
	Related         []string          `json:"related,omitempty"`
	Supersedes      []string          `json:"supersedes,omitempty"`
}

func (p PendingNote) payload() pendingPayload {
	return pendingPayload{p.Template, p.TemplateVersion, p.Summary, p.Sections, p.Blocks, p.Collections, p.Tags, p.Related, p.Supersedes}
}
func (p *PendingNote) readPayload(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	} // isolated legacy adapter: preserve original words
	var a pendingPayload
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return fmt.Errorf("decode pending authoring payload: %w", err)
	}
	p.Template, p.TemplateVersion, p.Summary, p.Sections = a.Template, a.TemplateVersion, a.Summary, a.Sections
	p.Blocks, p.Collections, p.Tags, p.Related, p.Supersedes = a.Blocks, a.Collections, a.Tags, a.Related, a.Supersedes
	return nil
}

// PendingID derives a stable id from type+title so re-extracting the same session (or
// the same learning from two sessions) does not create duplicate review items.
func PendingID(noteType, title string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(noteType) + "|" + strings.TrimSpace(title))))
	return "pending-" + hex.EncodeToString(sum[:8])
}

// pendingQueueCap bounds admission without discarding unreviewed work. Existing
// items can still be updated, reviewed, or discarded when the queue is full.
const pendingQueueCap = 200

// AddPending stores a review draft. Duplicate IDs update the existing payload.
func (s *Store) AddPending(p PendingNote) error {
	return s.AddPendingContext(context.Background(), p)
}

// AddPendingContext is AddPending with a caller-owned transaction lifetime.
func (s *Store) AddPendingContext(ctx context.Context, p PendingNote) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Type) == "" {
		return nil
	}
	if p.ID == "" {
		p.ID = PendingID(p.Type, p.Title)
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().Unix()
	}
	payload := ""
	if p.Template != "" {
		raw, err := json.Marshal(p.payload())
		if err != nil {
			return err
		}
		if len(raw) > 64<<10 {
			return fmt.Errorf("pending authoring payload exceeds 64 KiB")
		}
		payload = string(raw)
	}
	return s.WriteContext(ctx, func(tx *sql.Tx) error {
		var count, exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*), count(CASE WHEN id=? THEN 1 END) FROM pending_notes`, p.ID).Scan(&count, &exists); err != nil {
			return err
		}
		if exists == 0 && count >= pendingQueueCap {
			return ErrPendingQueueFull
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO pending_notes(id,type,title,do_text,dont_text,why,confidence,source,created_at,authoring_json)
			 VALUES(?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET
			   type=excluded.type, title=excluded.title,
             authoring_json=CASE WHEN excluded.authoring_json='' THEN pending_notes.authoring_json ELSE excluded.authoring_json END,
             do_text=CASE WHEN COALESCE(excluded.do_text,'')='' THEN pending_notes.do_text ELSE excluded.do_text END,
             dont_text=CASE WHEN COALESCE(excluded.dont_text,'')='' THEN pending_notes.dont_text ELSE excluded.dont_text END,
             why=CASE WHEN COALESCE(excluded.why,'')='' THEN pending_notes.why ELSE excluded.why END,
			   confidence=excluded.confidence, source=excluded.source`,
			p.ID, p.Type, p.Title, p.Do, p.Dont, p.Why, p.Confidence, p.Source, p.CreatedAt, payload); err != nil {
			return err
		}
		return nil
	})
}

// ListPending returns review items, newest first.
func (s *Store) ListPending() ([]PendingNote, error) {
	return s.ListPendingContext(context.Background())
}

// ListPendingContext is ListPending with caller-controlled cancellation.
func (s *Store) ListPendingContext(ctx context.Context) ([]PendingNote, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT id,type,title,COALESCE(do_text,''),COALESCE(dont_text,''),COALESCE(why,''),
		        COALESCE(confidence,''),COALESCE(source,''),created_at,COALESCE(authoring_json,'')
		   FROM pending_notes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingNote
	for rows.Next() {
		var p PendingNote
		var payload string
		if err := rows.Scan(&p.ID, &p.Type, &p.Title, &p.Do, &p.Dont, &p.Why, &p.Confidence, &p.Source, &p.CreatedAt, &payload); err != nil {
			return nil, err
		}
		if err := p.readPayload(payload); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPending fetches one review item by id.
func (s *Store) GetPending(id string) (PendingNote, error) {
	return s.GetPendingContext(context.Background(), id)
}

// GetPendingContext is GetPending with caller-controlled cancellation.
func (s *Store) GetPendingContext(ctx context.Context, id string) (PendingNote, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var p PendingNote
	var payload string
	err := s.readDB.QueryRowContext(ctx,
		`SELECT id,type,title,COALESCE(do_text,''),COALESCE(dont_text,''),COALESCE(why,''),
		        COALESCE(confidence,''),COALESCE(source,''),created_at,COALESCE(authoring_json,'')
		   FROM pending_notes WHERE id=?`, id).
		Scan(&p.ID, &p.Type, &p.Title, &p.Do, &p.Dont, &p.Why, &p.Confidence, &p.Source, &p.CreatedAt, &payload)
	if err == nil {
		err = p.readPayload(payload)
	}
	return p, err
}

// DeletePending removes a review item (on promote or discard).
func (s *Store) DeletePending(id string) error {
	return s.DeletePendingContext(context.Background(), id)
}

// DeletePendingContext is DeletePending with a caller-owned transaction lifetime.
func (s *Store) DeletePendingContext(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.WriteContext(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM pending_notes WHERE id=?`, id)
		return err
	})
}

// PendingCount returns the number of review items (for the dashboard badge).
func (s *Store) PendingCount() (int, error) {
	return s.PendingCountContext(context.Background())
}

// PendingCountContext is PendingCount with caller-controlled cancellation.
func (s *Store) PendingCountContext(ctx context.Context) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var n int
	err := s.readDB.QueryRowContext(ctx, `SELECT count(*) FROM pending_notes`).Scan(&n)
	return n, err
}
