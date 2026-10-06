# Mesh

A sovereign, single-binary knowledge base whose primary reader is a coding agent.

You edit plain markdown in your IDE. Your agent (Claude Code, Codex) searches it over MCP, and when it finishes a piece of work it **writes back what it learned**, a decision, a gotcha, a post-mortem, so the next agent inherits it. That write-back loop is the point: the knowledge base documents itself and gets smarter every run. Mesh has no reasoning AI inside it; it is the fast engine (parse, index, graph, retrieve), and the agent is the librarian.

It is one Go binary, no cgo, no external services. Retrieving from Mesh is cheaper than having the agent read whole files: it returns ranked cards (title + the matched snippet + why it surfaced) and packs the best bundle that fits a token budget, so the agent reads one note instead of three.

Source-code indexing also supports `mesh code reindex <vault> --through-owner`:
a deterministic, acknowledged refresh without stopping the live index owner.
See [source freshness](docs/source-freshness.md) for the owner upgrade requirement
and the opt-in workspace catch-up timer for server-side merges.

## Honest scope

- **The core, zero models:** cheap card-based retrieval (FTS + graph-BM25 + tier-0, pure Go, no inference, near-zero CPU) plus the agent write-back flywheel, in a single no-glue binary. `mesh_search` hands the agent ranked cards (title + snippet + why); **the agent reads the cards and picks** the 1-2 notes worth fetching. A capable coding agent is already a stronger relevance judge than any bolt-on reranker, so for the agent consumer the agent *is* the reranker, free. This is the whole product for an agent.

- **Optional BYOAI add-ons (off by default; Ollama is never required):**
  - **Vectors (`mesh embed`)** lift recall on paraphrase queries where keyword search breaks (13/20 -> 19/20 on the private corpus behind `docs/BENCHMARK.md`; read that file's caveats before quoting the number). Worth turning on when queries paraphrase the notes; can point at a cloud endpoint for zero local CPU, or be skipped (FTS keyword recall is already 23/25).
  - **Rerank** lifts top-1 precision for a consumer that *trusts the top result without reading the cards*. Use either a local/cloud cross-encoder, or let an already-authenticated Codex/Claude subscription rank 12 compact cards with its cheapest suitable model and return only the best 5. The subscription path needs no API key and no Ollama; it never scans the vault or receives full note bodies. See `docs/BENCHMARK.md` for the measured cross-encoder arm.
- **Also shipped:** a keyboard TUI (`mesh tui`) and a browser app (`mesh ui`) over the same index, plus the client side of sovereign team sync (`mesh join` / `mesh sync` / `mesh conflicts`). The team-sync **server** those talk to is the commercial product and is not in this repository, see [LICENSING.md](LICENSING.md). All optional; the solo, local core stands alone.

Search cards flag missing substantive sections from each versioned template as
`MissingGuidance`. Historical explicit placeholders remain visible; absent retired
fields are not completeness failures. The warning survives compact token packing;
CLI, TUI and web search mark the result as incomplete. Tier-0 describes the note
type, not verified truth. A populated note is not automatically current or correct:
fetch its evidence and check supersession before relying on it.

Imported text uses `[[untrusted-external-content ...]]` provenance markers that
remain literal inside MCP search/batch JSON. Case/spacing variants of forged
current or legacy markers are neutralized. Imported card text, including titles,
is still untrusted data: these cues do not grant authority or guarantee resistance
to prompt injection. Access checks remain independent of model behavior.

## Install

Mesh is a self-contained Go module (`github.com/bright-interaction/mesh`, no cgo,
no external services). One command:

```
go install github.com/bright-interaction/mesh/cmd/mesh@latest
```

That puts the binary in `$(go env GOPATH)/bin`, so make sure that directory is on
your `PATH`.

From source instead (Go 1.26 or newer):

```
git clone https://github.com/bright-interaction/mesh
cd mesh
make install            # builds a static binary to ~/.local/bin/mesh
```

`make install` writes to `~/.local/bin/mesh`; override with `make install
BIN=/usr/local/bin/mesh`. `make build` drops it in `./bin/mesh` instead if you
would rather not install anything.

Released builds check the public Go module tag at most once per 24 hours. When a
newer version exists, `mesh tui` and `mesh ui` show a one-line upgrade banner.
Prebuilt users can run `mesh upgrade [vault]`; Mesh downloads from that vault's
joined hub, verifies the published SHA-256 and embedded release identity, then
atomically replaces the client. Go users also get the exact pinned
`go install ...@vX.Y.Z` command. The check is silent on network
failure, never blocks the TUI from opening, and can be disabled with
`MESH_NO_UPDATE_CHECK=1`. Developer builds and commit-SHA builds without a stamped
release identity do not make the request.

Use `mesh upgrade --check [vault]` for a read-only check. Automatic replacement
currently supports macOS and Linux; Windows users receive the exact download URL.

## Quickstart

This repository ships a small sample vault in `vault/`: the real decisions and
gotchas written while building Mesh. It is the fastest way to see what the tool
returns before you have written any notes of your own. From a clone:

```
mesh index ./vault                                   # parse + build the index
mesh search "rerank" --vault ./vault --budget 4000   # ranked cards, not whole files
```

Six ranked cards come back (abridged here, paths shortened):

```
1. Blending fused score into rerank does not beat pure rerank [tier-0]  (decisions/blending-fused-score-...md)
   # Blending fused score into [rerank] does not beat pure [rerank] ## Context ## Decision ...
   ~ fts
2. Cross-encoder rerank is the answer@1 lever that chunking was not [tier-0]  (decisions/cross-encoder-rerank-...md)
   # Cross-encoder [rerank] is the answer@1 lever that chunking was not ...
   ~ fts
3. Learned fusion weights help the no-reranker path but wash out under rerank [tier-0]  (decisions/learned-fusion-weights-...md)
   ... It matters most for a vectors-on, [rerank]-off deployment, where vector ...
   ~ fts
4. Per-section embeddings do not beat whole-note ... [tier-0]  (decisions/per-section-embeddings-...md)
   ~ linked from Blending fused score into rerank does not beat pure rerank
5. ...
6. ...
packed 6 cards, ~854 tokens (budget 4000)
```

Cards 1 to 3 matched the text. Card 4 did not: it surfaced because the graph
links it to card 1. That one-hop expansion is the part plain full-text search
cannot do. The trailing `~ fts` / `~ linked from` line is the "why it surfaced"
reason, which is what an agent reads before deciding which single note to open.
Six cards for ~854 tokens, instead of six note bodies.

Now your own vault:

```
mesh init my-vault                 # bootstrap a vault (starter index + first build)
mesh templates decision          # fetch fixed sections and content guidance
mesh new decision "Use Postgres over Mongo" --vault my-vault
                                  # incomplete material stays in the draft inbox
mesh index my-vault                # rebuild the index after edits
mesh search "Postgres" --vault my-vault --budget 4000
mesh watch my-vault                # live-reindex as you edit (no manual index; Ctrl-C to stop)
mesh doctor my-vault               # is the index fresh? any drift or lint problems?
```

Search matches the words in your notes, so query with terms the note actually
uses. Semantic (paraphrase) matching is the optional BYOAI vector stage below.

Connect the vault to your agent with one command:

```
mesh install my-vault --client codex       # or: claude-code, claude-desktop, cursor, vscode, windsurf
```

Restart or reconnect that client. Mesh will introduce itself once, offer a
60-second tour, and then stay out of the way. Its permanent MCP instructions remain
small, and retrieval stays zero-model unless you explicitly enable the optional
subscription reranker described below. Claude Code also receives SessionStart hooks;
the other clients need no project prompt-file edits.

`mesh watch` is the local-first, Obsidian-like immediacy: edit a note in your
editor and it is searchable at once, no commit, no manual `mesh index`. It
reconciles at startup, on every change (debounced), and on a periodic safety
tick that always converges, so a missed file event never leaves the index stale.

`mesh doctor` is the one to put in CI, so treat its exit code as the contract: **0 only
when the index is fresh and every note is actually in it**. It exits non-zero when the
index is stale, when there is no index yet, when any note fails to parse, and when two
notes claim the same id. The last two matter most: such a note is invisible to search and
to the graph, so doctor names the offending files and reports `status: BROKEN` instead of
a healthy looking summary.

A missing **owning writer** is the one thing doctor reports without failing on. It always
prints the owner line (`owner: NONE` plus how to start one), but on a fresh, in-sync vault
that is a NOTICE and the exit code stays 0, because `mesh init` leaves exactly that state
and CI checkouts legitimately have nothing running. It is only a failure in combination:
an index that has already drifted with nothing running to catch it up exits non-zero as
`status: STALE`. `mesh status` never fails on a missing owner at all; it reports counts.

A note id is vault-wide, not per folder. Two files that resolve to the same id (two
`README.md` with no frontmatter `id:`, or a note copied as a template that kept its
`id:` line) cannot both be indexed, so Mesh keeps one, quarantines the other, and says
which is which: `mesh index`, `mesh init`, `mesh doctor` and `mesh health` all name the
file and exit non-zero. The fix is to give one of the two a different id, in its
frontmatter or by renaming the file, then reindex.

Already have a Foam / Obsidian-style vault? Bring it up to the Mesh schema in one idempotent pass:

```
mesh migrate my-vault              # dry run: shows what it would change, writes nothing
mesh migrate my-vault --apply      # synthesize ids, updated->when, lift ## Related into related:
mesh index my-vault
```

`mesh migrate` and `mesh scope backfill` rewrite every note in the vault in place,
so both are a **dry run unless you pass `--apply`**. They also exit non-zero if any
file failed, so a partial rewrite never reads as success in a script.

## Recovery: preserve database-only state before rebuilding

Published notes stay in Markdown, but **not everything in `.mesh/` is derived**.
`.mesh/mesh.db` also holds pending review notes, usage/reuse history and stored
embeddings. Deleting it loses those records. Connection grants, sync state,
credentials and queued operations elsewhere in `.mesh/` are not disposable either.

If a command reports `file is not a database (26)` or `database disk image is malformed
(11)`, preserve the evidence and verify the failure before repairing. Stop the
relevant vault services with operator approval and take a consistent protected
backup. Prefer a verified restore into a separate directory. Never delete a live
database or its WAL files by hand. See [backup and recovery](docs/RECOVERY.md).

If no usable database backup exists, an explicitly accepted Markdown-only recovery
can rebuild search and note links, but cannot recover database-only records:

```
mesh index my-vault                # discards a corrupt database; see loss warning above
```

Related failures that are **not** corruption and must not be fixed by deleting
anything:

- `database is locked (SQLITE_BUSY)` means another mesh process (usually a
  `mesh sync --watch` or `mesh mcp --watch` daemon) holds the write lock. Wait and
  retry and inspect the owner's progress; stopping that service needs operator
  approval. Elapsed time alone is not evidence of corruption.
- `no index at <path>` means there is no database yet. Run `mesh index <vault>`.
- `index schema mismatch` means the database was written by a different version of Mesh.
  The index is derived, so Mesh rebuilds rather than migrating, and only a WRITABLE open
  can do that. `mesh doctor`, `mesh ui` (without `--own-index`) and the TUI open the index
  read-only, so they report the mismatch and stop rather than answering from a schema they
  do not match. `mesh mcp` is **not** in that list: it elects itself the vault's owning
  writer when nothing else holds the lock and opens the index writable, so starting it
  against an index stamped with an older schema version rebuilds that index in place.
  Nothing is lost (the index is derived from your notes), but do not expect `mesh mcp` to
  leave an out-of-date index alone. Run `mesh index <vault>` once after upgrading.

Embeddings are the one thing a rebuild costs you: they are kept across schema upgrades
precisely because re-creating them is a paid API call, but they cannot survive a file
that SQLite cannot read. After recovering a corrupt index, re-run `mesh embed` if you
use semantic search.

## Optional: semantic search + rerank (BYOAI, sovereign)

The core above needs no models. These stages are **optional** and **off by
default**. Vectors improve paraphrase recall and require an embedding endpoint;
rerank improves the order after local FTS + graph candidate generation and can
use either an HTTP endpoint or a developer's existing Codex/Claude subscription.
Ollama is one optional local endpoint, not a Mesh dependency.

For an existing MCP install, enable the small subscription model without editing
shell startup files or adding an API key:

```
mesh rerank setup my-vault --client codex        # Codex: Luna, low effort
mesh rerank setup my-vault --client claude-code  # Claude: Haiku 4.5, low effort
mesh rerank status my-vault                      # executable check; no quota
mesh rerank disable my-vault                     # remove this vault's opt-in
```

Or opt in while installing Mesh:

```
mesh install my-vault --client codex --rerank-agent codex
mesh install my-vault --client claude-code --rerank-agent claude
```

Setup writes the provider, pinned small model, and `auto` policy into a 0600
user-private config, keyed by the vault's canonical path and outside both the vault
and project. It does not run the provider CLI, test authentication, or make a model call. Restart the client;
the first ambiguous search checks the existing subscription login. Before changing
the setting, the command says exactly what routed searches send to the provider.

Environment variables remain available for one-shot `mesh search` / `mesh eval`
use or custom deployments:

```
# 1. Vectors: embed notes via any OpenAI-compatible /embeddings endpoint (Ollama, etc.)
export MESH_EMBED_ENDPOINT=http://localhost:11434/v1
export MESH_EMBED_MODEL=nomic-embed-text
export MESH_EMBED_DOC_PREFIX="search_document: "   # nomic-style asymmetric models
export MESH_EMBED_QUERY_PREFIX="search_query: "
mesh embed my-vault                                 # one vector per note

# 2a. No-Ollama subscription rerank: no API key; uses the CLI's existing login.
#     Codex defaults to Luna/low. Claude defaults to Haiku 4.5/low.
export MESH_RERANK_AGENT=codex    # or: claude

# Optional token/latency caps (these defaults are already applied):
export MESH_RERANK_CANDIDATES=12  # local FTS + graph cards sent to the small model
export MESH_RERANK_RESULTS=5      # cards returned to the calling agent
export MESH_RERANK_CARD_CHARS=500 # maximum matched-snippet bytes per card
export MESH_RERANK_POLICY=auto    # default: exact/strong FTS stays local; ambiguous calls the model

# 2b. Or use a cross-encoder endpoint (see tools/rerank-server).
unset MESH_RERANK_AGENT
export MESH_RERANK_ENDPOINT=http://127.0.0.1:8787/rerank
export MESH_RERANK_MODEL=Xenova/ms-marco-MiniLM-L-6-v2

mesh status my-vault    # checks configured signals (subscription checks spend no quota)
mesh economics my-vault # local, content-free call/token/cache/fallback counters
```

Vectors are optional at query time. If the configured embedding provider is down,
blocked by the endpoint security policy, or returns an incompatible vector, an
ordinary search continues with FTS + graph and labels that fallback in CLI/API/MCP
output. A cancelled request and a search with an explicit per-call vector weight still
fail loudly, so benchmarks can never claim semantic retrieval when it did not run.

The subscription presets run non-interactively in an empty temporary directory,
disable tools, MCP servers, hooks, settings discovery, and session persistence,
and mark the process as a Mesh LLM child. This prevents the strict-JSON child from
loading the workspace's own Stop hook. Exact repeat rankings are cached and calls
are serialized within each Mesh process, preventing its concurrent searches from
becoming a quota burst. This path sends the query and compact card fields to the
selected provider; it is therefore opt-in and configured only by the user-private
per-vault file or local process environment, never by project or vault configuration.
Override the model with `MESH_RERANK_MODEL` and the timeout
with `MESH_RERANK_CMD_TIMEOUT` (default 90 seconds). `auto` is the subscription
default: an exact note/title lookup or a clearly separated full-text winner stays
zero-model, while an ambiguous slate is reranked. Set `MESH_RERANK_POLICY=always`
only for an evaluation or when reranking is a hard operator requirement. The strong
match threshold is tunable with `MESH_RERANK_CONFIDENCE_MARGIN` (default 0.45).

Both HTTP endpoints above are local, and that is the supported endpoint default:
an endpoint you pass on the command line or set in the environment is operator input, so Mesh dials
it as given, `localhost` and `127.0.0.1` included. The SSRF guard applies to the
endpoint someone else could have written for you: the `[embedding]` / `[rerank]`
fields in `.mesh/config.toml`, which the web UI's settings page rewrites over
`PUT /api/config`. If your endpoint lives there and is private, opt in explicitly:

```
export MESH_ALLOW_PRIVATE_LLM_ENDPOINT=1   # allow a private/loopback endpoint that
                                           # came from config.toml or the web UI
```

Mesh names that variable in the refusal itself, so you never have to find this
paragraph to get unstuck.

Vector search in this repository is a brute-force cosine scan, which stays under
5 ms well past a few thousand notes. The commercial build adds an approximate
(HNSW) index for vaults large enough to need one, see [LICENSING.md](LICENSING.md).

Once set, `mesh search` / `eval` / `mcp` fuse the semantic signal and route the
bounded rerank according to its policy. Turning a stage OFF is safe (no embedder
means lexical-only). HTTP rerank and subscription policy `always` fail loudly when
unavailable. Subscription policy `auto` returns the local ranking with an explicit
`fallback` receipt, opens a five-minute per-process circuit, and records the failure;
it does not repeatedly spend quota or pretend the model answered. Tune the circuit
with `MESH_RERANK_FAILURE_COOLDOWN` in seconds. Unset `MESH_RERANK_AGENT` and
`MESH_RERANK_ENDPOINT` to turn rerank off for real. Pointing an endpoint at a cloud
provider sends bounded note text off-box; enabling subscription rerank sends only
the query plus compact cards.
A ready-to-run local cross-encoder server lives in `tools/rerank-server/`.

Got a set of labelled queries for your corpus? `mesh tune cases.json --test
held-out.json` grid-searches the fusion weights to maximize answer@1 and prints
the held-out result plus the `MESH_WEIGHT_FTS/GRAPH/VEC` line to apply the
winner. It tunes the fused ranking, so it helps most when you run vectors
without a reranker (with a reranker on, the cross-encoder owns the top result and
fusion weights wash out). Always pass a held-out `--test` set; tuning to the
queries you report on is how you fool yourself.

`mesh economics` also attributes the first `mesh_fetch` after a search to its
content-free rank and route (within that MCP session and a ten-minute window), so
real use reveals whether agents choose rank 1 or keep digging without retaining
queries or note content.

`mesh eval cases.json --require-rerank-win` adds the economics gate when a
reranker is configured. It replays every labelled query through both the normal
route and the identical local Mesh route, then requires equal-or-better recall@5
and answer@1, a lower combined median after adding the second call's provider-reported
tokens when available (otherwise the same bundled tokenizer estimate), and zero
fallbacks. A failing gate is a reason to keep that provider/model
opt-in, not to tune until the sample agrees.

## Wire it to your coding agent

Mesh speaks MCP (JSON-RPC) over stdio. Point your agent at:

```json
{ "command": "mesh", "args": ["mcp", "--vault", "/abs/path/to/my-vault", "--watch"] }
```

The agent then gets: `mesh_search` (fused, budget-aware), `mesh_fetch` (a note or one heading by anchor), `mesh_god_nodes` (the hub map to orient), `mesh_changed_since` (deltas on resume), and the write-back tools `mesh_append_note` / `mesh_write_entity`. `mesh_templates` exposes the approved library; `mesh_note_template` and `mesh_block_template` fetch individual definitions. `mesh_author_note` prepares, validates, drafts and publishes through the shared authoring system. The `index` template creates collection entry points; ordinary notes using registered templates need no approval. See [the authoring guide](docs/AUTHORING.md) for templates, Markdown tables/lists, drafts and reviewed migration. The retrieval contract (how to query cheaply, and to write back when done) is served as the MCP `initialize` instructions and the `mesh://contract` resource, so any agent uses it well without extra prompting.

### Bounded batch fetches and worker settings

Start with search cards and stop if they suffice. Fetch one missing heading with
`mesh_fetch`; batch several known needed sections with `mesh_fetch_many`, especially
overlapping sections. Fetch the relevant guidance before acting on a restriction;
a search snippet is not complete operational context. Whole-note batches are not
automatically cheaper or faster.

Set a total response allowance and call limit before retrieval. For batch omissions,
map indices to that request's inputs and allow at most one prioritized follow-up
for still-needed sections within the remaining allowance. Below the 256-token
minimum, or if that follow-up still lacks required facts, stop and report the gap.
Never repeat successful items, silently increase the allowance, or bypass the cap
with a single fetch. `unavailable`/`too_large` are not budget omissions and do not
trigger automatic fallback. Preserve safety warnings and untrusted-content envelopes.
Use a one-item batch when a server-enforced response cap is needed; single fetch
has no such budget parameter.

The compact startup instructions point to `mesh://contract` for the full policy.
This is agent guidance, not a server-enforced whole-task quota. Count search and
contract responses too; separately reserve JSON-RPC/request/instruction/output
overhead rather than equating batch receipts with total model usage.

The scripted retrieval-journey checks reuse the HTTP evaluation harness. Run
`go test ./internal/mcp -run '^TestRetrievalJourney'` for correctness; opt into
measurements with `MESH_RETRIEVAL_JOURNEY_EVAL=1 go test ./internal/mcp -run
'^TestRetrievalJourneyEvaluation$' -count=1 -v`. These measure fixed client
decisions, not model adherence, and do not impose timing thresholds in CI.

`mesh_fetch_many` accepts 1–16 `items` with a note `id` and optional heading
`anchor`, plus a response-token `budget` (default 8000; range 256–32000).
Repeated notes are read once; duplicate and nested sections share one safety
header. An explicit whole-note request subsumes sections of that note. Results
follow first-note input order and list their original input indices; `omitted`
lists indices excluded by the budget. Whole results are omitted rather than
cutting safety text. `unavailable` is an opaque read/permission/anchor failure;
`too_large` means the note exceeds the batch's byte limits (1 MiB source,
128 KiB rendered per-note text).

The advanced **Batch fetch workers** web setting controls request-local concurrency.
Alternatively, edit `<vault>/.mesh/config.toml`:

```toml
[retrieval]
fetch_workers = 2
```

The default is **2**, with an allowed range of **1–16**. Omit the key (or clear
the web field) to restore the default. `MESH_FETCH_WORKERS=4` in the Mesh
process environment overrides the file and locks the web field. File changes
apply to the next batch; changing a running process's environment requires
restarting it through your normal operator workflow. Invalid explicit values
are rejected. A batch uses no more workers than its number of distinct notes;
the response's `workers_used` field reports that actual count.

Workers are created for the request and **all exit before it returns**. There is
no idle pool. Cancellation stops queued work and joins in-flight reads. The
five-second timeout is cooperative: a blocked filesystem operation can delay
return, but no request worker is abandoned. This limit is **per request**, not a
global cap across concurrent users. No model/extractor children are started.

More workers are not automatically faster for a larger vault: independent note
count, storage latency and contention matter. Benchmark complete requests before
raising the limit. Anchored results contain bounded safety-context excerpts and
explicit incomplete/truncated-context signals; they do not guarantee that all
relevant warnings elsewhere in the note have been fetched.

That is the whole setup. The server elects itself the vault's **owning writer**
when nothing else holds the vault (a claim in `<vault>/.mesh/owner.lock`), so a
note it writes back is queryable immediately and no separate daemon is needed.
Start a `mesh watch` or `mesh sync --watch` beside it and that one takes
ownership instead; the MCP server notices, reads the index rather than writing
it, and routes its write-backs through the owner. Exactly one process indexes
either way, which is the point.

Local file events carry their exact changed paths to that owner, and
`mesh sync --watch` publishes them to the local index before requesting a hub
round. From v0.19 onward, a separate serial sync worker keeps indexing responsive
even when a previous hub call is slow. One queued follow-up coalesces edits made
during sync; completion triggers local discovery without requesting another sync.
Shutdown drains the active durable sync round before releasing the index owner.

Startup still performs an authoritative full rebuild to catch offline edits and
seed the incremental cache. Its log separates indexing from lifecycle-health work.
From v0.20, contradiction checks tokenize guidance once per pass and restrict
comparisons to shared tags, preserving the existing findings and 0.6 similarity
threshold. This reduces work at startup and on the five-minute health cadence
without skipping checks, adding model calls, or weakening index validation.

Retriever setup and graph refreshes do not call embedding models. Actual semantic
queries validate vector width, and explicit health checks still probe reachability
and dimensions. Local write acknowledgements do not need an embedding endpoint.
From v0.21, the read-only MCP acknowledgement wait applies its existing
deadline to refresh-lock acquisition, note reads, SQLite snapshots, and retriever
construction too. A timeout preserves the saved note and returns an `index_stale`
receipt; it does not prove the owner is down. Canceled construction leaves the
previous graph/retriever pair intact. This bounds cooperative readback work, not
the file's durable write, owner recovery, or the owning writer's indexing pass.
Targeted indexing avoids rediscovering a known changed path; note creation still
scans vault-wide ID claims, and periodic/remote-triggered passes still scan the
vault as the convergence safety net. This is not an end-to-end write latency SLO.

From v0.22, slow note creation and owner indexing operations emit structured
phase timings through the logger (stderr by default, never the JSON-RPC reply).
Operations under one second remain silent. Longer operations log a summary when
they return; work still in flight logs its active phase every ten seconds.
`operation`, process `pid`, and process-local `trace_id` correlate progress with
the summary. Labels are static: these records contain no note content, titles,
paths, credentials, or raw errors. Nested operation times overlap; do not sum
them. `returned` means the function returned, not that the write succeeded.

Look for `note_plan.id_scan` before publication; `note_create` separates file
claim/write/fsync and collision checks. `mcp_write` separates related-note lookup,
publication, acknowledgement and telemetry. `owner_reconcile`/`owner_targeted`
include mutex waits, queued operations, indexing and health. `index_full`,
`index_incremental` and `index_targeted` break indexing into discovery/parsing,
graph construction, communities, persistence and code linking. These diagnostics
add no model calls and do not skip durability, collision or index-validation
checks. They locate stalls; they do not fix them or impose new deadlines.

From v0.30, ordinary reader refreshes reuse the installed graph/retriever when
both the database and the consumed retrieval configuration are unchanged.
A dedicated read-only SQLite connection samples
[`data_version`](https://www.sqlite.org/pragma.html#pragma_data_version) before and
after construction; values are never compared across connections. In v0.30–v0.31,
every database commit invalidates reuse, including graph/code-link, vector and
telemetry writes.
This is deliberately conservative, not a note-hashes-only cache. The monitor
holds no transaction between samples and does not occupy the normal read pool.
Queued callers recheck after acquiring the reload mutex, coalescing duplicate
startup/watch refreshes. Exact-version acknowledgement graph loads are unchanged;
a successful stable acknowledgement can prime the next ordinary refresh.

Construction consumes an immutable snapshot of parsed configuration, environment
and user-local subscription preferences; a fresh input comparison detects
same-size/mtime edits without mislabelling a racing configuration read. No model
is called. Config/monitor/optional-vector-read failures disable reuse, and a
relevant commit during construction forces another refresh on the next pass. Inputs and
digests are not logged. `mcp_refresh.freshness_check` and `retriever_config` time
the lightweight local checks. Hot replacement of an open database file is not a
supported rebuild and requires a reader restart. Those v0.30 changes require no
schema migration, new writer, longer deadline or weaker publication check.

From v0.31, full reader graph loads preallocate their maps from node/edge counts
read in the same SQLite snapshot, with hints capped at 65,536 entries per map.
Larger graphs still grow normally. Row-scan scratch is reused without sharing
mutable node attributes or edges. A synthetic 24,000-node / 48,000-edge benchmark
allocated 58.5 MB/load instead of 69.3 MB/load (15.5% less); measured runtime was
roughly 109–112 ms in both cases, so this is an allocation reduction, not a
demonstrated latency improvement or end-to-end acknowledgement SLO. The capacity
query is timed as `load_graph.capacity`.

From v0.32, an optional persisted retrieval revision lets ordinary reader refreshes
reuse their graph/retriever after bookkeeping-only commits. The owning writable
opener installs one derived singleton table and 30
[SQLite AFTER triggers](https://www.sqlite.org/lang_createtrigger.html), atomically.
The base schema version stays unchanged: existing notes, vectors, telemetry and
pending candidates are not rewritten. Old readers still work; new readers with
an old owner/database fall back to whole-database invalidation until an upgraded
writer installs the tracker. Activation therefore requires upgrading/restarting
the owner, not just the MCP reader. Never start a second writer to install it.

Triggers cover notes, nodes, edges, vectors, corpus stats, code tables, note/code
links and metadata (except `health_completed_at`). They also run for writes from
older processes that know nothing about this protocol. Usage/reuse counters,
health findings, dropped/pending notes and FTS tables are read live and do not
invalidate cached graph/retriever state. FTS is not an in-memory retrieval cache.
Adding cached database inputs requires reviewing the tracking protocol.

Readers validate the exact tracker definitions when SQLite's schema version
changes, then sample the epoch/revision on that same short read snapshot. Missing,
altered or additional persistent triggers, invalid/missing revision rows and read errors prevent
targeted reuse; schema/epoch changes invalidate it. Canonical partial installs
can be repaired by the owning opener with a new epoch; unrecognized definitions
are left untouched. Tracker rows and SQLite's schema counter are reserved, not
operator-editable metadata. No snapshot is held between samples. Model/config handling,
exact-version acknowledgement loads and final publication checks stay unchanged.
Content-free logs report `reader freshness tracking` mode `retrieval_revision_v1`
or the conservative `data_version` fallback.

On a synthetic 3,000-node graph, a real metric commit plus refresh took
0.37–0.41 ms with tracking versus 7.64–8.44 ms without, allocating about 29 KB
versus 5.39 MB. This is not a live acknowledgement SLO. Triggers have write cost:
an isolated 1,000-row UPDATE took 0.95–1.08 ms versus 0.36–0.49 ms without
tracking; a one-row UPDATE took 79–151 µs versus 59–84 µs (three runs of 50
iterations). These are Apple M3 microbenchmarks, not full indexing timings.
Avoiding repeated reader rebuilds is the intended tradeoff.

From v0.33, full vector loads look up each note through its existing primary-key
index instead of building a temporary retrieval-hash index. The original exact
node-ID, current retrieval-hash and canonical-model filters remain; metadata and
rows still share one cancellable read snapshot, with chunks ordered by index.
The lookup slices the five-byte `note:` prefix as bytes to preserve unusual IDs,
including embedded NULs, and still rejects other namespaces. No schema migration,
owner restart, embedding call or change to acknowledgement checks is required.

At v0.33, on an Apple M3 synthetic fixture with 3,000 notes and 2,350 768-dimensional
vectors, full loads took 10–11 ms versus 12–13 ms with distinct hashes. With all
notes sharing one hash, loads took 9.5–11.2 ms versus 875–942 ms: primary-key lookup
avoids walking all same-hash candidates for each vector. Allocation was unchanged
at about 22.3 MB/load. These microbenchmarks do not establish a live startup or
acknowledgement SLO. Run `go test ./internal/index -run '^$' -bench
'^BenchmarkVectorLookup$' -benchtime=3x -count=2` to compare both query strategies.

From v0.34, vector row scans borrow the driver's byte buffer until decoding is
finished, avoiding an intermediate copy. Only independently allocated float
slices enter the retriever; borrowed bytes never survive the next row or close.
Cancellation checks and deferred row closure release the borrowed-buffer read
hold and discard canceled partial results. Metadata/row snapshot consistency,
chunk order, exact vector bits and stale/orphan/model filtering are unchanged.
No schema migration, owner restart or model call is required.

The same 2,350-vector fixture now allocates about 15.0 MB/load instead of 22.3 MB
(33% less), with about 14,205 allocations instead of 21,253. Allocation profiles
confirm the scan's intermediate `bytes.Clone` is gone; SQLite's BLOB allocation
and the final float storage remain. Alternating 50-iteration runs measured
9.3–16.4 ms versus 11.2–11.7 ms for the prior loader, so this is not a demonstrated
latency improvement or an end-to-end acknowledgement SLO. Reproduce the current
loader with `go test ./internal/index -run '^$' -bench
'^BenchmarkVectorLookup$/^shared_hash=false$/^primary=true$' -benchtime=50x`;
use a v0.33 checkout for the old allocation baseline.

From v0.29, reader-side traces distinguish acknowledgement polling from snapshot
installation. `mcp_acknowledge` separates target parsing/hashing, version refresh,
final database/file verification and poll waits. `mcp_refresh` and
`mcp_version_refresh` separate the reload mutex, graph loading and installation;
`mcp_install_graph` separates note fingerprints, retriever construction,
reconciliation counts and publication lock/swap. `load_graph_snapshot` and
`load_versioned_graph` show read-transaction acquisition, version checking where
applicable, graph loading and transaction completion. `load_graph` splits node,
edge and degree work; `retriever_build` splits ranker construction, configuration,
stored vectors (including optional ANN construction), and reranker/weight setup.
Construction does not call a model. The same silent-under-one-second and static,
content-free logging rules apply; nested times overlap and cannot be summed.
This is diagnostic coverage, not a latency fix: the acknowledgement deadline,
single-snapshot version gate and final current-file checks are unchanged.

From v0.28, adding a new note checks the `notes` primary key inside the writer
transaction and skips deleting a nonexistent FTS row. FTS5's `node_id` is
`UNINDEXED`, so the former delete scanned the search table even for a new ID.
Existing IDs still replace their search rows; removals and full rebuilds are
unchanged. This relies on Mesh's atomic note/search persistence invariant, not a
cached ID set. A full rebuild repairs independently damaged/orphaned search rows.
No schema migration, search-ranking, model or durability changes are required.
`persist_note_upserts` now separates derivation, existing-note lookup, search-row
deletion, note writes and search writes. In `BenchmarkNewNoteUpsert` (3,000 notes
with ~9 KB bodies, Apple M3, two runs of five iterations), retaining the old
missing-key scan cost 6.8-9.3 ms versus 0.09-0.11 ms without it. Both arms run the
same upsert and roll back each iteration; this isolates the avoided scan, not
end-to-end writeback or the remaining cost of editing an existing note.

From v0.27, a note edit refreshes only the added/changed/removed note IDs in the
note-to-code bridge. It no longer rereads every note or replaces unrelated links.
Full startup and code-index refreshes still rebuild all links, since symbol changes
can affect any note. Both paths share the same title/raw-file token matching,
ambiguity rules and writer-transaction replacement; no model calls or schema changes.
The symbol resolver is rebuilt from current indexed symbols, not cached. Slow
`note_code_links` traces separate metadata, resolution, file reads and persistence.
On a synthetic 3,000-note / 12,000-symbol vault, single-note bridge refresh took
6.7-7.0 ms versus 97.8-97.9 ms for full refresh, allocating 3.6 MB versus 109.4 MB
(`BenchmarkCodeLinkRefresh`, Apple M3, two runs of three iterations). These are
bridge-only measurements, not end-to-end writeback latency or a fix for the
previous intermittent persistence stall. Linking remains a best-effort step after
the note/FTS/graph commit; a full refresh repairs a previously failed bridge pass.

From v0.26, slow persistence logs distinguish `index_write` queue wait from
`index_transaction` authorization, SQLite begin, callback, commit/rollback and
lease release. `persist_full` and `persist_incremental` separate notes/search,
graph and vector pruning; `persist_graph_delta` separates encoding from node and
edge work, with `persist_graph_nodes` / `persist_graph_edges` splitting scans,
deletes and upserts. `index_checkpoint` shows the existing periodic checkpoint.
PASSIVE avoids waiting for readers, but still performs page copies and possibly
file sync ([SQLite contract](https://www.sqlite.org/c3ref/wal_checkpoint_v2.html)).
Automatic checkpoints inside commit remain part of the commit phase.

These are diagnostics, not a concurrency or durability change: no new writers,
queue priority, timeout extension or weaker fsync settings. They remain silent
below one second, use static labels without content/paths/SQL/error text, and
report progress every ten seconds. Nested timings overlap, and a returned trace
is not proof of success. Correlate process IDs and time windows; transaction and
caller traces have separate IDs. They distinguish where time was spent, not
whether the kernel delay came from storage, memory pressure or CPU scheduling.

From v0.25, note planning reads identity headers with at most four concurrent
readers per scan and merges results in traversal order. Short notes no longer
reserve the full 64 KiB head-read ceiling. The scan still checks fresh disk
contents, including unindexed notes and metadata-preserving edits; it does not
trust a stale index or metadata cache to declare an ID free. O_EXCL publication,
fsync, cross-type collision checks and cancellation behavior are retained.
This reduces planning work; it does not bound filesystem latency or prioritize
writebacks over a running owner sweep.

From v0.24, incremental and targeted indexing compare the whole rebuilt graph
with the committed database and write only changed nodes and edges. This keeps
global community and supersession changes correct while avoiding a full graph
table rewrite for a one-note edit. Notes, search rows, and graph deltas still
commit atomically. The comparison remains linear in graph size; full/startup
indexing still uses the authoritative full rewrite. No model calls or schema
migration are added. See the [persistence benchmark](docs/BENCHMARK.md#incremental-graph-persistence)
for the measured scope and reproduction command.

From v0.36, graph comparison reuses SQL scan records and stores each edge key only once.
In the 24,000-node/48,000-edge single-label-edit benchmark, this reduced allocated
bytes from 41.9 MB to 28.7 MB per operation (about 31%) and removed about 72,000
allocations. Three alternating 30-iteration runs on Apple M3 measured 70.2-84.3 ms
before and 69.3-71.1 ms after; these are isolated persistence measurements, not an
end-to-end writeback latency guarantee. Whole-graph comparison, nullable-row repair,
changed-row-only writes, and atomic notes/search/graph publication are unchanged.

From v0.23, the owning watcher's lifecycle-health analysis runs in one background
pass per Store, outside the reconciliation mutex. Slow note reads, source-tree
walks and Git checks no longer hold up the next indexing callback. The normal
single database writer still publishes the complete dead-reference, overdue and
contradiction report atomically; there is no second writer. A newly created index
can have no health report until the first background pass succeeds.

Health attempts retain the five-minute cadence, coalesce while running, and retry
failed/changed-input passes no sooner than thirty seconds later. Analysis has a
two-minute cooperative deadline, including Git and cancellable reads; publication
has a separate two-second budget within that deadline. Indexed note/code identity
is checked again inside the publication transaction. Cancellation, changed inputs,
ownership loss or failed publication preserves the previous report rather than
replacing it with partial findings. `meta.health_completed_at` records successful
background publication; `background_health` logs separate snapshot, analysis,
contradiction and publication costs. Sustained edits or repeatedly slow analysis
can keep that report stale; explicit health commands remain available.

Store shutdown cancels and joins its health pass before closing database pools.
A kernel filesystem read cannot be forcibly canceled: any late read-only result
is isolated and discarded, never published. This change removes health analysis
from the serial indexing path, not all CPU/I/O contention or the brief health
publication transaction. Index persistence and code linking remain separate costs.
These are cooperative budgets, not hard wall-clock guarantees: existing ownership
metadata locks and kernel commit/fsync calls cannot be forcibly interrupted. Shutdown
joins any admitted publication before closing its database, even if that takes longer.

From v0.35, `mesh mcp --http` handles SIGINT/SIGTERM by stopping new request admission and
giving admitted requests 20 seconds to finish, including write-back receipts.
After that grace period it cancels remaining requests and closes their connections,
then still joins their handlers before stopping/joining the watcher and closing
the store. Expiry is reported as a shutdown error. The 20 seconds bounds graceful
HTTP draining, not total process exit: uninterruptible I/O and cleanup can take
longer. Allow additional supervisor stop time; a forced kill can still interrupt
a write or lose its response. A missing response does not prove a write failed:
check the saved note before retrying. Stdio and the separate sync owner are unchanged.

The `--watch` flag runs the live reindexer inside the server, so notes you (or a
teammate) edit in your editor become searchable in the same session without a
restart. Watch progress goes to stderr; stdout stays the pure JSON-RPC stream.
Omit it and the index only refreshes on the agent's own write-backs. On a vault
somebody else owns, `--watch` re-reads what that owner indexed rather than
indexing itself.

Not sure anything is indexing? `mesh doctor <vault>` names the owner, or prints
`owner: NONE` with the fix when there is none. That on its own is a notice, not a
failing exit; doctor fails when the index has drifted with no owner to catch it up.

## Team sync

Share a vault across a team with no git on any client. The sync **client** is part
of the open core; the **team-sync hub** is the commercial / pro product (hosted at
mesh-sync.cloudrebellion.tech, or self-host under a commercial license, see
[LICENSING.md](LICENSING.md)). Clients pull-reconcile against it:

```
# On each teammate's laptop (against a hosted or licensed hub):
mesh join https://mesh.example.com <invite-token> my-vault   # clone, no git needed
# ... edit notes in your editor ...
mesh sync my-vault                                    # push yours, pull theirs
```

Reconcile-first: `mesh sync` is a three-way merge. Two people adding blocks to the
same page auto-merge; a true overwrite of the same lines keeps the hub version and
saves yours to a `*.sync-conflict-*.md` sibling to resolve by hand. Long note names
use a reserved `.sync-conflict-base-v1` directory to keep every component within
the filesystem limit while preserving the exact original name. Use v0.17 or newer
to resolve these overflow conflicts; older clients retain the bytes but cannot
reconstruct the base path. Deletes and
renames propagate; the hub authors git history attributed to each user. Add
`mesh sync --watch` for real-time SSE push (the hub's changes pull in as they
land). A standalone `mesh-curator` worker can reconcile non-trivial conflicts with
the team's own BYOAI model, committing the merged note back through the normal sync
path (the hub itself stays AI-free).

Before uploading, v0.18+ checks notes against the hub's content limits (1 MiB,
no NUL bytes). Invalid notes stay untouched locally and are **not** marked
synced; other notes and incoming changes continue syncing. `mesh sync` names
each blocked path and its exact content problem. Correct the file to resume
uploads automatically. The watcher reports unchanged content problems once per
process, reports edited/reappearing problems again, and retains the outstanding
count on subsequent sync receipts. This does not suppress unknown hub-side
permission/scope rejections or change their retry behavior.

The team dashboard separates actual cross-user reuse from the older
same-user/later-session proxy. Agent-authored notes are attributed at their first
hub publication; successful hosted MCP fetches count immediately, while local
stdio MCP fetches upload a content-free, retry-safe event on the next `mesh sync`.
The hub retains only aggregate `same`/`cross` event relations, never the reader,
query, snippet, or note body. Hosted `mesh_append_note` writes are committed to the
team Git history before success is returned, so they reach every sync client rather
than remaining as untracked files in the hub worktree.

## Upgrading: behaviour changes you will notice

An audit pass changed how several commands behave. Each of these is deliberate, and each
one is the kind of change that is confusing if you meet it without warning.

**`mesh migrate` and `mesh scope backfill` are dry runs by default.** They rewrite every
note in the vault in place with no backup, so writing is now opt-in via `--apply`. This is
the one most likely to catch you: a script that calls either of them bare no longer
rewrites anything, and it exits 0, so it looks like it worked. Add `--apply`.

**Read-only surfaces refuse an index written by an older Mesh.** `mesh doctor`, `mesh ui`
(without `--own-index`) and the TUI open the index read-only, and only a writable open can
rebuild a changed schema, so nothing ever migrated an upgraded index. They now say so and
name the fix (`mesh index <vault>`) instead of answering from a schema they do not match.
That matters most for `mesh_health` and `mesh doctor`, which reported a CLEAN vault over an
index whose quarantine table their binary expected and the file did not have. Run
`mesh index <vault>` once after upgrading; your notes are untouched. `mesh mcp` is the
exception: it now elects itself the owning writer, opens the index writable, and therefore
rebuilds a schema-mismatched index rather than refusing it.

**`mesh doctor` exits 1 when a note does not parse.** It used to report `status: healthy`
while holding zero indexed notes, because it counted only what had made it into the index
and an unparseable note never gets there. It now reports `status: BROKEN` and names how
many notes are invisible to search. If you gate CI on `mesh doctor`, it can now fail.

**`mesh index` and `mesh init` exit 1 when two notes claim one id.** Only one of the two
can be indexed. Both commands used to pick a winner silently, so `mesh init` printed
"1 notes" for two files and exited 0, and the loser was missing from search with no
signal at all. They now name the file they left out and fail, and the winner is stable:
whichever file already holds the id in the index keeps it, so a rebuild and a live
`mesh watch` never disagree about which note the id means.

**`mesh index` can delete a corrupt index.** A `.mesh/mesh.db` that SQLite refuses to open
used to dead-end every command including the one that rebuilds it. `mesh index` now
removes and rebuilds it, strictly when the failure is a corrupt database and never for any
other open error, such as a busy lock. Your notes are the source of truth; the index is
derived (see Recovery above).

**`mesh conflicts resolve --take-mine` exits non-zero when the hub refuses your push.** It
used to delete your parked conflict sibling and print that it had pushed, even when the
hub had rejected the path for a role, ACL, scope or size reason. It now keeps the sibling,
names the refusal, and fails. Wrappers treating exit 0 as "resolved" should be rechecked.

**`mesh curator log --status` rejects unknown values** rather than silently matching
nothing. Valid values are `failed` and `resolved`.

**`GET /api/search` defaults changed** to `limit=20` and `budget=8000`, matching the MCP
tool. Results are token-packed now; previously `budget=0` skipped packing entirely and
`limit` had no ceiling. `limit` is capped at 100.

**The hub's curation activity endpoint takes `?cursor`**, so failed jobs older than the
newest page are reachable. A malformed cursor returns 400 rather than being ignored.

**Search no longer hangs on a very large repetitive note.** One multi-megabyte note of
repeated text, a pasted deploy log or a concatenated transcript, could pin a core at 100%
for minutes with nothing able to cancel it, because SQLite was being asked to pick the
result excerpt and that costs roughly the square of the number of matches inside a single
document. Mesh builds the excerpt itself now. Matching and ranking still see the whole
note, so nothing becomes less findable, and the excerpt looks the same. A search that
somehow still runs long fails after 10 seconds with a message rather than hanging.

**The search query itself is now capped**, on `mesh_search`, `mesh_code_search`,
`mesh_code_context`, `GET /api/search` and `POST /api/ask`. A query over 4096 bytes is
refused with a message naming the limit, and a query is read as at most 64 distinct terms
on every surface, the CLI included. Repeating a word never changed which notes matched,
only how long the search took.

## Commands

Set up and capture:

| Command | Purpose |
|---|---|
| `mesh install` | One-shot setup: register Mesh and deliver a one-time in-agent welcome; Claude Code also gets its SessionStart hook |
| `mesh install --rerank-agent codex\|claude` | Install and explicitly opt the local MCP server into the pinned small subscription model; no API key or setup inference call |
| `mesh install --remove` | The inverse: drop the mesh entry from that client's config (and the session hooks on Claude Code). Run it before deleting the binary |
| `mesh init [path]` | Bootstrap a new vault |
| `mesh new <template> "<title>"` | Create a complete note or an incomplete draft with automatic identity, timestamps and fixed headings |
| `mesh migrate [vault]` | Bring a Foam / Obsidian-style vault up to the Mesh schema (dry run unless `--apply`) |
| `mesh ingest <source>` | Pull external knowledge (GitHub, Slack, Linear, Jira, Notion) into the vault, incrementally |
| `mesh extract <transcript>` | Turn an agent session transcript into candidate write-back notes to keep or discard |
| `mesh hooks install` | Wire Claude Code session hooks: read Mesh at session start, nudge write-back at the end |

Index and retrieve:

| Command | Purpose |
|---|---|
| `mesh index [vault]` | Parse + persist the index (`.mesh/mesh.db`). Non-zero if a note was left out because another note claims its id |
| `mesh watch [vault]` | Live-reindex on every change (debounced + periodic reconcile) |
| `mesh embed [vault]` | Embed notes via a BYOAI endpoint (turns on semantic search) |
| `mesh search "<query>"` | Fused, budget-packed retrieval (semantic + rerank when configured) |
| `mesh ask "<question>"` | Answer a question from your notes + code with citations (needs a BYOAI model) |
| `mesh code <search\|context\|reindex>` | Source-code index: find a symbol by name (file:line), or pair it with the notes about it |
| `mesh orient [vault]` | Print a session orientation: entry points, recent changes, how to retrieve |
| `mesh mcp [--vault] [--watch]` | Serve the agent retrieval + write-back surface (live-reindex with `--watch`) |

Inspect and maintain:

| Command | Purpose |
|---|---|
| `mesh status [vault]` | Index row counts + which retrieval signals are active |
| `mesh version` | The commit this binary was built from, plus the Go version. Include it in a security report (see SECURITY.md) |
| `mesh lint [vault]` | Frontmatter / links / filenames (non-zero exit for CI) |
| `mesh doctor [vault]` | Index freshness (drift), counts, health. Non-zero if the index is stale or any note is invisible to search |
| `mesh health [vault]` | Knowledge lifecycle: dead source refs, overdue reviews, contradictions, plus notes missing from the index |
| `mesh structure [vault]` | Grade the vault's organization; `--wire-orphans` proposes only shared-tag-corroborated links by default |
| `mesh flywheel [vault]` | Write-back reuse metrics: does written-back knowledge get used again? |
| `mesh economics [vault]` | Content-free retrieval economics: call rate, accounted tokens, cache/fallbacks and search-to-fetch choices |
| `mesh rerank <setup\|status\|disable>` | Reversible local-only subscription rerank setup for an existing MCP registration |
| `mesh guards <list\|suggest>` | Turn gotchas into candidate pre-commit guards (knowledge to enforcement) |
| `mesh scope backfill` | Stamp an explicit access scope on notes that have none (which notes a given member may see; dry run unless `--apply`) |
| `mesh eval <cases.json>` | Gate-1 retrieval measurement vs FTS baselines; `--require-rerank-win` also gates rerank economics |
| `mesh tune <cases.json>` | Learn fusion weights from labelled queries (validated on held-out) |

View:

| Command | Purpose |
|---|---|
| `mesh tui [vault]` | Keyboard three-pane terminal view (notes, ranked search, preview + neighbors) |
| `mesh ui [vault]` | Browser app (graph, search, docs, API reference) over the same index, localhost |
| `mesh serve-ssh [vault]` | Serve the TUI over SSH so a teammate browses the graph with `ssh`, no install (key-auth, fail-closed: binds `127.0.0.1:2222` by default, and `--allow-anonymous` is refused off loopback) |

Team sync. These are the client side and ship here, but they all talk to a
**team-sync hub**, which is the commercial product and is not in this repository
(see [LICENSING.md](LICENSING.md)):

| Command | Purpose |
|---|---|
| `mesh join <hub> <invite> [vault]` | Join a team vault and clone it (no git). Needs a hub. |
| `mesh sync [vault]` | Reconcile with the hub (push local edits, pull teammates'). Needs a hub. |
| `mesh conflicts <list\|diff\|resolve>` | Review and resolve local sync-conflict siblings. Needs a hub. |
| `mesh curator <log\|show\|accept>` | Review what the BYOAI sync-curator merged, and failed on, across the team. Needs a hub plus the commercial curator. |

## Build

```
go build ./...
go test ./...
```

No cgo. Storage is pure-Go `modernc.org/sqlite` in WAL mode. Markdown is the source
of truth for published notes; `.mesh/` also contains state that cannot be rebuilt
from those notes. See [backup and recovery](docs/RECOVERY.md).

## License & editions

Open core, dual-licensed. This repository (the single-user vault, graph, retrieval,
viewers, CLI, MCP surface, and the sync **client**) is the **Mesh Sustainable Use
License** (fair-code, see `LICENSE`): free to self-host, use internally or
commercially, and run for your own clients; you just cannot resell it as a hosted
service.

The **team-sync hub** and **BYOAI sync-curator** are a commercial product:

- **Hosted** at mesh-sync.cloudrebellion.tech (managed team sync; operator UI at
  mesh.cloudrebellion.tech).
- **Sovereign self-host** under a commercial license + support, for EU / regulated
  orgs running the hub on their own infrastructure.

A commercial license to the core is available for uses the Mesh Sustainable Use
License does not fit. See [LICENSING.md](LICENSING.md) and
[docs/OPEN-CORE.md](docs/OPEN-CORE.md).
