// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/bright-interaction/mesh/internal/buildinfo"
	"github.com/bright-interaction/mesh/internal/embed"
	"github.com/bright-interaction/mesh/internal/eval"
	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/mcp"
	"github.com/bright-interaction/mesh/internal/meshcfg"
	"github.com/bright-interaction/mesh/internal/netaddr"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/shellpath"
	"github.com/bright-interaction/mesh/internal/sshserve"
	"github.com/bright-interaction/mesh/internal/tui"
	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/bright-interaction/mesh/internal/watch"
	"github.com/bright-interaction/mesh/internal/web"
	"github.com/bright-interaction/mesh/pkg/meshclient"
	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "mesh",
		Short:         "Mesh: a sovereign knowledge mesh built for coding agents",
		SilenceUsage:  true,
		SilenceErrors: false,
		Version:       buildinfo.Ver(),
	}
	// `mesh --version` and `mesh version` must print the SAME line. Cobra's built-in
	// --version flag has its own template, so point it at versionLine() too instead of
	// letting the two surfaces drift.
	root.SetVersionTemplate(versionLine() + "\n")
	root.AddCommand(
		versionCmd(),
		upgradeCmd(),
		initCmd(),
		newCmd(), templatesCmd(), draftsCmd(),
		indexCmd(),
		codeCmd(),
		embedCmd(),
		searchCmd(),
		evalCmd(),
		tuneCmd(),
		statusCmd(),
		healthCmd(),
		flywheelCmd(),
		economicsCmd(),
		rerankCmd(),
		ingestCmd(),
		migrateCmd(),
		scopeCmd(),
		lintCmd(),
		structureCmd(),
		mcpCmd(),
		watchCmd(),
		joinCmd(),
		syncCmd(),
		conflictsCmd(),
		curatorCmd(),
		tuiCmd(),
		uiCmd(),
		serveSSHCmd(),
		installCmd(),
		orientCmd(),
		hooksCmd(),
		extractCmd(),
		guardsCmd(),
		askCmd(),
		doctorCmd(),
	)
	return root
}

// versionLine is the single line every version surface prints: the build stamp that
// the Makefile and Dockerfile bake into buildinfo.Version via -ldflags (or the
// MESH_VERSION override), plus the Go toolchain the binary was built with. An
// unstamped local build reports "dev". SECURITY.md asks reporters for the affected
// version or commit, so this is the command that answers it.
func versionLine() string {
	return fmt.Sprintf("mesh %s (%s)", buildinfo.Ver(), runtime.Version())
}

func versionCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "version",
		Short: "Print the Mesh build version and Go runtime",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{
					"name": "mesh", "build": buildinfo.Ver(),
					"release": buildinfo.ReleaseVer(), "go": runtime.Version(),
				})
			}
			fmt.Fprintln(cmd.OutOrStdout(), versionLine())
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print machine-readable build and release identity")
	return c
}

func initCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init [path]",
		Short: "Bootstrap a new Mesh vault (starter index + first build)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			if err := os.MkdirAll(root, 0o755); err != nil {
				return err
			}
			files, err := vault.Walk(root)
			if err != nil {
				return err
			}
			if len(files) == 0 {
				date := vault.Now().Format("2006-01-02")
				starter := "---\nid: index\ntype: map\ntitle: Vault index\nwhen: \"" + date + "\"\n---\n\n" +
					"# Vault index\n\nA Mesh vault. Add notes with `mesh new <type> \"<title>\"`, then `mesh index`.\n"
				if err := writeStarterIndex(root, starter); err != nil {
					return err
				}
			}
			if err := reconcileOneShotThroughOwner(cmd.Context(), root, "mesh init"); err != nil {
				return err
			}
			store, err := index.OpenReadOnly(root)
			if err != nil {
				return err
			}
			defer store.Close()
			g, err := store.LoadGraph()
			if err != nil {
				return err
			}
			abs, _ := filepath.Abs(root)
			fmt.Printf("initialized Mesh vault at %s (%d notes, %d nodes, %d edges)\n", root, g.CountByKind()["note"], g.NodeCount(), g.EdgeCount())
			// Name what the pass could not index, and fail. Two files with the same
			// effective id (two README.md with no frontmatter id is the everyday case)
			// leave one note out of the vault entirely, and init used to print "1 notes"
			// for 2 files and exit 0, so the first thing a new user ever saw was a
			// quietly incomplete vault.
			nDropped, derr := reportDroppedNotes(store, root)
			if derr != nil {
				return derr
			}
			fmt.Println("next:")
			fmt.Println("  mesh new decision \"<title>\" --vault " + shellpath.Quote(root) + "   # capture a decision/gotcha")
			fmt.Println("  mesh index " + shellpath.Quote(root) + "                          # rebuild after edits")
			fmt.Println("  point your coding agent at the MCP server:")
			fmt.Printf("    {\"command\": \"mesh\", \"args\": [\"mcp\", \"--vault\", \"%s\", \"--watch\"]}\n", abs)
			// Say who indexes, because until this was printed the honest answer for a
			// vault set up this way was "nobody", and nothing said so.
			fmt.Println("    that server elects itself this vault's owning writer, so notes it writes")
			fmt.Println("    (and notes you edit) are searchable at once. Run `mesh doctor " + shellpath.Quote(root) + "`")
			fmt.Println("    to check one is running; `mesh watch " + shellpath.Quote(root) + "` starts a standalone one.")
			// Printed the next steps first, then fail: a vault that dropped notes is
			// incomplete, and init exiting 0 over it was the original defect.
			return droppedNotesError(nDropped)
		},
	}
	return c
}

// reportDroppedNotes names every note the last index pass left out of the index and
// returns how many there were. Nothing is printed for a vault that indexed cleanly.
//
// ONE function for `mesh init` and `mesh install`, on purpose. They are twins: both are a
// stranger's first command, both build that vault's first index, and they used to answer
// the same vault differently. Given eight files where one has broken YAML and one collides
// on id, init named both and exited 1 while install printed "+ indexed the vault (6 notes)"
// then "Done." and exited 0. The first-run command was the one that lied, which is the
// worst possible place for it. Sharing the census and the exit code means a later change
// to either lands on both by construction instead of by somebody remembering.
//
// The returned error is a failure to READ the census, never the drop itself. "no dropped
// notes" and "could not find out" are opposite answers and only one of them is good news,
// so the read failure propagates on its own; a writable store never returns it. The drop
// becomes the command's exit status through droppedNotesError, which callers return AFTER
// their closing advice, because a user whose vault is incomplete still needs to be told
// what to do next.
func reportDroppedNotes(store *index.Store, root string) (int, error) {
	dropped, err := store.DroppedNotes()
	if err != nil {
		return 0, err
	}
	if len(dropped) == 0 {
		return 0, nil
	}
	fmt.Fprintf(os.Stderr, "\n%d note(s) are NOT in the index and are invisible to search and the graph:\n", len(dropped))
	for _, d := range dropped {
		fmt.Fprintf(os.Stderr, "  %s: %v\n", d.Path, d.Err)
	}
	fmt.Fprintf(os.Stderr, "fix them, then run: mesh index %s\n\n", shellpath.Quote(root))
	return len(dropped), nil
}

// droppedNotesError turns the census count into the exit status, and is nil for a clean
// vault so a caller can `return droppedNotesError(n)` as its last line.
func droppedNotesError(n int) error {
	if n == 0 {
		return nil
	}
	return fmt.Errorf("%d note(s) could not be indexed", n)
}

// writeStarterIndex writes the starter index.md `mesh init` drops into an empty vault,
// and fsyncs both the file and the vault directory before returning.
//
// It takes the vault root and names index.md itself rather than taking a finished path,
// which is load-bearing for the census in atomic_write_durability_test.go: that guard
// decides scope from what a writer's own code says it writes, and a helper whose
// destination is a bare `path` parameter tells it nothing. Written the other way round
// this function was invisible to the guard, which is exactly the failure mode the census
// exists to prevent.
//
// It used to be a plain os.WriteFile, exempted from the fsync census as trivially
// re-creatable. That exemption did not survive the failure the fsync prevents. An
// unsynced write can leave a ZERO-LENGTH index.md after a power cut, and vault.Walk
// counts any .md file whatever its contents, so `len(files) == 0` in initCmd is false
// forever after and `mesh init` never re-creates it. The operator is left with an empty
// note holding the vault's index id, in a vault that reported a successful init. The
// other exempted writers really are re-creatable, because losing one leaves NOTHING at
// the path they guard; this one leaves a file.
//
// No temp+rename: the branch that calls this runs only when the vault holds no notes at
// all, so there is no previous content for a rename to protect and no concurrent reader
// to protect it from. Creating the final name directly also keeps the 0644 the note
// wants, which os.CreateTemp's 0600 would have silently changed. The census guard
// accepts either shape and checks the ordering that matters: the data is fsynced before
// the directory entry it is reached through.
func writeStarterIndex(root, body string) error {
	f, err := os.OpenFile(filepath.Join(root, "index.md"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(body)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	syncDir(root)
	return nil
}

// driftListLimit bounds the per-bucket file list doctor prints. Enough to diagnose,
// short enough that a vault mid-bulk-edit does not bury the verdict under a thousand
// paths.
const driftListLimit = 8

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [vault]",
		Short: "Diagnose index freshness (drift), counts, and vault health",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			dbPath := filepath.Join(root, ".mesh", "mesh.db")
			if _, err := os.Stat(dbPath); err != nil {
				fmt.Printf("no index at %s\n  fix: mesh index %s\n", dbPath, shellpath.Quote(root))
				return fmt.Errorf("no index")
			}
			// READ-ONLY: doctor only counts rows and reports drift, so taking the write
			// lock to do it is how `mesh doctor` came to fail with SQLITE_BUSY at "apply
			// schema" against a perfectly fresh index whenever the owning writer happened
			// to be mid-reconcile. Reproduced 2026-08-10 on the live vault.
			store, err := index.OpenReadOnly(root)
			if err != nil {
				if errors.Is(err, index.ErrSchemaTooNew) {
					fmt.Printf("index:  %s\n%v\nstatus: BROKEN - the index is newer than this Mesh binary\n  fix: upgrade Mesh; leave this index untouched\n", dbPath, err)
					return fmt.Errorf("index schema is newer than this Mesh binary")
				}
				// An index written by an older Mesh is a diagnosis, not a crash: doctor is
				// the command a stranger runs to find out what is wrong, and it used to
				// print "status: OK (index fresh)" with exit 0 over exactly this, because
				// the version comparison lived only on the writable path nothing runs.
				if errors.Is(err, index.ErrSchemaMismatch) {
					fmt.Printf("index:  %s\n%v\nstatus: BROKEN - the index does not match this Mesh binary\n  fix: mesh index %s\n", dbPath, err, shellpath.Quote(root))
					return fmt.Errorf("index schema mismatch")
				}
				return err
			}
			defer store.Close()
			if integrityErr := store.CheckIntegrity(root); integrityErr != nil {
				if errors.Is(integrityErr, index.ErrIndexCorrupt) {
					fmt.Printf("index:  %s\n%v\nstatus: BROKEN - the index database is corrupt\n  fix: mesh index %s\n",
						dbPath, integrityErr, shellpath.Quote(root))
					return fmt.Errorf("index database is corrupt")
				}
				return integrityErr
			}

			notes, _ := store.Count("notes")
			nodes, _ := store.Count("nodes")
			edges, _ := store.Count("edges")
			fmt.Printf("index:  %s\n  notes %d  nodes %d  edges %d\n", dbPath, notes, nodes, edges)
			// Who, if anyone, keeps this index fresh. Printed before drift because it
			// explains drift: an index nothing owns can only ever go staler.
			hasOwner := reportOwner(os.Stdout, root)

			drift, err := store.DriftReport(root)
			if err != nil {
				return err
			}
			fmt.Printf("drift:  +%d new  ~%d changed  -%d removed\n", len(drift.Added), len(drift.Changed), len(drift.Removed))
			// NAME the files. Counts alone say the index disagrees with the vault without
			// saying where, and "+1 new" against a 1200-note vault is indistinguishable
			// from a tool bug until you diff the two sets by hand, which is exactly the
			// hour this cost on 2026-08-10. A handful of paths is the whole diagnosis.
			for _, b := range []struct {
				label string
				paths []string
			}{{"new", drift.Added}, {"changed", drift.Changed}, {"removed", drift.Removed}} {
				for i, p := range b.paths {
					if i == driftListLimit {
						fmt.Printf("          ... and %d more %s\n", len(b.paths)-driftListLimit, b.label)
						break
					}
					fmt.Printf("          %-7s %s\n", b.label, p)
				}
			}

			files, err := vault.Walk(root)
			if err != nil {
				return err
			}
			// ferrs are notes that will not parse. Discarding them (this line read
			// `parsed, _ :=`) made doctor the one command that LIES: a vault whose every
			// note was unparseable indexed to zero notes, counted zero lint problems and
			// printed "status: healthy" with exit 0, while `mesh lint` on the same vault
			// exited 1. An unparseable note is invisible to search and the graph, which is
			// the most severe thing doctor can find, not something it may drop.
			parsed, ferrs := index.ParseFiles(files, 0)
			for _, pn := range parsed {
				if rel, rerr := filepath.Rel(root, pn.Path); rerr == nil {
					pn.Path = rel
				}
			}
			// Which notes an index pass would have to QUARANTINE, resolved exactly as the
			// indexer resolves it (incumbent first, then walk order). Without this, a
			// duplicate id was diagnosed only as endless drift: the quarantined file has
			// no notes row, so it read as "+1 new" on every run and doctor printed STALE
			// and exited 1 forever, pointing at `mesh index`, which produced a
			// byte-identical index every time. The advice after that was `mesh migrate`,
			// whose --apply stamps the SAME id into both files and cements the collision.
			incumbent, ierr := store.IDOwners()
			if ierr != nil {
				incumbent = nil
			}
			_, dupes := index.ClaimUniqueIDs(parsed, incumbent)
			_, issues := index.BuildGraph(parsed)
			lintProblems := 0
			for _, pn := range parsed {
				for _, e := range pn.FM.Validate() {
					if e != "missing id" {
						lintProblems++
					}
				}
			}
			lintProblems += len(issues) + len(ferrs)
			fmt.Printf("lint:   %d problems (run mesh lint for detail)\n", lintProblems)

			// A missing owner on an in-sync vault is a NOTICE, not a failure. `mesh init`
			// leaves exactly that state (fresh index, nothing running yet) and the README
			// puts `mesh doctor` in CI and calls its exit code the contract, so failing
			// here made the documented CI recipe impossible to pass on a healthy vault:
			// `mesh init CIvault && mesh doctor CIvault` printed +0 +0 -0, 0 lint problems
			// and then exited 1. It becomes a real failure only in the compound branch
			// below, where the index has ALREADY drifted and nothing is running to fix it.
			// The owner line and its remedy are still printed either way; only the verdict
			// and the exit code changed.
			ownerNotice := ""
			if !hasOwner {
				ownerNotice = " - NOTICE: no owning writer, so nothing will keep it fresh (see owner: NONE above)"
			}

			switch {
			case len(ferrs) > 0:
				for _, fe := range ferrs {
					fmt.Printf("  %s: %v\n", fe.Path, fe.Err)
				}
				fmt.Printf("status: BROKEN - %d note(s) invisible to search (they do not parse)\n  fix: mesh lint %s\n", len(ferrs), shellpath.Quote(root))
				return fmt.Errorf("%d note(s) invisible to search", len(ferrs))
			case len(dupes) > 0:
				// BEFORE the drift branch on purpose. A duplicate id manufactures drift
				// that no reindex can clear, so STALE was always the verdict printed and
				// the real cause never was.
				for _, d := range dupes {
					fmt.Printf("  %s: %v\n", d.Path, d.Err)
				}
				fmt.Printf("status: BROKEN - %d note(s) invisible to search (another note already claims their id)\n"+
					"  fix: give one of each pair a different id, then run mesh index %s\n", len(dupes), shellpath.Quote(root))
				return fmt.Errorf("%d note(s) share an id with another note", len(dupes))
			case drift.Any() && !hasOwner:
				// Stale AND unowned is the compound failure: the index is already behind
				// the vault and there is no process running that will ever catch it up.
				// Named apart from plain STALE because the fix is two commands, not one.
				fmt.Println("status: STALE - the index is behind the vault and no owning writer is running to catch it up")
				fmt.Printf("  fix: mesh index %s, then start an owner (see the owner line above)\n", shellpath.Quote(root))
				return fmt.Errorf("index stale, and no owning writer")
			case drift.Any():
				fmt.Println("status: STALE - run mesh index")
				return fmt.Errorf("index stale")
			case lintProblems > 0:
				fmt.Printf("status: OK (index fresh; lint problems exist)%s\n", ownerNotice)
			case ownerNotice != "":
				fmt.Printf("status: OK (index fresh)%s\n", ownerNotice)
			default:
				fmt.Println("status: healthy")
			}
			return nil
		},
	}
}

func searchCmd() *cobra.Command {
	var vaultDir string
	var limit, budget int
	c := &cobra.Command{
		Use:   "search <query>",
		Short: "Fused retrieval over the indexed vault (FTS + graph, tier-0 boosted, budget-packed)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dbPath := filepath.Join(vaultDir, ".mesh", "mesh.db")
			if _, err := os.Stat(dbPath); err != nil {
				return fmt.Errorf("no index at %s (run: mesh index %s)", dbPath, shellpath.Quote(vaultDir))
			}
			// Read-only: this command only ever LoadGraphs and retrieves, but opening
			// writable runs ensureSchema, which takes the write lock. That is the whole of
			// the documented "mesh search fails SQLITE_BUSY at apply schema while the index
			// is perfectly fresh" symptom: a pure reader queueing behind the owner's
			// reindex for no reason at all.
			store, err := index.OpenReadOnly(vaultDir)
			if err != nil {
				return err
			}
			defer store.Close()
			g, err := store.LoadGraph()
			if err != nil {
				return err
			}
			rt := retrieve.NewFromEnv(store, g)
			var economics retrieve.Economics
			cards, err := rt.Retrieve(cmd.Context(), strings.Join(args, " "), retrieve.Options{Limit: limit, Budget: budget, Economics: &economics})
			if err != nil {
				return err
			}
			economics.ReturnedCards = len(cards)
			economics.ReturnedTokens = retrieve.TotalTokens(cards)
			rt.RecordEconomics(economics)
			if economics.Fallback {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: subscription rerank unavailable; returned explicit local fallback (circuit_open=%t)\n", economics.CircuitOpen)
			}
			if economics.SemanticFallback {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: semantic retrieval unavailable; returned FTS + graph fallback (circuit_open=%t)\n", economics.SemanticCircuitOpen)
			}
			if len(cards) == 0 {
				fmt.Println("no matches")
				return nil
			}
			for i, c := range cards {
				tier := ""
				if c.Tier0 {
					tier = " [tier-0]"
				}
				fmt.Printf("%d. %s%s  (%s)\n", i+1, c.Title, tier, c.Path)
				if warning := c.GuidanceWarning(); warning != "" {
					fmt.Printf("   ! %s\n", warning)
				}
				if sn := strings.TrimSpace(c.Snippet); sn != "" {
					fmt.Printf("   %s\n", sn)
				}
				if c.Reason != "" {
					fmt.Printf("   ~ %s\n", c.Reason)
				}
			}
			if budget > 0 {
				fmt.Printf("packed %d cards, ~%d tokens (budget %d)\n", len(cards), retrieve.TotalTokens(cards), budget)
			}
			return nil
		},
	}
	c.Flags().StringVar(&vaultDir, "vault", ".", "vault root")
	c.Flags().IntVar(&limit, "limit", 20, "candidates per signal")
	c.Flags().IntVar(&budget, "budget", 0, "token budget for packing (0 = all ranked)")
	return c
}

func evalCmd() *cobra.Command {
	var vaultDir, casesFile string
	var budget int
	var requireRerankWin bool
	var jsonOutput bool
	c := &cobra.Command{
		Use:   "eval <cases.json>",
		Short: "Gate 1: measure Mesh retrieval vs the read-top-3-FTS baseline on a labelled query set",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				casesFile = args[0]
			}
			if casesFile == "" {
				return fmt.Errorf("provide a cases file: mesh eval <cases.json> --vault <dir>")
			}
			raw, err := os.ReadFile(casesFile)
			if err != nil {
				return err
			}
			var cases []eval.Case
			if err := json.Unmarshal(raw, &cases); err != nil {
				return fmt.Errorf("parse cases: %w", err)
			}
			store, err := index.OpenReadOnly(vaultDir)
			if err != nil {
				return err
			}
			defer store.Close()
			g, err := store.LoadGraph()
			if err != nil {
				return err
			}
			retriever, err := retrieve.NewFromEnvContext(cmd.Context(), store, g)
			if err != nil {
				return err
			}
			rep := eval.RunGateContext(cmd.Context(), store, retriever, vaultDir, cases, budget)
			if jsonOutput {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(rep); err != nil {
					return err
				}
			}
			if !rep.Valid {
				return fmt.Errorf("evaluation INVALID; no efficiency verdict: %s", strings.Join(rep.Errors, "; "))
			}

			if !jsonOutput {
				pf := func(b bool) string {
					if b {
						return "PASS"
					}
					return "FAIL"
				}
				fmt.Printf("Gate 1: Mesh vs FTS baselines  (vault: %s, %d cases, budget %d, tokenizer: estimate)\n", vaultDir, rep.N, budget)
				fmt.Printf("  surfacing recall @K=%d:   mesh %d/%d   fts %d/%d\n", 20, rep.MeshSurfaced, rep.N, rep.FTSSurfaced, rep.N)
				fmt.Printf("  answer@1 (one body read): mesh %d/%d   fts-top1 %d/%d\n", rep.MeshAnswer1, rep.N, rep.FTSAnswer1, rep.N)
				fmt.Printf("  tokens median:  mesh %.0f   fts-top1 %.0f (matched)   fts-top3 %.0f (naive)\n", rep.MeshMedian, rep.FTSTop1Median, rep.FTSTop3Median)
				fmt.Printf("  tokens mean:    mesh %.0f   fts-top1 %.0f             fts-top3 %.0f\n", rep.MeshMean, rep.FTSTop1Mean, rep.FTSTop3Mean)
				fmt.Printf("  latency ms (%s):\n", rep.LatencyMethod)
				for _, arm := range []struct {
					name    string
					latency eval.LatencySummary
				}{
					{"fts-top1", rep.FTSTop1Latency}, {"fts-top3", rep.FTSTop3Latency},
					{"local Mesh", rep.LocalMeshLatency}, {"configured Mesh", rep.MeshLatency},
				} {
					fmt.Printf("    %-16s median %.3f  p95 %.3f  samples %d\n", arm.name, arm.latency.MedianMillis, arm.latency.P95Millis, arm.latency.Samples)
				}
				fmt.Printf("  sub-claims: surfacing>=fts %s | answer@1>=fts-top1 %s | cheaper-than-naive-top3 %s\n",
					pf(rep.SurfacingWin), pf(rep.AnswerWin), pf(rep.NaiveCostWin))
				if rep.Pass {
					fmt.Println("  VERDICT: PASS (all three sub-claims hold)")
				} else {
					fmt.Println("  VERDICT: PARTIAL (see sub-claims; matched fts-top1 cost shows the card overhead honestly)")
				}

				if rep.RerankEvaluated {
					fmt.Printf("\nRerank economics: subscription/endpoint vs the identical local Mesh ranking\n")
					fmt.Printf("  recall@5:              reranked %d/%d   local %d/%d\n", rep.RerankTop5Surfaced, rep.N, rep.LocalMeshSurfaced, rep.N)
					fmt.Printf("  answer@1:              reranked %d/%d   local %d/%d\n", rep.MeshAnswer1, rep.N, rep.LocalMeshAnswer1, rep.N)
					fmt.Printf("  combined tokens median: reranked %.0f   local %.0f\n", rep.CombinedMedian, rep.LocalMeshMedian)
					fmt.Printf("  combined tokens mean:   reranked %.0f   local %.0f\n", rep.CombinedMean, rep.LocalMeshMean)
					fmt.Printf("  model use:             %d calls, %d cache hits, %d fallbacks, %d accounted tokens (%d provider-reported calls)\n",
						rep.RerankCalls, rep.RerankCacheHits, rep.RerankFallbacks, rep.RerankTokens, rep.ProviderReportedCalls)
					fmt.Printf("  economics gate: quality>=local %s | combined-median<local %s | no-fallbacks %s\n",
						pf(rep.RerankQualityWin), pf(rep.RerankCostWin), pf(rep.RerankFallbacks == 0))
					if rep.RerankPass {
						fmt.Println("  RERANK VERDICT: PASS (the second call earns its token cost)")
					} else {
						fmt.Println("  RERANK VERDICT: FAIL (keep this reranker opt-in; it has not earned default routing)")
					}
				}
			}
			if requireRerankWin && !rep.RerankEvaluated {
				return fmt.Errorf("--require-rerank-win needs a configured MESH_RERANK_AGENT or endpoint")
			}
			if !rep.Pass {
				return fmt.Errorf("gate 1 not fully met")
			}
			if requireRerankWin && !rep.RerankPass {
				return fmt.Errorf("rerank economics gate not met")
			}
			return nil
		},
	}
	c.Flags().StringVar(&vaultDir, "vault", ".", "vault root")
	c.Flags().IntVar(&budget, "budget", 0, "token budget for the Mesh arm (0 = unbudgeted)")
	c.Flags().BoolVar(&requireRerankWin, "require-rerank-win", false, "fail unless configured rerank beats local Mesh on quality and combined token median")
	c.Flags().BoolVar(&jsonOutput, "json", false, "emit the evaluation report as JSON (including validity, errors and latency); preserve failing exit status")
	return c
}

func loadCases(files ...string) ([]eval.Case, error) {
	var all []eval.Case
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var cs []eval.Case
		if err := json.Unmarshal(raw, &cs); err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		all = append(all, cs...)
	}
	return all, nil
}

func tuneCmd() *cobra.Command {
	var vaultDir, testFile string
	var step, holdout float64
	c := &cobra.Command{
		Use:   "tune <train-cases.json> [more-cases.json ...]",
		Short: "Learn fusion weights (FTS/graph/vector) from labelled queries, validated on held-out",
		Long:  "Grid-searches the fusion-weight simplex to maximize answer@1 on the training queries (rerank off, so the fused order is what is measured), then reports how the learned weights and the built-in defaults score on a held-out test split. Set the winner with MESH_WEIGHT_FTS/GRAPH/VEC. Tuning to the same queries you report on is p-hacking; pass --test (or use --holdout) so the headline number is held-out.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			train, err := loadCases(args...)
			if err != nil {
				return err
			}
			var test []eval.Case
			if testFile != "" {
				if test, err = loadCases(testFile); err != nil {
					return err
				}
			} else {
				// Deterministic interleave split so the headline is still held-out
				// without a separate file: every k-th case goes to test.
				if holdout <= 0 || holdout >= 1 {
					holdout = 0.33
				}
				k := int(math.Round(1 / holdout))
				if k < 2 {
					k = 2
				}
				var tr []eval.Case
				for i, cse := range train {
					if (i+1)%k == 0 {
						test = append(test, cse)
					} else {
						tr = append(tr, cse)
					}
				}
				train = tr
			}
			if len(train) == 0 || len(test) == 0 {
				return fmt.Errorf("need non-empty train and test sets (train %d, test %d)", len(train), len(test))
			}
			store, err := index.OpenReadOnly(vaultDir)
			if err != nil {
				return err
			}
			defer store.Close()
			g, err := store.LoadGraphContext(cmd.Context())
			if err != nil {
				return err
			}
			r, err := retrieve.NewFromEnvContext(cmd.Context(), store, g)
			if err != nil {
				return err
			}
			vectors := r.VectorsActive()
			rep, err := eval.TuneWeights(cmd.Context(), r, train, test, step, vectors)
			if err != nil {
				return err
			}

			fmt.Printf("mesh tune (vault %s, vectors %v, %d candidates, step %.2f)\n", vaultDir, vectors, rep.Candidates, step)
			fmt.Printf("  train %d cases, test %d cases (held-out)\n", len(train), len(test))
			w := func(s eval.WeightSet) string {
				return fmt.Sprintf("fts=%.2f graph=%.2f vec=%.2f", s.FTS, s.Graph, s.Vec)
			}
			sc := func(s eval.Score) string {
				return fmt.Sprintf("answer@1 %d/%d, recall %d/%d", s.Answer1, s.N, s.Recall, s.N)
			}
			fmt.Printf("  default (%s):\n      train %s | test %s\n", w(rep.Default), sc(rep.DefaultTrain), sc(rep.DefaultTest))
			fmt.Printf("  learned (%s):\n      train %s | test %s\n", w(rep.Best), sc(rep.BestTrain), sc(rep.BestTest))
			win := rep.BestTest.Answer1 > rep.DefaultTest.Answer1
			tie := rep.BestTest.Answer1 == rep.DefaultTest.Answer1
			switch {
			case win:
				fmt.Printf("  VERDICT: learned weights beat default on held-out (+%d answer@1). Apply with:\n", rep.BestTest.Answer1-rep.DefaultTest.Answer1)
				fmt.Printf("      export MESH_WEIGHT_FTS=%.2f MESH_WEIGHT_GRAPH=%.2f MESH_WEIGHT_VEC=%.2f\n", rep.Best.FTS, rep.Best.Graph, rep.Best.Vec)
			case tie:
				fmt.Println("  VERDICT: learned weights TIE the default on held-out; keep the default (no evidence of a real gain).")
			default:
				fmt.Printf("  VERDICT: learned weights LOSE on held-out (%d vs %d answer@1); the train win did not generalize. Keep the default.\n", rep.BestTest.Answer1, rep.DefaultTest.Answer1)
			}
			return nil
		},
	}
	c.Flags().StringVar(&vaultDir, "vault", ".", "vault root")
	c.Flags().StringVar(&testFile, "test", "", "held-out test cases file (else --holdout splits the train set)")
	c.Flags().Float64Var(&step, "step", 0.05, "weight grid step (smaller = finer search)")
	c.Flags().Float64Var(&holdout, "holdout", 0.33, "test fraction when --test is not given")
	return c
}

func embedCmd() *cobra.Command {
	var endpoint, model, keyEnv string
	var batch int
	var perSection bool
	var noCache bool
	c := &cobra.Command{
		Use:   "embed [vault]",
		Short: "Embed notes via a BYOAI endpoint and store vectors (turns on semantic search)",
		Long:  "Calls an OpenAI-compatible /embeddings endpoint (Ollama, OpenAI, Voyage, ...) you control. Vectors stay in .mesh/mesh.db. After this, mesh search / eval / mcp fuse the semantic signal automatically.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			// Resolve config flag-first, then env, then the persisted solo config.toml.
			cfg, _ := meshcfg.Load(filepath.Join(root, ".mesh"))
			// Track WHERE the endpoint came from. A flag or an env var is operator input
			// that no HTTP surface can write, so a localhost model server named there is
			// dialed directly; the same URL read back out of config.toml is
			// member-writable (the web config API rewrites that file) and stays behind
			// the SSRF guard. See internal/safehttp.
			operatorEndpoint := endpoint != "" || os.Getenv("MESH_EMBED_ENDPOINT") != ""
			if endpoint == "" {
				endpoint = firstNonEmpty(os.Getenv("MESH_EMBED_ENDPOINT"), cfg.Endpoint)
			}
			if model == "" {
				model = firstNonEmpty(os.Getenv("MESH_EMBED_MODEL"), cfg.Model)
			}
			// If --key-env was not passed, inherit the persisted key_env (so a re-embed
			// does not silently fall back to MESH_EMBED_KEY and clobber a custom name).
			if !cmd.Flags().Changed("key-env") && cfg.KeyEnv != "" {
				keyEnv = cfg.KeyEnv
			}
			if endpoint == "" || model == "" {
				return fmt.Errorf("set --endpoint and --model (or MESH_EMBED_ENDPOINT / MESH_EMBED_MODEL).\n  example: mesh embed %s --endpoint http://localhost:11434/v1 --model nomic-embed-text", shellpath.Quote(root))
			}
			if _, err := os.Stat(filepath.Join(root, ".mesh", "mesh.db")); err != nil {
				return fmt.Errorf("no index (run: mesh index %s)", shellpath.Quote(root))
			}
			store, closeStore, err := openOneShotCurrent(root, "mesh embed")
			if err != nil {
				if errors.Is(err, index.ErrOwnerHeld) {
					return fmt.Errorf("mesh embed must update the vector tables exclusively; stop the live mesh mcp/watch owner, run mesh embed, then restart it: %w", err)
				}
				return err
			}
			defer closeStore()
			files, err := store.NoteFiles()
			if err != nil {
				return err
			}
			if len(files) == 0 {
				fmt.Println("no notes to embed")
				return nil
			}
			// Default: one vector per note (the structured title + flywheel + titled
			// sections joined). --per-section instead stores one vector per heading
			// section and scores a note by its best-matching section (max-pool). On
			// a real production corpus per-section gave no recall or answer@1 lift at ~18x the
			// embedding cost, so whole-note is the default; the flag keeps the lever
			// available for long heterogeneous corpora where it may pay off.
			type chunkRef struct {
				NodeID   string
				ChunkIx  int
				Text     string
				NoteHash string // the note's retrieval hash, stamped so retrieval can detect a later edit
			}
			var refs []chunkRef
			for _, nf := range files {
				pn, err := index.ParseFile(filepath.Join(root, nf.Path))
				if err != nil {
					return fmt.Errorf("parse %s: %w", nf.Path, err)
				}
				noteHash := index.RetrievalHash(pn)
				if !perSection {
					refs = append(refs, chunkRef{NodeID: nf.NodeID, ChunkIx: 0, Text: strings.Join(index.ChunkText(pn), "\n"), NoteHash: noteHash})
					continue
				}
				for ix, text := range index.ChunkText(pn) {
					refs = append(refs, chunkRef{NodeID: nf.NodeID, ChunkIx: ix, Text: text, NoteHash: noteHash})
				}
			}
			newEmbedder := embed.NewHTTP
			if operatorEndpoint {
				newEmbedder = embed.NewOperatorHTTP
			}
			emb := newEmbedder(endpoint, model, os.Getenv(keyEnv))
			if batch <= 0 {
				batch = 32
			}
			ctx := context.Background()
			docPrefix := firstNonEmpty(os.Getenv("MESH_EMBED_DOC_PREFIX"), cfg.DocPrefix) // e.g. "search_document: " for nomic

			// Content-hash cache: reuse the stored vector for any chunk whose embedding
			// input is unchanged since the last embed, so a re-embed only pays for the
			// changed and new chunks. The cache is model-scoped (a different model
			// invalidates it). --no-cache forces a full re-embed (e.g. if a same-named
			// model changed its output width).
			cache := map[string]index.CachedVec{}
			if !noCache {
				cache, err = store.CachedVectors(model)
				if err != nil {
					return err
				}
			}
			hashes := make([]string, len(refs))
			for i, r := range refs {
				hashes[i] = index.ContentHash(docPrefix, r.Text)
			}
			rows := make([]index.VectorRow, 0, len(refs))
			var toEmbed []int
			for idx, r := range refs {
				if c, ok := cache[index.VecKey(r.NodeID, r.ChunkIx)]; ok && c.Hash == hashes[idx] {
					rows = append(rows, index.VectorRow{NodeID: r.NodeID, ChunkIx: r.ChunkIx, Vec: c.Vec, ContentHash: hashes[idx], NoteHash: r.NoteHash})
					continue
				}
				toEmbed = append(toEmbed, idx)
			}
			reused := len(refs) - len(toEmbed)
			for i := 0; i < len(toEmbed); i += batch {
				j := min(i+batch, len(toEmbed))
				inputs := make([]string, 0, j-i)
				for _, idx := range toEmbed[i:j] {
					inputs = append(inputs, docPrefix+refs[idx].Text)
				}
				vecs, err := emb.Embed(ctx, inputs)
				if err != nil {
					return fmt.Errorf("embed batch %d-%d via %s: %w", i, j, endpoint, err)
				}
				for k, v := range vecs {
					idx := toEmbed[i+k]
					rows = append(rows, index.VectorRow{NodeID: refs[idx].NodeID, ChunkIx: refs[idx].ChunkIx, Vec: v, ContentHash: hashes[idx], NoteHash: refs[idx].NoteHash})
				}
				fmt.Printf("\rembedded %d/%d new chunks", j, len(toEmbed))
			}
			if len(toEmbed) > 0 {
				fmt.Println()
			}
			if err := store.ReplaceVectors(model, rows); err != nil {
				return err
			}
			dim := 0
			if len(rows) > 0 {
				dim = len(rows[0].Vec)
			}
			mode := "whole-note"
			if perSection {
				mode = "per-section"
			}
			// Persist the solo config so mesh search / mcp work next session without
			// re-exporting env vars. Best-effort: a write failure must not fail the embed
			// (the vectors are already stored). Secrets are never written, only key_env.
			if err := meshcfg.Save(filepath.Join(root, ".mesh"), meshcfg.Embedding{
				Endpoint:    endpoint,
				Model:       model,
				Dim:         dim,
				KeyEnv:      keyEnv,
				QueryPrefix: firstNonEmpty(os.Getenv("MESH_EMBED_QUERY_PREFIX"), cfg.QueryPrefix),
				DocPrefix:   docPrefix,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write .mesh/config.toml (%v); set MESH_EMBED_* env vars to keep semantic search on\n", err)
			}
			fmt.Printf("stored %d vectors across %d notes (%s, model %s, dim %d; %d embedded, %d reused from cache); semantic search active for mesh search / eval / mcp\n", len(rows), len(files), mode, model, dim, len(toEmbed), reused)
			return nil
		},
	}
	c.Flags().StringVar(&endpoint, "endpoint", "", "OpenAI-compatible embeddings base URL (or MESH_EMBED_ENDPOINT)")
	c.Flags().StringVar(&model, "model", "", "embedding model id (or MESH_EMBED_MODEL)")
	c.Flags().StringVar(&keyEnv, "key-env", "MESH_EMBED_KEY", "env var holding the bearer key (empty for local)")
	c.Flags().IntVar(&batch, "batch", 32, "embeddings per request")
	c.Flags().BoolVar(&perSection, "per-section", false, "store one vector per heading section instead of one per note (~18x more vectors; no measured lift on a real production corpus)")
	c.Flags().BoolVar(&noCache, "no-cache", false, "re-embed every chunk, ignoring the content-hash cache (use if a same-named model changed its output width)")
	return c
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [vault]",
		Short: "Show index stats from .mesh/mesh.db",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			dbPath := filepath.Join(root, ".mesh", "mesh.db")
			if _, err := os.Stat(dbPath); err != nil {
				return fmt.Errorf("no index at %s (run: mesh index %s)", dbPath, shellpath.Quote(root))
			}
			// READ-ONLY: counts, the graph and the vector stats are all reads.
			store, err := index.OpenReadOnly(root)
			if err != nil {
				return err
			}
			defer store.Close()
			fmt.Printf("index:  %s\n", dbPath)
			hasOwner := reportOwner(os.Stdout, root)
			for _, t := range []struct{ label, table string }{
				{"notes", "notes"},
				{"nodes", "nodes"},
				{"edges", "edges"},
				{"fts rows", "search_index"},
				{"vectors", "vectors"},
			} {
				n, err := store.Count(t.table)
				if err != nil {
					return err
				}
				fmt.Printf("  %-9s %d\n", t.label, n)
			}

			// Report which retrieval signals will actually fire, reflecting both the
			// stored index and the current BYOAI env config, so the operator can see
			// at a glance what mesh search / eval / mcp will use.
			g, err := store.LoadGraph()
			if err != nil {
				return err
			}
			r := retrieve.NewFromEnv(store, g)
			fmt.Println("retrieval signals:")
			fmt.Println("  fts + graph  always on")
			total, live, stale, _ := store.VectorStats()
			// PROBE, do not just report configuration. Both stages used to print "active"
			// off the config alone, so an endpoint that was down, or refused by the SSRF
			// guard, was reported as working while every query silently ignored it.
			// retrieve.Signals is the single place that decides; the MCP mesh://retrieval
			// resource renders the same report.
			sig := r.Signals(cmd.Context())
			switch {
			case sig.VectorsConfigured && !sig.VectorsReachable:
				fmt.Printf("  vectors      UNREACHABLE (model %s, %d stored): %s\n", sig.VectorModel, live, sig.VectorsError)
				fmt.Println("               searches run without the semantic signal until the endpoint answers")
				fmt.Println("               check MESH_EMBED_ENDPOINT; a private address set in config.toml or the web UI needs MESH_ALLOW_PRIVATE_LLM_ENDPOINT=1")
			case sig.VectorsConfigured:
				fmt.Printf("  vectors      active (model %s, %d live", sig.VectorModel, live)
				if stale > 0 {
					fmt.Printf(", %d stale - run mesh embed to refresh", stale)
				}
				if sig.ANN {
					fmt.Print(", ANN/hnsw")
				}
				fmt.Println(")")
			case total > 0:
				fmt.Println("  vectors      stored but query embedder not configured (re-run mesh embed, or set MESH_EMBED_ENDPOINT + MESH_EMBED_MODEL)")
			default:
				fmt.Println("  vectors      off (run: mesh embed)")
			}
			switch {
			case sig.RerankConfigured && !sig.RerankReachable:
				fmt.Printf("  rerank       UNAVAILABLE (%s): %s\n", sig.RerankModel, sig.RerankError)
				fmt.Println("               searches FAIL while configured; fix the provider CLI/endpoint,")
				fmt.Println("               or unset MESH_RERANK_AGENT / MESH_RERANK_ENDPOINT to turn rerank off")
			case sig.RerankConfigured:
				fmt.Printf("  rerank       active (%s)", sig.RerankModel)
				if sig.RerankCheck != "" {
					fmt.Printf("; %s", sig.RerankCheck)
				}
				fmt.Println()
			default:
				fmt.Println("  rerank       off (set MESH_RERANK_AGENT=codex|claude, or configure a rerank endpoint)")
			}
			if wf, wg, wv := r.Weights(); wf != 0 || wg != 0 || wv != 0 {
				fmt.Printf("  weights      learned fts=%.2f graph=%.2f vec=%.2f (MESH_WEIGHT_*)\n", wf, wg, wv)
			} else {
				fmt.Println("  weights      built-in defaults (run: mesh tune <cases.json> to fit your corpus)")
			}
			// Reported last so the stats above are all printed first, and as a NOTICE
			// rather than a non-zero exit. `mesh status` reports what the index holds; it
			// never computes drift, so the one honest thing it can say about a missing
			// owner is that nothing will keep these numbers moving. Failing on it made a
			// freshly initialised vault (`mesh init` leaves no owner running) report a
			// perfectly correct index and then exit 1, the same over-reach `mesh doctor`
			// had. The owner line and its remedy are still printed by reportOwner above.
			if !hasOwner {
				fmt.Println("notice: no owning writer, so every count above stops moving from here (not a failure; see owner: NONE above)")
			}
			return nil
		},
	}
}

func ingestCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ingest",
		Short: "Pull external knowledge (GitHub, Slack, Linear, Jira, Notion) into the vault",
		Long:  "Sovereign ingestion: import from where your team already keeps knowledge, into YOUR vault on YOUR hardware. Each item becomes a note under imported/<source>/ with source/source_url/imported_at provenance, upserted (a re-pull updates, never duplicates). Pulls are incremental (a high-water mark per source in .mesh/ingest-state.json); --full re-pulls everything, --watch <dur> keeps pulling on a schedule, and `ingest all` runs every source listed in .mesh/ingest.json. Tokens come from env (never flags/config): MESH_INGEST_{GITHUB,SLACK,LINEAR,JIRA,NOTION}_TOKEN.",
	}
	c.AddCommand(ingestGitHubCmd(), ingestSlackCmd(), ingestLinearCmd(), ingestJiraCmd(), ingestNotionCmd(), ingestAllCmd())
	return c
}

func healthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health [vault]",
		Short: "Check knowledge lifecycle: dead source refs, overdue reviews, contradictions",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			if _, err := os.Stat(filepath.Join(root, ".mesh", "mesh.db")); err != nil {
				return fmt.Errorf("no index (run: mesh index %s)", shellpath.Quote(root))
			}
			store, writable, closeStore, err := openOneShotCurrentOrReadOnly(root, "mesh health")
			if err != nil {
				return err
			}
			defer closeStore()
			if integrityErr := store.CheckIntegrity(root); integrityErr != nil {
				if errors.Is(integrityErr, index.ErrIndexCorrupt) {
					fmt.Printf("health: BROKEN - the index database is corrupt\n%v\n", integrityErr)
				}
				return integrityErr
			}
			drift, err := store.DriftReport(root)
			if err != nil {
				return err
			}
			if drift.Any() {
				fmt.Printf("health: STALE - index does not match the Markdown vault (+%d new, ~%d changed, -%d removed)\n  fix: mesh index %s\n",
					len(drift.Added), len(drift.Changed), len(drift.Removed), shellpath.Quote(root))
				return fmt.Errorf("index is stale")
			}
			now := time.Now()
			findings, counts, err := commandHealthPass(store, root, now, writable)
			if err != nil {
				return err
			}
			// Every finding above is computed from INDEX ROWS, and a note that will not
			// parse never became a row. So health used to print "vault healthy" and exit
			// 0 for a vault whose every note was unparseable: nothing to have a dead ref,
			// nothing to be overdue, nothing to contradict anything. That is the same
			// blind spot `mesh doctor` had (it discarded the []FileError from ParseFiles),
			// and doctor was fixed while its twin here was not.
			//
			// A note that does not parse is invisible to search, to the graph, and to
			// every check below, which outranks any lifecycle finding: the content is
			// simply not in the knowledge base. It is read from the FILES, not the index,
			// because the index is exactly what it is missing from.
			files, werr := vault.Walk(root)
			if werr != nil {
				return werr
			}
			parsed, ferrs := index.ParseFiles(files, 0)
			if len(ferrs) > 0 {
				fmt.Printf("health: BROKEN - %d note(s) do not parse, so they are invisible to search, "+
					"to the graph, and to every lifecycle check below\n  fix: mesh lint %s\n\n", len(ferrs), root)
				for _, fe := range ferrs {
					rel := fe.Path
					if r, rerr := filepath.Rel(root, fe.Path); rerr == nil {
						rel = r
					}
					fmt.Printf("  [unparseable] %s - %v\n", rel, fe.Err)
				}
				fmt.Println()
			}
			// The same blind spot, one step further along: a note that parses fine but
			// whose id another note already holds IS quarantined by every index pass, so
			// it is just as absent from the rows every check below reads. health printed
			// "vault healthy" over it, and mesh_health returned nothing, because
			// dropped_notes was only ever written by the incremental path.
			for _, pn := range parsed {
				if rel, rerr := filepath.Rel(root, pn.Path); rerr == nil {
					pn.Path = rel
				}
			}
			incumbent, ierr := store.IDOwners()
			if ierr != nil {
				incumbent = nil
			}
			_, dupes := index.ClaimUniqueIDs(parsed, incumbent)
			if len(dupes) > 0 {
				fmt.Printf("health: BROKEN - %d note(s) share an id with another note, so they are quarantined: "+
					"invisible to search, to the graph, and to every lifecycle check below\n"+
					"  fix: give one of each pair a different id, then run mesh index %s\n\n", len(dupes), shellpath.Quote(root))
				for _, d := range dupes {
					fmt.Printf("  [duplicate-id] %s - %v\n", d.Path, d.Err)
				}
				fmt.Println()
			}
			invisible := len(ferrs) + len(dupes)
			if len(findings) == 0 {
				if invisible == 0 {
					fmt.Println("vault healthy: no dead refs, overdue reviews, or contradictions")
					return nil
				}
				return fmt.Errorf("%d note(s) invisible to search", invisible)
			}
			fmt.Printf("health: %d dead refs, %d overdue, %d contradictions\n\n",
				counts["dead_ref"], counts["overdue"], counts["contradiction"])
			for _, f := range findings {
				fmt.Printf("  [%s] %s - %s\n", f.Issue, f.Path, f.Detail)
			}
			if invisible > 0 {
				return fmt.Errorf("%d note(s) invisible to search", invisible)
			}
			return nil
		},
	}
}

func commandHealthPass(store *index.Store, root string, now time.Time, writable bool) ([]index.HealthFinding, map[string]int, error) {
	if writable {
		if _, err := store.ComputeHealth(root, now); err != nil {
			return nil, nil, err
		}
		if _, err := store.ComputeContradictions(now); err != nil {
			return nil, nil, err
		}
		findings, err := store.ListHealth("")
		counts, _ := store.HealthCounts()
		return findings, counts, err
	}
	findings, err := store.ScanHealth(root, now)
	if err != nil {
		return nil, nil, err
	}
	contradictions, err := store.ScanContradictions()
	if err != nil {
		return nil, nil, err
	}
	findings = append(findings, contradictions...)
	persisted, err := store.ListHealth("")
	if err != nil {
		return nil, nil, err
	}
	computed := map[string]bool{"dead_ref": true, "overdue": true, "contradiction": true}
	for _, finding := range persisted {
		if !computed[finding.Issue] {
			findings = append(findings, finding)
		}
	}
	counts := make(map[string]int, len(findings))
	for _, finding := range findings {
		counts[finding.Issue]++
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Issue != findings[j].Issue {
			return findings[i].Issue < findings[j].Issue
		}
		return findings[i].NoteID < findings[j].NoteID
	})
	return findings, counts, nil
}

func flywheelCmd() *cobra.Command {
	var asJSON bool
	var top int
	c := &cobra.Command{
		Use:   "flywheel [vault]",
		Short: "Show write-back reuse metrics: does the knowledge get reused by later sessions?",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			if _, err := os.Stat(filepath.Join(root, ".mesh", "mesh.db")); err != nil {
				return fmt.Errorf("no index (run: mesh index %s)", shellpath.Quote(root))
			}
			store, err := index.OpenReadOnly(root)
			if err != nil {
				return err
			}
			defer store.Close()
			st, err := store.FlywheelStats()
			if err != nil {
				return err
			}
			reused := store.TopReused(top)
			if asJSON {
				b, _ := json.MarshalIndent(map[string]any{"stats": st, "top_reused": reused}, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("Mesh write-back flywheel (does knowledge get reused by later sessions?)\n\n")
			fmt.Printf("  reuse rate:            %.0f%%   (%d of %d write-backs reused in a later session)\n", st.ReuseRatePct, st.Reused, st.Authored)
			fmt.Printf("  total reuses:          %d\n", st.TotalReuses)
			fmt.Printf("  median time to reuse:  %.1f h\n", st.MedianHoursToReuse)
			fmt.Printf("  input health:          %.0f writes per 100 reads\n", st.WritesPer100Reads)
			if len(reused) > 0 {
				fmt.Printf("\n  most-reused notes:\n")
				for _, r := range reused {
					fmt.Printf("    %3dx  %s\n", r.ReuseCount, r.NoteID)
				}
			}
			fmt.Printf("\n  Reuse = a mesh_fetch of a note >=10min after it was written (a LATER session\n")
			fmt.Printf("  inheriting it). Search-surfaced reuse is not counted, so this is a floor.\n")
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	c.Flags().IntVar(&top, "top", 8, "show the N most-reused notes")
	return c
}

type retrievalEconomicsReport struct {
	Searches              int64   `json:"searches"`
	RerankConfigured      int64   `json:"rerank_configured_searches"`
	RerankCalls           int64   `json:"rerank_calls"`
	CacheHits             int64   `json:"cache_hits"`
	LocalExact            int64   `json:"local_exact"`
	LocalConfident        int64   `json:"local_confident"`
	Fallbacks             int64   `json:"fallbacks"`
	CircuitOpen           int64   `json:"circuit_open"`
	RerankInputTokens     int64   `json:"rerank_input_tokens"`
	RerankOutputTokens    int64   `json:"rerank_output_tokens"`
	AccountedModelTokens  int64   `json:"accounted_model_tokens"`
	ProviderReportedCalls int64   `json:"provider_reported_calls"`
	LocalContextTokens    int64   `json:"local_context_tokens"`
	ActualContextTokens   int64   `json:"actual_context_tokens"`
	ReturnedTokens        int64   `json:"returned_tokens"`
	ReturnedCards         int64   `json:"returned_cards"`
	RerankLatencyMS       int64   `json:"rerank_latency_ms"`
	SearchLatencyMS       int64   `json:"search_latency_ms"`
	CallRatePct           float64 `json:"call_rate_pct"`
	AvgRerankTokens       float64 `json:"avg_rerank_tokens_per_call"`
	AvgReturnedTokens     float64 `json:"avg_returned_tokens_per_search"`
	AvgLocalContext       float64 `json:"avg_local_context_tokens_per_configured_search"`
	AvgActualContext      float64 `json:"avg_actual_context_tokens_per_configured_search"`
	AttributedFetches     int64   `json:"attributed_fetches"`
	SelectedRank1         int64   `json:"selected_rank_1"`
	SelectedTop5          int64   `json:"selected_top_5"`
	SelectedAfterModel    int64   `json:"selected_after_model"`
	SelectedAfterCache    int64   `json:"selected_after_cache"`
	SelectedAfterFallback int64   `json:"selected_after_fallback"`
}

func economicsCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "economics [vault]",
		Short: "Show content-free retrieval and optional-rerank token economics",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			store, err := index.OpenReadOnly(root)
			if err != nil {
				return err
			}
			defer store.Close()
			metric := func(key string) (int64, error) { return store.MetricContext(cmd.Context(), key) }
			keys := map[string]*int64{}
			rep := retrievalEconomicsReport{}
			keys["retrieval:searches"] = &rep.Searches
			keys["rerank:configured"] = &rep.RerankConfigured
			keys["rerank:calls"] = &rep.RerankCalls
			keys["rerank:cache_hits"] = &rep.CacheHits
			keys["rerank:route:local_exact"] = &rep.LocalExact
			keys["rerank:route:local_confident"] = &rep.LocalConfident
			keys["rerank:fallbacks"] = &rep.Fallbacks
			keys["rerank:circuit_open"] = &rep.CircuitOpen
			keys["rerank:input_tokens"] = &rep.RerankInputTokens
			keys["rerank:output_tokens"] = &rep.RerankOutputTokens
			keys["rerank:accounted_tokens"] = &rep.AccountedModelTokens
			keys["rerank:provider_reported_calls"] = &rep.ProviderReportedCalls
			keys["rerank:local_context_tokens"] = &rep.LocalContextTokens
			keys["rerank:actual_context_tokens"] = &rep.ActualContextTokens
			keys["retrieval:returned_tokens"] = &rep.ReturnedTokens
			keys["retrieval:returned_cards"] = &rep.ReturnedCards
			keys["rerank:latency_ms"] = &rep.RerankLatencyMS
			keys["retrieval:latency_ms"] = &rep.SearchLatencyMS
			keys["retrieval:search_to_fetch"] = &rep.AttributedFetches
			keys["retrieval:selected_rank:1"] = &rep.SelectedRank1
			keys["rerank:selected_route:model"] = &rep.SelectedAfterModel
			keys["rerank:selected_route:cache"] = &rep.SelectedAfterCache
			keys["rerank:selected_route:fallback"] = &rep.SelectedAfterFallback
			for key, dst := range keys {
				v, err := metric(key)
				if err != nil {
					return err
				}
				*dst = v
			}
			for rank := 1; rank <= 5; rank++ {
				v, err := metric(fmt.Sprintf("retrieval:selected_rank:%d", rank))
				if err != nil {
					return err
				}
				rep.SelectedTop5 += v
			}
			if rep.RerankConfigured > 0 {
				rep.CallRatePct = 100 * float64(rep.RerankCalls) / float64(rep.RerankConfigured)
				rep.AvgLocalContext = float64(rep.LocalContextTokens) / float64(rep.RerankConfigured)
				rep.AvgActualContext = float64(rep.ActualContextTokens) / float64(rep.RerankConfigured)
			}
			if rep.RerankCalls > 0 {
				rep.AvgRerankTokens = float64(rep.AccountedModelTokens) / float64(rep.RerankCalls)
			}
			if rep.Searches > 0 {
				rep.AvgReturnedTokens = float64(rep.ReturnedTokens) / float64(rep.Searches)
			}
			if asJSON {
				b, _ := json.MarshalIndent(rep, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("Mesh retrieval economics (content-free local counters)\n\n")
			fmt.Printf("  searches:                 %d\n", rep.Searches)
			fmt.Printf("  rerank configured:        %d\n", rep.RerankConfigured)
			fmt.Printf("  model calls:              %d (%.1f%% of configured searches)\n", rep.RerankCalls, rep.CallRatePct)
			fmt.Printf("  cache hits:               %d\n", rep.CacheHits)
			fmt.Printf("  local routes:             %d exact, %d confident\n", rep.LocalExact, rep.LocalConfident)
			fmt.Printf("  explicit fallbacks:       %d (%d circuit-open)\n", rep.Fallbacks, rep.CircuitOpen)
			fmt.Printf("  model tokens (accounted): %d (%.0f/call; %d/%d calls provider-reported)\n", rep.AccountedModelTokens, rep.AvgRerankTokens, rep.ProviderReportedCalls, rep.RerankCalls)
			fmt.Printf("  tokenizer detail:         %d prompt + %d output estimated\n", rep.RerankInputTokens, rep.RerankOutputTokens)
			fmt.Printf("  returned context:         %d tokens / %d cards (%.0f tokens/search)\n", rep.ReturnedTokens, rep.ReturnedCards, rep.AvgReturnedTokens)
			fmt.Printf("  configured-search cost:   %.0f local-only vs %.0f actual tokens/search\n", rep.AvgLocalContext, rep.AvgActualContext)
			fmt.Printf("  search→fetch choices:     %d attributed; %d rank-1, %d top-5\n", rep.AttributedFetches, rep.SelectedRank1, rep.SelectedTop5)
			fmt.Printf("  chosen route:             %d model, %d cache, %d fallback\n", rep.SelectedAfterModel, rep.SelectedAfterCache, rep.SelectedAfterFallback)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}

func newCmd() *cobra.Command {
	var vaultDir, summary, related, tags, collections, supersedes, status, severity, by, sectionsFile, blocksFile, draftID, draftRevision, verifiedAt string
	var sectionValues []string
	var version int
	c := &cobra.Command{
		Use:   "new <template> <title...>",
		Short: "Author a purpose-specific note, or save an incomplete draft in inbox",
		Long:  "Choose a template with mesh templates. Supply a factual summary and authored sections. Drafts remain in inbox; publication requires all required content. Mesh derives identity, timestamps and placement.",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sections := map[string]string{}
			if sectionsFile != "" {
				if err := readAuthoringJSON(sectionsFile, &sections); err != nil {
					return err
				}
			}
			for _, value := range sectionValues {
				key, content, ok := strings.Cut(value, "=")
				if !ok || strings.TrimSpace(key) == "" {
					return fmt.Errorf("section must be key=authored prose")
				}
				key = strings.TrimSpace(key)
				if _, exists := sections[key]; exists {
					return fmt.Errorf("section %q supplied more than once", key)
				}
				sections[key] = content
			}
			var blocks []vault.BlockSpec
			if blocksFile != "" {
				if err := readAuthoringJSON(blocksFile, &blocks); err != nil {
					return err
				}
			}
			spec := vault.NewNoteSpec{
				Template: args[0], TemplateVersion: version, Title: strings.Join(args[1:], " "),
				Summary: summary, Sections: sections, Blocks: blocks, Related: splitCSV(related),
				Tags: splitCSV(tags), Collections: splitCSV(collections), Supersedes: splitCSV(supersedes),
				Status: status, Severity: severity, By: by, DraftID: draftID, DraftRevision: draftRevision, VerifiedAt: verifiedAt,
			}
			if !cmd.Flags().Changed("status") {
				normalized, err := vault.NormalizeSpec(spec)
				if err != nil {
					return err
				}
				spec = normalized
				spec.Status = "active"
				if len(vault.MissingContent(spec)) > 0 {
					spec.Status = "draft"
				}
			}
			if err := validateLocalAuthoringReferences(cmd.Context(), vaultDir, spec); err != nil {
				return err
			}
			res, err := vault.CreateNoteContext(cmd.Context(), vaultDir, spec)
			if err != nil {
				return err
			}
			fmt.Printf("saved %s\n  id: %s   when: %s\n  revision: %s\n", res.Path, res.ID, res.When, res.Revision)
			if len(res.TODOs) > 0 {
				fmt.Printf("draft needs: %s\n", strings.Join(res.TODOs, "; "))
			}
			return nil
		},
	}
	c.Flags().StringVar(&vaultDir, "vault", ".", "vault root")
	c.Flags().IntVar(&version, "template-version", 1, "exact template version")
	c.Flags().StringVar(&summary, "summary", "", "factual summary of the note")
	c.Flags().StringVar(&draftID, "draft-id", "", "existing draft ID to revise or publish")
	c.Flags().StringVar(&draftRevision, "draft-revision", "", "exact current draft revision")
	c.Flags().StringVar(&verifiedAt, "verified-at", "", "date or timestamp supported by a verification block")
	c.Flags().StringArrayVar(&sectionValues, "section", nil, "authored section key=prose (repeat for each section)")
	c.Flags().StringVar(&sectionsFile, "sections-file", "", "JSON object of section keys and authored prose")
	c.Flags().StringVar(&blocksFile, "blocks-file", "", "JSON array of optional supporting blocks")
	c.Flags().StringVar(&related, "related", "", "comma-separated verified note IDs")
	c.Flags().StringVar(&tags, "tags", "", "comma-separated cross-cutting topic tags")
	c.Flags().StringVar(&collections, "collections", "", "comma-separated collection IDs")
	c.Flags().StringVar(&supersedes, "supersedes", "", "comma-separated replaced note IDs")
	c.Flags().StringVar(&status, "status", "", "auto publishes complete notes and saves incomplete drafts; active/draft override")
	c.Flags().StringVar(&severity, "severity", "", "incident severity")
	c.Flags().StringVar(&by, "by", "", "author/contributor")
	return c
}

// readAuthoringJSON bounds local input before decoding; prose validation stays in
// the shared vault API rather than creating a separate CLI publication contract.
func readAuthoringJSON(path string, out any) error {
	return readAuthoringJSONBounded(path, out, 128<<10)
}

func readAuthoringJSONBounded(path string, out any, maxBytes int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > maxBytes {
		return fmt.Errorf("authoring input exceeds %d bytes", maxBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("authoring input must contain exactly one JSON value")
	}
	return nil
}

func templatesCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{Use: "templates [id]", Short: "Show the canonical note and supporting-block templates", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			tmpl, err := vault.TemplateFor(args[0], 1)
			if err != nil {
				return err
			}
			raw, _ := json.MarshalIndent(tmpl, "", "  ")
			fmt.Println(string(raw))
			return nil
		}
		if asJSON {
			raw, _ := json.MarshalIndent(map[string]any{"templates": vault.Templates(), "blocks": vault.BlockTemplates()}, "", "  ")
			fmt.Println(string(raw))
			return nil
		}
		for _, tmpl := range vault.Templates() {
			fmt.Printf("%s (v%d): %s\n", tmpl.ID, tmpl.Version, tmpl.Purpose)
		}
		return nil
	}}
	c.Flags().BoolVar(&asJSON, "json", false, "emit template and block schemas as JSON")
	c.AddCommand(authoringMigrationPreviewCmd(), authoringMigrationApplyCmd())
	return c
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func indexCmd() *cobra.Command {
	var dryRun bool
	var workers int
	c := &cobra.Command{
		Use:   "index [vault]",
		Short: "Parse a markdown vault into the knowledge graph",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			files, err := vault.Walk(root)
			if err != nil {
				return err
			}
			w := workers
			if w <= 0 {
				w = runtime.NumCPU()
			}
			start := time.Now()
			notes, ferrs := index.ParseFiles(files, workers)
			// Store vault-relative paths: portable across machines and far
			// cheaper to carry in a token-budgeted card than an absolute path.
			for _, pn := range notes {
				if rel, err := filepath.Rel(root, pn.Path); err == nil {
					pn.Path = rel
				}
			}
			parseDur := time.Since(start)
			for _, fe := range ferrs {
				fmt.Fprintf(os.Stderr, "parse %s: %v\n", fe.Path, fe.Err)
			}
			// The store is opened BEFORE the graph is built, because which of two files
			// sharing an id gets indexed is resolved against the ids already in the index
			// (see index.ClaimUniqueIDs), and building the graph from a set that still
			// holds both files is exactly how the full path and the incremental path came
			// to disagree about which note an id meant. --dry-run still opens nothing.
			var store *index.Store
			var incumbent map[string]string
			if !dryRun {
				// OpenRebuild, not Open: this command IS the rebuild, so a database it
				// cannot read is the thing being replaced, not a dead end. Before this, a
				// corrupt .mesh/mesh.db failed search, doctor, health AND index with the
				// same "file is not a database (26)", so the repair command was itself
				// blocked. It discards the file only for that one error class; see
				// recoverCorruptIndex.
				var recovered bool
				var oerr error
				var closeStore func()
				store, recovered, closeStore, oerr = openOneShotRebuild(root, "mesh index")
				if oerr != nil {
					return oerr
				}
				defer closeStore()
				if recovered {
					fmt.Fprintf(os.Stderr, "warning: %s was corrupt and unreadable; removed it and rebuilt from the markdown. "+
						"Markdown notes are unchanged. Any pending review notes, usage/reuse history and stored embeddings in the discarded database are not recovered from Markdown. "+
						"Restore a verified backup to recover database-only state; re-run mesh embed only if you need to regenerate embeddings.\n",
						filepath.Join(root, ".mesh", "mesh.db"))
				}
				if owners, ierr := store.IDOwners(); ierr == nil {
					incumbent = owners
				}
			}
			notes, dupes := index.ClaimUniqueIDs(notes, incumbent)
			for _, d := range dupes {
				fmt.Fprintf(os.Stderr, "quarantined %s: %v\n", d.Path, d.Err)
			}
			g, issues := index.BuildGraph(notes)
			communities := g.DetectCommunities(0)
			printStats(root, len(files), len(ferrs)+len(dupes), parseDur, w, communities, notes, g, issues)
			if dryRun {
				if len(dupes) > 0 {
					return fmt.Errorf("%d note(s) share an id with another note and would not be indexed", len(dupes))
				}
				return nil
			}
			n, err := store.IndexVault(notes, g)
			if err != nil {
				return err
			}
			// Record what this pass left out, so `mesh health`, `mesh doctor` and
			// mesh_health in every MCP window can see it. Only ReindexFull and the
			// incremental reconcile did this, so a vault indexed with `mesh index`
			// reported dropped_notes empty however many notes it had quarantined.
			store.RecordDropped(root, append(ferrs, dupes...))
			fmt.Printf("wrote:  %d notes to %s\n", n, store.Path())
			// Apply whatever the read-only surfaces queued for an owning writer. This is
			// the command every owner_down message points people at ("start an owner, or
			// run `mesh index <vault>` once"), so it has to actually settle the queue: a
			// review item the dashboard promoted while nothing owned the index stays in
			// that queue forever otherwise, and the advice would be wrong.
			if applied, err := store.DrainOps(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not apply the queued index ops: %v\n", err)
			} else if applied > 0 {
				fmt.Printf("applied: %d queued op(s) from read-only surfaces (review-queue changes, usage counters)\n", applied)
			}
			// Non-zero AFTER the index is written, not instead of it: the notes that did
			// not collide belong in the index. The exit code is what stops a duplicate
			// from being reported as a clean rebuild by a script or a CI step.
			if len(dupes) > 0 {
				return fmt.Errorf("%d note(s) share an id with another note and were left out of the index; give one of each pair a different id, then run mesh index %s again", len(dupes), shellpath.Quote(root))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "parse and report without writing .mesh/mesh.db")
	c.Flags().IntVar(&workers, "workers", 0, "parse workers (0 = NumCPU)")
	return c
}

// codeCmd is the source-code index. It walks the
// configured code roots (separate from the note vault), extracts symbols (Go via the
// stdlib AST with a call graph; other languages via a declaration scanner), and lets
// mesh_code_search / mesh_code_neighbors locate definitions by name.
func codeCmd() *cobra.Command {
	c := &cobra.Command{Use: "code", Short: "Source-code index: locate symbols + Go call graph"}
	c.AddCommand(codeReindexCmd(), codeSearchCmd(), codeContextCmd())
	return c
}

func codeReindexCmd() *cobra.Command {
	var rootsFlag []string
	var langsFlag string
	var full bool
	var throughOwner bool
	var wait time.Duration
	var expectRoot string
	c := &cobra.Command{
		Use:   "reindex [vault]",
		Short: "Walk the configured code roots and refresh the source-code index (incremental; --full rebuilds)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			if err := vault.RequireRoot(root); err != nil {
				return err
			}
			if throughOwner {
				if cmd.Flags().Changed("root") || cmd.Flags().Changed("languages") || cmd.Flags().Changed("full") || wait <= 0 {
					return fmt.Errorf("--through-owner uses only [code] config, accepts no root/languages/full overrides, and requires --wait > 0")
				}
				store, writable, closeStore, err := openOneShotCurrentOrReadOnly(root, "mesh code refresh")
				if err != nil {
					return err
				}
				defer closeStore()
				id, err := store.EnqueueCodeRefresh(expectRoot)
				if err != nil {
					return err
				}
				if writable {
					if _, err := store.DrainOpsContext(cmd.Context()); err != nil {
						return err
					}
				}
				if err := store.AwaitCodeRefresh(cmd.Context(), id, wait); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "code refresh acknowledged by index owner: "+id)
				return nil
			}
			store, closeStore, err := openOneShotCurrent(root, "mesh code reindex")
			if err != nil {
				if errors.Is(err, index.ErrOwnerHeld) {
					return fmt.Errorf("mesh code reindex must update the code tables exclusively; stop the live mesh mcp/watch owner, run mesh code reindex, then restart it: %w", err)
				}
				return err
			}
			defer closeStore()
			cfg, _ := meshcfg.LoadConfig(store.MeshDir())
			roots := rootsFlag
			if len(roots) == 0 {
				roots = cfg.Code.Roots
			}
			if env := os.Getenv("MESH_CODE_ROOTS"); env != "" && len(rootsFlag) == 0 {
				roots = strings.Split(env, ",")
			}
			if len(roots) == 0 {
				return fmt.Errorf("no code roots: set [code] roots in %s or pass --root", filepath.Join(store.MeshDir(), "config.toml"))
			}
			langs := cfg.Code.Languages
			if langsFlag != "" {
				langs = strings.Split(langsFlag, ",")
			}
			start := time.Now()
			reindex := index.ReindexCode
			if full {
				reindex = index.ReindexCodeFull
			}
			st, err := reindex(store, roots, codeLangSet(langs))
			if err != nil {
				return err
			}
			links := refreshNoteCodeLinks(os.Stderr, store, root)
			fmt.Printf("code index: %d files parsed (%d unchanged, %d removed), %d symbols, %d edges, %d note links in %s\n  roots: %s\n  db:    %s\n",
				st.Files, st.Unchanged, st.Removed, st.Symbols, st.Edges, links, time.Since(start).Round(time.Millisecond), strings.Join(roots, ", "), store.Path())
			return nil
		},
	}
	c.Flags().StringSliceVar(&rootsFlag, "root", nil, "code root to index (repeatable); overrides config")
	c.Flags().StringVar(&langsFlag, "languages", "", "comma list of language tags (default: config or all)")
	c.Flags().BoolVar(&full, "full", false, "wipe and rebuild the whole index instead of the incremental mtime-drift refresh")
	c.Flags().BoolVar(&throughOwner, "through-owner", false, "queue a full refresh of [code] config roots through the index owner and require its completion receipt")
	c.Flags().DurationVar(&wait, "wait", time.Minute, "maximum wait for an owner-routed refresh acknowledgement")
	c.Flags().StringVar(&expectRoot, "expect-root", "", "with --through-owner, require this to be the sole configured code root (does not override config)")
	return c
}

// noteCodeLinker is the one method refreshNoteCodeLinks needs, so the failure path can be
// exercised without a database that refuses to answer.
type noteCodeLinker interface {
	LinkNotesToCode(vaultRoot string) (int, error)
}

// refreshNoteCodeLinks rebuilds the note<->code bridge and SAYS SO when it fails. The
// error used to be dropped (`links, _ := store.LinkNotesToCode(root)`) and the zero count
// printed inside the success line, so a bridge that never ran read exactly like a vault
// whose notes mention no code: the operator's only signal was a 0 that has a legitimate
// meaning. The count is still returned so the summary line keeps its shape.
func refreshNoteCodeLinks(w io.Writer, l noteCodeLinker, root string) int {
	links, err := l.LinkNotesToCode(root)
	if err != nil {
		fmt.Fprintf(w, "warning: note<->code bridge not refreshed (%v); the note-link count below is not a real 0\n", err)
		return 0
	}
	return links
}

func codeSearchCmd() *cobra.Command {
	var vaultRoot, langs string
	var limit int
	c := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the source-code symbol index (file:line results)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := vault.RequireRoot(vaultRoot); err != nil {
				return err
			}
			store, err := index.OpenReadOnly(vaultRoot)
			if err != nil {
				return err
			}
			defer store.Close()
			var langList []string
			if langs != "" {
				langList = strings.Split(langs, ",")
			}
			hits, err := store.SearchCode(cmd.Context(), strings.Join(args, " "), limit, langList)
			if err != nil {
				return err
			}
			for _, h := range hits {
				fmt.Printf("%-9s %-40s %s:%d\n", h.Kind, h.Name, h.Path, h.Line)
			}
			fmt.Printf("(%d symbols)\n", len(hits))
			return nil
		},
	}
	c.Flags().StringVar(&vaultRoot, "vault", ".", "vault root (the .mesh/mesh.db location)")
	c.Flags().IntVar(&limit, "limit", 15, "max results")
	c.Flags().StringVar(&langs, "languages", "", "comma list of language tags to filter")
	return c
}

// codeContextCmd: "what do we know about this code" - symbols + the notes that
// reference them (the note<->code bridge).
func codeContextCmd() *cobra.Command {
	var vaultRoot string
	var limit int
	c := &cobra.Command{
		Use:   "context <query>",
		Short: "Show code symbols together with the notes that reference them",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := vault.RequireRoot(vaultRoot); err != nil {
				return err
			}
			store, err := index.OpenReadOnly(vaultRoot)
			if err != nil {
				return err
			}
			defer store.Close()
			q := strings.Join(args, " ")
			hits, err := store.SearchCode(cmd.Context(), q, limit, nil)
			if err != nil {
				return err
			}
			fmt.Println("code:")
			for _, h := range hits {
				fmt.Printf("  %-9s %-40s %s:%d\n", h.Kind, h.Name, h.Path, h.Line)
			}
			notes, _ := store.NotesForSymbolName(q)
			fmt.Printf("notes about %q (%d):\n", q, len(notes))
			for _, nt := range notes {
				fmt.Printf("  - [%s] %s (%s)\n", nt.Type, nt.Title, nt.Path)
			}
			return nil
		},
	}
	c.Flags().StringVar(&vaultRoot, "vault", ".", "vault root (the .mesh/mesh.db location)")
	c.Flags().IntVar(&limit, "limit", 8, "max symbols")
	return c
}

func codeLangSet(langs []string) map[string]bool {
	if len(langs) == 0 {
		return nil
	}
	m := map[string]bool{}
	for _, l := range langs {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			m[l] = true
		}
	}
	return m
}

func printStats(root string, files, parseErrs int, parseDur time.Duration, workers, communities int, notes []*index.ParsedNote, g *graph.Graph, issues []index.Issue) {
	byType := map[string]int{}
	for _, n := range notes {
		byType[string(n.FM.Type)]++
	}
	byIssue := map[string]int{}
	for _, is := range issues {
		byIssue[is.Kind]++
	}

	fmt.Printf("vault:  %s\n", root)
	fmt.Printf("parse:  %d files in %s (%d workers, %d errors)\n", files, parseDur.Round(time.Microsecond), workers, parseErrs)
	fmt.Printf("nodes:  %d\n", g.NodeCount())
	for _, kv := range sortedCounts(g.CountByKind()) {
		fmt.Printf("          %-8s %d\n", kv.k, kv.v)
	}
	fmt.Printf("edges:  %d\n", g.EdgeCount())
	fmt.Printf("communities: %d\n", communities)
	fmt.Printf("types:\n")
	for _, kv := range sortedCounts(byType) {
		fmt.Printf("          %-12s %d\n", kv.k, kv.v)
	}
	if len(issues) > 0 {
		fmt.Printf("issues: %d\n", len(issues))
		for _, kv := range sortedCounts(byIssue) {
			fmt.Printf("          %-14s %d\n", kv.k, kv.v)
		}
	}
}

type kvCount struct {
	k string
	v int
}

func sortedCounts(m map[string]int) []kvCount {
	out := make([]kvCount, 0, len(m))
	for k, v := range m {
		out = append(out, kvCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].v != out[j].v {
			return out[i].v > out[j].v
		}
		return out[i].k < out[j].k
	})
	return out
}

func migrateCmd() *cobra.Command {
	var apply, dryRunCompat bool
	c := &cobra.Command{
		Use:   "migrate [vault]",
		Short: "Bring a legacy pre-Mesh markdown vault up to the Mesh schema (dry run unless --apply; idempotent)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			// Dry run by DEFAULT, writing only on --apply, matching `mesh structure
			// --fill-bodies`. This rewrites every note in the vault in place with no
			// backup, so one `mesh migrate` typed at the wrong directory used to rewrite
			// the whole thing; the safe direction has to be the one you get by accident.
			// The legacy --dry-run still WINS over --apply rather than being ignored: it
			// is hidden now, so anyone still passing it cannot read its help, and a flag
			// whose name promises "do not write" must never be the reason something wrote.
			dryRun := !apply || dryRunCompat
			files, err := vault.Walk(root)
			if err != nil {
				return err
			}
			// ONE pass, one id-claim scan, shared by every file in it. A note id is
			// vault-global, so migrating file by file with no memory of the vault is how
			// `id: readme` used to land in both README.md of a two-folder vault.
			m, err := vault.NewMigration(root)
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Println("dry run (pass --apply to write); nothing on disk is changed")
			}
			var changed, flywheel, errored int
			for _, f := range files {
				res, err := m.File(f, dryRun)
				if err != nil {
					errored++
					fmt.Fprintf(os.Stderr, "migrate %s: %v\n", f, err)
					continue
				}
				if res.Changed {
					changed++
				}
				if len(res.Issues) > 0 {
					flywheel++
				}
			}
			verb := "migrated"
			if dryRun {
				verb = "would migrate"
			}
			fmt.Printf("%s %d of %d files (%d already clean, %d errored)\n", verb, changed, len(files), len(files)-changed-errored, errored)
			if flywheel > 0 {
				fmt.Printf("note:   %d legacy notes need reviewed migration to purpose-specific prose (never auto-filled)\n", flywheel)
			}
			// A migrate that failed on 800 of 1150 files used to print the failures and
			// exit 0, so every script wrapping it read a partial rewrite as success.
			if errored > 0 {
				return fmt.Errorf("%d file(s) failed to migrate", errored)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "write the changes into the notes (without it this is a dry run)")
	c.Flags().BoolVar(&dryRunCompat, "dry-run", false, "compatibility: a dry run is the default now, and this still forces one")
	_ = c.Flags().MarkHidden("dry-run") // kept so existing scripts still parse, and still honoured
	return c
}

func scopeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "scope",
		Short: "Access-scope tools (label notes for the team scope model)",
	}
	c.AddCommand(scopeBackfillCmd())
	return c
}

func scopeBackfillCmd() *cobra.Command {
	var apply, dryRunCompat bool
	var scope string
	c := &cobra.Command{
		Use:   "backfill [vault]",
		Short: "Stamp an explicit scope on every note that has none (dry run unless --apply; idempotent; default dev)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			// Same inversion as migrate, legacy --dry-run included: this is the second
			// bulk in-place rewriter, and the two have to agree or the safe default is a
			// coin flip per command.
			dryRun := !apply || dryRunCompat
			if strings.TrimSpace(scope) == "" {
				scope = "dev"
			}
			files, err := vault.Walk(root)
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Println("dry run (pass --apply to write); nothing on disk is changed")
			}
			var changed, errored int
			for _, f := range files {
				res, err := vault.BackfillScopeFile(f, scope, dryRun)
				if err != nil {
					errored++
					fmt.Fprintf(os.Stderr, "backfill %s: %v\n", f, err)
					continue
				}
				if res.Changed {
					changed++
				}
			}
			verb := "labeled"
			if dryRun {
				verb = "would label"
			}
			fmt.Printf("%s %d of %d notes with scope %q (%d already scoped, %d errored)\n",
				verb, changed, len(files), scope, len(files)-changed-errored, errored)
			fmt.Println("note: unlabeled notes already behave as dev by the fail-safe; this just makes it explicit so they can be relabeled.")
			if errored > 0 {
				return fmt.Errorf("%d note(s) failed to backfill", errored)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "write the changes into the notes (without it this is a dry run)")
	c.Flags().BoolVar(&dryRunCompat, "dry-run", false, "compatibility: a dry run is the default now, and this still forces one")
	_ = c.Flags().MarkHidden("dry-run") // kept so existing scripts still parse, and still honoured
	c.Flags().StringVar(&scope, "scope", "dev", "scope to stamp on unlabeled notes")
	return c
}

func lintCmd() *cobra.Command {
	var showAll bool
	c := &cobra.Command{
		Use:   "lint [vault]",
		Short: "Check vault health: ERRORS break retrieval, NOTICES are authoring debt",
		Long: `Check vault health.

Lint reports two different things and does not conflate them:

  ERRORS   break retrieval. A note that will not parse is invisible to search and the
           graph; a duplicate or ambiguous id makes one note absorb another's links.
           These are defects. Non-zero exit.

  NOTICES  are work, not damage. A [[link]] to a note nobody has written yet is a
           deliberate marker (see the vault structure standard), and an unfilled
           required authored section on a note is authoring debt only a human can settle, which
           is why no tool fills them in. Zero exit.

This split exists because the old single count reported ~1100 "problems" for a vault
whose retrieval was entirely healthy, so the number meant nothing and got ignored. A
check that cannot fail meaningfully is worse than no check.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			files, err := vault.Walk(root)
			if err != nil {
				return err
			}
			notes, ferrs := index.ParseFiles(files, 0)
			for _, pn := range notes {
				if rel, rerr := filepath.Rel(root, pn.Path); rerr == nil {
					pn.Path = rel
				}
			}
			_, issues := index.BuildGraph(notes)

			type item struct{ where, what string }
			var errorsList, noticesList []item

			for _, fe := range ferrs {
				errorsList = append(errorsList, item{fe.Path, "will not parse, so it is invisible to search and the graph: " + fe.Err.Error()})
			}
			for _, is := range issues {
				switch is.Kind {
				case "broken-link":
					noticesList = append(noticesList, item{is.Path, is.Msg})
				case "unterminated-comment":
					// Work, not damage: it is confined to one note and only the author knows
					// whether the tail was meant to be hidden. Reported because the alternative
					// is what it used to be, which is nothing at all.
					noticesList = append(noticesList, item{is.Path, is.Msg})
				default: // missing-id, duplicate-id, ambiguous-link-key, ambiguous-link
					errorsList = append(errorsList, item{is.Path, is.Kind + ": " + is.Msg})
				}
			}
			for _, pn := range notes {
				for _, e := range pn.FM.Validate() {
					if e == "missing id" {
						continue // already reported via BuildGraph
					}
					if strings.Contains(e, "not filled") || strings.Contains(e, "contains an unfilled placeholder") || e == "missing when" {
						noticesList = append(noticesList, item{pn.Path, e})
						continue
					}
					errorsList = append(errorsList, item{pn.Path, e})
				}
				if pn.FM.Template != "" || pn.FM.TemplateVersion != 0 {
					authored, aerr := vault.ReadAuthoring(pn.FM, pn.Body)
					if aerr != nil {
						errorsList = append(errorsList, item{pn.Path, "invalid authored structure: " + aerr.Error()})
					} else {
						for _, missing := range authored.MissingSections {
							noticesList = append(noticesList, item{pn.Path, "missing authored content: " + missing})
						}
					}
				}
				if base := filepath.Base(pn.Path); !isKebab(base) && !isConventionalDoc(base) {
					noticesList = append(noticesList, item{pn.Path, "filename is not kebab-case"})
				}
			}

			show := func(label string, list []item) {
				if len(list) == 0 {
					return
				}
				fmt.Printf("\n%s (%d):\n", label, len(list))
				limit := len(list)
				if !showAll && limit > 15 {
					limit = 15
				}
				for _, it := range list[:limit] {
					fmt.Printf("  %s: %s\n", it.where, it.what)
				}
				if limit < len(list) {
					fmt.Printf("  ... and %d more (--all to list every one)\n", len(list)-limit)
				}
			}

			fmt.Printf("lint %s: %d files, %d errors, %d notices\n", root, len(files), len(errorsList), len(noticesList))
			show("ERRORS (these break retrieval)", errorsList)
			show("NOTICES (work, not damage)", noticesList)

			if len(errorsList) > 0 {
				return fmt.Errorf("%d error(s) break retrieval", len(errorsList))
			}
			fmt.Println("\nno lint errors: index freshness, database health and factual accuracy were not checked")
			return nil
		},
	}
	c.Flags().BoolVar(&showAll, "all", false, "list every item instead of the first 15 per section")
	return c
}

func vaultArg(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return "."
}

// firstNonEmpty returns the first non-empty string (the config-resolution chain:
// flag, then env, then persisted config.toml).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// isConventionalDoc reports whether a filename is one of the SHOUTING-CASE documents every
// repository is expected to carry. README.md, CLAUDE.md and ORGANIZATION.md are supposed to
// look like that, so flagging them as badly named is the tool reporting a non-defect, which
// costs a reader more than it saves: notices only stay useful while every one of them is
// worth acting on. Matched by shape (all caps, no lowercase) rather than by a fixed list, so
// a vault's own AGENTS.md or CONTRIBUTING.md is covered without an edit here.
func isConventionalDoc(filename string) bool {
	stem := strings.TrimSuffix(filename, filepath.Ext(filename))
	if stem == "" {
		return false
	}
	for _, r := range stem {
		if unicode.IsLower(r) {
			return false
		}
	}
	return true
}

func isKebab(filename string) bool {
	name := strings.TrimSuffix(filename, filepath.Ext(filename))
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// claimIndexOwnership takes the vault's owning-writer claim for a DECLARED owner: a
// command whose whole job is to keep the index fresh (`mesh watch`, `mesh sync --watch`,
// `mesh ui --own-index`). It fails when another declared owner already holds the vault,
// which is the case nothing checked at all before: two watchers against one mesh.db is
// exactly the contention the single-writer split removed, and it used to be started by
// hand routinely, because the only symptom is latency.
//
// An opportunistic `mesh mcp` claim is taken over rather than refused; that server drops
// back to reading the moment it notices, so the operator's explicit command wins.
func claimIndexOwnership(vaultDir, role string) (*index.OwnerLock, error) {
	return index.AcquireOwnerLock(filepath.Join(vaultDir, ".mesh"), role, false)
}

// reportOwner prints the vault's owning writer and reports whether one is missing, for
// the two commands whose job is to tell an operator whether this vault is working. A
// vault with no owner indexes nothing: notes written or edited go missing from search
// with no other signal, so it is always PRINTED, with the remedy.
//
// Whether it also FAILS is the caller's decision, and on its own neither caller does:
// a fresh `mesh init` vault has no owner running yet and is not broken, and the README
// puts `mesh doctor` in CI. Only `mesh doctor` escalates, and only when the index has
// also drifted, which is the case a missing owner guarantees nobody will clear.
func reportOwner(w io.Writer, vaultDir string) bool {
	info, live := index.OwnerStatus(filepath.Join(vaultDir, ".mesh"))
	if live {
		fmt.Fprintf(w, "owner:  %s\n", info.Describe())
		return true
	}
	fmt.Fprintf(w, "owner:  NONE\n  %s\n", index.NoOwnerRemedy(vaultDir))
	return false
}

// mcpOwnerRole names this server in the vault's owner lock, so a peer that loses the
// election (or a `mesh doctor` run) can say which process to go and look at.
func mcpOwnerRole(watching bool) string {
	if watching {
		return "mesh mcp --watch"
	}
	return "mesh mcp"
}

func mcpCmd() *cobra.Command {
	var vaultDir string
	var doWatch bool
	var httpAddr, httpToken string
	var debounce, reconcile, fullReconcile time.Duration
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the agent retrieval contract over MCP (JSON-RPC on stdio, or HTTP with --http)",
		Long: "Long-running MCP server a coding agent spawns to search, fetch, and write back to the vault. Default transport is stdio: {\"command\": \"mesh\", \"args\": [\"mcp\", \"--vault\", \"<path>\"]}. Use --http :PORT to serve over HTTP instead (POST /mcp) so any remote MCP client (Claude, Cursor, ChatGPT, ...) connects without a local install; a bearer --token is REQUIRED when binding beyond loopback. " +
			"The server elects itself the vault's owning writer when nothing else holds it, so write-back is queryable at once with no separate daemon; beside a running `mesh watch` / `mesh sync --watch` it reads instead and routes writes through that owner. Add --watch so notes changed in your editor are searchable in the same session.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := vault.RequireRoot(vaultDir); err != nil {
				return err
			}
			// Elect this process the owning writer when the vault has none. Without it the
			// shipped agent config (`mesh mcp --vault <path>`, and nothing else) is a
			// server that can never index: every mesh_append_note waits out the full owner
			// bound and the note stays unqueryable. See mcp.NewOwningServer.
			srv, err := mcp.NewOwningServer(vaultDir, mcpOwnerRole(doWatch))
			if err != nil {
				return err
			}
			defer srv.Close()
			if srv.OwnsIndex() {
				fmt.Fprintf(os.Stderr, "mesh mcp: index OWNED by this process (nothing else holds this vault)\n")
			} else {
				fmt.Fprintf(os.Stderr, "mesh mcp: index read-only; another mesh process owns this vault and indexes for it\n")
			}
			if httpAddr != "" {
				return serveMCPHTTP(srv, httpAddr, httpToken, doWatch, debounce, reconcile, fullReconcile)
			}
			if !doWatch {
				return srv.ServeStdio()
			}
			// Background watcher keeps the in-memory index fresh while the stdio
			// loop serves the agent. On stdin EOF (agent disconnect) ServeStdio
			// returns; we then stop the watcher and wait for it before Close.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				logf := func(format string, a ...any) {
					fmt.Fprintf(os.Stderr, "mesh watch: "+format+"\n", a...)
				}
				if err := srv.Watch(ctx, debounce, reconcile, fullReconcile, logf); err != nil {
					fmt.Fprintf(os.Stderr, "mesh watch: %v\n", err)
				}
			}()
			serveErr := srv.ServeStdio()
			cancel()
			<-done
			return serveErr
		},
	}
	c.Flags().StringVar(&vaultDir, "vault", ".", "vault root")
	c.Flags().BoolVar(&doWatch, "watch", false, "live-reindex the vault in the background so editor changes are searchable without a restart (needs this server to be the owning writer, which it elects itself to be unless another one is running)")
	c.Flags().StringVar(&httpAddr, "http", "", "serve MCP over HTTP at host:port (POST /mcp) instead of stdio")
	c.Flags().StringVar(&httpToken, "token", "", "bearer token for --http (or MESH_MCP_TOKEN); REQUIRED when binding beyond loopback")
	c.Flags().DurationVar(&debounce, "debounce", 300*time.Millisecond, "quiet window to coalesce a burst of saves")
	c.Flags().DurationVar(&reconcile, "reconcile", defaultLocalReconcile, "periodic reconcile safety net (0 to disable); keep it under the write-back bound so a note that missed its file event is still indexed in time")
	c.Flags().DurationVar(&fullReconcile, "full-reconcile", watch.DefaultFullReconcile, "how often the safety net escalates to the authoritative content-hash pass over every note")
	return c
}

// serveMCPHTTP serves the MCP server over HTTP (POST /mcp). Fail-closed: a
// non-loopback bind REQUIRES a bearer token. Optionally runs the background watcher.
func serveMCPHTTP(srv *mcp.Server, addr, token string, doWatch bool, debounce, reconcile, fullReconcile time.Duration) error {
	if token == "" {
		token = os.Getenv("MESH_MCP_TOKEN")
	}
	if !netaddr.IsLoopback(addr) && token == "" {
		return fmt.Errorf("refusing to bind %s without a token: set --token or MESH_MCP_TOKEN (fail-closed)", addr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if doWatch {
		stopWatch := startMCPBackgroundWatch(func(ctx context.Context) {
			logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "mesh watch: "+format+"\n", a...) }
			if err := srv.Watch(ctx, debounce, reconcile, fullReconcile, logf); err != nil {
				fmt.Fprintf(os.Stderr, "mesh watch: %v\n", err)
			}
		})
		// mcpCmd closes srv as soon as this function returns. Join the watcher first:
		// its reconcile callback uses srv's store, cache and graph, so merely cancelling
		// it leaves a close/use race whenever shutdown lands during a reconcile.
		defer stopWatch()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		srv.HandleHTTP(w, r)
	})
	// Full timeouts (mirroring the hub server) so a slow/idle client cannot pin a
	// connection: ReadHeaderTimeout alone leaves a byte-by-byte body unreaped and
	// keep-alive connections accumulating.
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "mesh mcp: serving HTTP at %s/mcp (auth: %v)\n", listener.Addr(), token != "")
	return serveMCPHTTPListener(ctx, httpSrv, listener, mcpHTTPDrainGrace)
}

// Leave room for an admitted write's normal owner acknowledgement bound. This
// bounds the graceful phase, not uninterruptible I/O or the subsequent joins.
const mcpHTTPDrainGrace = 20 * time.Second

// serveMCPHTTPListener owns listener and the HTTP server's request lifecycle.
// The caller must stop/join its watcher and only then close the MCP store AFTER
// this returns. MCP has no hijacked connections or detached HTTP handlers.
func serveMCPHTTPListener(ctx context.Context, srv *http.Server, listener net.Listener, grace time.Duration) error {
	// A termination signal stops admission, but must not cancel a write that can
	// still finish normally. Only expiry of the graceful phase cancels requests.
	requestCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	srv.BaseContext = func(net.Listener) context.Context { return requestCtx }
	drain := &mcpHTTPRequestDrain{}
	srv.Handler = drain.wrap(srv.Handler)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()

	var serveErr error
	var shutdownErr error
	select {
	case serveErr = <-served:
		// A fatal accept error can leave already-admitted handlers running.
		drain.stop()
		cancelRequests()
		shutdownErr = srv.Close()
	case <-ctx.Done():
		drain.stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		shutdownErr = srv.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			// Close disconnects clients, but does NOT wait for their handlers.
			cancelRequests()
			shutdownErr = errors.Join(fmt.Errorf("mesh mcp HTTP drain: %w", shutdownErr), srv.Close())
		}
		// Serve returns ErrServerClosed as soon as Shutdown starts, not when
		// it finishes. Wait here, never close the MCP store on that early return.
		serveErr = <-served
	}
	drain.active.Wait()
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr)
}

// Serialize admission against stopping: no WaitGroup.Add may race a Wait after
// the count reaches zero. Late requests on accepted connections never reach MCP.
type mcpHTTPRequestDrain struct {
	mu       sync.Mutex
	stopping bool
	active   sync.WaitGroup
}

func (d *mcpHTTPRequestDrain) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		if d.stopping {
			d.mu.Unlock()
			w.Header().Set("Connection", "close")
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		d.active.Add(1)
		d.mu.Unlock()
		defer d.active.Done()
		next.ServeHTTP(w, r)
	})
}

func (d *mcpHTTPRequestDrain) stop() {
	d.mu.Lock()
	d.stopping = true
	d.mu.Unlock()
}

// startMCPBackgroundWatch starts one watcher and returns a stop function that cancels
// AND joins it. Joining is part of the ownership contract: callers may close the MCP
// server only after stop returns.
func startMCPBackgroundWatch(run func(context.Context)) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// defaultLocalReconcile is how often an owning writer re-checks the vault when no file
// event told it to. It is the safety net under fsnotify, and it MUST stay under
// mcp.OwnerIndexBound: a note that misses its event (the moment right after the owner
// starts, before its watches are registered, or a burst caught mid-reconcile) becomes
// queryable only on this tick, and every reader waiting on that note gives up at the
// bound. It sat at 30s against a 10s bound, so that whole 20s window reported a durable,
// perfectly fine note as owner_down.
//
// The tick used to be an authoritative content-hash pass over the whole vault, which made
// the bound honest but cost 5.6% of a core on an idle 1216-note vault, times one daemon
// per open agent session. It is now the mtime pass (one stat per note), which still sees
// every added, removed and normally-edited file, so the bound holds for the cases that
// motivated it; the content-hash pass moved to watch.DefaultFullReconcile.
// TestLocalSweepStaysUnderTheWriteBackBound pins the relationship so it cannot drift apart
// again.
const defaultLocalReconcile = 8 * time.Second

// defaultHubSync is how often `mesh sync --watch` talks to the hub when nothing local
// changed. Unlike the local reconcile this is a network round trip, and it answers a
// different question (has a teammate pushed?), so it stays on its own slower cadence
// instead of following the sweep down.
const defaultHubSync = 60 * time.Second

// hubDue decides whether this reconcile pass also does a hub round. Startup and any real
// change (a local edit, an SSE nudge) always sync: those are the passes with something to
// push. The bare periodic tick is rate-limited to interval, because a hub round is a
// network trip plus a re-hash of every note to compute the outbox, and SSE already
// delivers teammates' changes in real time.
//
// It is a named function with a test because it used to be an inline condition keyed off
// "authoritative", which silently meant "is this the periodic tick" until the tick stopped
// being authoritative on every fire. That kind of coupling is invisible in a closure.
func hubDue(reason string, last time.Time, interval time.Duration, now time.Time) bool {
	if reason == watch.ReasonRefresh {
		return false // a completed hub round is not a request for another one
	}
	if reason != watch.ReasonTick {
		return true
	}
	return last.IsZero() || now.Sub(last) >= interval
}

func watchCmd() *cobra.Command {
	var debounce, reconcile, fullReconcile time.Duration
	c := &cobra.Command{
		Use:   "watch [vault]",
		Short: "Watch the vault and live-reindex on every change (local-first immediacy)",
		Long:  "Long-running reindexer: edit a note in your editor and it is searchable at once, no commit, no manual mesh index. A reconcile runs at startup, on every change (debounced), and on a periodic safety tick that always converges. Keeps .mesh/mesh.db fresh for mesh search and any reader; for a live MCP session, run mesh mcp --watch instead so the server hot-reloads its own in-memory index.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			if err := vault.RequireRoot(root); err != nil {
				return err
			}
			// Claim the vault before opening a writable store. This is the declared owner,
			// so it takes the claim from an opportunistic `mesh mcp` (which drops back to
			// reading) and refuses to start beside another declared one.
			owner, err := claimIndexOwnership(root, "mesh watch")
			if err != nil {
				return err
			}
			defer owner.Release()
			if err := owner.MarkStarting(); err != nil {
				return err
			}
			store, err := index.OpenOwned(root, owner)
			if err != nil {
				return err
			}
			defer store.Close()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			abs, _ := filepath.Abs(root)
			n, _ := store.Count("notes")
			fmt.Printf("watching %s (%d notes indexed); edits reindex live. Ctrl-C to stop.\n", abs, n)
			logf := func(format string, a ...any) {
				fmt.Printf("%s  "+format+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
			}
			// Incremental: the first reconcile seeds the cache (full), later ones parse
			// only changed files and rebuild the graph in memory.
			live := index.NewLiveIndexer(store, root)
			startupReady := false
			err = watch.Run(ctx, watch.Options{
				Root:          root,
				Debounce:      debounce,
				Reconcile:     reconcile,
				FullReconcile: fullReconcile,
				Logf:          logf,
				OnReindex: func(p watch.Pass) (watch.Result, error) {
					var rec index.Reconciliation
					var err error
					if len(p.Paths) > 0 {
						rec, err = live.ReconcilePaths(p.Paths)
					} else {
						rec, err = live.Reconcile(p.Authoritative)
					}
					if err != nil {
						return watch.Result{}, err
					}
					if p.Reason == watch.ReasonStartup && !startupReady {
						if err := owner.MarkReady(); err != nil {
							return watch.Result{}, err
						}
						startupReady = true
					}
					return watch.Result{
						Added:     rec.Added,
						Changed:   rec.Changed,
						Removed:   rec.Removed,
						Reindexed: rec.Reindexed,
						Dur:       rec.Dur,
					}, nil
				},
			})
			fmt.Println("stopped.")
			return err
		},
	}
	c.Flags().DurationVar(&debounce, "debounce", 300*time.Millisecond, "quiet window to coalesce a burst of saves")
	c.Flags().DurationVar(&reconcile, "reconcile", defaultLocalReconcile, "periodic reconcile safety net (0 to disable); keep it under the write-back bound so a note that missed its file event is still indexed in time")
	c.Flags().DurationVar(&fullReconcile, "full-reconcile", watch.DefaultFullReconcile, "how often the safety net escalates to the authoritative content-hash pass over every note")
	return c
}
func joinCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "join <hub-url> <invite-token> [vault]",
		Short: "Join a team vault: redeem an invite and clone it, no git needed",
		Long:  "Redeem a one-time invite from a mesh-hub, store the client token under <vault>/.mesh, fail closed if the team embedding config conflicts with yours, then clone the vault via a reconcile. After this, edit locally and run mesh sync.",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			hubURL, invite := args[0], args[1]
			vaultDir := "."
			if len(args) == 3 {
				vaultDir = args[2]
			}
			sum, err := meshclient.JoinVault(hubURL, invite, vaultDir)
			if err != nil {
				return err
			}
			return finishJoin(cmd.Context(), vaultDir, sum, reconcileOneShotThroughOwner)
		},
	}
	return c
}

type joinIndexReconcile func(context.Context, string, string) error

func finishJoin(ctx context.Context, vaultDir string, sum meshclient.Summary, reconcile joinIndexReconcile) error {
	abs, _ := filepath.Abs(vaultDir)
	fmt.Printf("joined and cloned %s\n", abs)
	// JoinVault already redeemed the one-time invite and completed its sync round.
	// Print that durable receipt before asking about index liveness so a stale owner
	// cannot make a completed, non-repeatable join look safe to retry.
	for _, line := range syncSummaryLines(sum) {
		fmt.Println(line)
	}
	if err := reconcile(ctx, vaultDir, "mesh join"); err != nil {
		fmt.Printf("index stale: the invite was redeemed and the join/sync file writes are complete, but the owning writer did not make them queryable: %v\n", err)
		return err
	}
	fmt.Println("next:")
	fmt.Println("  mesh sync " + shellpath.Quote(vaultDir) + "                       # push your edits, pull teammates'")
	fmt.Printf("  mesh mcp --vault %s --watch       # point your agent at the vault\n", shellpath.Quote(vaultDir))
	return nil
}

func syncCmd() *cobra.Command {
	var doWatch bool
	var debounce, reconcile, fullReconcile, hubInterval time.Duration
	c := &cobra.Command{
		Use:   "sync [vault]",
		Short: "Reconcile the vault with the hub (push local edits, pull teammates', no git)",
		Long:  "One pull-based reconcile round: pushes your changed notes, applies the hub's merged result and any teammates' changes, and reindexes. Additive edits to a shared page auto-merge; a true overwrite keeps the hub version and saves yours to a *.sync-conflict sibling. Add --watch to stay running: local edits push and the hub's changes pull in real time (SSE), with a periodic reconcile as the safety net.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vaultDir := vaultArg(args)
			if err := vault.RequireRoot(vaultDir); err != nil {
				return err
			}
			if !doWatch {
				// Keep the durable sync receipt separate from index liveness. SyncVault may
				// have pushed/pulled successfully even when a live owner then fails to absorb
				// the resulting files; print that completed round before returning a loud
				// index error, so a retry is never mistaken for an unperformed network write.
				sum, serr := meshclient.SyncVault(vaultDir)
				rerr := reconcileOneShotThroughOwner(cmd.Context(), vaultDir, "mesh sync")
				if serr == nil {
					for _, line := range syncSummaryLines(sum) {
						fmt.Println(line)
					}
				}
				if rerr != nil {
					fmt.Printf("index stale: the sync round and its file writes are complete, but the owning writer did not make them queryable: %v\n", rerr)
				}
				return errors.Join(serr, rerr)
			}

			// The long-lived half of this command is an owning writer, so it claims the
			// vault like `mesh watch` does.
			owner, err := claimIndexOwnership(vaultDir, "mesh sync --watch")
			if err != nil {
				return err
			}
			defer owner.Release()
			if err := owner.MarkStarting(); err != nil {
				return err
			}
			store, err := index.OpenOwned(vaultDir, owner)
			if err != nil {
				return err
			}
			defer store.Close()

			// The index callback is the sole SQLite writer. Hub sync runs separately
			// so writes arriving during a network round stay locally queryable.
			live := index.NewLiveIndexer(store, vaultDir)
			startupReady := false
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			abs, _ := filepath.Abs(vaultDir)
			fmt.Printf("syncing %s continuously: local edits push, hub changes pull. Ctrl-C to stop.\n", abs)
			logf := func(format string, a ...any) {
				fmt.Printf("%s  "+format+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
			}
			meshclient.Logf = logf // surface non-fatal stream diagnostics (e.g. auth rejections)
			nudge := make(chan struct{}, 1)
			go func() {
				if err := meshclient.StreamEvents(ctx, vaultDir, nudge); err != nil {
					logf("event stream unavailable, falling back to periodic sync: %v", err)
				}
			}()
			err = runSyncWatch(ctx, watch.Options{
				Root: vaultDir, Debounce: debounce, Reconcile: reconcile,
				FullReconcile: fullReconcile, Trigger: nudge, Logf: logf,
				OnReindex: func(p watch.Pass) (watch.Result, error) {
					var rec index.Reconciliation
					var err error
					if len(p.Paths) > 0 {
						rec, err = live.ReconcilePaths(p.Paths)
					} else {
						rec, err = live.Reconcile(p.Authoritative)
					}
					if err == nil && p.Reason == watch.ReasonStartup && !startupReady {
						if err = owner.MarkReady(); err == nil {
							startupReady = true
						}
					}
					return watch.Result{Added: rec.Added, Changed: rec.Changed, Removed: rec.Removed,
						Reindexed: rec.Reindexed, Dur: rec.Dur}, err
				},
			}, hubInterval, func() (meshclient.Summary, error) { return meshclient.SyncVault(vaultDir) })
			fmt.Println("stopped.")
			return err
		},
	}
	c.Flags().BoolVar(&doWatch, "watch", false, "stay running: push local edits and pull hub changes in real time (SSE) plus a periodic safety reconcile")
	c.Flags().DurationVar(&debounce, "debounce", 500*time.Millisecond, "quiet window to coalesce a burst of local saves before syncing")
	c.Flags().DurationVar(&reconcile, "reconcile", defaultLocalReconcile, "periodic LOCAL reconcile interval (0 to disable); keep it under the write-back bound so a note that missed its file event is still indexed in time")
	c.Flags().DurationVar(&fullReconcile, "full-reconcile", watch.DefaultFullReconcile, "how often the safety net escalates to the authoritative content-hash pass over every note")
	c.Flags().DurationVar(&hubInterval, "hub-interval", defaultHubSync, "how often the periodic tick also syncs with the hub (local edits and SSE nudges always sync immediately)")
	return c
}

// syncSummaryLines renders one sync round for the operator: a headline, then one
// indented line per thing that needs a decision. Shared by `mesh join`, the one-shot
// `mesh sync`, and its --watch loop, which had drifted into separate wordings of the
// same four lines.
//
// The renderer itself lives in pkg/meshclient next to Summary, because cmd/mesh is not
// the only binary that runs a round: cmd/mesh-curator joins a hub too, could not reach
// a renderer that lived here, and grew its own two-field receipt as a result. These
// wrappers stay so the CLI keeps one local name for the thing it prints.
func syncSummaryLines(sum meshclient.Summary) []string {
	return sum.Lines()
}

// syncHeadlineLines renders the part of a sync round that is true wherever the round
// was triggered from: what moved, and whether the push was complete.
//
// It is split out because `mesh conflicts resolve --take-mine` owns its own receipt
// wording for the note it just rescued, but still has to tell the truth about the
// round that carried it. That site had its own hand-rolled copy of the headline
// format and therefore never grew the remainder line, so resolving a conflict on a
// vault with a deferred backlog printed a headline that read complete.
// TestEveryCLISyncRoundRendersThroughTheSharedRenderer discovers the callers from the
// AST, so a new one that hand-rolls a receipt fails the build instead of shipping.
func syncHeadlineLines(sum meshclient.Summary) []string {
	return sum.HeadlineLines()
}

// syncSummaryMoved reports whether a round is worth a log line under --watch, which
// idles at a periodic reconcile and would otherwise print a no-op every tick.
// Remaining counts: a round that pushed a full batch and deferred the rest has moved,
// and the deferred tail is exactly what the operator needs to be told about.
func syncSummaryMoved(sum meshclient.Summary) bool {
	return sum.Moved()
}

func short8(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	if s == "" {
		return "(none)"
	}
	return s
}

func serveSSHCmd() *cobra.Command {
	var addr, hostKey, authKeys string
	var allowAnon bool
	c := &cobra.Command{
		Use:   "serve-ssh [vault]",
		Short: "Serve the TUI over SSH (ssh into your knowledge graph, no local install)",
		Long: "Run an SSH server that hands every connection the Mesh TUI over the same index the agent uses, " +
			"so a teammate browses the graph with `ssh -p <port> <host>` and no Mesh install. Read-only. " +
			"Auth is fail-closed: pass --authorized-keys (OpenSSH format) and only those keys may connect; " +
			"--allow-anonymous opts out, and is refused on any --addr that is not loopback, because an " +
			"unauthenticated bind beyond loopback hands the whole vault to anyone who can reach the host. " +
			"The default bind is 127.0.0.1:2222, so serving teammates means both --authorized-keys and an " +
			"explicit --addr.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vaultDir := vaultArg(args)
			if hostKey == "" {
				hostKey = filepath.Join(vaultDir, ".mesh", "ssh_host_ed25519_key")
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			logf := func(format string, a ...any) {
				fmt.Printf("%s  "+format+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
			}
			err := sshserve.Serve(ctx, vaultDir, sshserve.Options{
				Addr: addr, HostKeyPath: hostKey, AuthKeysPath: authKeys, AllowAnon: allowAnon, Logf: logf,
			})
			fmt.Println("stopped.")
			return err
		},
	}
	// Loopback by default, like `mesh ui`. The old ":2222" published the vault to
	// every interface, which made --allow-anonymous a LAN-wide open door rather than
	// the "localhost demo" its own help promised.
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:2222", "listen address; binding beyond loopback requires --authorized-keys")
	c.Flags().StringVar(&hostKey, "host-key", "", "host key path (default <vault>/.mesh/ssh_host_ed25519_key; generated if missing)")
	c.Flags().StringVar(&authKeys, "authorized-keys", "", "OpenSSH authorized_keys file; only these public keys may connect (required unless --allow-anonymous)")
	c.Flags().BoolVar(&allowAnon, "allow-anonymous", false, "DANGER: serve with no auth; refused unless --addr is loopback (localhost demos only)")
	return c
}

func tuiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui [vault]",
		Short: "Open the keyboard-driven terminal view of the vault",
		Long:  "A three-pane terminal UI over the same index + graph the agent uses: browse the notes list (hub-first), search (the same ranked cards as the agent), and preview a note with its frontmatter and neighbors. Keyboard-driven; press ? for help.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run(vaultArg(args))
		},
	}
}
func uiCmd() *cobra.Command {
	var addr, token, basePath, hubDB string
	var ownIndex bool
	c := &cobra.Command{
		Use:   "ui [vault]",
		Short: "Serve the local web app: graph, search, settings, docs, API reference",
		Long:  "Open the Mesh web app for a vault: the graph (force + galaxy + 3D), a search view, editable settings, in-app docs, and the API reference. Same index the agent reads over MCP, served from the single binary with no CDN. Loopback bind needs no auth; binding beyond loopback requires --token (or MESH_UI_TOKEN), fail-closed. Use --base-path to serve under a path (e.g. behind a reverse proxy at /app). Pass --hub-db <hub.db> to serve a TEAM: each member signs in with their own client token and the graph/search/note views are scoped to them.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" {
				token = os.Getenv("MESH_UI_TOKEN")
			}
			if basePath == "" {
				basePath = os.Getenv("MESH_UI_BASE_PATH")
			}
			if hubDB == "" {
				hubDB = os.Getenv("MESH_UI_HUB_DB")
			}
			if !cmd.Flags().Changed("own-index") {
				ownIndex = os.Getenv("MESH_UI_OWN_INDEX") == "1"
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// Standalone single-token mode.
			if hubDB == "" {
				return web.ServeContext(ctx, vaultArg(args), addr, token, basePath, ownIndex, nil, nil, nil, nil)
			}
			// Per-member team mode: resolve each request against the hub's client
			// store and scope reads to the signed-in member. The hub store is the
			// commercial layer, so this is wired only in the pro build (openHubTeam);
			// the open core returns a clear "needs the pro build" error here. Break-glass
			// (the shared MESH_UI_TOKEN as an unrestricted admin login) is preserved by
			// the pro impl so flipping a live app to member mode never locks anyone out.
			verify, scopesFor, pathsFor, roleFor, browser, closeHub, err := openHubTeam(hubDB, token)
			if err != nil {
				return err
			}
			defer closeHub()
			return web.ServeContext(ctx, vaultArg(args), addr, token, basePath, ownIndex, verify, scopesFor, pathsFor, roleFor, browser)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:7474", "host:port to bind the local viewer")
	c.Flags().StringVar(&token, "token", "", "bearer token required for /api access (or MESH_UI_TOKEN); mandatory when binding beyond loopback")
	c.Flags().StringVar(&basePath, "base-path", "", "serve the app under a path, e.g. /app (or MESH_UI_BASE_PATH), for a reverse proxy")
	c.Flags().StringVar(&hubDB, "hub-db", "", "hub.db path (or MESH_UI_HUB_DB): serve a team with per-member login + scoped views")
	// Off by default, because the common case is a laptop where `mesh sync --watch`
	// already owns the index and this viewer is one of several readers of it. Turn it on
	// ONLY where nothing else writes that index (the mesh-ui container), or you have two
	// long-lived writers against one mesh.db again.
	c.Flags().BoolVar(&ownIndex, "own-index", false, "own the vault's index: reindex at startup and write directly (or MESH_UI_OWN_INDEX=1). Only where no `mesh watch` / `mesh sync --watch` runs against this vault")
	return c
}
