-- SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
-- Copyright (C) 2026 Bright Interaction AB
-- Mesh SQLite index schema (spec section 3.2). Pure-Go modernc.org/sqlite, WAL.
-- The index is a derived, deletable artifact; the markdown vault is the source
-- of truth. No vec0 virtual table: modernc cannot load C extensions, so vectors
-- (milestone V) are a flat []float32 blob scored with brute-force cosine.

CREATE TABLE IF NOT EXISTS notes (
  id             TEXT PRIMARY KEY,           -- frontmatter id (stable identity)
  path           TEXT UNIQUE NOT NULL,       -- vault-relative path (lookup, not identity)
  type           TEXT NOT NULL,              -- note|post-mortem|decision|gotcha|entity
  title          TEXT NOT NULL,
  retrieval_hash TEXT NOT NULL,              -- SHA256(body + retrieval-critical frontmatter)
  frontmatter    TEXT NOT NULL,              -- whitelisted-key JSON; never raw YAML
  summary_short  TEXT,                       -- BYOAI one-liner (LOCAL/advisory)
  summary_card   TEXT,                       -- BYOAI ~5-line (LOCAL/advisory)
  mtime          INTEGER NOT NULL,
  updated        TEXT,                        -- frontmatter when/updated
  contributor    TEXT,                        -- last author (LOCAL in v1)
  review_by      TEXT,                        -- frontmatter review_by (lifecycle re-check date)
  source         TEXT,                        -- frontmatter source (provenance: manual|agent|import:*)
  scope          TEXT NOT NULL DEFAULT 'dev', -- access-control scope(s), comma-joined; absent frontmatter = dev
  local_version  INTEGER NOT NULL DEFAULT 0,  -- monotonic local counter (mesh_changed_since)
  hub_version    INTEGER NOT NULL DEFAULT 0   -- DEFERRED: set only when sync (milestone S) is live
);

CREATE TABLE IF NOT EXISTS nodes (
  id         TEXT PRIMARY KEY,               -- note:<frontmatter-id> | tag:<name> | note:<id>#<anchor>
  kind       TEXT NOT NULL,                  -- note|heading|block|tag|external|rationale|decision|gotcha
  label      TEXT NOT NULL,
  note_id    TEXT,                           -- owning note's frontmatter id
  note_path  TEXT,                           -- denormalized for fast file open
  anchor     TEXT,                           -- heading slug / block id
  source_loc TEXT,                           -- "L<line>" for the editor deep link
  community  INTEGER,
  attrs      TEXT                            -- JSON bag (do/dont/why/when, captured_at, author)
);
CREATE INDEX IF NOT EXISTS idx_nodes_kind ON nodes(kind);
CREATE INDEX IF NOT EXISTS idx_nodes_community ON nodes(community);
CREATE INDEX IF NOT EXISTS idx_nodes_note_id ON nodes(note_id);

CREATE TABLE IF NOT EXISTS edges (
  source           TEXT NOT NULL,            -- node id
  target           TEXT NOT NULL,
  relation         TEXT NOT NULL,            -- contains|references|tagged|ordered_after|
                                             -- rationale_for|decision_for|supersedes
  confidence       TEXT NOT NULL,            -- EXTRACTED|INFERRED|AMBIGUOUS
  confidence_score REAL NOT NULL DEFAULT 1.0,
  weight           REAL NOT NULL DEFAULT 1.0,
  source_loc       TEXT,
  PRIMARY KEY (source, target, relation)
);
CREATE INDEX IF NOT EXISTS idx_edges_target ON edges(target);

-- Vectors: milestone V. Flat blob, brute-force cosine, homogeneous per vault.
CREATE TABLE IF NOT EXISTS vectors (
  node_id      TEXT NOT NULL,
  chunk_ix     INTEGER NOT NULL,
  model        TEXT NOT NULL,                -- canonical embedding model id (homogeneity guard)
  dim          INTEGER NOT NULL,
  embedding    BLOB NOT NULL,                -- []float32 little-endian
  content_hash TEXT NOT NULL DEFAULT '',     -- sha256 of the embedding input, so an unchanged chunk is not re-embedded
  note_hash    TEXT NOT NULL DEFAULT '',     -- the note's retrieval_hash when embedded; a mismatch with notes.retrieval_hash means this vector is stale and is excluded from retrieval
  PRIMARY KEY (node_id, chunk_ix)
);

CREATE VIRTUAL TABLE IF NOT EXISTS search_index USING fts5(
  node_id UNINDEXED, kind UNINDEXED, anchor UNINDEXED,
  title, body,
  tokenize = 'unicode61 remove_diacritics 2'
);

-- Persisted corpus stats so graph-BM25 does not recompute IDF/avgdl per query.
CREATE TABLE IF NOT EXISTS corpus_stats (key TEXT PRIMARY KEY, value REAL NOT NULL);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

-- Source-code index: the pure-Go replacement for codeindex's source-code role.
-- Self-contained (own tables + own FTS) so locating a function never pollutes the
-- note knowledge graph or the tier-0 budget. Identity is the symbol id
-- "code:<path>#<qualified-name>"; a file rename is delete+add by path, exactly as
-- an untitled note is. Code roots are configured separately from the note vault.
CREATE TABLE IF NOT EXISTS code_files (
  path           TEXT PRIMARY KEY,           -- root-relative source path
  lang           TEXT NOT NULL,              -- go|ts|tsx|js|jsx|svelte|astro|py
  package        TEXT,                       -- Go package name; null for others
  mtime          INTEGER NOT NULL,
  retrieval_hash TEXT NOT NULL               -- SHA256 over the file's extracted symbols (drift check)
);

CREATE TABLE IF NOT EXISTS code_symbols (
  id         TEXT PRIMARY KEY,               -- code:<path>#<qualified-name>[~<line>]
  path       TEXT NOT NULL,
  lang       TEXT NOT NULL,
  name       TEXT NOT NULL,                  -- qualified: "Server.Search"
  kind       TEXT NOT NULL,                  -- func|method|type|interface|struct|const|var|class|enum
  start_line INTEGER NOT NULL,               -- 1-based; the editor deep-link target
  end_line   INTEGER NOT NULL,
  signature  TEXT,
  doc        TEXT,
  calls      TEXT                            -- JSON array of callee names (Go); lets edges rebuild without re-parsing
);
CREATE INDEX IF NOT EXISTS idx_code_symbols_path ON code_symbols(path);
CREATE INDEX IF NOT EXISTS idx_code_symbols_name ON code_symbols(name);

CREATE TABLE IF NOT EXISTS code_edges (
  src_id   TEXT NOT NULL,                    -- caller symbol id
  dst_id   TEXT NOT NULL,                    -- callee symbol id
  relation TEXT NOT NULL,                    -- calls (Go only; the call graph)
  PRIMARY KEY (src_id, dst_id, relation)
);
CREATE INDEX IF NOT EXISTS idx_code_edges_dst ON code_edges(dst_id);

-- note_health: knowledge-lifecycle findings (dead_ref|overdue|contradiction) the
-- health pass writes, surfaced by mesh_health + the web dashboard. Derived +
-- rebuildable; cleared and rewritten on each pass.
CREATE TABLE IF NOT EXISTS note_health (
  id          INTEGER PRIMARY KEY,
  note_id     TEXT NOT NULL,                 -- the flagged note's frontmatter id
  path        TEXT NOT NULL,
  issue       TEXT NOT NULL,                 -- dead_ref | overdue | contradiction
  detail      TEXT NOT NULL DEFAULT '',      -- the missing ref / partner note / date
  detected_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_note_health_issue ON note_health(issue);
CREATE INDEX IF NOT EXISTS idx_note_health_note ON note_health(note_id);

-- dropped_notes: files an index pass could NOT index (unparseable frontmatter, or an
-- effective id another file already claims). They are invisible to search and the graph
-- until a human fixes them, which is the whole reason they are tracked: a broken note
-- otherwise vanishes with zero signal.
--
-- Persisted because the process that INDEXES is no longer the process that REPORTS. The
-- drops used to live only in the indexing process's memory, which was fine when every
-- server indexed its own vault; now a read-only `mesh mcp` window has no memory of a drop
-- the owning writer found, and mesh_health would call a vault clean while a note was
-- missing from it. Derived + rebuildable: replaced wholesale by each pass that finds a
-- different set.
CREATE TABLE IF NOT EXISTS dropped_notes (
  path        TEXT PRIMARY KEY,           -- vault-relative
  err         TEXT NOT NULL DEFAULT '',
  duplicate   INTEGER NOT NULL DEFAULT 0, -- 1 when the cause is a duplicate note id
  detected_at INTEGER NOT NULL
);

-- metrics: monotonic usage counters for the ROI dashboard (queries served, notes
-- fetched/written, plus per-note fetch counts keyed "fetch:<id>"). Derived usage
-- telemetry, local only.
CREATE TABLE IF NOT EXISTS metrics (
  key   TEXT PRIMARY KEY,
  value INTEGER NOT NULL DEFAULT 0
);

-- note_reuse: the flywheel measurement. One row per agent write-back, stamped at
-- authoring time, so we can prove (not assert) that written notes get reused by a
-- LATER session. A fetch at least reuse_gap after authored_at counts as reuse (the
-- cross-session proxy). Accumulated runtime telemetry, NOT re-derivable from the
-- vault (the reuse events are observed live), so it is kept across a schema rebuild.
CREATE TABLE IF NOT EXISTS note_reuse (
  note_id     TEXT PRIMARY KEY,
  authored_at INTEGER NOT NULL,           -- unix secs the note was written back
  source      TEXT,                       -- 'agent' | 'manual' | ...
  reuse_count INTEGER NOT NULL DEFAULT 0, -- qualifying (cross-session) fetches
  first_reuse INTEGER,                    -- first qualifying fetch (NULL = never reused)
  last_reuse  INTEGER
);

-- pending_notes: auto-extracted write-back candidates awaiting one-click human review
-- (the input side of the flywheel: notes pulled from a finished session the agent did
-- not write back itself). NOT part of retrieval until promoted to a real note.
-- Accumulated runtime data, kept across a schema rebuild.
CREATE TABLE IF NOT EXISTS pending_notes (
  id         TEXT PRIMARY KEY,    -- content-derived id (dedupes re-extraction)
  type       TEXT NOT NULL,       -- decision | gotcha | post-mortem
  title      TEXT NOT NULL,
  do_text    TEXT,
  dont_text  TEXT,
  why        TEXT,
  confidence TEXT,
  source     TEXT,                -- session id / transcript that produced it
  created_at INTEGER NOT NULL,
  authoring_json TEXT NOT NULL DEFAULT '' -- structured draft; empty retains historical fields
);

-- note_code_links: which notes reference which code symbols (the note<->code bridge).
-- Derived from note bodies + the code index at reindex (rebuilt, not kept), so an
-- agent can go from a symbol to the institutional knowledge about it and back.
CREATE TABLE IF NOT EXISTS note_code_links (
  note_id   TEXT NOT NULL,
  symbol_id TEXT NOT NULL,
  name      TEXT NOT NULL, -- the code symbol's (qualified) name, denormalized for display
  PRIMARY KEY (note_id, symbol_id)
);
CREATE INDEX IF NOT EXISTS idx_note_code_links_symbol ON note_code_links(symbol_id);
CREATE INDEX IF NOT EXISTS idx_note_code_links_name ON note_code_links(name);

-- FTS over symbol name (boosted), signature, and doc. The first five columns are
-- carried unindexed so a hit returns the deep link without a second lookup.
CREATE VIRTUAL TABLE IF NOT EXISTS code_search USING fts5(
  symbol_id UNINDEXED, lang UNINDEXED, kind UNINDEXED, path UNINDEXED, start_line UNINDEXED,
  name, signature, doc,
  tokenize = 'unicode61 remove_diacritics 2'
);
