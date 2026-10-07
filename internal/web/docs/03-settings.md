# Settings and configuration

The Settings section edits your solo `<vault>/.mesh/config.toml`. There are three
ways a value can be set, shown as a badge on each field:

- **env**: an environment variable is set, so it wins and the field is read-only
  here (editing the file would have no effect while the variable is set).
- **file**: the value comes from `config.toml` and you can edit it.
- **default**: nothing is set; the built-in default applies.

Precedence is always environment over file over default, everywhere in Mesh.

## What you can set

- **Embedding**: endpoint, model, dimensions, query/doc prefixes, and the NAME of
  the env var holding the bearer key. Turns on semantic search.
- **Retrieval**: the fusion weights for full-text, graph, and vector signals.
- **Rerank**: a cross-encoder endpoint, model, key env var, and blend. A user's
  Codex/Claude subscription reranker is deliberately env-only
  (`MESH_RERANK_AGENT`) so shared vault config cannot force provider egress.
  `MESH_RERANK_POLICY=auto|always` controls confidence routing; auto is default.
- **Scale (pro)**: the HNSW threshold.

## Secrets

Mesh never stores or shows API keys. Key fields hold the NAME of the environment
variable that holds the key, never the key itself. Set the actual key in your
environment; Mesh reads it at run time.

## Reindex

The Index block shows note/node/edge/vector counts and which signals are active.
An owning viewer automatically updates its index and search when another Mesh
client or the hub adds, edits, or removes vault notes. Filesystem events trigger
updates, with periodic checks recovering missed events. A read-only viewer uses
the index maintained by its configured owner.
"Reindex now" re-parses the vault and rebuilds the graph, the same as `mesh index`.
