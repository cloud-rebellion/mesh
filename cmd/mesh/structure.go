// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/relate"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/shellpath"
	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/spf13/cobra"
)

// structureCmd grades how well a vault is ORGANIZED (canonical types, note-to-note
// connectivity, tier-0 capture, a map per cluster) against the standard in
// the vault structure standard. It complements `mesh lint` (frontmatter validity) and
// `mesh health` (knowledge lifecycle): validity, organization, lifecycle.
func structureCmd() *cobra.Command {
	var verbose, wireOrphans, allowUncorroboratedTop, apply, fillBodies, fillTimelines bool
	var repoPath string
	c := &cobra.Command{
		Use:   "structure [vault]",
		Short: "Grade the vault's organization: types, connectivity, tier-0, maps",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			files, err := vault.Walk(root)
			if err != nil {
				return err
			}
			parsed, parseErrs := index.ParseFiles(files, 0)
			for _, pn := range parsed {
				if rel, err := filepath.Rel(root, pn.Path); err == nil {
					pn.Path = rel
				}
			}
			for i := range parseErrs {
				if rel, err := filepath.Rel(root, parseErrs[i].Path); err == nil {
					parseErrs[i].Path = rel
				}
			}
			g, _ := index.BuildGraph(parsed)
			g.DetectCommunities(0)
			rep := index.AnalyzeStructure(g, parsed, parseErrs)

			if wireOrphans {
				return wireOrphanNotes(cmd, root, rep, apply, allowUncorroboratedTop)
			}
			if fillBodies {
				return fillNoteBodies(root, files, apply)
			}
			if fillTimelines {
				return fillNoteTimelines(root, files, repoPath, apply)
			}

			fmt.Printf("structure: grade %s  (%d/100)\n", rep.Grade, rep.Score)
			fmt.Printf("  %d notes, %d clusters, %d tier-0 (decisions/gotchas/post-mortems)\n", rep.Notes, rep.Clusters, rep.Tier0)
			if rep.Unparseable > 0 {
				fmt.Printf("  !! %d note(s) fail to parse and are INVISIBLE to search/the graph - fix these first (see --verbose)\n", rep.Unparseable)
			}

			fmt.Print("  types:  ")
			for _, kv := range sortedCounts(rep.ByType) {
				label := kv.k
				if label == "" {
					label = "(none)"
				}
				fmt.Printf("%s %d  ", label, kv.v)
			}
			fmt.Println()

			if len(rep.Findings) == 0 {
				fmt.Println("status: well-organized")
				return nil
			}
			counts := map[string]int{}
			for _, f := range rep.Findings {
				counts[f.Kind]++
			}
			fmt.Print("  fix:    ")
			for _, kv := range sortedCounts(counts) {
				fmt.Printf("%s %d  ", kv.k, kv.v)
			}
			fmt.Println()

			if verbose {
				for _, f := range rep.Findings {
					where := f.Path
					if where == "" {
						where = "(cluster)"
					}
					fmt.Printf("    [%s] %-16s %s\n              %s\n", f.Severity, f.Kind, where, f.Detail)
				}
				for _, ci := range rep.MaplessClusters {
					fmt.Printf("\n  cluster #%d needs a map - %d notes, most-connected first:\n", ci.ID, ci.Size)
					for i, m := range ci.Members {
						if i >= 16 {
							fmt.Printf("    ... and %d more\n", ci.Size-16)
							break
						}
						key := filepath.Base(m.Path)
						key = strings.TrimSuffix(key, filepath.Ext(key))
						fmt.Printf("    %-9s [[%s]]  %s\n", m.Type, key, m.Title)
					}
				}
			} else {
				fmt.Println("  run `mesh structure --verbose` for the per-note list")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&verbose, "verbose", false, "list every finding with its note path")
	c.Flags().BoolVar(&wireOrphans, "wire-orphans", false, "propose `related:` links for every orphan note (dry run unless --apply)")
	c.Flags().BoolVar(&allowUncorroboratedTop, "allow-uncorroborated-top", false, "with --wire-orphans, accept the top retrieval hit without a shared tag (unsafe for bulk apply)")
	c.Flags().BoolVar(&fillBodies, "fill-bodies", false, "retired: use reviewed mesh templates migration-preview instead")
	c.Flags().BoolVar(&fillTimelines, "fill-timelines", false, "build a post-mortem's \"What happened\" from its own dates and the commit ids it names (dry run unless --apply)")
	c.Flags().StringVar(&repoPath, "repo", "", "with --fill-timelines, a git repository to resolve commit ids against, so the timeline carries real committer times and subjects")
	c.Flags().BoolVar(&apply, "apply", false, "with --wire-orphans or --fill-bodies, write the changes into the notes")
	return c
}

// fillNoteTimelines builds the "What happened" section of every post-mortem still holding
// its placeholder, from the note's own recorded date, the commit ids it names, and the
// dates in its prose (see vault.BackfillTimelineFile). Dry run unless --apply, like its
// two siblings, because it rewrites the author's files.
//
// Without --repo it still runs, on dates alone. With one, each hex-looking word in the
// note is resolved against that repository and only real commits survive, which is both
// where the times and subjects come from and how the many hex-looking words in prose get
// filtered out.
func fillNoteTimelines(root string, files []string, repoPath string, apply bool) error {
	if !apply {
		fmt.Println("dry run (pass --apply to write); scanning post-mortems for an unfilled \"What happened\"")
	}
	var resolve vault.CommitResolver
	if repoPath != "" {
		r, err := gitCommitResolver(repoPath)
		if err != nil {
			return err
		}
		resolve = r
	} else {
		fmt.Println("no --repo given: timelines will carry dates only, no commit times or subjects")
	}

	var changed, failed int
	for _, path := range files {
		rel := path
		if r, err := filepath.Rel(root, path); err == nil {
			rel = r
		}
		res, err := vault.BackfillTimelineFile(path, resolve, !apply)
		if err != nil {
			fmt.Printf("  !! %s: %v\n", rel, err)
			failed++
			continue
		}
		if !res.Changed {
			continue
		}
		changed++
		fmt.Printf("  %s\n", rel)
	}
	verb := "would build"
	if apply {
		verb = "built"
	}
	fmt.Printf("\n%s %d timeline(s)", verb, changed)
	if failed > 0 {
		fmt.Printf(", failed on %d", failed)
	}
	fmt.Println()
	if apply && changed > 0 {
		fmt.Printf("run `mesh index %s` to pick the new bodies up\n", shellpath.Quote(root))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d note(s) failed", failed, len(files))
	}
	return nil
}

// gitCommitResolver resolves commit ids against a real repository, memoising both hits and
// misses: a vault's post-mortems name the same commits repeatedly, and the misses (every
// hex-looking word in prose) outnumber the hits several times over.
//
// It verifies the path is a repository up front rather than letting every lookup fail
// quietly, because a resolver that never resolves anything produces timelines with no
// commits and looks exactly like a vault whose notes name none.
func gitCommitResolver(repoPath string) (vault.CommitResolver, error) {
	if out, err := exec.Command("git", "-C", repoPath, "rev-parse", "--git-dir").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("--repo %s is not a git repository: %s", repoPath, strings.TrimSpace(string(out)))
	}
	cache := map[string]*vault.Commit{}
	return func(id string) (vault.Commit, bool) {
		if c, seen := cache[id]; seen {
			if c == nil {
				return vault.Commit{}, false
			}
			return *c, true
		}
		out, err := exec.Command("git", "-C", repoPath, "show", "-s", "--format=%cI%x09%s", id+"^{commit}").Output()
		if err != nil {
			cache[id] = nil
			return vault.Commit{}, false
		}
		parts := strings.SplitN(strings.TrimRight(string(out), "\n"), "\t", 2)
		if len(parts) != 2 {
			cache[id] = nil
			return vault.Commit{}, false
		}
		when, perr := time.Parse(time.RFC3339, parts[0])
		if perr != nil {
			cache[id] = nil
			return vault.Commit{}, false
		}
		c := vault.Commit{ID: id, When: when, Subject: parts[1]}
		cache[id] = &c
		return c, true
	}, nil
}

func fillNoteBodies(root string, files []string, apply bool) error {
	return fmt.Errorf("automatic body backfill is retired; use mesh templates migration-preview and review the reconstructed content")
}

// wireOrphanNotes gives every orphan note a `related:` list derived from Mesh's own
// retrieval, so notes stop arriving disconnected and staying that way. It is a dry run
// unless --apply: this rewrites the author's files, so the default has to be "show me".
//
// Notes that already declare `related` are skipped by BackfillRelatedFile, so running
// this twice is safe and the second run reports nothing to do.
func wireOrphanNotes(cmd *cobra.Command, root string, rep index.StructureReport, apply, allowUncorroboratedTop bool) error {
	var orphans []index.StructureFinding
	for _, f := range rep.Findings {
		if f.Kind == "orphan" {
			orphans = append(orphans, f)
		}
	}
	if len(orphans) == 0 {
		fmt.Println("no orphans: every note links to at least one other note")
		return nil
	}

	// Read-only: wire-orphans writes LINKS INTO THE MARKDOWN (BackfillRelatedFile), never
	// into the index, which is why it tells you to run `mesh index` afterwards to pick the
	// new links up. A writable open took the write lock for a pure read, and worse, it
	// CREATED the index when absent, so the "needs a built index" message below could
	// never fire: the command instead ran against an empty graph and reported that no
	// orphan had any candidate link.
	store, err := index.OpenReadOnly(root)
	if err != nil {
		return fmt.Errorf("wire-orphans needs a built index (run: mesh index %s): %w", shellpath.Quote(root), err)
	}
	defer store.Close()
	ig, err := store.LoadGraph()
	if err != nil {
		return err
	}
	rt := retrieve.NewFromEnv(store, ig)

	if !apply {
		fmt.Printf("%d orphan(s); proposing up to 3 corroborated links each (dry run, pass --apply to write)\n", len(orphans))
		if allowUncorroboratedTop {
			fmt.Println("warning: rank 1 may be accepted without a shared tag")
		}
		fmt.Println()
	}
	// One counter used to absorb three unrelated outcomes: "retrieval found no
	// candidate links" (benign, the note simply has no neighbours yet), "the note
	// already declares related" (benign, and the whole point of the idempotence), and
	// "the write failed" (an error). Summing them made the number meaningless, and
	// because the third was buried in it the function could fail on every note, print
	// each failure, and still return nil. Splitting the counters is what makes a
	// non-zero exit possible at all, so it has to come first.
	var changed, noLinks, alreadyRelated, failed int
	for _, f := range orphans {
		path := filepath.Join(root, f.Path)
		id := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
		// The slug IS the title, hyphenated: Slugify built it from the title when the
		// note was scaffolded, so it is the closest thing to the note's own words that
		// is available here without re-reading the file.
		query := strings.ReplaceAll(id, "-", " ")
		var links []string
		if allowUncorroboratedTop {
			links = relate.Derive(cmd.Context(), rt, ig, query, id, relate.TagsOf(ig, id), 3)
		} else {
			links = relate.DeriveCorroborated(cmd.Context(), rt, ig, query, id, relate.TagsOf(ig, id), 3)
		}
		if len(links) == 0 {
			noLinks++
			continue
		}
		res, err := vault.BackfillRelatedFile(path, links, !apply)
		if err != nil {
			fmt.Printf("  !! %s: %v\n", f.Path, err)
			failed++
			continue
		}
		if !res.Changed {
			alreadyRelated++
			continue
		}
		changed++
		fmt.Printf("  %s\n", f.Path)
		for _, l := range links {
			fmt.Printf("      -> %s\n", l)
		}
	}
	verb := "would wire"
	if apply {
		verb = "wired"
	}
	fmt.Printf("\n%s %d note(s), skipped %d (%d with no corroborated candidate links, %d already declaring related)\n",
		verb, changed, noLinks+alreadyRelated, noLinks, alreadyRelated)
	if failed > 0 {
		fmt.Printf("failed on %d note(s)\n", failed)
	}
	if apply && changed > 0 {
		fmt.Printf("run `mesh index %s` to pick the new links up\n", shellpath.Quote(root))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d orphan(s) failed to wire", failed, len(orphans))
	}
	return nil
}
