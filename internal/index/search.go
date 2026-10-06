// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/vault"
	"modernc.org/sqlite"
)

func init() {
	// Register for every subsequently opened reader/writer connection. This is
	// query-time navigation, not a schema function or a new persisted index.
	sqlite.MustRegisterDeterministicScalarFunction("mesh_full_title_match", 2,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			query, queryOK := args[0].(string)
			title, titleOK := args[1].(string)
			if queryOK && titleOK && graph.MatchesFullTitle(query, title) {
				return int64(1), nil
			}
			return int64(0), nil
		})
}

// SearchTimeout is the server-side deadline applied to an FTS read whose caller
// brought no deadline of its own (the CLI, the TUI, the eval harness). It exists so
// a pathological corpus degrades into a named error a user can act on instead of a
// process pinned at 100% CPU with nothing to cancel it. A caller that already has a
// deadline, such as an HTTP request or an agent tool call, keeps its own.
const SearchTimeout = 10 * time.Second

// searchExcerptTokens is the excerpt width for note hits, matching the token count
// the FTS5 snippet() call used before the excerpt moved into Go.
const searchExcerptTokens = 12

// withSearchDeadline returns ctx unchanged when it already carries a deadline, and
// otherwise bounds it with SearchTimeout.
func withSearchDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, SearchTimeout)
}

// searchErr names the remedy when a search is cut short. A bare
// "context deadline exceeded" tells a user nothing about what to do next, and the
// driver reports an interrupted statement rather than the context error, so the
// context is consulted as well as the error.
//
// The guard below is a disjunction over (err, ctx.Err()), but downstream callers
// (retrievalErr in internal/mcp) classify purely via errors.Is against the context
// sentinels. Wrapping only err with %w is correct when err itself already carries
// the sentinel, but when the match comes ONLY from ctx.Err() - exactly the case
// this function's comment above describes, a driver that reports its own
// "interrupted" error rather than surfacing the context error - that same wrap
// would drop the sentinel from the returned chain and the caller would fall
// through to an opaque error. So that branch wraps the sentinel itself instead,
// and keeps the driver error visible alongside it with %v for operators reading
// logs: informative, but deliberately not part of the Unwrap chain a caller could
// accidentally match on instead of the sentinel.
func searchErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("search timed out after %s: use fewer, more specific words, or split your largest note files into smaller ones: %w", SearchTimeout, err)
		}
		return fmt.Errorf("search timed out after %s: use fewer, more specific words, or split your largest note files into smaller ones: %w (driver: %v)", SearchTimeout, context.DeadlineExceeded, err)
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("search cancelled: %w", err)
		}
		return fmt.Errorf("search cancelled: %w (driver: %v)", context.Canceled, err)
	}
	return err
}

// SearchHit is one FTS5 result over the note corpus.
type SearchHit struct {
	NodeID  string // note:<id>
	Title   string
	Path    string  // vault-relative path of the owning note
	Snippet string  // bracketed match excerpt from the body
	Score   float64 // normalized so higher is more relevant (negated bm25)
}

// Search runs an FTS5 MATCH over search_index and returns the most relevant
// notes. User input is sanitized into quoted literal tokens so FTS5's reserved
// grammar (NEAR/OR/NOT/AND/*/parens) can never break the parser or inject.
// Unrestricted: see SearchScoped for the access-controlled form.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	return s.SearchScoped(ctx, query, limit, nil)
}

// SearchScoped is Search restricted to notes whose scope intersects allowed
// (nil = unrestricted, the solo / no-ACL fast path).
//
// The scope predicate lives IN the SQL rather than in a post-filter on purpose:
// LIMIT is applied by SQLite after WHERE, so filtering afterwards let a run of
// higher-ranked unreadable notes consume the whole limit and starve a scoped
// caller down to zero hits even though readable matches existed. Filtering here
// means the limit counts only rows the caller may actually read.
func (s *Store) SearchScoped(ctx context.Context, query string, limit int, allowed map[string]bool) ([]SearchHit, error) {
	return s.SearchScopedPaths(ctx, query, limit, allowed, nil)
}

// SearchScopedPaths applies both the scope predicate and an optional vault-path read
// predicate while consuming the ranked result stream. The path rule is a callback and
// cannot be represented safely as SQL, so the query is left unbounded and scanning
// stops as soon as limit readable rows have been found. Applying it after SQL LIMIT
// lets stronger fenced rows consume the whole candidate budget and starve readable
// matches. Each hit's path and body still come from the same SQLite snapshot.
func (s *Store) SearchScopedPaths(ctx context.Context, query string, limit int, allowed map[string]bool, allowPath func(string) bool) ([]SearchHit, error) {
	terms := graph.TokenizeQuery(query)
	match := fts5Match(terms)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	scopeSQL, scopeArgs, readable := scopePredicate(allowed)
	if !readable {
		// Scoping is on but the caller may read nothing: no row can qualify.
		return nil, nil
	}
	// The body prefix, not snippet(): see buildExcerpt for why asking FTS5 to pick the
	// window is what made one repetitive note hang the whole product. substr runs in
	// SQLite so a multi-megabyte body is never carried into Go, and it counts
	// characters, so it cannot cut a rune in half. MATCH and bm25 still see the WHOLE
	// body; only the excerpt is bounded. An explicit full-title candidate precedes
	// BM25 candidates before LIMIT without changing its numerical relevance score.
	q := `
SELECT si.node_id, si.title,
       COALESCE(n.path, ''),
       substr(si.body, 1, ` + strconv.Itoa(excerptScanChars) + `),
       bm25(search_index)
FROM search_index si
LEFT JOIN notes n ON n.id = substr(si.node_id, 6)
WHERE search_index MATCH ?` + scopeSQL + draftPredicate + `
ORDER BY mesh_full_title_match(?, n.title) DESC, bm25(search_index), si.node_id`
	args := make([]any, 0, len(scopeArgs)+3)
	args = append(args, match)
	args = append(args, scopeArgs...)
	args = append(args, strings.TrimSpace(query))
	if allowPath == nil {
		q += `
LIMIT ?`
		args = append(args, limit)
	}

	ctx, cancel := withSearchDeadline(ctx)
	defer cancel()
	rows, err := s.readDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, searchErr(ctx, err)
	}
	defer rows.Close()

	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var body string
		var rank float64
		if err := rows.Scan(&h.NodeID, &h.Title, &h.Path, &body, &rank); err != nil {
			return nil, searchErr(ctx, err)
		}
		if allowPath != nil && (h.Path == "" || !allowPath(h.Path)) {
			continue
		}
		h.Snippet = buildExcerpt(trimExcerptScanTail(body), terms, searchExcerptTokens)
		// FTS5 bm25 returns lower-is-better; negate so the fuser (M1 step 3)
		// can treat all signals as higher-is-better.
		h.Score = -rank
		out = append(out, h)
		if allowPath != nil && len(out) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, searchErr(ctx, err)
	}
	return out, nil
}

// scopePredicate renders the notes.scope read filter as a parameterized SQL
// fragment plus its bind values. It returns readable=false when scoping is on but
// the allowed set is empty, i.e. no row can qualify and the caller should skip the
// query entirely.
//
// notes.scope holds the effective scopes comma-joined, so membership is tested by
// wrapping both haystack and needle in commas and using instr(), NOT LIKE: instr
// is a literal substring test, so a scope name containing % or _ cannot widen the
// match the way a LIKE pattern would. A NULL (outer-joined) or empty scope falls
// back to vault.DefaultScope, the same fail-safe vault.ScopeAllows applies to an
// unlabeled note.
func scopePredicate(allowed map[string]bool) (sql string, args []any, readable bool) {
	if allowed == nil {
		return "", nil, true // unrestricted: scoping not configured
	}
	names := make([]string, 0, len(allowed))
	for s, ok := range allowed {
		if t := strings.TrimSpace(s); ok && t != "" {
			names = append(names, t)
		}
	}
	if len(names) == 0 {
		return "", nil, false
	}
	sort.Strings(names) // deterministic SQL text, so the prepared-statement cache hits
	conds := make([]string, 0, len(names))
	for _, n := range names {
		conds = append(conds, "instr(',' || COALESCE(NULLIF(n.scope, ''), ?) || ',', ?) > 0")
		args = append(args, vault.DefaultScope, ","+n+",")
	}
	return " AND (" + strings.Join(conds, " OR ") + ")", args, true
}

// buildFTS5Query turns raw user input into an FTS5 MATCH expression. It uses the
// shared graph query tokenizer (lowercase, unicode boundaries, stopwords dropped,
// duplicates dropped, capped at graph.MaxQueryTerms) so FTS and graph-BM25 see the
// same terms, then joins them with OR: an agent's natural-language query ("how do we
// store data") should recall any note that matches a content word and let bm25 rank,
// not require that every word be present AND-style. Reserved FTS grammar can't leak
// because each term is quoted: an alphanumeric token or a bounded version phrase.
// Phrases retain component order (including one-digit patches) with the existing
// unicode61 index. They are token-exact, not punctuation-exact or SemVer filters;
// ordinary OR terms still recall related warnings and absent-version context.
// Empty input returns "" so the
// caller short-circuits.
//
// The cap is load-bearing, not hygiene: FTS5 evaluates one phrase per OR term, so a
// 1 MiB query (accepted by the hub's 1 MiB request body limit, from any peer holding
// a valid team token) turned into ~100k phrases and 4 minutes 9 seconds of
// single-core CPU on a 500-note vault, running to completion after the client had
// already hung up.
func buildFTS5Query(q string) string {
	return fts5Match(graph.TokenizeQuery(q))
}

// fts5Match renders already-tokenized query terms as an FTS5 MATCH expression. Split
// out from buildFTS5Query so a caller that also needs the terms themselves, to build
// the result excerpt, tokenizes exactly once and cannot drift from what was matched.
func fts5Match(terms []string) string {
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		out = append(out, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	return strings.Join(out, " OR ")
}
