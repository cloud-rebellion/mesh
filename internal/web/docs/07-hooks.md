# Make your agent use Mesh automatically (session hooks)

This is the superpower. Mesh is most valuable when an agent **reads it at the
start of every session** and **writes back what it learned at the end**, every
time, without anyone remembering to. Two Claude Code session hooks make that the
default.

## What they do

- **SessionStart -> the agent reads the mesh.** When a session begins, a hook runs
  `mesh orient` and injects a short orientation: the most-connected entry-point
  notes, what changed in the last 7 days, and how to retrieve cheaply. The agent
  starts already knowing the shape of your knowledge.
- **Stop -> the agent writes back.** When the agent is about to finish, a hook
  nudges it once to record any decision, gotcha, or post-mortem with
  `mesh_append_note`. It only nudges once per session, and not at all if the agent
  already wrote something, so it never loops.

Together that is the **flywheel**: every session starts informed and ends a little
smarter, so knowledge compounds across sessions and teammates instead of being
relearned each time.

## These are session hooks, not git hooks

A common mix-up: this is **not** a git pre-push or post-push hook. Those fire on
code pushes and are a separate layer (Mesh's own post-commit reindex, your repo's
sync). The read/write-back discipline is tied to the **agent session lifecycle**,
which is what Claude Code's SessionStart and Stop hooks control. (SessionEnd exists
but can only do cleanup, so the write-back nudge lives on Stop.)

## Setting it up

### One command, then it onboards you

```
mesh install /path/to/vault
```

That single command registers the Mesh MCP server (`.mcp.json`), indexes your
vault, installs the SessionStart read hook, and arms a one-time welcome. Start a
new agent session and
**the agent greets you and finishes onboarding itself**, no further commands: it
explains the flywheel, asks whether to enable the write-back nudge, and offers a
quick tour. Add `--enforce-writeback` to also turn on the Stop nudge up front, or
`--no-mcp` if your MCP server is already registered.

**Other clients.** `mesh install --client cursor|claude-desktop|vscode|windsurf|codex`
registers the Mesh MCP server in that client's own config (and indexes your vault),
so the agent gets the `mesh_*` tools there too. Session hooks (the auto-onboard and
the read/write-back enforcement) are Claude Code only, so in other clients the agent
uses Mesh via the MCP server's instructions, just tell it to use Mesh, or run
`mesh hooks install` as well if you also use Claude Code in that project.

### Or let the agent do all of it

If Mesh is already connected over MCP, just say "set up the Mesh session hooks" and
it calls `mesh_setup_hooks`, explains the trade-offs, asks which project and whether
to enforce write-back, and wires it in.

### Or the explicit CLI

```
mesh hooks install /path/to/vault                 # read at start + nudge write-back
mesh hooks install /path/to/vault --read-only     # just the start-of-session read
mesh hooks install /path/to/vault --dry-run       # preview the settings.json first
mesh hooks uninstall                              # remove the hooks (not the MCP server)
```

All of these merge into the project's `.claude/settings.json` (they never clobber
your other settings and are safe to run twice). After installing, run `/hooks` in
Claude Code to verify and start a new session.

## Removing Mesh again

`mesh hooks uninstall` removes the **session hooks only**. The MCP server
registration lives in a different file (`.mcp.json` for Claude Code, and a global
config for Cursor, Claude Desktop, Windsurf and Codex), so removing it is a
separate command:

```
mesh install --remove                             # drop the mesh MCP entry + the session hooks
mesh install --remove --client cursor             # same, for another client's config
```

Run it for each client you set up, and **run it before you delete the mesh
binary**: the registration stores the binary's absolute path, so a client left
pointing at a deleted binary keeps retrying a server that can never start. Your
vault, your notes and the `.mesh` index are never touched by either command;
delete the vault folder yourself if you want those gone.

## Read-only vs enforce write-back

- **Read-only** adds only the SessionStart orientation. Low friction, no nagging.
- **Enforce write-back** also adds the Stop nudge. Use it when you want the flywheel
  to actually turn, the agent gets one reminder per session to capture what it
  learned. You can always switch with `uninstall` then `install --read-only`.


## Purpose-specific write-back

The Stop nudge asks the agent to choose a template with `mesh_note_template`,
then author its summary and meaningful sections with supporting evidence and
verification limits. Empty or unverified fields stay explicit in a draft; they
must not be invented to satisfy a checklist.

Automatic extraction proposes review drafts using the same versioned template
registry as the writer. The review queue does not publish knowledge until a
reviewer has supplied the required content and checked its references. Historical
suggestions remain available as original content for a reviewed rewrite. At the
queue limit, admission stops rather than deleting unreviewed suggestions.


Purpose-specific sections accept normal Markdown: paragraphs, bullet and ordered
lists, tables, fenced code and diagrams. Formatting a section does not require a
new library template or approval. Keep code and verification context explicit;
choose optional supporting blocks when their extra source or verification fields
are useful.


## Hosted folder-permission limitation

The current pending-review publisher refuses configured folder rules, including
admin requests. A per-author read callback does not establish who can read the
destination note. Identified members can publish when the provider confirms that no
folder rules exist. A shared-token unrestricted callback does not prove that absence
and remains refused. Refused suggestions remain readable and can be discarded.
This limitation requires audience-aware folder publication support; it does not add
approval to ordinary notes.
