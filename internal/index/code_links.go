// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bright-interaction/mesh/internal/latency"
	"github.com/bright-interaction/mesh/internal/vault"
)

// The note<->code bridge: link a note to the code symbols it names in backticks, so an
// agent can ask "what do we know about this function" (symbol -> notes) and see a
// note's code alongside it (note -> symbols). Resolution is conservative: a backtick
// token only links when it is distinctive AND exactly matches an indexed symbol name
// (qualified or by last segment), so generic words never create noise.

// backtickRe pulls `code spans` out of a note body.
var backtickRe = regexp.MustCompile("`([^`\n]{2,80})`")

// symbolTokenRe matches a single code identifier path (Foo, Foo.Bar, pkg_thing).
var symbolTokenRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// distinctive reports whether a token is specific enough to link safely: it must look
// like code (qualified, snake_case, or mixedCase) and not be a short common word, so
// "Open"/"New"/"git" never link but "RecordReuse"/"Server.toolSearch" do.
func distinctive(tok string) bool {
	if len(tok) < 5 || !symbolTokenRe.MatchString(tok) {
		return false
	}
	if strings.ContainsAny(tok, "._") {
		return true
	}
	hasUpper, hasLower := false, false
	for _, r := range tok {
		if r >= 'A' && r <= 'Z' {
			hasUpper = true
		}
		if r >= 'a' && r <= 'z' {
			hasLower = true
		}
	}
	return hasUpper && hasLower // mixedCase identifier
}

// identRe pulls identifier-like tokens (Foo, Foo.Bar) out of a note TITLE, which is
// short and high-signal, so a symbol it names links even without backticks.
var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*`)

// LinkNotesToCode rebuilds note_code_links from note titles + bodies + the code index.
// A distinctive token links to a symbol when it equals the symbol name or its last
// segment (so a bare `RecordReuse` matches `Store.RecordReuse`). Titles are scanned in
// full (high signal); bodies only inside backticks (prose would be too noisy). Capped
// per token so a common name does not fan out. No-op when the code index is empty.
func (s *Store) LinkNotesToCode(vaultRoot string) (int, error) {
	return s.LinkNotesToCodeContext(context.Background(), vaultRoot)
}

// LinkNotesToCodeContext is LinkNotesToCode with cooperative cancellation across its
// SQL scans, note-file pass, symbol resolution, and final replacement transaction.
func (s *Store) LinkNotesToCodeContext(ctx context.Context, vaultRoot string) (int, error) {
	return s.linkNotesToCodeContext(ctx, vaultRoot, nil)
}

// linkChangedNotesToCode refreshes only changed IDs, including removed IDs whose
// old links must disappear. Code-index changes and full reconciliation still use
// LinkNotesToCode: a symbol change can change resolution for ANY note.
func (s *Store) linkChangedNotesToCode(root string, upserts []*ParsedNote, removed []string) (int, error) {
	return s.linkChangedNotesToCodeContext(context.Background(), root, upserts, removed)
}

func (s *Store) linkChangedNotesToCodeContext(ctx context.Context, root string, upserts []*ParsedNote, removed []string) (int, error) {
	ids := make(map[string]bool, len(upserts)+len(removed))
	for _, pn := range upserts {
		ids[effectiveID(pn)] = true
	}
	for _, id := range removed {
		ids[id] = true
	}
	changed := make([]string, 0, len(ids))
	for id := range ids {
		changed = append(changed, id)
	}
	sort.Strings(changed)
	return s.linkNotesToCodeContext(ctx, root, changed)
}

// A nil changed set means full rebuild; an empty non-nil set means no work.
// Read current indexed metadata, not cached note titles or a cached symbol map.
// Keep the same raw-file token semantics as the full rebuild (including spans in
// frontmatter), and replace the selected links atomically through the writer.
func (s *Store) linkNotesToCodeContext(ctx context.Context, vaultRoot string, changed []string) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if changed != nil && len(changed) == 0 {
		return 0, nil
	}
	trace := latency.Start("note_code_links", "symbols")
	defer trace.End()
	var symbolCount int
	if err := s.readDB.QueryRowContext(ctx, `SELECT count(*) FROM code_symbols`).Scan(&symbolCount); err != nil {
		return 0, err
	}
	if symbolCount == 0 {
		err := s.WriteContext(ctx, func(tx *sql.Tx) error {
			_, e := tx.ExecContext(ctx, `DELETE FROM note_code_links`)
			return e
		})
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		// Preserve LinkNotesToCode's best-effort empty-index cleanup: the bridge is
		// already semantically empty when there are no symbols.
		_ = err
		return 0, nil
	}
	trace.Phase("notes")
	notes, err := s.codeLinkNotesContext(ctx, changed)
	if err != nil {
		return 0, err
	}
	trace.Phase("resolve")
	resolve, err := s.symbolResolverContext(ctx)
	if err != nil {
		return 0, err
	}
	trace.Phase("files")
	links, err := resolveNoteCodeLinksContext(ctx, vaultRoot, notes, resolve)
	if err != nil {
		return 0, err
	}
	trace.Phase("persist")
	err = s.WriteContext(ctx, func(tx *sql.Tx) error {
		if changed == nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM note_code_links`); err != nil {
				return err
			}
		} else {
			del, err := tx.PrepareContext(ctx, `DELETE FROM note_code_links WHERE note_id=?`)
			if err != nil {
				return err
			}
			defer del.Close()
			for _, id := range changed {
				if _, err := del.ExecContext(ctx, id); err != nil {
					return err
				}
			}
		}
		ins, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO note_code_links(note_id,symbol_id,name) VALUES(?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		for _, l := range links {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := ins.ExecContext(ctx, l.noteID, l.symID, l.name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	var n int
	_ = s.readDB.QueryRowContext(ctx, `SELECT count(*) FROM note_code_links`).Scan(&n)
	return n, nil
}

type codeLinkNote struct{ id, path, title string }
type noteCodeLink struct{ noteID, symID, name string }

func (s *Store) codeLinkNotesContext(ctx context.Context, changed []string) ([]codeLinkNote, error) {
	var notes []codeLinkNote
	if changed != nil {
		// Point lookups avoid a vault-wide metadata scan and SQLite's variable limit.
		stmt, err := s.readDB.PrepareContext(ctx, `SELECT id, path, title FROM notes WHERE id=?`)
		if err != nil {
			return nil, err
		}
		defer stmt.Close()
		for _, id := range changed {
			var n codeLinkNote
			if err := stmt.QueryRowContext(ctx, id).Scan(&n.id, &n.path, &n.title); err != nil {
				if err == sql.ErrNoRows {
					continue // deleted or quarantined note: delete its old links only
				}
				return nil, err
			}
			notes = append(notes, n)
		}
		return notes, nil
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT id, path, title FROM notes`)
	if err != nil {
		return nil, err
	}
	// A leaked *sql.Rows pins a WAL read snapshot for the life of the process, which
	// stops every checkpoint from reclaiming past it. In a long-running daemon that
	// grows the WAL without bound and starves other processes' writes into SQLITE_BUSY.
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		var nm codeLinkNote
		if err := rows.Scan(&nm.id, &nm.path, &nm.title); err != nil {
			rows.Close()
			return nil, err
		}
		notes = append(notes, nm)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return notes, nil
}

func resolveNoteCodeLinksContext(ctx context.Context, vaultRoot string, notes []codeLinkNote, resolve func(string, int) []symRow) ([]noteCodeLink, error) {
	var links []noteCodeLink
	for _, n := range notes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		consider := func(tok string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			tok = strings.TrimSpace(tok)
			if seen[tok] || !distinctive(tok) {
				return nil
			}
			seen[tok] = true
			syms := resolve(tok, 5)
			// Precision gate: an unqualified (bare) token must resolve to EXACTLY ONE
			// symbol to link. Common PascalCase words (Close, Server, Store, Config,
			// Handler) name a method or type on many packages, so linking a note that
			// merely says `Store` to every service's Store is noise. A qualified token
			// (Type.Method) is already specific, so its capped matches link as before.
			if !strings.Contains(tok, ".") && len(syms) != 1 {
				return nil
			}
			for _, sym := range syms {
				links = append(links, noteCodeLink{n.id, sym.id, sym.name})
			}
			return nil
		}
		// Title: scan every identifier-like token (the note's subject).
		for _, tok := range identRe.FindAllString(n.title, -1) {
			if err := consider(tok); err != nil {
				return nil, err
			}
		}
		// Body: only backtick code spans (prose is too noisy for a full scan).
		body, readErr := vault.ReadFileContext(ctx, filepath.Join(vaultRoot, n.path))
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if readErr == nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for _, m := range backtickRe.FindAllStringSubmatch(string(body), -1) {
				if err := consider(m[1]); err != nil {
					return nil, err
				}
			}
		}
	}
	return links, nil
}

type symRow struct{ id, name string }

// symbolResolver loads the symbol table once and resolves tokens in memory: exact
// name match plus dotted-suffix match (a bare `RecordReuse` matches
// `Store.RecordReuse`). Suffix keys are lowercased to keep the semantics of the
// SQL LIKE this replaces (LIKE is case-insensitive, `=` is not). The per-token
// query it replaces used a leading-wildcard LIKE, a full code_symbols scan for
// every distinctive token in every note: ~57s per rebuild against a
// monorepo-sized index, versus ~1s for this map build.
func (s *Store) symbolResolver() (func(tok string, limit int) []symRow, error) {
	return s.symbolResolverContext(context.Background())
}

func (s *Store) symbolResolverContext(ctx context.Context) (func(tok string, limit int) []symRow, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT id, name FROM code_symbols`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byExact := map[string][]symRow{}
	bySuffix := map[string][]symRow{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var r symRow
		if err := rows.Scan(&r.id, &r.name); err != nil {
			return nil, err
		}
		byExact[r.name] = append(byExact[r.name], r)
		low := strings.ToLower(r.name)
		for i := strings.IndexByte(low, '.'); i >= 0; i = strings.IndexByte(low, '.') {
			low = low[i+1:]
			bySuffix[low] = append(bySuffix[low], r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return func(tok string, limit int) []symRow {
		out := append([]symRow(nil), byExact[tok]...)
		out = append(out, bySuffix[strings.ToLower(tok)]...)
		if len(out) > limit { // too ambiguous to link confidently
			return nil
		}
		return out
	}, nil
}

// NoteCodeRef is a note linked to a symbol (either direction).
type NoteCodeRef struct {
	NoteID   string `json:"note_id"`
	Title    string `json:"title"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	SymbolID string `json:"symbol_id,omitempty"`
	Symbol   string `json:"symbol,omitempty"`
}

// NotesForSymbol returns the notes that reference a given code symbol id.
func (s *Store) NotesForSymbol(symbolID string) ([]NoteCodeRef, error) {
	rows, err := s.readDB.Query(
		`SELECT n.id, n.title, n.path, n.type
		   FROM note_code_links l JOIN notes n ON n.id = l.note_id
		  WHERE l.symbol_id = ? ORDER BY n.title`, symbolID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteCodeRef
	for rows.Next() {
		var r NoteCodeRef
		if err := rows.Scan(&r.NoteID, &r.Title, &r.Path, &r.Type); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SymbolsForNote returns the code symbols a note references.
func (s *Store) SymbolsForNote(noteID string) ([]NoteCodeRef, error) {
	rows, err := s.readDB.Query(
		`SELECT symbol_id, name FROM note_code_links WHERE note_id = ? ORDER BY name`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteCodeRef
	for rows.Next() {
		var r NoteCodeRef
		if err := rows.Scan(&r.SymbolID, &r.Symbol); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NotesForSymbolName returns notes linked to any symbol matching a name (exact, or as
// the type/receiver or last segment), so "Verifier" surfaces notes about Verifier,
// Verifier.Configured, or x.Verifier, regardless of which symbol id FTS ranked first.
func (s *Store) NotesForSymbolName(name string) ([]NoteCodeRef, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	rows, err := s.readDB.Query(
		`SELECT DISTINCT n.id, n.title, n.path, n.type
		   FROM note_code_links l JOIN notes n ON n.id = l.note_id
		  WHERE l.name = ? OR l.name LIKE ? OR l.name LIKE ?
		  ORDER BY n.title`,
		name, name+".%", "%."+name)
	if err != nil {
		return nil, err
	}
	// A leaked *sql.Rows pins a WAL read snapshot for the life of the process, which
	// stops every checkpoint from reclaiming past it. In a long-running daemon that
	// grows the WAL without bound and starves other processes' writes into SQLITE_BUSY.
	defer rows.Close()
	var out []NoteCodeRef
	for rows.Next() {
		var r NoteCodeRef
		if err := rows.Scan(&r.NoteID, &r.Title, &r.Path, &r.Type); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NoteCountForSymbol returns how many notes reference a symbol (for result badges).
//
// JOINed to notes deliberately. note_code_links is written ONLY by LinkNotesToCode, which
// no reindex path calls, so a deleted note leaves its link row behind forever. The two
// sibling readers (NotesForSymbol, NotesForSymbolName) join and therefore filter those
// orphans; this one did not, so mesh_code_search stamped a "2 notes" badge on a symbol
// that mesh_code_context then showed 1 note for. The badge is the more visible of the two
// and was the wrong one.
func (s *Store) NoteCountForSymbol(symbolID string) int {
	var n int
	_ = s.readDB.QueryRow(`SELECT count(*) FROM note_code_links l
	    JOIN notes n ON n.id = l.note_id
	    WHERE l.symbol_id = ?`, symbolID).Scan(&n)
	return n
}
