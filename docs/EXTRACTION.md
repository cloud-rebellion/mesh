# Extraction and completion providers

Automatic session extraction produces registered-template review drafts. It never
publishes a note merely because a model proposes it. Missing evidence or sections
remain explicit; the shared publisher checks completed content and current access
when a reviewer promotes it. A judge failure leaves the item in human review.

Mesh's default authenticated Claude CLI is invoked in completion-only mode: safe
mode, no built-in tools, an explicit empty MCP configuration, no skills or Chrome,
no session persistence, a hard limit of one turn, and a fixed `dontAsk` permission
mode. Existing user or
workspace `bypassPermissions` settings cannot enable tools in this invocation.
The real system prompt is passed separately from user input. Safe mode retains
normal provider authentication, including OAuth; Mesh does not use `--bare`.
Configured Claude commands may select print mode, model or effort. Tool, settings,
MCP, permission, plugin, session or prompt overrides are refused.

Custom non-Claude executables require an explicit operator contract:

```text
MESH_CURATOR_AGENT=cli
MESH_CURATOR_CMD=/path/to/completion-adapter
MESH_CURATOR_CLI_CONTRACT=mesh-completion-v1
```

The adapter receives one JSON request on stdin:

```json
{"protocol":"mesh-completion-v1","system":"system instructions","user":"input data"}
```

It must return completion text on stdout and must not discover or execute agent
tools, MCP servers, project instructions or plugin actions. The executable is
operator-owned; this contract is not an operating-system sandbox. Unsupported
custom commands are rejected and do not fall back to an unrestricted agent.
The same `_CLI_CONTRACT` suffix applies to configured `MESH_JUDGE`, `MESH_JUDGE2`
and `MESH_JUDGE3` clients. Existing API-backed Anthropic/local clients and judge
majority semantics remain unchanged. Deterministic fixtures can attest this custom
contract to test protocol boundaries; they establish no actual model isolation.

Command stdout is capped at 4 MiB and stderr at 64 KiB, with immediate cancellation
on overflow. Provider subprocesses receive sanitized environment values and the
fixed `MESH_LLM_CHILD=1` marker. Their Mesh Stop hook exits before scanning input,
creating markers, nudging or launching another extractor.

Automatic Stop extraction requires both an exclusive private session claim and an
exclusive per-vault daily slot. The default daily cap is 20; zero disables the
fallback. Concurrent Stops for one session can consume only one claim. Failed
claims or spawns do not reset the ceiling. The ceiling counts extractor launches,
not individual judge calls or manually invoked `mesh extract` commands.

Transcript text is data. Turn continuations and clipped line fragments stay
indented, including the retained first-user head, so embedded turn labels or
delimiter strings cannot become column-zero protocol labels after truncation.
Structural validation and process containment do not establish factual quality.
Enable automatic fallback only after the installed provider and CLI pass actual
containment and review-only acceptance; retain a reversible settings backup.
