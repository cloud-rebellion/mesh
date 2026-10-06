// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bright-interaction/mesh/internal/mcp"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
)

// maxWebNoteBytes bounds both the filesystem read and the markdown/frontmatter
// work performed by the public note route.
const maxWebNoteBytes = 1 << 20

// handleSearch runs the same fused retrieval the agent gets over MCP and returns
// ranked cards, so a human can search the vault from the browser. The retriever is
// cached (built lazily, invalidated on reindex/config change) instead of rebuilt per
// request, so a search no longer pays a full LoadVectors + ANN rebuild every time.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	// Bound the query TEXT, single-sourced from internal/mcp like the numeric caps
	// below. This surface is the exposed one: on the default loopback bind there is no
	// auth, no rate limiter and no Origin check on /api/search, so any page open in the
	// user's browser could fire searches in a loop, and a 65001-byte q cost 1.07s of CPU
	// each. graph.MaxQueryTerms bounds the work itself; this is the clear refusal.
	if len(q) > mcp.SearchQueryMaxBytes {
		http.Error(w, fmt.Sprintf("q is too long: %d bytes, maximum %d. Search for the few words you want, not a whole document.", len(q), mcp.SearchQueryMaxBytes), http.StatusBadRequest)
		return
	}
	// Bound the request with the SAME numbers the MCP twin enforces, single-sourced from
	// internal/mcp so the two surfaces over one retriever cannot drift apart again. This
	// one had no bound at all: retrieve.Retrieve only FLOORS Limit, and it skips
	// packToBudget entirely when Budget is 0, which was the default here. Measured on a
	// 400-note vault, ?q=deploy returned 12 cards / 974 tokens while
	// ?q=deploy&limit=100000 returned 401 cards / 30950 tokens / 116654 bytes, and the
	// server paid corpus-sized retrieval work for it.
	limit := atoiOr(r.URL.Query().Get("limit"), mcp.SearchLimitDefault)
	if limit > mcp.SearchLimitMax {
		limit = mcp.SearchLimitMax
	}
	budget := atoiOr(r.URL.Query().Get("budget"), mcp.SearchBudgetDefault)
	rt, err := s.retrieverContext(r.Context())
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	var economics retrieve.Economics
	cards, err := rt.Retrieve(r.Context(), q, retrieve.Options{Limit: limit, Budget: budget, AllowedScopes: s.allowedScopes(r), AllowPath: s.allowedPath(r), Economics: &economics})
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	receiptTokens := economics.ReceiptTokens()
	if receiptTokens > 0 {
		cardBudget := budget - receiptTokens
		if cardBudget <= 0 {
			cards = nil
		} else {
			cards = retrieve.PackToBudget(cards, cardBudget, nil)
		}
	}
	economics.ReturnedCards = len(cards)
	economics.ReturnedTokens = retrieve.TotalTokens(cards) + receiptTokens
	rt.RecordEconomics(economics)
	_ = s.store.IncrMetric("queries", 1) // ROI telemetry (best-effort)
	result := map[string]any{"cards": cards, "tokens": economics.ReturnedTokens}
	if economics.Fallback {
		result["rerank"] = economics.Receipt()
	}
	if economics.SemanticFallback {
		result["semantic"] = economics.SemanticReceipt()
	}
	writeJSON(w, result)
}

// handleNote returns one note's raw markdown by frontmatter id, the browser
// equivalent of mesh_fetch. Indexed path/scope are read atomically, the file is
// opened through confined directory handles, and its current scope is rechecked.
func (s *Server) handleNote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	metadata, err := s.store.NoteMetadataFor(r.Context(), []string{"note:" + id})
	m, ok := metadata["note:"+id]
	if err != nil || !ok || !filepath.IsLocal(m.Path) {
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "unknown note id", http.StatusNotFound)
		return
	}
	rel := m.Path
	// Folder read check, before the scope one: the path is already resolved and a team
	// can fence folders without defining a single scope, in which case the scope set is
	// nil and this is the only boundary there is.
	if !s.canReadPath(r, rel) {
		http.Error(w, "unknown note id", http.StatusNotFound)
		return
	}
	// Scope read check: opaque 404 (same as a missing note) so a scoped member cannot
	// probe which ids exist outside their scope.
	allowed := s.allowedScopes(r)
	if allowed != nil && !vault.ScopeAllowsCSV(m.Scope, allowed) {
		http.Error(w, "unknown note id", http.StatusNotFound)
		return
	}
	data, err := vault.ReadConfinedFileContext(r.Context(), s.vaultRoot, rel, maxWebNoteBytes)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, vault.ErrConfinedFileTooLarge) {
			http.Error(w, "note is too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "read failed", http.StatusInternalServerError)
		}
		return
	}
	if r.Context().Err() != nil {
		return
	}
	// Split the YAML frontmatter off before rendering, so the reader only shows prose,
	// not "id: ...\ntitle: ..." dumped as markdown text above the note. markdown stays
	// the full raw file (unchanged) for any existing caller; body/meta are the new,
	// separated pieces. Unscoped local viewing keeps the tolerant parse behavior;
	// scoped callers must also be authorized by this exact file's frontmatter.
	fmYAML, body, _ := vault.SplitFrontmatter(string(data))
	meta := map[string]any{}
	fm, _, ferr := vault.ParseFrontmatter([]byte(fmYAML))
	if allowed != nil && (ferr != nil || vault.UnterminatedFrontmatter(string(data)) || !scopeIntersect(fm.EffectiveScopes(), allowed)) {
		http.Error(w, "unknown note id", http.StatusNotFound)
		return
	}
	if ferr != nil {
		slog.Warn("mesh ui: note frontmatter did not parse", "id", id, "path", rel, "error", ferr)
	} else {
		meta = map[string]any{
			"title": fm.Title, "type": fm.Type, "when": fm.When, "severity": fm.Severity,
			"tags": fm.Tags, "scope": fm.Scope, "confidence": fm.Confidence, "source": fm.Source,
			"related": fm.Related, "template": fm.Template, "template_version": fm.TemplateVersion,
			"summary": fm.Summary, "collections": fm.Collections, "status": fm.Status, "supersedes": fm.Supersedes,
		}
		if authored, aerr := vault.ReadAuthoring(fm, body); aerr == nil {
			meta["summary"] = authored.Summary
		}
	}
	if r.Context().Err() != nil {
		return
	}
	// html is server-rendered (gomarkdown) from the body only, so the frontmatter never
	// shows up as prose above the content. Note bodies are UNTRUSTED (ingested connector
	// content can carry raw HTML), so render with the sanitising path; markdown is kept
	// verbatim (full file, frontmatter included) for any raw consumer.
	writeJSON(w, map[string]any{
		"id": id, "path": rel,
		"markdown": string(data),
		"body":     body,
		"meta":     meta,
		"html":     renderMDSafe([]byte(body)),
	})
}

// scopeIntersect reports whether a note's scopes intersect the allowed set. Empty
// scopes = the dev fail-safe default. Delegates to the one shared predicate so this
// surface cannot drift from the MCP/retrieve scope checks.
func scopeIntersect(scopes []string, allowed map[string]bool) bool {
	return vault.ScopeAllows(scopes, allowed)
}

func atoiOr(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return def
}
