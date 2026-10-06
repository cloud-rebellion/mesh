# Open-core architecture

Mesh is split into a public fair-code core and a private commercial pro layer. This
doc is the source of truth for what lives where and how the public mirror is produced.

## The boundary

The split is a pure package boundary: **nothing in the open core imports the pro
packages**, so the core builds and ships on its own. This is enforced, not assumed:
`scripts/check-open-core-boundary.sh` walks the open build's import graph (`go list
-deps`) and fails if any open package imports a pro package. Run it yourself, and CI
runs it on every push and pull request, as does the release pipeline before it mirrors
anything, so the boundary cannot silently rot (it did once, 2026-06-30: the flywheel
features pulled in `internal/llm` and `mesh ui --hub-db` pulled in `internal/hub`; see
the fix below).

### Open core (public repo, Mesh Sustainable Use License)

The whole single-user + hub-client experience:

- `cmd/mesh`: the CLI (`index`, `search`, `mcp`, `tui`, `ui`, `watch`, `join`, `sync`, `doctor`, `tune`, `migrate`).
- `internal/vault`, `internal/index`, `internal/graph`: parse, the in-memory graph, Louvain communities.
- `internal/retrieve`, `internal/rerank`, `internal/embed`, `internal/tokenize`: fused FTS + graph-BM25 + BYOAI vectors/rerank retrieval. The open core uses brute-force cosine for the vector signal (sub-5ms well past v1 scale).
- `internal/llm`: the BYOAI chat boundary (a stdlib-only client shim over `claude -p` / the Anthropic Messages API / any OpenAI-compatible endpoint). It is open because it is a thin client with no defensible moat, and the flywheel features built on it are the open product.
- `internal/ask`, `internal/extract`, `internal/guards`: the BYOAI flywheel - grounded Q&A over notes+code (`mesh ask`), auto-extraction of write-back candidates (`mesh extract` + the Review queue), and gotcha pre-commit guards. All use `internal/llm`.
- `internal/mcp`: the agent retrieval + write-back surface (read tools, `mesh_append_note`/`mesh_write_entity`, `mesh_reindex`), plus the secret-broker tools (`mesh_secret_status`/`mesh_secret_list`/`mesh_secret_use`) that let an agent fetch a use-once capability token for a secret it can never read.
- `internal/secretbridge`: a thin stdlib HTTP client for an ATTACHED Dockyard capability-mode vault (open because it holds no crypto/vault/rotation of its own - all of that stays in Dockyard). It lets Mesh broker secrets for an agent (names + capability tokens only, never a plaintext value). Kept open so OSS users who run their own Dockyard get the feature.
- `internal/web`, `internal/tui`, `internal/sshserve`: the 2D/3D viewers, the terminal UI, and the SSH viewer (`mesh serve-ssh`: the TUI over SSH, key-auth, fail-closed).
- `internal/merge`, `internal/syncproto`, `pkg/meshclient`: the sync **client** and wire protocol, so the open core can join a hub.
- `internal/watch`: the live index (`mesh watch`), so an edit in your editor is searchable at once, with no commit and no manual `mesh index`.
- `internal/ingest`: the sovereign importers (`mesh ingest`, GitHub/Slack/Linear/Jira/Notion) that turn what a team already wrote into notes in **their** vault.
- `internal/hooks`: installs the Claude Code session hooks (`mesh hooks install`) that make an agent read the mesh at session start and write back before it finishes.
- `internal/safehttp`: the SSRF-guarded HTTP clients every outbound path shares (the ingest connectors and the BYOAI embedding / rerank / chat endpoints).
- `internal/buildinfo`: the running version and the source-availability link rendered by every network-served surface.
- `internal/meshcfg`, `internal/eval`, `internal/textdiff`: config, eval harness, diff.

### Pro layer (private, commercial, NOT published)

The collaboration server and the AI layer, the monetized value:

- `internal/hub`, `cmd/mesh-hub`: the team-sync hub server (the durable moat: a managed/licensed service, not code value).
- `internal/curator`, `cmd/mesh-curator`: the BYOAI sync-curator (AI conflict merge). It imports the open `internal/llm` - pro importing open is fine.
- `internal/hnsw` + `internal/retrieve/retrieve_ann_pro.go`: the HNSW ANN index, the "1000+ vectors" scale gate. The core has a build-tag seam (`annSearcher`/`buildANN`, nil in the open build) so it compiles brute-force-only; the pro build wires HNSW with `-tags pro`. This is gated by ABSENCE (the impl is stripped from the mirror), not a removable flag.
- `cmd/mesh/ui_hubteam_pro.go`: the team-mode wiring for `mesh ui --hub-db`, which reads the pro hub store. Same build-tag seam as HNSW: the open core ships `cmd/mesh/ui_hubteam_stub.go` (an `openHubTeam` that refuses team mode with a "needs the pro build" error), and this pro file - the only `cmd/mesh` file importing `internal/hub` - is stripped from the mirror. Plain `mesh ui` (solo vault) stays fully open.
- The `cmd/mesh` hub-harness tests (`conflicts_test.go`, `cli_receipts_test.go`, `resolve_ordering_test.go`, `sync_local_reconcile_test.go`) and the `pkg/meshclient` ones (`e2e_test.go`, `events_test.go`, `tombstone_test.go`): integration tests that stand up a real hub to exercise the open sync client and conflicts command, so they need the pro hub and are stripped with it. Each set strips together, because within a set one file defines the harness the others reuse (`conflicts_test.go` provides `runSub`/`parkedConflict`; `e2e_test.go` provides `setupHub`), and removing the definition without its users leaves the mirror's own suite unable to compile. The production code they cover ships open; the private monorepo runs the full suite. (`pkg/meshclient/vault_test.go` is hub-free and stays in the mirror.)
- Future: cross-vault federation, team-scale collaboration analytics, pro graph functions. These belong here, behind the service/license, not as a flag in the open binary.

The tagged `cmd/mesh/ui_signin_pro_test.go` SSO/browser-approval integration also
requires the private hub and is stripped. A build tag controls compilation, not
publication: the mirror preflight checks surviving source headers independently
of the default build's import graph.

Authoritative exclude set: every path below is removed from the public mirror's entire
history. It is kept in sync with the release script's strip list and with
`check-open-core-boundary.sh`.

```
# pro code
internal/hub  cmd/mesh-hub  internal/curator  cmd/mesh-curator
internal/hnsw  internal/retrieve/retrieve_ann_pro.go  internal/retrieve/retrieve_ann_pro_test.go
cmd/mesh/ui_hubteam_pro.go
internal/flarereport

# tests that need a running pro hub
cmd/mesh/conflicts_test.go  cmd/mesh/cli_receipts_test.go
cmd/mesh/resolve_ordering_test.go  cmd/mesh/sync_local_reconcile_test.go
cmd/mesh/ui_signin_pro_test.go
pkg/meshclient/e2e_test.go  pkg/meshclient/events_test.go  pkg/meshclient/tombstone_test.go

# files that build or run the pro hub
docker-compose.yml  deploy/Dockerfile.hub  deploy/entrypoint.sh  deploy/hub-image.sh

# internal-only docs
docs/S1-PLAN.md  docs/S2-PLAN.md  docs/M3-PLAN.md  docs/SPEC.md  deploy/DEPLOY.md
docs/AUTHORING-VALIDATION.md

# the release tooling and its runbook (they only run from the private monorepo)
the monorepo-only release tooling and its runbook

# retrieval-eval fixtures built from a private vault
the retrieval-eval fixtures built from a private vault
```

The **deployment** files build and run `./cmd/mesh-hub`, which the mirror does not
contain, and they describe a private deployment, so they stay out. The root `Dockerfile`
is the PUBLIC open-core image and stays in the mirror: it builds only `./cmd/mesh` and
must never name a stripped path, which a publish gate enforces.

`internal/flarereport` is the pro binaries' error-reporting shim. Nothing in the open
core imports it, and it is licensed commercially, so it goes with the pro layer.

The **internal-only docs** (milestone working-plans, the deployment runbook, the full
internal spec and release acceptance receipts) describe Bright Interaction's own
infrastructure and the monorepo the code is developed in, rather than the project
itself. The public architecture story is
`README.md` and the in-app docs (`internal/web/docs`). The user-facing template
contract in `docs/AUTHORING.md` remains public.

The **eval fixtures** are query/answer sets written against a private vault, so each
case names an internal system. `eval/tier0.json` and `eval/extract_judge_cases.json` are
generic and ship.

`internal/llm` is NOT excluded, it is open core (see the open-core list above).

The pro binaries build with `-tags pro` (wires HNSW + the hub team-mode impl); the open mirror builds with the default tags (brute-force only, team mode stubbed out).

## How the public mirror is produced

The monorepo is the source of truth and the build source for the hosted service and the
licensed binaries. The public fair-code repo is a history-filtered mirror of the core,
produced by a release script that lives in the monorepo and is not part of this
repository:

1. `git subtree split --prefix=mesh` lifts the `mesh/` subtree with its history, then
   `git filter-repo --invert-paths` strips every excluded path from **all** of it, so
   pro code never appears in the fair-code repo, not even in old commits.
2. Publish gates then refuse the push if anything slipped: a surviving excluded path, a
   build file pointing at one, internal vocabulary in a commit message or in the content
   of any tracked file, a secret anywhere in the history, or a core that does not build
   and test-compile on its own.
3. Dry-run by default; publishing is an explicit, deliberate operator action.

One consequence for contributors: commits from before 2026-06-30 (when the boundary was
fixed) were written against packages that are now stripped, so an old commit checked out
from this mirror can fail to build. `git bisect` over recent history is fine; going back
past that point is not.

## Revenue model

- **Free / clout:** the fair-code core. Individuals, OSS, top-of-funnel.
- **Hosted team hub** (primary, build first): subscription, low-friction funnel from
  the free core via `mesh join`.
- **Sovereign self-host license:** the hub + curator on the customer's own infra
  with support + SLA, for EU / regulated buyers (Bright Interaction's lane).

The fair-code license keeps the core free to self-host, use internally or
commercially, and run for your own clients, but bars reselling it as a hosted
service; the dual commercial license is how a buyer who wants to offer Mesh as a
hosted product (or otherwise needs terms the fair-code license does not grant) pays
instead.
