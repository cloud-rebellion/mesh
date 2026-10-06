# Agents and the flywheel

Mesh is built for coding agents. An agent connects over MCP and retrieves cheaply
instead of reading whole files, then writes back what it learned so the next agent
inherits it. That loop is the flywheel, and it is the whole point.

## Connect an agent

Point your agent at the MCP server (the API tab has a copy-paste config for this exact
vault):

```
mesh mcp --vault /path/to/vault --watch
```

`--watch` keeps the index fresh as you edit notes, so a file you change is searchable
in the same session. It works because that server claims this vault's owning-writer
role when nothing else holds it; beside a running `mesh watch` or `mesh sync --watch`
it reads what that owner indexes and routes its write-backs through it. One indexer,
either way. Check with `mesh doctor <vault>`, which names the owner or prints
`owner: NONE` with the fix (a notice on an in-sync vault, a failing exit once the
index has drifted with no owner). New to a project? `mesh_setup_hooks` (or `mesh hooks install`)
wires Claude Code so the agent reads the mesh at session start and is nudged to write
back before finishing, automatically.

## The retrieval contract

1. Use the relevant collection or `mesh_search` to find a starting point. Hubs from
   `mesh_god_nodes` can help; they are not a mandatory first hop.
2. Search with a token budget; compare the returned cards, completeness and dates.
3. `mesh_fetch` only when a card is not enough, using a section's stable anchor when
   the question concerns one part of a note.
4. Follow supported links with `mesh_neighbors` and `mesh_community` when the
   relationship helps answer the question.
5. On resume, `mesh_changed_since` returns only what changed.
6. Choose a purpose-specific schema with `mesh_templates` and `mesh_note_template`.
   Use `mesh_author_note` to prepare, validate, save a draft or publish authored
   sections. Keep claims tied to evidence and uncertainty; link existing notes only
   when the relationship is supported. Mesh derives identity, dates and placement.
   Complete ordinary notes need no approval. Missing facts belong in a draft.
7. If you edited files directly, `mesh_reindex` makes them queryable now.

## The full toolset

Knowledge:

- `mesh_search` fused full-text + graph + (optional) vectors, tier-0 first, budgeted.
- `mesh_fetch` a note's markdown (or one heading section).
- `mesh_neighbors` / `mesh_community` walk the graph one hop at a time.
- `mesh_god_nodes` the map (hubs) to orient.
- `mesh_changed_since` deltas since a timestamp.
- `mesh_health` what is rotting (dead references, overdue reviews, contradictions).
  See "Knowledge health".

Source code (see "The code index"):

- `mesh_code_search` find a function/type/method by name (file:line + signature).
- `mesh_code_neighbors` the Go call graph (callers + callees) of a symbol.
- `mesh_code_context` a symbol PLUS the team's notes about it (code + the knowledge
  around it, in one call). Use this before changing a function.

Write-back:

- `mesh_templates` list the available note and supporting-block templates.
- `mesh_note_template` / `mesh_block_template` inspect one selected schema.
- `mesh_author_note` prepare, validate, draft or publish through the shared writer.
- `mesh_drafts` find incomplete notes and their current revision before resuming them.
- `mesh_append_note` / `mesh_write_entity` aliases using the same template writer.
- `mesh_propose_template` save a library proposal. New or changed reusable templates
  and blocks require approval; ordinary notes and Markdown formatting do not.
- `mesh_reindex` re-read the vault now (after editing files directly).

Secrets. These three need an attached **secret bridge**: an external secret manager
that speaks Mesh's small capability-mode HTTP contract. (Dockyard, the authors'
self-hosted server platform, is one such implementation; anything that implements the
contract works.) Mesh stores no credentials of its own. You point it at the bridge with
`MESH_SECRET_BRIDGE_URL` (plus `MESH_SECRET_BRIDGE_KEY`) and it brokers access, so an
agent can *use* a credential without ever being handed the value. With no bridge
configured the tools are still listed, and each one answers `configured: false` instead
of failing.

- `mesh_secret_status` is a bridge attached, and how to use it.
- `mesh_secret_list` the stored credentials (names + rotation status only, never values).
- `mesh_secret_use` get a short-lived, single-use capability token for a destination and
  call it through the bridge's proxy. The real key is injected server-side; you never see it.

Onboarding:

- `mesh_setup_hooks` wire the session hooks (read at start, nudge write-back at end).

## Why the agent is the reranker

Mesh returns cheap ranked cards and lets the agent pick the one note to open. That is
why the core runs zero models. When saving the primary agent's context is more important
than avoiding an extra call, optional subscription rerank lets Luna/Haiku rank a bounded
card slate and returns only its small head. Vectors and HTTP rerank remain optional
alternatives; Ollama is never required. The default confidence gate keeps obvious
matches local. Normal-route economics stay in content-free local counters; an
exceptional fallback includes a tiny receipt so it cannot masquerade as model-ranked
output.

You can author normal Markdown inside the selected sections through your editor or
the shared write API. Paragraphs, lists, tables, code and diagrams need no separate
approval. The watcher or `mesh_reindex` keeps direct edits queryable. The API tab lists
the tools and their schemas.
