// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bright-interaction/mesh/internal/hooks"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/latency"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/teamtelemetry"
	"github.com/bright-interaction/mesh/internal/vault"
	"golang.org/x/text/unicode/norm"
)

func obj(m map[string]any) map[string]any { return m }

func (s *Server) handleToolsList() any { return map[string]any{"tools": ToolSpecs()} }

// ToolSpecs returns the MCP tool definitions (name, description, inputSchema). It
// is the single source for both the MCP tools/list response and the web app's API
// reference, so the two never drift.
func ToolSpecs() []map[string]any {
	str := map[string]any{"type": "string"}
	intp := map[string]any{"type": "integer"}
	strList := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}

	tools := []map[string]any{
		{
			"name":        "mesh_search",
			"description": "Search via text, graph and optional vector/subscription ranking. Defaults: budget 8000, limit 20 (max 100). MissingGuidance flags unfilled fields; Tier0 denotes type, not verification.",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"query"},
				"properties": map[string]any{"query": str, "budget": intp, "limit": intp},
			}),
		},
		{
			"name":        "mesh_fetch",
			"description": "Fetch a note by id; an optional unique heading anchor includes bounded safety-context excerpts and an incomplete-context warning. Use only when its search card is insufficient.",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"id"},
				"properties": map[string]any{"id": str, "anchor": str},
			}),
		},
		{
			"name":        "mesh_fetch_many",
			"description": "Batch-fetch 1-16 note/anchor items; deduplicates notes and sections. Workers exit before return. Budget 256-32000 response tokens (default 8000); omitted lists input indices excluded by budget.",
			"inputSchema": obj(map[string]any{
				"type": "object", "required": []string{"items"},
				"properties": map[string]any{
					"items":  obj(map[string]any{"type": "array", "minItems": 1, "maxItems": 16, "items": obj(map[string]any{"type": "object", "required": []string{"id"}, "properties": map[string]any{"id": str, "anchor": str}})}),
					"budget": intp,
				},
			}),
		},
		{
			"name":        "mesh_god_nodes",
			"description": "List the most-connected notes as orientation entry points.",
			"inputSchema": obj(map[string]any{"type": "object", "properties": map[string]any{"limit": intp}}),
		},
		{
			"name":        "mesh_changed_since",
			"description": "List notes modified after a Unix-seconds timestamp (default limit 100, max 500; truncated signals more).",
			"inputSchema": obj(map[string]any{"type": "object", "required": []string{"since"}, "properties": map[string]any{"since": intp, "limit": intp}}),
		},
		{
			"name":        "mesh_neighbors",
			"description": "List a note's typed inbound and outbound graph neighbors to a small depth.",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"id"},
				"properties": map[string]any{"id": str, "depth": intp, "limit": intp},
			}),
		},
		{
			"name":        "mesh_community",
			"description": "Show one note's community, or omit id for a cluster overview.",
			"inputSchema": obj(map[string]any{"type": "object", "properties": map[string]any{"id": str, "limit": intp}}),
		},
		{
			"name":        "mesh_reindex",
			"description": "Rebuild the index after direct note-file edits; Mesh write tools already reindex automatically.",
			"inputSchema": obj(map[string]any{"type": "object", "properties": map[string]any{}}),
		},
		{
			"name":        "mesh_health",
			"description": "Report dead source references, overdue reviews, contradictions, and stale-index status.",
			"inputSchema": obj(map[string]any{"type": "object", "properties": map[string]any{"issue": str}}),
		},
		{
			"name":        "mesh_code_search",
			"description": "Find source-code symbols by name and return file, line, and signature (default limit 12, max 100).",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"query"},
				"properties": map[string]any{"query": str, "limit": intp, "languages": strList},
			}),
		},
		{
			"name":        "mesh_code_neighbors",
			"description": "Show callers and callees for a code-symbol id; Go has the full call graph.",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"id"},
				"properties": map[string]any{"id": str},
			}),
		},
		{
			"name":        "mesh_code_context",
			"description": "Find code symbols and the team knowledge notes that reference them.",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"query"},
				"properties": map[string]any{"query": str, "limit": intp},
			}),
		},
		{
			"name":        "mesh_secret_status",
			"description": "Report whether a capability-mode secret vault is attached; never returns secret values.",
			"inputSchema": obj(map[string]any{"type": "object", "properties": map[string]any{}}),
		},
		{
			"name":        "mesh_secret_list",
			"description": "List secret names, providers, and rotation status only; never returns values.",
			"inputSchema": obj(map[string]any{"type": "object", "properties": map[string]any{}}),
		},
		{
			"name":        "mesh_secret_use",
			"description": "Mint a short-lived, single-use capability bound to a destination and method; never store or log it.",
			"inputSchema": obj(map[string]any{
				"type":     "object",
				"required": []string{"destination"},
				"properties": map[string]any{
					"destination": str, "secret_name": str, "method": str, "ttl_seconds": intp,
				},
			}),
		},
		{
			"name":        "mesh_setup_hooks",
			"description": "Inspect, install, or remove Claude Code session hooks; use dry_run to preview and read_only to omit write-back nudges.",
			"inputSchema": obj(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action":      map[string]any{"type": "string", "enum": []string{"status", "install", "uninstall"}},
					"project_dir": str,
					"read_only":   map[string]any{"type": "boolean"},
					"dry_run":     map[string]any{"type": "boolean"},
				},
			}),
		},
	}
	return append(tools, authoringToolSpecs()...)
}

// Contract returns the agent-usage contract text (how to retrieve cheaply), shared
// by the MCP initialize instructions and the web app's API reference.
func Contract() string { return contractText }

// ToolNames returns just the tool names in ToolSpecs() order. The mesh://capabilities
// resource uses it so its advertised tool list can never drift from the real surface
// (it used to hardcode a slice that silently went stale on every new tool).
func ToolNames() []string {
	specs := ToolSpecs()
	names := make([]string, 0, len(specs))
	for _, t := range specs {
		if n, ok := t["name"].(string); ok {
			names = append(names, n)
		}
	}
	return names
}

// toolClass is how a tool relates to scope-RBAC. It is the choke point that stops the
// recurring "a new read tool leaks across scopes by default" bug (changed_since /
// health / code_* each shipped that way and were patched one at a time). EVERY tool
// must be classified here: TestEveryToolIsScopeClassified fails at build time if a tool
// in ToolSpecs()/the dispatch has no class, and handleToolsCall refuses an unclassified
// tool at runtime (fail closed). The per-handler filtering still does the fine-grained
// work; this map forces a conscious scope decision for every tool that ships.
type toolClass int

const (
	// classFiltered: a read tool that returns only the caller's readable SUBSET (it
	// consults scopeFromCtx and filters per note / opaque-404s an out-of-scope id).
	classFiltered toolClass = iota
	// classCodeDev: reads the code index, which carries no per-note scope and is treated
	// as dev-scoped in whole. The handler denies the ENTIRE call when the caller cannot
	// read the dev scope (codeScopeDenied).
	classCodeDev
	// classWrite: creates a note; the handler write-gates and stamps the caller's scope.
	classWrite
	// classOpen: no vault content crosses a scope boundary (a local operator action).
	classOpen
)

// toolScopeClass MUST contain every tool name in ToolSpecs(). Add a new tool here at
// the same time you add it to ToolSpecs() and the dispatch, having decided how it
// relates to scope. The test + the runtime check below both fail closed otherwise.
var toolScopeClass = map[string]toolClass{
	"mesh_prepare_update":   classWrite,
	"mesh_drafts":           classFiltered,
	"mesh_templates":        classOpen,
	"mesh_note_template":    classOpen,
	"mesh_block_template":   classOpen,
	"mesh_author_note":      classWrite,
	"mesh_propose_template": classWrite,
	"mesh_search":           classFiltered,
	"mesh_fetch":            classFiltered,
	"mesh_fetch_many":       classFiltered,
	"mesh_god_nodes":        classFiltered,
	"mesh_changed_since":    classFiltered,
	"mesh_neighbors":        classFiltered,
	"mesh_community":        classFiltered,
	"mesh_reindex":          classFiltered,
	"mesh_health":           classFiltered,
	"mesh_append_note":      classWrite,
	"mesh_write_entity":     classWrite,
	"mesh_code_search":      classCodeDev,
	"mesh_code_neighbors":   classCodeDev,
	"mesh_code_context":     classCodeDev,
	"mesh_setup_hooks":      classOpen,
	// Secret-broker tools broker an ATTACHED Dockyard vault, not vault-note content, so
	// no per-note scope crosses a boundary (classOpen). All three apply the write-role
	// gate inside their handler: minting a token spends the team's credential, and
	// listing or status disclose the vault inventory and the broker endpoint, none of
	// which a read-only hosted viewer should reach.
	"mesh_secret_status": classOpen,
	"mesh_secret_list":   classOpen,
	"mesh_secret_use":    classOpen,
}

func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "bad params"}
	}
	// Fail closed on any tool that was never scope-classified: a dispatched tool missing
	// from toolScopeClass must not run, so a new tool cannot silently bypass the gate.
	if _, classified := toolScopeClass[p.Name]; !classified {
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown tool", Data: p.Name}
	}
	switch p.Name {
	case "mesh_prepare_update":
		return s.toolPrepareUpdate(ctx, p.Arguments)
	case "mesh_drafts":
		return s.toolDrafts(ctx, p.Arguments)
	case "mesh_templates":
		return textResult(templateCatalog()), nil
	case "mesh_note_template":
		return s.toolTemplate(p.Arguments, false)
	case "mesh_block_template":
		return s.toolTemplate(p.Arguments, true)
	case "mesh_author_note":
		return s.toolAuthorNote(ctx, p.Arguments)
	case "mesh_propose_template":
		return s.toolProposeTemplate(ctx, p.Arguments)
	case "mesh_search":
		return s.toolSearch(ctx, p.Arguments)
	case "mesh_fetch":
		return s.toolFetch(ctx, p.Arguments)
	case "mesh_fetch_many":
		return s.toolFetchMany(ctx, p.Arguments)
	case "mesh_god_nodes":
		return s.toolGodNodes(ctx, p.Arguments)
	case "mesh_changed_since":
		return s.toolChangedSince(ctx, p.Arguments)
	case "mesh_neighbors":
		return s.toolNeighbors(ctx, p.Arguments)
	case "mesh_community":
		return s.toolCommunity(ctx, p.Arguments)
	case "mesh_append_note":
		return s.toolWrite(ctx, p.Arguments, "")
	case "mesh_write_entity":
		return s.toolWrite(ctx, p.Arguments, "entity")
	case "mesh_reindex":
		return s.toolReindex(ctx)
	case "mesh_health":
		return s.toolHealth(ctx, p.Arguments)
	case "mesh_code_search":
		return s.toolCodeSearch(ctx, p.Arguments)
	case "mesh_code_context":
		return s.toolCodeContext(ctx, p.Arguments)
	case "mesh_code_neighbors":
		return s.toolCodeNeighbors(ctx, p.Arguments)
	case "mesh_secret_status":
		return s.toolSecretStatus(ctx)
	case "mesh_secret_list":
		return s.toolSecretList(ctx, p.Arguments)
	case "mesh_secret_use":
		return s.toolSecretUse(ctx, p.Arguments)
	case "mesh_setup_hooks":
		return s.toolSetupHooks(ctx, p.Arguments)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown tool", Data: p.Name}
	}
}

// toolReindex makes an agent's direct file edits (via the editor or CLI) queryable on
// demand, instead of waiting on the --watch debounce or restarting a no-watch server.
// Authoritative = full content-hash check, so it also catches an edit that did not move
// the mtime.
//
// On a read-only server it does not reindex anything, because it cannot: it measures what
// the vault has drifted by, waits for the single owning writer to absorb that, and
// re-reads the result. Same observable contract ("after this returns, your edits are
// queryable"), reached the only way a process without the write lock can reach it.
func (s *Server) toolReindex(ctx context.Context) (any, *rpcError) {
	// Role write gate: a reindex content-hashes the whole vault and re-indexes the code
	// roots, so it is not a read even though what it returns is (and on a writable store
	// it writes the result too). A read-only hosted viewer must not be able to drive it.
	if can, set := writeAllowed(ctx); set && !can {
		return nil, &rpcError{Code: codeInvalidParams, Message: "forbidden: your role is read-only"}
	}
	// Throttle remote callers only (see reconcileThrottled): the hosted transports are
	// where back-to-back passes contend with the hub's post-sync reconcile worker for
	// reloadMu. The local operator's "reindex now" still means now.
	rec, throttled, err := s.reconcileThrottled(ctx, !localOperator(ctx))
	ownerDown := errors.Is(err, ErrOwnerNotIndexing)
	if err != nil && !ownerDown {
		return nil, internalErr(err)
	}
	// Count from the graph THIS reconcile produced, not a fresh snapshot() that a
	// concurrent watcher tick could have swapped underneath us, so the reported counts
	// always describe this call's result. Fall back to the snapshot on a no-op pass
	// (rec.Graph is nil when nothing changed) and on a throttled replay, where the
	// remembered graph pointer may predate a watcher swap.
	g := rec.Graph
	if g == nil || throttled {
		g, _ = s.snapshot()
	}
	out := map[string]any{
		"reindexed": rec.Reindexed,
		"ms":        rec.Dur.Milliseconds(),
	}
	if throttled {
		out["throttled"] = true
		out["note"] = "a reindex ran moments ago; this is that pass's result. Retry in a few seconds to force a fresh one."
	}
	// The one outcome mesh_reindex must never round up to success: this server does not
	// index, it waits for the owning writer to, and the wait ran out. Same vocabulary as a
	// write-back that could not be published, because it is the same failure and the same
	// remedy, and the counts above describe what DID land, not what is still missing.
	if ownerDown {
		out["index_stale"] = true
		out["owner_down"] = true
		out["warning"] = "Your edits on disk are NOT queryable yet: the single owning writer did " +
			"not index them inside the wait, and this server only re-reads what that writer " +
			"persists. The usual cause is that `mesh watch` / `mesh sync --watch` is not " +
			"running, so check that first. Calling this tool again will not help until one is: " +
			"start an owner (or run `mesh index <vault>` once) and the edits are picked up as " +
			"soon as it runs."
	}
	// A scope-confined caller must not learn out-of-scope volume: the global graph
	// totals AND the reconcile deltas (added/changed/removed, which span every scope)
	// leak it. Report only the caller's readable view; an unrestricted caller (nil
	// filter or nil AllowedRead, e.g. an admin or a solo run) gets the full numbers.
	if sf := scopeFromCtx(ctx); sf != nil && sf.AllowedRead != nil {
		out["nodes"], out["edges"] = scopedGraphCounts(g, sf)
	} else {
		out["added"] = rec.Added
		out["changed"] = rec.Changed
		out["removed"] = rec.Removed
		out["nodes"] = g.NodeCount()
		out["edges"] = g.EdgeCount()
	}
	// Refresh the source-code index too, so one mesh_reindex catches both note and
	// code edits. ok=false means code indexing is not enabled for this vault. The
	// counts are only returned to callers who may read the (dev-scoped) code index;
	// a scope-confined caller must not learn the code corpus volume (still refreshed,
	// just not reported), mirroring the code_search/neighbors gate. Skipped entirely on
	// a throttled replay: re-walking the code roots is the other half of the cost the
	// cooldown exists to bound.
	if !throttled {
		if cr, cerr := s.refreshCode(); cerr == nil && cr.enabled && !codeScopeDenied(ctx) {
			out["code_files"] = cr.stats.Files
			out["code_symbols"] = cr.stats.Symbols
			out["code_edges"] = cr.stats.Edges
			if cr.note != "" {
				out["code_index"] = cr.note
			}
		}
	}
	return textResult(out), nil
}

// computedHealthIssue are the issue kinds the health pass derives itself. Everything
// else in note_health belongs to another writer (the curator), so a server that computes
// its own findings still reads those back rather than dropping them.
var computedHealthIssue = map[string]bool{"dead_ref": true, "overdue": true, "contradiction": true}

// healthPass produces the current findings and their counts by whichever route this
// server is allowed to take.
//
// A writable store runs the pass and PERSISTS it, which is what feeds the web dashboard
// and the curator. A read-only store (a `mesh mcp` window beside another owner) computes exactly the same
// findings in memory and writes nothing: the analysis is a read over the vault plus this
// index, and only recording it is the owning writer's privilege. Serving the persisted
// rows instead would answer with whatever the owner last wrote, which on a vault whose
// owner never runs the pass is nothing at all, and mesh_health would report a clean vault
// by virtue of never having looked.
func (s *Server) healthPass(now time.Time, issue string) ([]index.HealthFinding, map[string]int, *rpcError) {
	if !s.owns() {
		findings, err := s.store.ScanHealth(s.vaultRoot, now)
		if err != nil {
			return nil, nil, internalErr(err)
		}
		contradictions, err := s.store.ScanContradictions()
		if err != nil {
			return nil, nil, internalErr(err)
		}
		findings = append(findings, contradictions...)
		persisted, err := s.store.ListHealth("")
		if err != nil {
			return nil, nil, internalErr(err)
		}
		for _, f := range persisted {
			if !computedHealthIssue[f.Issue] {
				findings = append(findings, f)
			}
		}
		// Counts are global (the caller's issue filter narrows the findings, not the
		// counts), matching HealthCounts on the writable path. Ordering matches
		// ListHealth's so the two routes are indistinguishable to a caller.
		counts := make(map[string]int, len(findings))
		for _, f := range findings {
			counts[f.Issue]++
		}
		sort.Slice(findings, func(i, j int) bool {
			if findings[i].Issue != findings[j].Issue {
				return findings[i].Issue < findings[j].Issue
			}
			return findings[i].NoteID < findings[j].NoteID
		})
		if issue != "" {
			kept := findings[:0]
			for _, f := range findings {
				if f.Issue == issue {
					kept = append(kept, f)
				}
			}
			findings = kept
		}
		return findings, counts, nil
	}
	if _, err := s.store.ComputeHealth(s.vaultRoot, now); err != nil {
		return nil, nil, internalErr(err)
	}
	if _, err := s.store.ComputeContradictions(now); err != nil {
		return nil, nil, internalErr(err)
	}
	findings, err := s.store.ListHealth(issue)
	if err != nil {
		return nil, nil, internalErr(err)
	}
	counts, _ := s.store.HealthCounts()
	if counts == nil {
		counts = map[string]int{}
	}
	return findings, counts, nil
}

// toolHealth runs the lifecycle health pass (dead refs + overdue reviews) and
// returns the findings grouped by issue plus the current counts (incl. any
// contradiction rows the curator wrote).
func (s *Server) toolHealth(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Issue string `json:"issue"`
	}
	_ = json.Unmarshal(raw, &a)
	// Role write gate: the health pass reads EVERY note file in the vault (and on a
	// writable store persists the findings + contradiction rows), so like mesh_reindex it
	// is an expensive operation wearing a read tool's clothes. A read-only hosted viewer
	// must not be able to drive it repeatedly.
	if can, set := writeAllowed(ctx); set && !can {
		return nil, &rpcError{Code: codeInvalidParams, Message: "forbidden: your role is read-only"}
	}
	// Lifecycle queries touch only a few tables and can miss corruption in an unrelated
	// b-tree page. A complete clean-vault claim requires a whole-index integrity check.
	if err := s.store.CheckIntegrity(s.vaultRoot); err != nil {
		if errors.Is(err, index.ErrIndexCorrupt) {
			slog.Error("mesh mcp: health found a corrupt index", "err", err)
			return nil, &rpcError{Code: codeInternalError, Message: "mesh_health cannot verify this vault because its index database is corrupt. " +
				"Markdown notes are unchanged, but pending review notes, usage/reuse history and stored embeddings cannot be recovered from Markdown. " +
				"Before repair, obtain approval to stop the vault services and preserve a consistent backup. Prefer a verified restore. " +
				"Only after accepting database-only state loss, run `mesh index <vault>` in a terminal to discard and rebuild the index."}
		}
		return nil, internalErr(err)
	}
	// Every lifecycle pass below starts from the notes table. If the Markdown vault has
	// moved ahead of it, a newly-added or changed note is absent from the very rows being
	// inspected and an empty result is not a clean bill of health. Report the operational
	// failure first and ask for a reindex; do not persist findings computed from stale rows.
	drift, err := s.store.DriftReport(s.vaultRoot)
	if err != nil {
		return nil, internalErr(err)
	}
	if drift.Any() {
		counts := map[string]int{index.StaleIndexIssue: 1}
		all := []index.HealthFinding{{
			Issue:  index.StaleIndexIssue,
			Detail: "the index does not match the Markdown vault, so lifecycle findings would be incomplete. Fix: call mesh_reindex now (or run `mesh index <vault>`).",
		}}
		if owner, missing := s.ownerFinding(); missing {
			all = append(all, owner)
			counts[index.NoOwnerIssue]++
		}
		findings := all
		if issue := strings.TrimSpace(a.Issue); issue != "" {
			findings = findings[:0]
			for _, finding := range all {
				if finding.Issue == issue {
					findings = append(findings, finding)
				}
			}
		}
		return textResult(map[string]any{"findings": findings, "counts": counts}), nil
	}
	findings, counts, rerr := s.healthPass(time.Now(), strings.TrimSpace(a.Issue))
	if rerr != nil {
		return nil, rerr
	}
	// Unparseable notes never made it into the DB, which is exactly the bug: a note
	// with broken frontmatter vanishes from search and the graph with no signal.
	// Surface the ones the last reindex dropped so an operator can find and fix the
	// note that disappeared. They carry no note id or scope (they never parsed), so
	// they are operational findings, always shown, never scope-filtered out.
	//
	// DroppedNotes carries two distinct causes with two different remedies, so they get
	// two issue kinds rather than one misleading label: "unparseable" means fix the
	// frontmatter, "duplicate-id" means the note was quarantined because another note
	// already holds its id and one of the two needs a new one.
	//
	// A failure to READ that record is reported, never treated as "nothing dropped":
	// answering {"counts":{},"findings":null} to a question the tool could not answer is
	// how an index written by an older Mesh passed as a clean vault. The message names the
	// remedy rather than going through internalErr, whose deliberately generic "internal
	// error" left the cause on stderr where the agent client hides it.
	dropped, derr := s.store.DroppedNotes()
	if derr != nil {
		slog.Error("mesh mcp: health could not read the dropped-note record", "err", derr)
		return nil, &rpcError{Code: codeInternalError, Message: "mesh_health cannot read which notes the index dropped, " +
			"so it cannot tell you whether the vault is clean. The index was written by a different version of Mesh " +
			"and no read-only surface can migrate it. With a compatible binary and a consistent backup, run `mesh index <vault>` in a terminal. " +
			"A supported schema rebuild preserves pending reviews; do not delete the database or downgrade a newer schema by hand."}
	}
	for _, d := range dropped {
		detail := ""
		if d.Err != nil {
			// The THIRD copy of the same shape, and the one that survived the sweep that
			// closed the other two: a relativized Path sitting next to a raw error, which
			// reads as already handled. These errors come from ParseFile, which os.ReadFile
			// calls with the ABSOLUTE path, so an unreadable note answered
			// detail = "open <abs vault root>/decisions/x.md: permission denied" while the
			// path field beside it said "decisions/x.md". Duplicate-id details name only
			// vault-relative paths, so the scrub leaves those untouched.
			detail = ScrubPathsUnder(d.Err.Error(), s.vaultRoot)
		}
		kind := "unparseable"
		if errors.Is(d.Err, index.ErrDuplicateNoteID) {
			kind = "duplicate-id"
		}
		if a.Issue != "" && a.Issue != kind {
			continue
		}
		findings = append(findings, index.HealthFinding{Issue: kind, Path: d.Path, Detail: detail})
		counts[kind]++
	}
	// No owning writer is a health finding about the VAULT's setup, not about a note, and
	// it is the most consequential one this tool can report: with nothing owning the
	// index, every note written from here (and every note edited in an editor) stays
	// unqueryable, silently. It used to be invisible, which is how a shipped agent config
	// that could never index anything survived for days.
	if a.Issue == "" || a.Issue == index.NoOwnerIssue {
		if f, missing := s.ownerFinding(); missing {
			findings = append(findings, f)
			counts[index.NoOwnerIssue]++
		}
	}
	// Scope read check: findings carry a note id + path, so an unfiltered health pass
	// leaks the existence and paths of notes outside the caller's scope (and the global
	// counts do the same in aggregate). Drop out-of-scope findings and recompute counts
	// from what is left. A nil filter (solo / no-scope hub) leaves both untouched.
	if sf := scopeFromCtx(ctx); sf != nil {
		kept := findings[:0]
		scoped := make(map[string]int, len(counts))
		for _, f := range findings {
			if f.Issue == "unparseable" || f.Issue == index.NoOwnerIssue {
				// No parsed note, so no scope to check: an operational finding shown to all.
				kept = append(kept, f)
				scoped[f.Issue]++
				continue
			}
			sc, serr := s.store.NoteScope(f.NoteID)
			if serr != nil || !sf.allowsRead(sc) {
				continue
			}
			kept = append(kept, f)
			scoped[f.Issue]++
		}
		findings = kept
		counts = scoped
	}
	return textResult(map[string]any{"findings": findings, "counts": counts}), nil
}

// ownerFinding reports the vault's missing owning writer, and whether there is one to
// report. It answers from the lock rather than from this server's own role, because a
// read-only window beside a live `mesh watch` is perfectly healthy and must not be told
// otherwise.
func (s *Server) ownerFinding() (index.HealthFinding, bool) {
	if s.owns() {
		return index.HealthFinding{}, false
	}
	if _, live := index.OwnerStatus(s.store.MeshDir()); live {
		return index.HealthFinding{}, false
	}
	// No vault root in the detail: this string can reach a remote MCP client (--http, the
	// hosted viewer), and every other operator-facing path on this surface is scrubbed
	// before it leaves the process. The caller already knows which vault it connected to.
	return index.HealthFinding{Issue: index.NoOwnerIssue, Detail: index.NoOwnerRemedy("")}, true
}

// toolSetupHooks drives the session-hook onboarding: status returns the pitch +
// questions for the agent to run the conversation; install/uninstall apply it. The
// hooks make the agent read the mesh at session start and write back at the end.
func (s *Server) toolSetupHooks(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	// This tool writes .claude/settings.json under a caller-supplied project_dir on the
	// SERVER host. It only makes sense for the local binary configuring its own agent;
	// over any remote transport it lets an authenticated caller drive a server-side
	// filesystem write at a path they choose. Gate POSITIVELY on the local stdio marker:
	// the previous check ("is the hosted write capability set?") failed OPEN on
	// `mesh mcp --http`, which sets neither marker, so a bearer-token holder reached it.
	if !localOperator(ctx) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "mesh_setup_hooks is only available on a local mesh install over stdio, not over a network transport"}
	}
	var a struct {
		Action     string `json:"action"`
		ProjectDir string `json:"project_dir"`
		ReadOnly   bool   `json:"read_only"`
		DryRun     bool   `json:"dry_run"`
	}
	json.Unmarshal(raw, &a)
	proj := a.ProjectDir
	if proj == "" {
		proj, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(proj); err == nil {
		proj = abs
	}
	bin, _ := os.Executable()
	if bin == "" {
		bin = "mesh"
	}
	vaultAbs, _ := filepath.Abs(s.vaultRoot)

	switch a.Action {
	case "install":
		// project_dir must already BE a project: hooks.Install does MkdirAll(.claude)
		// under it, so accepting a path that does not exist turns a typo (or a
		// hallucinated path from the agent) into directory creation anywhere this
		// process can write. Installing hooks into a directory nobody is working in
		// is never the intent, so requiring it to exist costs nothing.
		if fi, serr := os.Stat(proj); serr != nil || !fi.IsDir() {
			return nil, &rpcError{Code: codeInvalidParams, Message: "project_dir must be an existing directory"}
		}
		res, err := hooks.Install(hooks.Options{ProjectDir: proj, Vault: vaultAbs, Bin: bin, EnforceWriteback: !a.ReadOnly, DryRun: a.DryRun})
		if err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		out := map[string]any{"settings_path": res.SettingsPath, "added": res.Added}
		if a.DryRun {
			out["dry_run"] = true
			out["preview"] = res.Preview
		} else if len(res.Added) == 0 {
			out["note"] = "already installed; nothing to do"
		} else {
			out["installed"] = true
			out["next"] = "Tell the user to run /hooks in Claude Code to verify, then restart the session for it to take effect."
		}
		return textResult(out), nil
	case "uninstall":
		n, p, err := hooks.Uninstall(proj)
		if err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		out := map[string]any{"removed": n, "settings_path": p}
		// The CLI receipt for this same operation used to read as a complete
		// uninstall and left the MCP registration behind; this is the twin of that
		// receipt, so it has to say the same thing. This tool cannot remove the
		// registration itself: it is the entry that launched this very server.
		if reg, mp, rerr := hooks.MCPRegistered("claude-code", proj); rerr == nil && reg {
			out["still_registered"] = mp
			out["tell_the_user"] = "This removed the session hooks only. The Mesh MCP server is still registered in " + mp +
				"; they can run `mesh install --remove` (add --client <name> for claude-desktop, cursor, vscode, windsurf or codex) to drop that too. Doing it before deleting the mesh binary avoids leaving their agent retrying a server that no longer exists."
		}
		return textResult(out), nil
	default:
		st, _ := hooks.GetStatus(proj)
		return textResult(map[string]any{
			"status":       st,
			"vault":        vaultAbs,
			"project_dir":  proj,
			"what_it_does": "Wires two Claude Code session hooks: SessionStart runs `mesh orient` so you begin every session having read the mesh (its entry points + recent changes + how to retrieve); Stop nudges you once to write back what you learned (mesh_append_note) before finishing.",
			"why":          "It turns Mesh from a tool you must remember to use into the default: every session starts informed and ends a little smarter, so knowledge compounds across sessions and teammates instead of being relearned. This is the flywheel, the real superpower.",
			"clarify":      "These are Claude Code SESSION hooks, not git pre/post-push hooks (those are a separate layer for code pushes).",
			"questions_to_ask_the_user": []string{
				"Set up the session hooks for this project now? (recommended)",
				"Enforce write-back too (a one-time Stop nudge per session), or read-only (just the start-of-session orientation)?",
				"Is " + proj + " the right project directory? Its .claude/settings.json will be edited.",
			},
			"how_to_apply": "After they answer: call mesh_setup_hooks action=install (read_only=true for orientation-only). Pass dry_run=true first if they want to see the exact settings.json. Then they restart the session and verify with /hooks.",
		}), nil
	}
}

// searchLimitDefault/Max and searchBudgetDefault bound one mesh_search. Without them
// a single call returned every FTS-matching note as a card (Retrieve only FLOORS the
// limit, and packToBudget is skipped when Budget is 0), which blows out the calling
// agent's context and, on the hub, does corpus-sized work per request. The ceilings
// match what graph_tools.go already enforces on the other tools.
const (
	searchLimitDefault  = 20
	searchLimitMax      = 100
	searchBudgetDefault = 8000
)

// SearchLimitDefault/Max and SearchBudgetDefault export the caps above for the OTHER
// surface over the same retriever: internal/web's GET /api/search, which shipped with
// neither and let ?limit=100000 return 401 cards / ~31k tokens / 114 KB from a 400-note
// vault, and made the hub do corpus-sized work per unauthenticated-ish page request.
// Exported as aliases rather than as a rename so package-internal callers stay as they
// were, and so the two surfaces cannot drift apart again by editing one number.
const (
	SearchLimitDefault  = searchLimitDefault
	SearchLimitMax      = searchLimitMax
	SearchBudgetDefault = searchBudgetDefault
	SearchQueryMaxBytes = searchQueryMaxBytes
)

// searchQueryMaxBytes bounds the raw query TEXT, the one search input that had no
// bound while limit, budget, neighbors depth and changed_since were all clamped and
// single-sourced.
//
// It mattered because the retrieval cost scales with the query: the hub accepts a
// 1 MiB POST /mcp body from any peer holding a valid team token at 20 req/s, and a
// 1048578-byte query burned 4 minutes 9 seconds of single-core CPU on a 500-note
// vault, running to completion after the client had hung up. graph.MaxQueryTerms is
// the structural fix and applies on every surface including the CLI; this cap is the
// entry-point half, so a stranger gets a clear refusal instead of a slow success.
// 4 KiB is far more than any real question and still leaves room for a pasted
// paragraph or stack trace.
const searchQueryMaxBytes = 4096

// checkQueryLength rejects a query that is too long to be a question, naming both
// the measurement and the remedy. Shared by every tool that takes free text so the
// surfaces cannot drift apart.
func checkQueryLength(q string) *rpcError {
	if len(q) <= searchQueryMaxBytes {
		return nil
	}
	return &rpcError{
		Code:    codeInvalidParams,
		Message: fmt.Sprintf("query is too long: %d bytes, maximum %d. Send the few words you are actually searching for, not a whole document.", len(q), searchQueryMaxBytes),
	}
}

// searchCard is the MCP wire shape of a retrieval card: the retriever's card plus the
// provenance the agent needs to judge the snippet. retrieve.Card carries no source
// field, so Source is derived here from the note path (see importedSource) and set
// only for third-party ingested notes, keeping the common card byte-identical.
type searchCard struct {
	retrieve.Card
	// NodeID is dropped from the wire: it is exactly notePrefix+NoteID, so it costs
	// every card a second copy of its own id and tells the agent nothing NoteID does
	// not. Measured 2026-08-23 over 62 live cards: zero divergence from "note:"+NoteID,
	// and 17.6% of all card bytes. No tool needs it as INPUT either - toolNeighbors and
	// toolCommunity build centerID as notePrefix+a.ID themselves, and mesh_fetch takes
	// the bare id - so nothing has to reconstruct it. Shadowing the embedded field at
	// depth 0 is what removes it, but the tag MUST be `json:"NodeID,omitempty"` and
	// NOT `json:"-"`. A `-` tag does not shadow: encoding/json drops that field from
	// consideration entirely, so the embedded Card.NodeID stays the only candidate for
	// the name and is still marshalled. Verified the wrong way round first - the whole
	// package test suite stayed GREEN over a no-op change, because every existing test
	// asserts NodeID is PRESENT. TestNodeIDIsNotOnTheSearchWire below pins the real
	// behaviour. This field is never assigned, so omitempty drops it; Card.NodeID stays
	// populated for any in-process caller.
	//
	// Type is deliberately NOT dropped alongside it. It looks derivable from the Path
	// prefix, but it is not: 76 of 2120 notes (3.6%) disagree with their directory -
	// the 12 root-level notes have no directory at all (ORGANIZATION.md is a concept),
	// and every entities/<name>-log*.md is a note, not an entity.
	NodeID string `json:"NodeID,omitempty"`
	Source string `json:"Source,omitempty"`
}

func (s *Server) toolSearch(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Query  string `json:"query"`
		Budget int    `json:"budget"`
		Limit  int    `json:"limit"`
	}
	json.Unmarshal(raw, &a)
	if strings.TrimSpace(a.Query) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "query is required"}
	}
	if e := checkQueryLength(a.Query); e != nil {
		return nil, e
	}
	limit := clampLimit(a.Limit, searchLimitDefault, searchLimitMax)
	budget := a.Budget
	if budget <= 0 {
		budget = searchBudgetDefault
	}
	_, retriever := s.snapshot()
	var economics retrieve.Economics
	var allowed map[string]bool
	if sf := scopeFromCtx(ctx); sf != nil {
		allowed = sf.AllowedRead // nil-safe: nil => retriever does not filter
	}
	// Retrieve UNPACKED and pack below instead. labelCard rewrites the snippet of every
	// connector-ingested card AFTER retrieval, so packing inside Retrieve prices bytes
	// this tool does not send: measured on a 300-note imported vault, the default budget
	// of 8000 shipped 11284 tokens (+41%) while reporting 7988, and 1000/2000/3000 were
	// each ~40% over. The budget exists so an agent can bound what one call costs its
	// context, so it has to be measured on the FINAL form. Budget 0 here is not the
	// /api/search defect (internal/web/search_cap_test.go): limit is already clamped
	// above, so the unpacked set is bounded, and searchCardTokens packs it below.
	cards, err := retriever.Retrieve(ctx, a.Query, retrieve.Options{Limit: limit, AllowedScopes: allowed, Economics: &economics})
	if err != nil {
		// retrievalErr, not internalErr: a timed-out search and a vault with nothing on
		// the topic must not look the same to the agent. See retrievalUnavailableMsg.
		return nil, retrievalErr(err)
	}
	receiptTokens := economics.ReceiptTokens()
	cardBudget := budget - receiptTokens
	if cardBudget <= 0 {
		cards = nil
	} else {
		cards = retrieve.PackToBudget(cards, cardBudget, searchCardTokens)
	}
	economics.ReturnedCards = len(cards)
	economics.ReturnedTokens = retrieve.TotalTokensFunc(cards, searchCardTokens) + receiptTokens
	retriever.RecordEconomics(economics)
	s.rememberSearch(ctx, cards, economics)
	_ = s.store.IncrMetric("queries", 1) // ROI telemetry (best-effort)
	result := map[string]any{
		"cards": labelCards(cards),
		// Reported with the SAME cost function the packer used, so the number the agent
		// reads is the number that was packed to.
		"tokens": economics.ReturnedTokens,
	}
	// The normal route stays out of the wire payload: local counters hold the
	// detailed economics without charging every agent response for telemetry.
	// A fallback is exceptional and must be visible so local cards never
	// masquerade as model-ranked output.
	if economics.Fallback {
		result["rerank"] = economics.Receipt()
	}
	if economics.SemanticFallback {
		result["semantic"] = economics.SemanticReceipt()
	}
	return textResult(result), nil
}

// labelCard marks a card whose note came from a connector import: it stamps the source
// and wraps the snippet in the data envelope, so third-party text can never reach the
// agent as an unlabelled instruction-shaped span (the instruction boundary the HTML and
// TTY sinks already have). A card from the team's own notes passes through untouched.
func labelCard(c retrieve.Card) searchCard {
	sc := searchCard{Card: c}
	src, imported := importedSource(c.Path)
	if strings.HasPrefix(c.Source, "import:") {
		src, imported = c.Source, true
	}
	if imported {
		sc.Source = src
		sc.Snippet = wrapUntrusted(src, c.SourceURL, c.Snippet)
		if c.Summary != "" {
			sc.Summary = wrapUntrusted(src, c.SourceURL, c.Summary)
		}
	}
	return sc
}

func labelCards(cards []retrieve.Card) []searchCard {
	out := make([]searchCard, 0, len(cards))
	for _, c := range cards {
		out = append(out, labelCard(c))
	}
	return out
}

// searchCardTokens prices a card as mesh_search actually sends it: the LABELLED card,
// envelope and Source field included. This is the retrieve.CardCost the packer runs on,
// which is what keeps the bytes sent equal to the bytes budgeted. It must stay in sync
// with labelCard by construction, so it calls it rather than re-deriving the shape.
func searchCardTokens(c retrieve.Card) int {
	sc := labelCard(c)
	b, err := json.Marshal(sc)
	if err != nil {
		// A card that cannot be marshaled cannot be sent either. Price the wrapped card
		// with the retriever's own counter so the packer keeps making progress instead
		// of treating it as free.
		return retrieve.TotalTokens([]retrieve.Card{sc.Card}) + retrieve.EstimateTokens(sc.Source)
	}
	return retrieve.EstimateTokens(string(b))
}

func (s *Server) toolFetch(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		ID     string `json:"id"`
		Anchor string `json:"anchor"`
	}
	json.Unmarshal(raw, &a)
	if ctx.Err() != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "fetch canceled or timed out"}
	}
	// Path and indexed scope must describe the same SQL snapshot. Then require
	// the current file's scope too: a note can become private before reindexing.
	metadata, err := s.store.NoteMetadataFor(ctx, []string{"note:" + a.ID})
	if err != nil && ctx.Err() != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "fetch canceled or timed out"}
	}
	m, ok := metadata["note:"+a.ID]
	sf := scopeFromCtx(ctx)
	if err != nil || !ok || !filepath.IsLocal(m.Path) || (sf != nil && !vault.ScopeAllowsCSV(m.Scope, sf.AllowedRead)) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown note id", Data: a.ID}
	}
	// Share the batch reader's root/handle checks, without silently imposing its
	// byte cap on the existing single-note contract. Never follow a replaced
	// symlink to an unrelated note, whether inside or outside this vault.
	data, err := readFetchFile(ctx, s.vaultRoot, m.Path, 0)
	if ctx.Err() != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "fetch canceled or timed out"}
	}
	if err != nil || !fetchFileScopeAllowed(data, sf) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown note id", Data: a.ID}
	}
	body, rerr := s.formatFetchDocument(ctx, a.ID, m.Path, string(data), []string{a.Anchor})
	if rerr != nil {
		return nil, rerr
	}
	if ctx.Err() != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "fetch canceled or timed out"}
	}
	s.recordFetch(ctx, a.ID, m.Path)
	return rawText(body), nil
}

// Both fetch surfaces must agree even while file edits are ahead of the index.
// Missing, malformed or unterminated scope metadata is never permission to widen
// a scoped caller's access. An unscoped local operator keeps its existing access.
func fetchFileScopeAllowed(body []byte, sf *ScopeFilter) bool {
	if sf == nil {
		return true
	}
	text := string(body)
	fmText, _, _ := vault.SplitFrontmatter(text)
	fm, _, err := vault.ParseFrontmatter([]byte(fmText))
	return err == nil && !vault.UnterminatedFrontmatter(text) && sf.allowsRead(fm.EffectiveScopes())
}

// Formatting is shared by single and batch fetch. Batch calls this once per
// authorized note, combining sections without repeating the safety envelope.
func (s *Server) formatFetchDocument(ctx context.Context, id, rel, body string, anchors []string) (string, *rpcError) {
	// Provenance is read from the WHOLE file, before any anchor slicing: an anchored
	// fetch cuts the frontmatter off, and that is exactly the case where the agent
	// would otherwise get a bare span of third-party prose with nothing saying so.
	src, srcURL := frontmatterProvenance(body)
	// Resolve import provenance before rendering any nested JSON safety context.
	// Missing/edited source metadata retains the fail-safe imported-path fallback.
	if !strings.HasPrefix(src, importSourcePrefix) {
		if ps, ok := importedSource(rel); ok {
			src = ps
		}
	}
	imported := strings.HasPrefix(src, importSourcePrefix)
	whole := len(anchors) == 0
	for _, anchor := range anchors {
		whole = whole || anchor == ""
	}
	if !whole {
		type span struct {
			text       string
			start, end int
		}
		var spans []span
		for _, anchor := range anchors {
			sec, start, end, matches := resolveAnchorSpan(body, anchor)
			if matches > 1 {
				return "", &rpcError{Code: codeInvalidParams, Message: "ambiguous heading anchor; request a unique heading or explicitly fetch the full note"}
			}
			if matches == 0 {
				return "", &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(
					"note %q has no section with anchor %q; available anchors: %s",
					id, anchor, strings.Join(anchorsOf(body), ", "))}
			}
			// A parent already contains its nested sections, and legacy/current
			// aliases can resolve to identical spans. Return each span only once.
			covered := false
			for _, existing := range spans {
				covered = covered || (existing.start <= start && existing.end >= end)
			}
			if !covered {
				kept := spans[:0]
				for _, existing := range spans {
					if !(start <= existing.start && end >= existing.end) {
						kept = append(kept, existing)
					}
				}
				spans = append(kept, span{sec, start, end})
			}
		}
		prefix, rerr := s.sectionContext(ctx, id, rel, body, imported, anchors)
		if rerr != nil {
			return "", rerr
		}
		var sections []string
		for _, span := range spans {
			sections = append(sections, span.text)
		}
		body = prefix + strings.Join(sections, "\n\n")
	}
	// Connector-ingested text is data, not instructions. Wrap it in the envelope the
	// contract describes so the agent has an explicit boundary. The frontmatter source
	// is authoritative (ingest stamps source: import:<connector>); the path check is
	// the fallback for a note whose frontmatter was hand-edited away.
	if imported {
		body = wrapUntrusted(src, srcURL, body)
	}
	return body, nil
}

// Called only for content actually returned; speculative/budget-omitted reads
// must not fabricate reuse or win search attribution based on worker completion.
func (s *Server) recordFetch(ctx context.Context, id, rel string) {
	now := time.Now()
	_ = s.store.IncrMetric("fetches", 1)          // ROI telemetry (best-effort)
	_ = s.store.IncrMetric("fetch:"+id, 1)        // per-note reuse (most-reused list)
	_ = s.store.RecordReuse(id, flywheelReuseGap) // flywheel: a later fetch = the next run inheriting it
	observeTeamReuse(ctx, id, filepath.ToSlash(rel), now)
	// Only the trusted local stdio transport may enqueue an event for later sync.
	// A bare shared HTTP MCP has no authenticated logical reader, so attributing its
	// requests to the machine's sync credential would fabricate cross-user reuse.
	// Ordinary reference notes are excluded too: only agent write-backs participate
	// in the team flywheel.
	if localOperator(ctx) && s.store.IsAgentAuthoredNote(id) {
		_ = teamtelemetry.RecordForJoinedVault(s.vaultRoot, id, now)
	}
	s.recordAttributedFetch(ctx, id)
}

const searchAttributionWindow = 10 * time.Minute

func (s *Server) rememberSearch(ctx context.Context, cards []retrieve.Card, economics retrieve.Economics) {
	ranks := make(map[string]int, len(cards))
	for i, card := range cards {
		ranks[card.NoteID] = i + 1
	}
	s.searchMu.Lock()
	if s.lastSearch == nil {
		s.lastSearch = make(map[string]searchAttribution)
	}
	s.lastSearch[attributionActor(ctx)] = searchAttribution{at: time.Now(), ranks: ranks, route: economics.Route}
	s.searchMu.Unlock()
}

func (s *Server) recordAttributedFetch(ctx context.Context, noteID string) {
	actor := attributionActor(ctx)
	s.searchMu.Lock()
	last := s.lastSearch[actor]
	if time.Since(last.at) > searchAttributionWindow {
		delete(s.lastSearch, actor)
		s.searchMu.Unlock()
		return
	}
	rank, ok := last.ranks[noteID]
	if ok {
		// Attribute only the first matching fetch after a search: this is the
		// agent's selection from that slate, not a count of later note reading.
		delete(s.lastSearch, actor)
	}
	s.searchMu.Unlock()
	if !ok {
		return
	}
	_ = s.store.IncrMetric("retrieval:search_to_fetch", 1)
	if rank > 5 {
		rank = 6
	}
	_ = s.store.IncrMetric(fmt.Sprintf("retrieval:selected_rank:%d", rank), 1)
	switch last.route {
	case "model", "cache", "local_exact", "local_confident", "fallback", "too_few", "disabled":
		_ = s.store.IncrMetric("rerank:selected_route:"+last.route, 1)
	default:
		_ = s.store.IncrMetric("rerank:selected_route:other", 1)
	}
}

func (s *Server) toolGodNodes(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Limit int `json:"limit"`
	}
	json.Unmarshal(raw, &a)
	// Cap it like every other limit-taking tool. This was the one that called neither
	// clampLimit nor anything else, so mesh_god_nodes{"limit":1000000} returned the whole
	// note corpus as "hubs" - the orientation call, the one an agent makes FIRST, was the
	// one that could blow out its context before it had read anything.
	a.Limit = clampLimit(a.Limit, 10, 100)
	// degree is the note's KNOWLEDGE degree: how many distinct other notes link to it
	// or from it. Raw fan-out would rank the vault's longest note first (its own
	// headings and tags each add one) and report a link count it does not have, which
	// is the worst possible answer from the tool an agent calls first to orient.
	type hub struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Path      string `json:"path"`
		Degree    int    `json:"degree"`
		Community int    `json:"community"`
	}
	g, _ := s.snapshot()
	sf := scopeFromCtx(ctx)
	var hubs []hub
	for _, n := range g.Nodes() {
		if n.Kind != "note" {
			continue
		}
		if !sf.allowsNode(n) { // hide hubs the caller cannot read (no title enumeration)
			continue
		}
		hubs = append(hubs, hub{n.NoteID, n.Label, n.NotePath, n.KnowledgeDegree, n.Community})
	}
	sort.Slice(hubs, func(i, j int) bool {
		if hubs[i].Degree != hubs[j].Degree {
			return hubs[i].Degree > hubs[j].Degree
		}
		return hubs[i].ID < hubs[j].ID
	})
	if len(hubs) > a.Limit {
		hubs = hubs[:a.Limit]
	}
	return textResult(map[string]any{"hubs": hubs}), nil
}

// changedSince* bound one delta. ChangedSince is a bare "WHERE mtime > ? ORDER BY mtime
// DESC" with no LIMIT and no validation, and the same parameter had two opposite traps.
//
// Too small: since=0 (or a negative) returned every note in the vault, unbounded, from
// the tool an agent calls on RESUME, when its context is already loaded.
//
// Too large: since is UNIX SECONDS, and passing milliseconds is the single most likely
// caller mistake, because most runtimes hand out milliseconds by default. A millisecond
// stamp is a year-57578 timestamp, so the query matched nothing and the tool answered
// "changed": null. The agent reads that as "nothing changed since I left" and proceeds
// on stale context, which is worse than an error: it is a wrong answer with no signal.
// So a future `since` is refused, and the refusal names the likely cause.
const (
	changedSinceLimitDefault = 100
	changedSinceLimitMax     = 500
	// changedSinceFutureSlack is how far ahead of the server's clock a `since` may sit
	// before it is a caller mistake rather than clock skew between two machines.
	changedSinceFutureSlack = int64(3600) // seconds
)

func (s *Server) toolChangedSince(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Since int64 `json:"since"`
		Limit int   `json:"limit"`
	}
	json.Unmarshal(raw, &a)
	now := time.Now().Unix()
	if a.Since > now+changedSinceFutureSlack {
		msg := fmt.Sprintf("since=%d is in the future (server time is %d), so this can only ever report that nothing changed", a.Since, now)
		if a.Since/1000 <= now+changedSinceFutureSlack {
			msg += fmt.Sprintf("; that looks like MILLISECONDS, and since is UNIX SECONDS: pass %d", a.Since/1000)
		}
		return nil, &rpcError{Code: codeInvalidParams, Message: msg}
	}
	if a.Since < 0 {
		a.Since = 0 // "everything" is a legitimate ask; the limit below is what bounds it
	}
	limit := clampLimit(a.Limit, changedSinceLimitDefault, changedSinceLimitMax)
	refs, err := s.store.ChangedSince(a.Since)
	if err != nil {
		return nil, internalErr(err)
	}
	// Scope read check: each ref exposes a note id, path and mtime, so an unfiltered
	// delta lets a scoped caller enumerate notes outside their scope. Drop the ones
	// they cannot read. A nil filter (solo / no-scope hub) leaves the list untouched.
	if sf := scopeFromCtx(ctx); sf != nil {
		kept := refs[:0]
		for _, r := range refs {
			sc, serr := s.store.NoteScope(r.ID)
			if serr != nil || !sf.allowsRead(sc) {
				continue
			}
			kept = append(kept, r)
		}
		refs = kept
	}
	// An explicit truncated flag is what lets a caller tell a CAPPED delta from a
	// complete one. Without it a capped list is indistinguishable from "that is all
	// there is", which is the same silent-wrong-answer shape as the millisecond case.
	// The refs are newest-first, so the cap keeps the newest and drops the oldest.
	total := len(refs)
	truncated := total > limit
	if truncated {
		refs = refs[:limit]
	}
	if refs == nil {
		refs = []index.NoteRef{} // never "changed": null; an empty delta says so as [] plus count 0
	}
	out := map[string]any{"changed": refs, "count": len(refs)}
	if truncated {
		out["truncated"] = true
		out["total"] = total
		out["limit"] = limit
		out["note"] = fmt.Sprintf("%d notes changed since that timestamp; only the %d most recent are listed. "+
			"Pass a more recent `since`, or a larger `limit` (max %d).", total, limit, changedSinceLimitMax)
	}
	return textResult(out), nil
}

// Keep reversible preparation below either HTTP server's write window, reserving
// room for a read-only reader's separate 10s index acknowledgement. A socket
// WriteTimeout does not cancel request work: without this deadline a stalled ID
// scan could publish minutes after the client lost its response. Once publication
// starts, the publisher still owns finishing or withdrawing its atomic claim;
// never detach it to meet a response deadline. This does not bound owner indexing
// or a publisher's filesystem/transaction work after its durable boundary.
const writePreparationTimeout = 15 * time.Second

func (s *Server) toolWrite(ctx context.Context, raw json.RawMessage, forceType string) (any, *rpcError) {
	if err := vault.RequireAuthoringWrites(); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	trace := latency.Start("mcp_write", "validate")
	defer trace.End()
	budget := s.writePrepareTimeout
	if budget <= 0 {
		budget = writePreparationTimeout
	}
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, budget)
	defer cancelPrepare()
	// Cancellation is still reversible until CreateNote starts. Once CreateNote returns,
	// the note is durable and must receive a success-with-staleness receipt rather than
	// an error that invites a duplicate retry. Refuse a request that was already cancelled
	// before crossing that boundary.
	if ctx.Err() != nil || prepareCtx.Err() != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "request cancelled before the note was written"}
	}
	var a authoringArgs
	if err := decodeAuthoring(raw, &a); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	spec, rerr := s.authoringSpec(prepareCtx, a, forceType)
	if rerr != nil {
		return nil, rerr
	}
	if spec.Status != "draft" {
		if err := vault.ValidateSpec(spec); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
	}
	if err := s.validateAuthoringLinks(prepareCtx, spec); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown, invalid or inaccessible authoring reference"}
	}
	t, source := string(spec.Type), spec.Source

	// Preparation above is reversible and can include retrieval work. Cancellation may
	// arrive after the entry check while it runs, so check once more at the exact durable
	// boundary. There is deliberately no cancellation error after CreateNote returns:
	// from that point the success-with-staleness receipt prevents duplicate retries.
	if ctx.Err() != nil || prepareCtx.Err() != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "request cancelled before the note was written"}
	}
	writtenAt := time.Now()
	trace.Phase("publish")
	res, err := s.publishNote(prepareCtx, spec)
	// Release the preparation timer now. The original caller context governs the
	// independent indexing acknowledgement; an expired preparation budget must not
	// turn a publisher's confirmed durable result into a failed-write receipt.
	cancelPrepare()
	if err != nil {
		// Do NOT echo a raw error here. Everything vault.CreateNote raises about the
		// FILESYSTEM names the note's absolute path, so a too-long title came back as
		// "open /srv/hub/vault/gotchas/<slug>.md: file name too long" and handed the
		// agent the server's absolute vault root - the exact leak the success path 30
		// lines below goes out of its way to prevent by relativizing res.Path. Errors
		// about the caller's own input (vault.ErrInvalidSpec, which now covers the
		// too-long title) are authored in that package, carry no path, and are the ones
		// worth reading, so those go back verbatim.
		if errors.Is(err, vault.ErrInvalidSpec) {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		slog.Error("mesh write: the note could not be created", "type", t, "error", err)
		return nil, &rpcError{Code: codeInvalidParams, Message: "the note could not be written: " + ScrubPathsUnder(err.Error(), s.vaultRoot)}
	}
	// Make the new note queryable through the INCREMENTAL path, not a full reindex.
	// reload() re-walked and re-parsed the entire vault and rewrote every notes /
	// search_index / nodes / edges row in one transaction to publish one small file, so
	// write-back cost scaled with vault size instead of with the change; because rebuilds
	// serialize on reloadMu, concurrent hosted write-backs then queued head to tail
	// against the hub's 30s write timeout. reconcileOnce sees exactly this file as one
	// Added path (authoritative, so a same-second mtime cannot hide it) and rebuilds the
	// graph from the parsed-note cache the startup load seeded.
	//
	// A failure here must NOT be reported as a failed write. The note is already
	// durably on disk, so returning -32603 tells the agent "nothing was written",
	// it retries, and Mesh mints a near-duplicate with a -2 id suffix. Two notes on
	// one topic split retrieval and the next agent reads whichever ranks first,
	// which is worse than the error. That happened three times (2026-07-26, 07-28,
	// 07-30), the last two with a warning gotcha already in the vault, which is
	// what settled that a note cannot fix a tool that misreports its own outcome.
	//
	// reconcileOnce writes to the store, so it fails whenever a `mesh sync --watch`
	// daemon holds the write lock. That is a refresh problem, never a durability
	// problem. So: succeed, and say the index is stale, so the caller knows to run
	// mesh_reindex and knows not to retry the write.
	var indexStale string
	trace.Phase("acknowledge")
	var ownerDown bool
	if err := s.publishWriteBack(ctx, res.ID, res.Path); err != nil {
		ownerDown = errors.Is(err, ErrOwnerNotIndexing)
		slog.Error("mesh write: the note was saved but the index did not refresh",
			"id", res.ID, "path", res.Path, "owner_down", ownerDown, "error", err)
		// Scrubbed like every other message that leaves this process. This one is easy to
		// miss because it sits right next to a "path" field that IS relativized, but the
		// reconcile walks the vault, so its errors are filesystem errors: an unreadable
		// subdirectory came back as "open <abs vault root>/locked: permission denied" and
		// shipped the whole server layout inside the staleness warning.
		indexStale = ScrubPathsUnder(err.Error(), s.vaultRoot)
	}
	// Both of these are writes, so on a read-only server they do nothing: IncrMetric
	// drops the counter (recordTelemetry returns early, since no writer goroutine will
	// ever flush it) and RecordWriteback returns ErrReadOnly. That is deliberate, and
	// only the flywheel stamp is recovered: the owner's BackfillWritebacks is idempotent
	// and re-derives it from the note's own `source: agent`, which CreateNote already
	// set. The "writes" counter genuinely is lost on read-only windows, so
	// FlywheelStats.WritesPer100Reads under-reports there; it is a telemetry number, not
	// a correctness one, and paying a write lock per window to keep it exact is the
	// contention this split exists to remove.
	trace.Phase("telemetry")
	if spec.Status != "draft" {
		_ = s.store.IncrMetric("writes", 1)
		_ = s.store.RecordWriteback(res.ID, source)
		writebackPath := res.Path
		if rel, rerr := filepath.Rel(s.vaultRoot, res.Path); rerr == nil {
			writebackPath = filepath.ToSlash(rel)
		}
		observeTeamWriteback(ctx, res.ID, writebackPath, source, writtenAt)
	}
	// Return a vault-relative path, never the server's absolute filesystem path:
	// on a hosted hub the absolute path would leak the server's absolute vault path
	// to the agent.
	notePath := res.Path
	if rel, err := filepath.Rel(s.vaultRoot, res.Path); err == nil && !strings.HasPrefix(rel, "..") {
		notePath = rel
	} else {
		notePath = filepath.Base(res.Path)
	}
	out := map[string]any{"id": res.ID, "path": notePath, "when": res.When, "todo": res.TODOs, "status": spec.Status, "template": spec.Template, "template_version": spec.TemplateVersion, "revision": res.Revision}
	if spec.UpdateID != "" {
		out["updated"] = true
		out["previous_revision"] = spec.UpdateRevision
	}
	if indexStale != "" {
		out["index_stale"] = true
		out["index_error"] = indexStale
		// Both messages start from the same fact (the note is saved, do not retry) and
		// then diverge on the remedy, because giving the wrong one is worse than giving
		// none. On a read-only server whose owner is down, mesh_reindex only re-reads
		// what the owner already persisted, so it cannot make this note appear; saying
		// "call mesh_reindex" there would send the agent in a loop against a tool that
		// is structurally incapable of fixing it.
		if ownerDown {
			out["owner_down"] = true
			out["warning"] = "The note IS saved at the path above; queryability was not confirmed before the wait ended. " +
				"The existing owner may be busy, or this reader's refresh may be delayed. " +
				"Check the owner and index state before acting; owner_down is a legacy timeout flag, not proof of a missing owner. " +
				"Do NOT retry this write or start a second index writer. Do NOT call mesh_reindex repeatedly to force indexing: " +
				"this reader cannot make the owner index sooner. Once indexing catches up, read or search the saved note."
		} else {
			out["warning"] = "The note IS saved at the path above. Only the index refresh failed, " +
				"so it is not queryable yet. Do NOT call this tool again: a retry creates a " +
				"duplicate note. Call mesh_reindex instead."
			if spec.UpdateID != "" {
				out["warning"] = "The update IS saved at the path above. Only the index refresh failed; do not retry the update with the old revision. Call mesh_reindex, then read the current note."
			}
		}
	}
	return textResult(out), nil
}

// ScrubPathsUnder makes an error message safe to hand a caller, for a process serving the
// vault at root. It is THE entry point for every surface: internal/web's promote handler
// calls it too, because the identical leak shipped on both surfaces and a second private
// copy of the logic is exactly how one of them keeps the bug after the other is fixed.
//
// The symlink-resolved spelling of root is scrubbed as well, because the server can be
// started with one spelling while the kernel names the other in an error (a symlinked
// /tmp resolves to /private/tmp on macOS).
func ScrubPathsUnder(msg, root string) string {
	roots := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		roots = append(roots, resolved)
	}
	return scrubPaths(msg, roots...)
}

// scrubPaths rewrites every absolute path in an error message down to its base name, so
// a message that reaches a remote agent cannot carry the server's directory layout.
// It replaces rather than deletes, because the base name (the note's own slug) is the
// part the caller supplied and the part that makes the message useful. Full detail is
// logged server-side by the caller.
//
// roots are the absolute paths this process KNOWS it serves. They are matched as
// substrings, which is the only thing that works for a root CONTAINING A SPACE: this
// estate's own vault lives under ".../Automation HQ/...", and the whitespace scan below
// saw that as two tokens, rewrote the first to "<path>/Automation" and left the tail
// ("HQ/vault/gotchas/x.md") standing, so most of the layout still shipped.
//
// The whitespace scan stays as the fallback for prefixes this process was never told
// about, which is what a root-only comparison would miss.
func scrubPaths(msg string, roots ...string) string {
	known := append([]string(nil), roots...)
	// Longest first, so a nested root is not half-consumed by its parent.
	sort.Slice(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })
	for _, root := range known {
		msg = scrubRoot(msg, root)
	}
	fields := strings.Fields(msg)
	for i, f := range fields {
		trimmed := strings.TrimRight(f, ":;,")
		if !filepath.IsAbs(trimmed) {
			continue
		}
		fields[i] = "<path>/" + filepath.Base(trimmed) + f[len(trimmed):]
	}
	return strings.Join(fields, " ")
}

// scrubRoot cuts every path that STARTS at a known root down to "<path>/<base>". It scans
// from the END of the root, so whitespace inside the root cannot end the match: that is
// the whole reason it exists, and why tokenizing on whitespace alone was not enough.
func scrubRoot(msg, root string) string {
	root = strings.TrimRight(root, string(filepath.Separator))
	// A relative root would match all over a message, and "/" trims to "" here, so both
	// are refused rather than allowed to swallow the text.
	if root == "" || !filepath.IsAbs(root) {
		return msg
	}
	var b strings.Builder
	for {
		i := strings.Index(msg, root)
		if i < 0 {
			break
		}
		end := i + len(root)
		for end < len(msg) && !endsPath(msg[end]) {
			end++
		}
		span := msg[i:end]
		tail := strings.TrimRight(span, ":;,")                      // trailing punctuation is prose, not path
		path := strings.TrimRight(tail, string(filepath.Separator)) // "<root>/" still names the root
		b.WriteString(msg[:i])
		b.WriteString("<path>")
		if base := filepath.Base(path); path != root && base != "." && base != string(filepath.Separator) {
			b.WriteString(string(filepath.Separator))
			b.WriteString(base)
		}
		b.WriteString(span[len(tail):]) // put the punctuation back
		msg = msg[end:]
	}
	b.WriteString(msg)
	return b.String()
}

// endsPath reports whether c cannot be part of a filesystem path inside an error message.
// Whitespace and quotes end one; ':' does not, because it is both the separator in
// "open <path>: reason" and legal in a filename, so the trailing run is trimmed instead.
func endsPath(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '"', '\'', '`':
		return true
	}
	return false
}

// flywheelReuseGap is how long after a write-back a fetch must land to count as reuse
// by a LATER session rather than a re-read inside the same work burst (the
// cross-session proxy that works for both the solo CLI and the long-lived hub).
const flywheelReuseGap = 600 // seconds (10 min)

// Bound the added context, not the requested section. These are excerpts, never
// a promise that every warning elsewhere in the document has been discovered.
const sectionContextMaxBytes = 4096
const sectionContextNotice = "Mesh section context: excerpts only; other sections omitted. Fetch more context before acting on incomplete or truncated guidance.\n"

type sectionContextEnvelope struct {
	IncompleteContext bool              `json:"incomplete_context"`
	ContextTruncated  bool              `json:"context_truncated"`
	Fields            map[string]string `json:"fields"`
}

func (s *Server) sectionContext(ctx context.Context, id, rel, doc string, imported bool, anchors []string) (string, *rpcError) {
	fmText, noteBody, _ := vault.SplitFrontmatter(doc)
	fm, _, err := vault.ParseFrontmatter([]byte(fmText))
	if err != nil || vault.UnterminatedFrontmatter(doc) {
		return "", &rpcError{Code: codeInvalidParams, Message: "invalid note metadata; explicitly fetch the full note to inspect it"}
	}
	if fm.Template != "" || fm.TemplateVersion != 0 {
		if _, err := vault.ReadAuthoring(fm, noteBody); err != nil {
			return "", &rpcError{Code: codeInvalidParams, Message: "invalid authored structure; explicitly fetch the full note to inspect it"}
		}
	}
	metadata, err := s.store.NoteMetadataFor(ctx, []string{"note:" + id})
	if err != nil {
		return "", internalErr(err)
	}
	m, found := metadata["note:"+id]
	sf := scopeFromCtx(ctx)
	if !found || m.Path != rel || (sf != nil && !vault.ScopeAllowsCSV(m.Scope, sf.AllowedRead)) {
		return "", &rpcError{Code: codeInvalidParams, Message: "unknown note id", Data: id}
	}
	fields := map[string]string{
		"status": fm.Status, "severity": fm.Severity, "review_by": fm.ReviewBy,
		"supersedes": strings.Join(fm.Supersedes, ", "),
		"source":     fm.Source, "source_url": fm.SourceURL, "template": fm.Template,
	}

	// Historical fields retain their original labels only in the compatibility
	// envelope. Modern notes obtain their context from authored body sections.
	for key, text := range vault.ReadLegacy(fm).Values() {
		if !vault.Unfilled(text) {
			fields["legacy_"+key] = text
		}
	}
	for _, key := range []string{"summary", "applicability", "prerequisites", "limitations", "evidence", "verification", "dependencies", "cause", "root_cause", "resolution", "impact", "consequences", "recovery"} {
		if text := contextualSectionText(fm, noteBody, key); text != "" {
			fields[key] = text
		}
	}

	addSelectedBlockContext(fields, fm, noteBody, anchors)
	// The source file may have no retirement mark at all. Resolve the current
	// incoming relation without exposing the existence of a fenced replacement.
	if m.SupersededBy != "" && m.SupersederPath != "" && (sf == nil || vault.ScopeAllowsCSV(m.SupersederScope, sf.AllowedRead)) {
		fields["superseded_by"] = m.SupersededBy
	}
	lines, markers, headings := anchorDocumentLines(doc)
	end := len(lines)
	for i := range markers {
		if _, ok := vault.ParseATXHeading(markers[i], headings[i]); ok {
			end = i
			break
		}
	}
	fields["preamble"] = strings.TrimSpace(strings.Join(lines[:end], "\n"))
	if imported {
		// Neutralize BEFORE JSON encoding: legacy angle-bracket markers would
		// otherwise become \\u003c sequences invisible to the outer matcher.
		// Do not rewrite the document before resolving headings or parsing YAML.
		for key, value := range fields {
			fields[key] = stripEnvelopeTags(value)
		}
	}
	return encodeSectionContext(fields), nil
}

func encodeSectionContext(fields map[string]string) string {
	envelope := sectionContextEnvelope{IncompleteContext: true, Fields: make(map[string]string)}
	for key, value := range fields {
		if value == "" {
			continue
		}
		limit := 512
		if key == "preamble" {
			limit = 1024
		}
		clipped := clipSectionContext(value, limit)
		envelope.ContextTruncated = envelope.ContextTruncated || clipped != value
		envelope.Fields[key] = clipped
	}
	// JSON escaping can expand individual bytes sixfold. Bound the serialized
	// header including the notice, not merely the unescaped string lengths.
	for {
		data, _ := json.Marshal(envelope) // only bools and strings; cannot fail
		if len(sectionContextNotice)+len(data)+2 <= sectionContextMaxBytes {
			return sectionContextNotice + string(data) + "\n\n"
		}
		envelope.ContextTruncated = true
		for key, value := range envelope.Fields {
			if len(value) < 2 {
				delete(envelope.Fields, key)
				continue
			}
			envelope.Fields[key] = clipSectionContext(value, len(value)/2)
		}
	}
}

func clipSectionContext(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

// sectionByAnchor returns the markdown of the heading section whose slug matches
// anchor (from that heading until the next heading of the same or higher level), and
// whether that heading resolved uniquely. A miss must NOT fall back to the whole note: this is a
// narrowing function, and returning its unnarrowed input turned one wrong character in an
// anchor into a multi-megabyte reply that no caller asked for and none can afford.
func sectionByAnchor(body, anchor string) (string, bool) {
	section, matches := resolveAnchorSection(body, anchor)
	return section, matches == 1
}

// An exact current slug wins over legacy aliases, but duplicate matches within
// the winning namespace are ambiguous and must never silently choose a section.
func resolveAnchorSection(body, anchor string) (string, int) {
	section, _, _, matches := resolveAnchorSpan(body, anchor)
	return section, matches
}

func resolveAnchorSpan(body, anchor string) (string, int, int, int) {
	header, noteBody, _ := vault.SplitFrontmatter(body)
	fm, _, _ := vault.ParseFrontmatter([]byte(header))
	return resolveAuthoredAnchorSpan(fm, noteBody, anchor)
}

func resolveAuthoredAnchorSpan(fm *vault.Frontmatter, noteBody, anchor string) (string, int, int, int) {
	lines, markerLines, headingLines := anchorDocumentLines(noteBody)
	blockAnchors := vault.AuthoredHeadingAnchors(fm, noteBody)
	anchor = norm.NFC.String(anchor)
	if anchor == "" {
		return "", 0, 0, 0
	}

	// Search every current anchor before accepting a legacy alias. Otherwise the legacy
	// slug of an earlier Unicode heading can shadow the exact current slug of a later
	// heading, returning a valid but entirely wrong section.
	start, level, matches := findAnchorHeading(markerLines, headingLines, anchor, false, blockAnchors)
	if start < 0 {
		if fm != nil {
			if content, err := vault.ReadAuthoring(fm, noteBody); err == nil {
				for _, section := range content.OrderedSections {
					if section.Key == anchor {
						start, level, matches = findAnchorHeading(markerLines, headingLines, section.Anchor, false, blockAnchors)
						break
					}
				}
			}
		}
	}
	if start < 0 {
		start, level, matches = findAnchorHeading(markerLines, headingLines, anchor, true, nil)
	}
	if matches != 1 {
		return "", 0, 0, matches
	}
	end := len(lines)
	for i := start + 1; i < len(markerLines); i++ {
		if h, ok := vault.ParseATXHeading(markerLines[i], headingLines[i]); ok && h.Level <= level {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), start, end, 1
}

// anchorsOf lists the slugs a note actually offers, so an anchor miss can name the real
// options instead of leaving the caller to guess. It is the same vault.Slugify the index
// uses to build heading nodes, so what it lists is exactly what resolves.
func anchorsOf(body string) []string {
	var out []string
	header, noteBody, _ := vault.SplitFrontmatter(body)
	fm, _, _ := vault.ParseFrontmatter([]byte(header))
	blockAnchors := vault.AuthoredHeadingAnchors(fm, noteBody)
	_, markerLines, headingLines := anchorDocumentLines(body)
	for i := range markerLines {
		if heading, ok := vault.ParseATXHeading(markerLines[i], headingLines[i]); ok && heading.Anchor != "" {
			anchor := heading.Anchor
			if address := blockAnchors[i+1]; address != "" {
				anchor = address
			}
			out = append(out, anchor)
		}
	}
	return out
}

// anchorDocumentLines drops YAML frontmatter, then builds two length-preserving views:
// markerLines hides code as well as comments/fences so a heading marker inside code is
// inert; headingLines keeps visible inline-code text so "## Use `mesh index`" has the
// same use-mesh-index anchor a Markdown reader sees.
func anchorDocumentLines(doc string) (original, markerLines, headingLines []string) {
	_, body, _ := vault.SplitFrontmatter(doc)
	markers, _ := vault.StripNonContent(body)
	headings, _ := vault.StripFencesAndComments(body)
	return strings.Split(body, "\n"), strings.Split(markers, "\n"), strings.Split(headings, "\n")
}

func findAnchorHeading(markerLines, headingLines []string, anchor string, legacy bool, blockAnchors map[int]string) (start, level, matches int) {
	start = -1
	for i := range markerLines {
		heading, ok := vault.ParseATXHeading(markerLines[i], headingLines[i])
		if !ok {
			continue
		}
		candidate := heading.Anchor
		if address := blockAnchors[i+1]; address != "" {
			candidate = address
		}
		if legacy {
			candidate = slugifyLegacy(heading.VisibleText)
		}
		if candidate == anchor {
			if start < 0 {
				start, level = i, heading.Level
			}
			matches++
		}
	}
	return start, level, matches
}

// slugifyLegacy reproduces the slug Mesh emitted before vault.Slugify learned to fold
// diacritics: keep [a-z0-9], collapse everything else to a dash. It is a LOOKUP
// fallback only (see anchorMatches). Nothing new is ever minted in this form.
func slugifyLegacy(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
