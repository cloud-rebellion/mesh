// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/extract"
	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/hooks"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/llm"
	"github.com/bright-interaction/mesh/internal/mcp"
	"github.com/bright-interaction/mesh/internal/onboarding"
	"github.com/bright-interaction/mesh/internal/rerank"
	"github.com/bright-interaction/mesh/internal/shellpath"
	"github.com/spf13/cobra"
)

// orientCmd prints a session orientation: the most-connected entry points, what
// changed recently, and the retrieval contract. With --hook it emits the Claude
// Code SessionStart JSON envelope so a hook injects it as the session's first
// context, i.e. the agent literally starts having read the mesh.
func orientCmd() *cobra.Command {
	var hook bool
	var vaultFlag string
	c := &cobra.Command{
		Use:   "orient [vault]",
		Short: "Print a session orientation (entry points + recent changes + how to retrieve)",
		Long:  "Front-load an agent's session with the knowledge mesh. With --hook it emits the Claude Code SessionStart JSON envelope (hookSpecificOutput.additionalContext) so a SessionStart hook injects it automatically.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultFlag
			if len(args) == 1 {
				root = args[0]
			}
			if root == "" {
				root = "."
			}
			text, err := orientTextFor(root)
			if err != nil {
				return err
			}
			// First session after `mesh install`: prepend a one-time welcome so the
			// agent greets the user and finishes onboarding. Only consume on the hook
			// path (the real SessionStart), never on a manual `mesh orient`.
			if hook && hooks.ConsumeOnboardPending(root) {
				text = onboardWelcome + text
			}
			if hook {
				out, _ := json.Marshal(map[string]any{
					"hookSpecificOutput": map[string]any{
						"hookEventName":     "SessionStart",
						"additionalContext": text,
					},
				})
				fmt.Println(string(out))
				return nil
			}
			fmt.Print(text)
			return nil
		},
	}
	c.Flags().BoolVar(&hook, "hook", false, "emit the Claude Code SessionStart JSON envelope (additionalContext)")
	c.Flags().StringVar(&vaultFlag, "vault", "", "vault root (defaults to the positional arg or .)")
	return c
}

// orientTextFor renders the orientation for a vault from a READ-ONLY store.
//
// Read-only is load-bearing twice over, because this is the one command wired into
// .claude/settings.json for SessionStart (startup AND resume), so it runs at the start
// of every single session, concurrently with whatever else the user has running.
//
//  1. A writable open applies the schema, starts a writer goroutine and TRUNCATE-
//     checkpoints on Close, so while `mesh watch` holds the write lock mid-reindex this
//     blocked for the DSN's full busy_timeout and then failed: the agent started its
//     session with NO orientation at all, which is the entire feature. Orient only reads
//     (LoadGraph + ChangedSince), so it has no business taking the write lock.
//  2. index.Open CREATES the database when it is absent. That turned the repair printed
//     by corruptIndexError ("rm -f mesh.db mesh.db-wal mesh.db-shm") into a trap: the very
//     next SessionStart minted an empty database, and from then on every read-only surface
//     opened it happily and served an empty mesh, so `mesh search` answered "no matches"
//     instead of "no index at <path>". Read-only cannot create, so the vault stays
//     honestly index-less until the user runs `mesh index`.
//
// ErrNoIndexYet is therefore an ORIENTATION, not an error: the hook still has to emit its
// envelope (a failed SessionStart hook says nothing to the user on Claude Code), so the
// session begins knowing the mesh exists and exactly which command builds it.
func orientTextFor(root string) (string, error) {
	store, err := index.OpenReadOnly(root)
	if err != nil {
		if errors.Is(err, index.ErrNoIndexYet) {
			return noIndexOrientText(root), nil
		}
		return "", err
	}
	defer store.Close()
	g, err := store.LoadGraph()
	if err != nil {
		return "", err
	}
	return orientText(store, g), nil
}

// noIndexOrientText is the orientation for a vault that has no index yet. It names the
// one command that fixes it, because the agent reading this is the one who can run it.
func noIndexOrientText(root string) string {
	var b strings.Builder
	b.WriteString("# Your knowledge mesh (not indexed yet)\n\n")
	b.WriteString(fmt.Sprintf("A knowledge mesh is wired up for %s, but it has no index yet, so there is nothing to orient from and the mesh-* MCP tools will return nothing.\n\n", root))
	b.WriteString("Build it once, then start a new session:\n\n")
	b.WriteString(fmt.Sprintf("    mesh index %s\n\n", shellpath.Quote(root)))
	b.WriteString("Until then, treat an empty mesh result as \"not indexed\", not as \"the mesh knows nothing about this\".\n")
	return b.String()
}

func orientText(store *index.Store, g *graph.Graph) string {
	var b strings.Builder
	b.WriteString("# Your knowledge mesh (read this first)\n\n")
	b.WriteString("A knowledge mesh is available via the mesh-* MCP tools. Orient with it before exploring files.\n\n")

	type hub struct {
		label, path string
		deg         int
	}
	var hubs []hub
	for _, n := range g.Nodes() {
		if n.Kind != "note" {
			continue
		}
		// KnowledgeDegree, not Degree: this line is printed as "[N links]", and raw
		// degree counts a note's own headings and tags, so the longest note in the
		// vault would head the entry points with a link count it does not have.
		hubs = append(hubs, hub{n.Label, n.NotePath, n.KnowledgeDegree})
	}
	sort.Slice(hubs, func(i, j int) bool {
		if hubs[i].deg != hubs[j].deg {
			return hubs[i].deg > hubs[j].deg
		}
		return hubs[i].label < hubs[j].label
	})
	if len(hubs) > 10 {
		hubs = hubs[:10]
	}
	if len(hubs) > 0 {
		b.WriteString("## Entry points (most-connected notes)\n")
		for _, h := range hubs {
			b.WriteString(fmt.Sprintf("- %s (%s) [%d links]\n", h.label, h.path, h.deg))
		}
		b.WriteString("\n")
	}

	since := time.Now().Add(-7 * 24 * time.Hour).Unix()
	if refs, err := store.ChangedSince(since); err == nil && len(refs) > 0 {
		if len(refs) > 10 {
			refs = refs[:10]
		}
		b.WriteString("## Changed in the last 7 days\n")
		for _, r := range refs {
			b.WriteString(fmt.Sprintf("- %s (%s)\n", r.ID, r.Path))
		}
		b.WriteString("\n")
	}

	b.WriteString("## How to use it\n")
	b.WriteString(mcp.Contract())
	b.WriteString("\n")
	return b.String()
}

// onboardWelcome is injected (once) at the first SessionStart after `mesh install`,
// so the agent greets the user and finishes setup itself, no commands needed.
const onboardWelcome = `# First Mesh session - onboard the user now

The user just installed Mesh and this is their first session. Before their task:
1. Greet them warmly and say in one line what Mesh is: a knowledge mesh you (the agent) read at the start of every session and write back to at the end - the flywheel, so knowledge compounds instead of being relearned.
2. Tell them it is now wired to read automatically each session (this orientation IS that hook firing).
3. Ask two short questions: (a) enable write-back enforcement? - a one-time nudge per session to record decisions/gotchas, recommended; (b) want a 60-second tour of what is already in their mesh?
4. If they say yes to write-back, call mesh_setup_hooks with action=install. For a tour, use mesh_god_nodes + mesh_search.
Keep it short and friendly, then carry on with whatever they need.

---
`

// indexVault builds the initial index so the first orientation (and the agent) see
// real content immediately, instead of an empty mesh on the first session.
//
// It returns its error, and that is the whole point. This used to swallow all three of
// them (the open, the reindex and the count), so `mesh install` registered the MCP server
// over an unreadable .mesh/mesh.db or an unwritable vault, printed "Done.", armed a
// welcome that could never fire and exited 0. The user restarted their agent, the MCP
// server died at startup, and on Claude Code that surfaces as a bare connect timeout with
// no message at all: first-run setup reporting success is exactly the moment a failure
// must be loud.
//
// OpenRebuild rather than Open, matching `mesh index`: install IS the first build, so a
// corrupt index file is the thing being replaced, not an obstacle. It discards the file
// only for that one error class (see recoverCorruptIndex), and says so out loud.
//
// It also returns how many notes the pass had to DROP, which is a different failure from
// the ones above and was the half BLOCKER 6's first fix left open. A hard index failure
// was made loud; a partial one stayed silent. A vault of eight files with one broken
// frontmatter block and one duplicate id opens, reindexes and counts perfectly well, so
// install printed "+ indexed the vault (6 notes)" and "Done." and exited 0 over the exact
// vault `mesh index` and `mesh init` both exit 1 on. Six is not eight, and the count alone
// never says so: the user has no other copy of that number to compare it against on their
// first run. The census and the exit code are shared with init through reportDroppedNotes
// so the two commands cannot answer the same vault differently again.
func indexVault(vaultAbs string) (int, error) {
	store, recovered, closeStore, err := openOneShotRebuild(vaultAbs, "mesh install")
	if err != nil {
		return 0, installIndexError(vaultAbs, err)
	}
	defer closeStore()
	if recovered {
		fmt.Printf("  ! %s was corrupt and unreadable; discarded it and rebuilt from the markdown\n", filepath.Join(vaultAbs, ".mesh", "mesh.db"))
		fmt.Println("    your notes are intact; stored embeddings went with it, so re-run `mesh embed` if you use semantic search")
	}
	if _, err := index.Reindex(store, vaultAbs); err != nil {
		return 0, installIndexError(vaultAbs, err)
	}
	n, err := store.Count("notes")
	if err != nil {
		return 0, installIndexError(vaultAbs, err)
	}
	// A failure to READ the census is an index failure like any other here, so it gets
	// the same "do NOT restart your agent yet" wrapper: install cannot tell the user
	// whether the mesh it just wired them to is complete.
	nDropped, derr := reportDroppedNotes(store, vaultAbs)
	if derr != nil {
		return 0, installIndexError(vaultAbs, derr)
	}
	switch {
	case nDropped > 0:
		// Never the clean receipt, even when n is 0: an empty index over a vault full of
		// broken notes is the loudest case, not the quietest.
		fmt.Printf("  ! indexed the vault (%d notes, %d NOT indexed and invisible to the agent; see above)\n", n, nDropped)
	case n == 0:
		fmt.Printf("  + indexed the vault (no notes yet: add markdown under %s and run `mesh index %s`)\n", vaultAbs, shellpath.Quote(vaultAbs))
	default:
		fmt.Printf("  + indexed the vault (%d notes)\n", n)
	}
	return nDropped, nil
}

// installIndexError names the remedy for the one failure a stranger meets on their first
// run. The wrapped cause already carries its own repair when it is a corrupt index or a
// held write lock; what it cannot know is that the agent has just been wired to a mesh
// that will not open, which is what makes this worth stopping for.
func installIndexError(vaultAbs string, cause error) error {
	return fmt.Errorf("the agent is wired up, but the index for %s could not be built, so do NOT restart your agent yet: it would start against a mesh that cannot open.\n"+
		"  check that %s is writable by you (a read-only mount, a synced notes folder or a directory owned by another user all fail here)\n"+
		"  then run: mesh index %s\n"+
		"  cause: %w", vaultAbs, filepath.Join(vaultAbs, ".mesh"), shellpath.Quote(vaultAbs), cause)
}

// installCmd is the one-shot agent setup. For Claude Code it registers the MCP
// server + SessionStart read hook and arms the hook welcome. Other clients have no
// session hooks, so their first local MCP initialization carries the one-time welcome.
func installCmd() *cobra.Command {
	var dir, client, rerankAgent string
	var noMCP, enforce, remove bool
	c := &cobra.Command{
		Use:   "install [vault]",
		Short: "One-shot setup: register Mesh and arm a one-time in-agent welcome. --remove undoes it",
		Long:  "Wires a coding agent to use Mesh and arms a one-time welcome in the next agent session. --client claude-code (default) also installs its SessionStart read hook; other clients receive the welcome and read/write workflow through the local MCP server instructions, without changing project prompt files.\n\nIt also builds the vault's first index. If that fails (an unwritable vault, an unreadable .mesh/mesh.db) install stops and prints the repair instead of reporting success, because an agent wired to a mesh that cannot open just fails to connect, with no message.\n\nUse --remove to undo it: that drops the mesh entry from the same client config this command wrote (and, on claude-code, the session hooks too), leaving every other MCP server untouched. Run it before you delete the mesh binary, or the client keeps retrying a server that no longer exists. Your vault and its notes are never touched.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vaultPath := "."
			if len(args) == 1 {
				vaultPath = args[0]
			}
			vaultAbs, err := filepath.Abs(vaultPath)
			if err != nil {
				return err
			}
			projAbs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			bin, err := os.Executable()
			if err != nil || bin == "" {
				bin = "mesh"
			}
			if remove {
				if rerankAgent != "" {
					return fmt.Errorf("--rerank-agent cannot be combined with --remove; use mesh rerank disable %s", shellpath.Quote(vaultAbs))
				}
				return removeInstall(client, projAbs)
			}
			if noMCP && rerankAgent != "" {
				return fmt.Errorf("--rerank-agent needs the local MCP registration; remove --no-mcp")
			}
			var rerankModel, rerankBin string
			if rerankAgent != "" {
				rerankAgent = strings.ToLower(strings.TrimSpace(rerankAgent))
				rerankModel, err = rerank.DefaultSubscriptionModel(rerankAgent)
				if err != nil {
					return err
				}
				rerankBin, err = exec.LookPath(rerankAgent)
				if err != nil {
					return fmt.Errorf("%s CLI not found; install and sign in to it before enabling subscription rerank: %w", rerankAgent, err)
				}
			}
			fmt.Printf("Setting up Mesh for %s...\n", client)

			if client == "claude-code" {
				if !noMCP {
					added, p, err := hooks.InstallMCP(projAbs, vaultAbs, bin)
					if err != nil {
						return err
					}
					if added {
						fmt.Printf("  + registered the Mesh MCP server in %s\n", p)
					} else {
						fmt.Printf("  . MCP server already present in %s\n", p)
					}
					if rerankAgent != "" {
						if err := installSubscriptionRerank(vaultAbs, rerankAgent, rerankModel, rerankBin); err != nil {
							return err
						}
					}
				}
				res, err := hooks.Install(hooks.Options{ProjectDir: projAbs, Vault: vaultAbs, Bin: bin, EnforceWriteback: enforce})
				if err != nil {
					return err
				}
				if len(res.Added) > 0 {
					for _, a := range res.Added {
						fmt.Printf("  + %s\n", a)
					}
				} else {
					fmt.Printf("  . session hooks already in %s\n", res.SettingsPath)
				}
				// Before SetOnboardPending and before "Done.": a welcome armed over a
				// vault with no working index fires into a session that has no mesh.
				nDropped, err := indexVault(vaultAbs)
				if err != nil {
					return err
				}
				if err := hooks.SetOnboardPending(vaultAbs); err != nil {
					return err
				}
				fmt.Println("  + armed the first-run welcome")
				// A dropped note is not a reason to leave the agent unwired, so the setup
				// above stands and the welcome is armed. It IS a reason to withhold the
				// word "Done.": what the agent will read is not what is in the folder, and
				// the closing line is the only place a first-run user would learn that.
				if nDropped > 0 {
					fmt.Println("\nThe agent is wired up, but your mesh is INCOMPLETE: the note(s) listed above")
					fmt.Println("are not in the index, so the agent will not see them. Fix them and run:")
					fmt.Printf("  mesh index %s\n", shellpath.Quote(vaultAbs))
					fmt.Println("Then start a new agent session and Mesh will finish onboarding.")
					return droppedNotesError(nDropped)
				}
				fmt.Println("\nDone. Start a new agent session (or reconnect the MCP server) and Mesh will")
				fmt.Println("greet you and finish onboarding automatically, no commands needed.")
				if rerankAgent == "" && !noMCP {
					printSubscriptionRerankHint(client, vaultAbs)
				}
				return nil
			}

			// Other clients: MCP only (no session hooks exist for them).
			added, p, err := hooks.RegisterMCP(client, projAbs, vaultAbs, bin)
			if err != nil {
				return err
			}
			if added {
				fmt.Printf("  + registered the Mesh MCP server for %s in %s\n", client, p)
			} else {
				fmt.Printf("  . MCP server already registered for %s in %s\n", client, p)
			}
			if rerankAgent != "" {
				if err := installSubscriptionRerank(vaultAbs, rerankAgent, rerankModel, rerankBin); err != nil {
					return err
				}
			}
			nDropped, err := indexVault(vaultAbs)
			if err != nil {
				return err
			}
			if err := onboarding.SetPending(vaultAbs, client); err != nil {
				return err
			}
			fmt.Println("  + armed the one-time in-agent welcome")
			// The twin of the claude-code branch above, and it gets the same treatment for
			// the same reason: the registration stands, the word "Done." does not.
			if nDropped > 0 {
				fmt.Printf("\nThe MCP server is registered for %s, but your mesh is INCOMPLETE: the note(s)\n", client)
				fmt.Println("listed above are not in the index, so the agent will not see them. Fix them and run:")
				fmt.Printf("  mesh index %s\n", shellpath.Quote(vaultAbs))
				fmt.Printf("Then restart %s so it loads the MCP server and finishes onboarding.\n", client)
			} else {
				fmt.Printf("\nDone. Restart %s so it loads Mesh; it will greet you and offer a quick tour.\n", client)
			}
			fmt.Println("Note: Claude Code is the only client with session lifecycle hooks. This client")
			fmt.Println("receives the read/write workflow and one-time welcome through local MCP server")
			fmt.Println("instructions; no project prompt file was edited.")
			if rerankAgent == "" {
				printSubscriptionRerankHint(client, vaultAbs)
			}
			return droppedNotesError(nDropped)
		},
	}
	c.Flags().StringVar(&client, "client", "claude-code", "agent client: "+strings.Join(hooks.Clients, ", "))
	c.Flags().StringVar(&dir, "dir", ".", "project dir (used for claude-code .mcp.json/.claude and vscode .vscode)")
	c.Flags().BoolVar(&noMCP, "no-mcp", false, "skip registering the MCP server (claude-code only)")
	c.Flags().BoolVar(&enforce, "enforce-writeback", false, "also install the Stop write-back nudge now (claude-code; default: the agent asks during onboarding)")
	c.Flags().BoolVar(&remove, "remove", false, "undo the install: drop the mesh MCP entry from this client's config (and, on claude-code, the session hooks). Your vault is untouched")
	c.Flags().StringVar(&rerankAgent, "rerank-agent", "", "opt in to subscription rerank through an existing codex or claude CLI login (pins the small Luna or Haiku model; no API key)")
	return c
}

// removeInstall is the inverse of an install, and the only complete uninstall Mesh
// has. `mesh hooks uninstall` removes the session hooks and nothing else, so before
// this existed a user who ran it, believed the receipt, and deleted the binary was
// left with a dead mesh entry in a GLOBAL client config, pointing at an absolute path
// that no longer resolves, retried on every agent launch.
//
// It removes only what an install wrote, prints a per-file receipt, and says plainly
// what it deliberately left alone.
func removeInstall(client, projAbs string) error {
	fmt.Printf("Removing Mesh from %s...\n", client)
	dropped, p, err := hooks.UnregisterMCP(client, projAbs)
	if err != nil {
		return err
	}
	if dropped {
		fmt.Printf("  - removed the Mesh MCP server from %s\n", p)
	} else {
		fmt.Printf("  . no Mesh MCP server registered in %s\n", p)
	}
	if client == "claude-code" {
		removed, sp, herr := hooks.Uninstall(projAbs)
		switch {
		case herr != nil:
			// The common case is no .claude/settings.json at all, which is not a
			// failure of the uninstall; report it and carry on rather than aborting
			// half way and leaving the user unsure what was removed.
			fmt.Printf("  . no session hooks to remove (%v)\n", herr)
		case removed > 0:
			fmt.Printf("  - removed %d mesh session hook(s) from %s\n", removed, sp)
		default:
			fmt.Printf("  . no mesh session hooks in %s\n", sp)
		}
	}
	fmt.Println("\nDone. Restart the client so it drops the server.")
	fmt.Println("Left alone on purpose: your vault, its notes, and its .mesh index (delete the")
	fmt.Println("vault folder yourself if you want those gone), plus every other MCP server in")
	fmt.Println("that config. Registrations for OTHER clients are separate: re-run with")
	fmt.Println("--client <name> for each one you set up.")
	return nil
}

// hooksCmd installs/removes the Claude Code session hooks that enforce the read-at-
// start / write-back-at-end discipline (the flywheel). These are SESSION hooks, not
// git pre/post-push hooks. The merge logic lives in internal/hooks, shared with the
// mesh_setup_hooks MCP onboarding tool.
func hooksCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hooks",
		Short: "Set up Claude Code session hooks: read Mesh at session start, nudge write-back at end",
		Long:  "Installs Claude Code SessionStart + Stop hooks into a project's .claude/settings.json so an agent starts every session having read the mesh (SessionStart -> mesh orient) and is reminded once to write back what it learned before finishing (Stop -> mesh hooks stop-check). These are session-lifecycle hooks, not git pre/post-push hooks.",
	}
	c.AddCommand(hooksInstallCmd(), hooksUninstallCmd(), hooksStopCheckCmd())
	return c
}

func hooksInstallCmd() *cobra.Command {
	var dir string
	var readOnly, dryRun, autoExtract bool
	c := &cobra.Command{
		Use:   "install [vault]",
		Short: "Wire SessionStart (read Mesh) + Stop (nudge write-back) into .claude/settings.json",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vaultPath := "."
			if len(args) == 1 {
				vaultPath = args[0]
			}
			vaultAbs, err := filepath.Abs(vaultPath)
			if err != nil {
				return err
			}
			projAbs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			bin, err := os.Executable()
			if err != nil || bin == "" {
				bin = "mesh"
			}
			res, err := hooks.Install(hooks.Options{ProjectDir: projAbs, Vault: vaultAbs, Bin: bin, EnforceWriteback: !readOnly, AutoExtract: autoExtract, DryRun: dryRun})
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Printf("# dry run: would write %s\n\n%s\n", res.SettingsPath, res.Preview)
				return nil
			}
			if len(res.Added) == 0 {
				fmt.Println("mesh hooks already installed (nothing to do).")
				return nil
			}
			fmt.Printf("installed into %s:\n", res.SettingsPath)
			for _, a := range res.Added {
				fmt.Printf("  + %s\n", a)
			}
			fmt.Println("\nRun /hooks in Claude Code to verify, then start a new session.")
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", ".", "project dir whose .claude/settings.json to edit")
	c.Flags().BoolVar(&readOnly, "read-only", false, "only the SessionStart read hook; skip the Stop write-back nudge")
	c.Flags().BoolVar(&autoExtract, "extract", false, "Stop hook auto-extracts session learnings into the review queue when the agent did not write back (spawns the BYOAI LLM per such session)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be written without changing anything")
	return c
}

func hooksUninstallCmd() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the mesh session hooks from .claude/settings.json (NOT the MCP registration)",
		Long:  "Removes the SessionStart and Stop hook entries this project's .claude/settings.json holds. It does not touch the MCP server registration that `mesh install` wrote, because that lives in a different file (and, for most clients, a global one). Run `mesh install --remove` for that.",
		RunE: func(cmd *cobra.Command, args []string) error {
			projAbs, _ := filepath.Abs(dir)
			removed, p, err := hooks.Uninstall(projAbs)
			if err != nil {
				return err
			}
			fmt.Printf("removed %d mesh hook(s) from %s\n", removed, p)
			// Name what this did NOT do. The bare receipt above read as a complete
			// uninstall, so people ran it and then deleted the binary, leaving a dead
			// mesh entry in their client config that the agent retried every launch.
			if reg, mp, rerr := hooks.MCPRegistered("claude-code", projAbs); rerr == nil && reg {
				fmt.Printf("still registered: the Mesh MCP server in %s.\n", mp)
				fmt.Println("Session hooks and the MCP registration are separate files. Run `mesh install --remove`")
				fmt.Println("to drop it (add --client <name> for claude-desktop, cursor, vscode, windsurf or codex).")
			}
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", ".", "project dir")
	return c
}

// hooksStopCheckCmd is the Stop hook itself: if the agent already wrote back this
// session it allows the stop; otherwise it blocks ONCE (a per-session marker) with
// a reminder to call mesh_append_note, so the discipline is enforced without an
// infinite loop.
func hooksStopCheckCmd() *cobra.Command {
	var vault string
	var autoExtract bool
	var extractCap int
	c := &cobra.Command{
		Use:    "stop-check",
		Short:  "Internal: a Stop hook that nudges write-back to Mesh once per session",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv(llm.ChildEnv) != "" {
				return nil
			}
			var in struct {
				SessionID      string `json:"session_id"`
				TranscriptPath string `json:"transcript_path"`
			}
			data, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
			_ = json.Unmarshal(data, &in)

			if in.TranscriptPath != "" && transcriptHasWriteback(in.TranscriptPath) {
				return nil // already wrote back: let it stop, nothing to extract
			}
			sid := in.SessionID
			if sid == "" {
				sid = "default"
			}
			marker := filepath.Join(os.TempDir(), "mesh-stop-"+sanitizeID(sid))
			if _, err := os.Stat(marker); err == nil {
				// Already nudged this session and the agent still has not written back.
				// As the fallback, auto-extract the session's learnings into the review
				// queue (once per session), if enabled. Never blocks the stop.
				if autoExtract && vault != "" && in.TranscriptPath != "" {
					exMarker := filepath.Join(os.TempDir(), "mesh-extracted-"+sanitizeID(sid))
					if _, err := os.Stat(exMarker); err != nil {
						_ = os.WriteFile(exMarker, []byte("1"), 0o644)
						if claimExtractionSlot(vault, extractCap, extractionNow()) {
							spawnExtractionFn(vault, in.TranscriptPath)
						} else if extractCap > 0 {
							logExtractionSkip(vault, extractCap, in.TranscriptPath)
						}
					}
				}
				return nil // do not loop
			}
			_ = os.WriteFile(marker, []byte("1"), 0o644)
			out, _ := json.Marshal(map[string]any{
				"decision": "block",
				"reason":   "Mesh: record what you learned before finishing (keeps the next session smart).",
				"hookSpecificOutput": map[string]any{
					"hookEventName":     "Stop",
					"additionalContext": "Before you finish: record durable, non-obvious knowledge with mesh_author_note. Use mesh_note_template to choose the purpose-specific template, then author a factual summary and its meaningful sections with evidence and verification limits. Validate before publishing and confirm the saved result. Use verified collection memberships and links only. Keep missing facts explicit in a draft; do not publish a title-only stub or invent content. If nothing useful was learned, you may stop.",
				},
			})
			fmt.Println(string(out))
			return nil
		},
	}
	c.Flags().StringVar(&vault, "vault", "", "vault to queue auto-extracted candidates into (enables the fallback extractor)")
	c.Flags().BoolVar(&autoExtract, "extract", false, "auto-extract session learnings into the review queue when the agent did not write back")
	c.Flags().IntVar(&extractCap, "extract-cap", 20, "maximum automatic extractions per local day (0 disables auto-extraction)")
	return c
}

var (
	spawnExtractionFn = spawnExtraction
	extractionNow     = time.Now
)

// claimExtractionSlot atomically claims one of cap daily slots. O_EXCL makes the
// ceiling hold even when many Stop hooks finish concurrently.
func claimExtractionSlot(vault string, cap int, now time.Time) bool {
	if cap <= 0 {
		return false
	}
	root := filepath.Join(vault, ".mesh", "extract-slots")
	today := now.Format("2006-01-02")
	dir := filepath.Join(root, today)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	pruneExtractionSlots(root, now.AddDate(0, 0, -1))
	for i := 1; i <= cap; i++ {
		p := filepath.Join(dir, fmt.Sprintf("%d", i))
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return true
		}
		if !errors.Is(err, os.ErrExist) {
			return false
		}
	}
	return false
}

func pruneExtractionSlots(root string, yesterday time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	keepFrom := yesterday.Format("2006-01-02")
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() < keepFrom {
			_ = os.RemoveAll(filepath.Join(root, entry.Name()))
		}
	}
}

func logExtractionSkip(vault string, cap int, transcript string) {
	logPath := filepath.Join(vault, ".mesh", "extract.log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o700)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "auto-extract: daily cap %d reached, skipping %s\n", cap, transcript)
}

// spawnExtraction launches `mesh extract --to-pending <vault> <transcript>` as a
// detached background process so the Stop hook returns immediately (the extraction
// runs the BYOAI LLM, which takes seconds). Best-effort: failures are logged to the
// vault's .mesh/extract.log, never surfaced to the hook's stdout (that is the hook
// protocol channel). Runs in its own process group so it outlives the hook.
func spawnExtraction(vault, transcript string) {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "mesh"
	}
	logPath := filepath.Join(vault, ".mesh", "extract.log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o700)
	lf, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	// The perm above applies only on CREATE, and this is an append to a long-lived log,
	// so a vault that ran an older mesh keeps its 0644 extract.log forever. That file is
	// the extraction subprocess's stdout over a session transcript.
	if lf != nil {
		if fi, serr := lf.Stat(); serr == nil && fi.Mode().Perm()&0o077 != 0 {
			_ = lf.Chmod(fi.Mode() &^ 0o077)
		}
	}
	cmd := exec.Command(self, "extract", "--to-pending", vault, transcript)
	if lf != nil {
		cmd.Stdout, cmd.Stderr = lf, lf
	}
	cmd.SysProcAttr = detachAttr() // detach from the hook's process group (platform-specific)
	if err := cmd.Start(); err != nil {
		if lf != nil {
			fmt.Fprintf(lf, "spawn extraction failed: %v\n", err)
		}
		return
	}
	_ = cmd.Process.Release() // do not wait; let it finish in the background
	if lf != nil {
		_ = lf.Close()
	}
}

// transcriptHasWriteback scans structured write requests, sharing the extractor's
// durable-action policy. It does not replace checking the result of publication.
func transcriptHasWriteback(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		if extract.TranscriptWritebackCall(sc.Bytes()) {
			return true
		}
	}
	return false
}

func sanitizeID(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}
