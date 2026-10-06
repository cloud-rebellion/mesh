// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bright-interaction/mesh/internal/extract"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/llm"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/spf13/cobra"
)

// minDigestChars is the smallest --max-chars that still yields a digest with both a
// head (the task) and a tail (the conclusions). extract.Digest reserves up to ~1.5 KB
// for the head, so anything under this leaves no tail budget at all.
const minDigestChars = 2000

// extractCmd pulls candidate write-back notes from a finished session transcript (the
// input side of the flywheel), and --benchmark measures it against the current manual
// algo (the Stop-hook nudge): coverage (sessions that yield a note) and precision (how
// many extracted notes a strict judge would keep). BYOAI via MESH_CURATOR_* (claude -p).
func extractCmd() *cobra.Command {
	var benchmark bool
	var n, concurrency, maxChars, repeat, samples int
	var asJSON bool
	var toPending, dedupVault, judgeEval string
	var recurring bool
	c := &cobra.Command{
		Use:   "extract [transcript.jsonl]",
		Short: "Extract candidate write-back notes from a session transcript (--benchmark to grade vs manual write-back)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := llm.NewFromEnv()
			if err != nil {
				return err
			}
			// Digest keeps the task head (up to ~1.5 KB) plus a tail. Below that floor the
			// tail budget goes negative: it used to panic on the slice bounds, and after
			// that guard landed it silently degrades to a head-only digest, which is
			// useless model input. Refuse instead of quietly producing garbage.
			if maxChars > 0 && maxChars < minDigestChars {
				return fmt.Errorf("--max-chars %d is too small: use at least %d, or 0 for no limit", maxChars, minDigestChars)
			}
			// Self-consistency sample count resolves from --samples or MESH_EXTRACT_SAMPLES
			// (so the Stop hook can opt in via env without changing its command).
			if samples <= 1 {
				if v, e := strconv.Atoi(strings.TrimSpace(os.Getenv("MESH_EXTRACT_SAMPLES"))); e == nil && v > 1 {
					samples = v
				}
			}
			if judgeEval != "" {
				return runJudgeEval(cmd.Context(), client, judgeEval, concurrency, asJSON)
			}
			if benchmark {
				if len(args) < 1 {
					return fmt.Errorf("--benchmark needs a directory of transcripts")
				}
				return runExtractBenchmark(cmd.Context(), client, args[0], n, concurrency, maxChars, asJSON, dedupVault, repeat, samples)
			}
			if recurring {
				if len(args) < 1 {
					return fmt.Errorf("--recurring needs a directory of transcripts")
				}
				return runRecurring(cmd.Context(), client, args[0], n, concurrency, maxChars, asJSON)
			}
			if len(args) < 1 {
				return fmt.Errorf("usage: mesh extract <transcript.jsonl>  (or --benchmark <dir>)")
			}
			digest, st, err := extract.Digest(args[0], maxChars)
			if err != nil {
				return err
			}
			cands, err := extract.ExtractConsistent(cmd.Context(), client, digest, samples)
			if err != nil {
				return err
			}
			if toPending != "" {
				judges, _ := llm.NewJudgePanelFromEnv(client)
				return writeToPending(toPending, args[0], cands, judges)
			}
			b, _ := json.MarshalIndent(map[string]any{"stats": st, "candidates": cands}, "", "  ")
			fmt.Println(string(b))
			return nil
		},
	}
	c.Flags().BoolVar(&benchmark, "benchmark", false, "grade the extractor over a directory of transcripts vs manual write-back")
	c.Flags().IntVar(&n, "n", 15, "benchmark: max transcripts to sample")
	c.Flags().IntVar(&concurrency, "concurrency", 4, "benchmark: parallel LLM calls")
	c.Flags().IntVar(&maxChars, "max-chars", 48000, "max digest size fed to the model")
	c.Flags().BoolVar(&asJSON, "json", false, "benchmark: emit JSON instead of a report")
	c.Flags().StringVar(&toPending, "to-pending", "", "persist extracted candidates to this vault's review queue instead of printing")
	c.Flags().StringVar(&dedupVault, "dedup-vault", "", "benchmark: dedup candidates against this vault and report fresh vs already-known")
	c.Flags().StringVar(&judgeEval, "judge-eval", "", "grade the judge PANEL's discrimination over a labeled good/bad fixture (JSON): keeps good AND rejects bad")
	c.Flags().IntVar(&repeat, "repeat", 1, "benchmark: run K times and report coverage/precision mean +/- stddev + per-session stability (variance-aware)")
	c.Flags().IntVar(&samples, "samples", 1, "extraction self-consistency: union K extraction passes per session to lift coverage on marginal sessions (also MESH_EXTRACT_SAMPLES)")
	c.Flags().BoolVar(&recurring, "recurring", false, "find problems that RECUR across sessions (systemic issues), not one-offs")
	return c
}

// runRecurring extracts candidates from many transcripts and clusters them by
// similarity to surface SYSTEMIC issues: a learning that recurs across multiple
// sessions is worth a permanent fix, not just another write-back.
func runRecurring(ctx context.Context, client llm.Client, dir string, n, concurrency, maxChars int, asJSON bool) error {
	files, err := sampleTranscripts(dir, n)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no usable transcripts in %s", dir)
	}
	if concurrency < 1 {
		concurrency = 4
	}
	if !asJSON {
		fmt.Fprintf(os.Stderr, "extracting from %d transcripts via %s...\n", len(files), client.Describe())
	}
	var mu sync.Mutex
	var occs []extract.Occurrence
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, f := range files {
		wg.Add(1)
		go func(f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			digest, _, e := extract.Digest(f, maxChars)
			if e != nil {
				return
			}
			cands, e := extract.Extract(ctx, client, digest)
			if e != nil {
				return
			}
			sess := filepath.Base(f)
			mu.Lock()
			for _, c := range cands {
				occs = append(occs, extract.Occurrence{Cand: c, Session: sess})
			}
			mu.Unlock()
		}(f)
	}
	wg.Wait()

	clusters := extract.ClusterRecurring(occs, extract.DuplicateThreshold)
	var recurringClusters []extract.Cluster
	for _, c := range clusters {
		if c.Count >= 2 {
			recurringClusters = append(recurringClusters, c)
		}
	}
	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"sessions": len(files), "candidates": len(occs),
			"recurring": recurringClusters,
		}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("\nRecurring problems across %d sessions (%d candidates extracted):\n", len(files), len(occs))
	if len(recurringClusters) == 0 {
		fmt.Println("  none yet: no learning recurred across 2+ sessions in this sample.")
		return nil
	}
	for _, c := range recurringClusters {
		fmt.Printf("\n* [%s] %s\n  recurred in %d sessions; a permanent fix beats re-learning it:\n", c.Rep.Type, c.Rep.Title, c.Count)
		seen := map[string]bool{}
		for _, m := range c.Members {
			if seen[m.Title] {
				continue
			}
			seen[m.Title] = true
			fmt.Printf("    - %s\n", m.Title)
		}
	}
	return nil
}

// writeToPending opens the vault index and stores extracted candidates in the review
// queue (pending_notes), skipping any that restate a note already in the vault (so the
// queue surfaces only NEW knowledge). Idempotent. Used by the Stop hook's extraction.
//
// judges is the quality gate (nil/empty = no gate, for tests / when no LLM is configured):
// each fresh candidate is scored by a 3-lens judge panel and low-confidence self-ratings
// are dropped, so the queue a human sees is the JUDGED set, not every raw extraction.
// Without this the queue held every non-duplicate candidate (the ~64% measured
// precision), i.e. the benchmark judged but production did not. The panel fails OPEN
// (an LLM hiccup queues the candidate for the human, never silently drops knowledge).
func writeToPending(vaultRoot, transcript string, cands []extract.Candidate, judges []llm.Client) error {
	store, writable, closeStore, err := openOneShotCurrentOrReadOnly(vaultRoot, "mesh extract")
	if err != nil {
		return err
	}
	defer closeStore()
	rtr, closeRtr := buildVaultRetriever(store, vaultRoot) // best-effort dedup context
	defer closeRtr()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Review items already queued, so a candidate that restates one (from an earlier run
	// or another session) is suppressed. The exact-title PendingID is not enough: the LLM
	// rephrases the same learning differently each run, so two reruns hash to different
	// ids and would both pile up. Compare by title similarity, the same bar used against
	// the vault, so the queue cannot flood with reworded duplicates.
	queuedTitles, _ := store.ListPending()

	src := filepath.Base(transcript)
	queued, dupes, filtered := 0, 0, 0
	var queuedOps []string
	for _, cnd := range cands {
		if cnd.Template == "" {
			return fmt.Errorf("candidate requires modern template authoring")
		}
		spec, err := vault.NormalizeSpec(cnd.Spec())
		if err != nil {
			return fmt.Errorf("invalid candidate: %w", err)
		}
		cnd.Type = string(spec.Type)
		if known, of := knownInVault(ctx, rtr, cnd); known {
			dupes++
			fmt.Printf("skip (already known): %q ~ %q\n", cnd.Title, of)
			continue
		}
		if of, dup := nearDuplicatePending(cnd.Title, queuedTitles); dup {
			dupes++
			fmt.Printf("skip (already queued): %q ~ %q\n", cnd.Title, of)
			continue
		}
		// Quality gate: drop the extractor's own low-confidence ratings, then let the
		// judge veto weak notes, so the review queue is the judged set (not every raw
		// extraction). Fails open on a judge error (queue for the human, never drop).
		if extract.LowConfidence(cnd) {
			filtered++
			fmt.Printf("skip (low confidence): %q\n", cnd.Title)
			continue
		}
		if len(judges) > 0 {
			jctx, jcancel := context.WithTimeout(context.Background(), 4*time.Minute)
			v, jerr := extract.JudgePanel(jctx, judges, cnd, extract.PanelMajority)
			jcancel()
			if jerr != nil {
				fmt.Fprintf(os.Stderr, "judge panel unavailable, queueing for human review anyway: %v\n", jerr)
			} else if !v.Keep {
				filtered++
				fmt.Printf("skip (panel %d/%d keep): %q\n", v.KeepN, v.Total, cnd.Title)
				continue
			}
		}
		pending := index.PendingNote{
			Type: cnd.Type, Title: cnd.Title, Template: cnd.Template, TemplateVersion: cnd.TemplateVersion,
			Summary: cnd.Summary, Sections: cnd.Sections, Blocks: cnd.Blocks, Collections: cnd.Collections,
			Tags: cnd.Tags, Related: cnd.Related, Supersedes: cnd.Supersedes,
			Confidence: cnd.Confidence, Source: src,
		}
		if writable {
			if err := store.AddPending(pending); err != nil {
				return err
			}
		} else {
			name, err := store.EnqueueOp(index.Op{Kind: index.OpAddPending, Pending: &pending})
			if err != nil {
				return err
			}
			queuedOps = append(queuedOps, name)
			if afterPendingOpEnqueue != nil {
				afterPendingOpEnqueue()
			}
		}
		// Track it so two near-duplicate candidates in the SAME batch also collapse.
		queuedTitles = append(queuedTitles, index.PendingNote{Type: cnd.Type, Title: cnd.Title})
		queued++
	}
	if len(queuedOps) > 0 {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), index.OwnerIndexBound)
		defer waitCancel()
		for _, name := range queuedOps {
			if err := store.AwaitOpApplied(waitCtx, name, index.OwnerIndexBound); err != nil {
				return fmt.Errorf("extracted candidate is durably queued but the owning writer did not add it to review in time: %w", err)
			}
		}
	}
	fmt.Printf("queued %d candidate(s) for review in %s (%d duplicate, %d below quality bar)\n", queued, vaultRoot, dupes, filtered)
	return nil
}

// Deterministic test seam after a read-only extraction publishes its op file and before
// it waits for the owner. Production leaves it nil.
var afterPendingOpEnqueue func()

// nearDuplicatePending reports whether a candidate title restates a review item already
// in the queue, by the same title-similarity bar used to dedup against the vault. This
// stops the queue from filling with the LLM's slightly reworded reruns of one learning.
func nearDuplicatePending(title string, existing []index.PendingNote) (string, bool) {
	for _, p := range existing {
		if extract.TitleSimilarity(title, p.Title) >= extract.DuplicateThreshold {
			return p.Title, true
		}
	}
	return "", false
}

// buildVaultRetriever builds a retriever over the vault for dedup. Best-effort: any
// failure returns a nil retriever (dedup is then skipped, candidates all queue).
func buildVaultRetriever(store *index.Store, vaultRoot string) (*retrieve.Retriever, func()) {
	if !store.ReadOnly() {
		if _, err := index.Reconcile(store, vaultRoot); err != nil {
			fmt.Fprintf(os.Stderr, "dedup disabled (reindex %s: %v)\n", vaultRoot, err)
			return nil, func() {}
		}
	}
	if store.ReadOnly() {
		if err := store.AwaitOwnerCaughtUp(context.Background(), vaultRoot, index.OwnerIndexBound); err != nil {
			fmt.Fprintf(os.Stderr, "dedup may be stale (owner catch-up %s: %v)\n", vaultRoot, err)
		}
	}
	g, err := store.LoadGraph()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dedup disabled (reindex %s: %v)\n", vaultRoot, err)
		return nil, func() {}
	}
	return retrieve.NewFromEnv(store, g), func() {}
}

// knownInVault reports whether a candidate restates a note already in the vault, by
// retrieving its topical neighbors and comparing titles. Returns the matched title.
func knownInVault(ctx context.Context, rtr *retrieve.Retriever, c extract.Candidate) (bool, string) {
	if rtr == nil {
		return false, ""
	}
	cards, err := rtr.Retrieve(ctx, c.Title+" "+c.Summary, retrieve.Options{Limit: 5})
	if err != nil {
		return false, ""
	}
	for _, card := range cards {
		if extract.TitleSimilarity(c.Title, card.Title) >= extract.DuplicateThreshold {
			return true, card.Title
		}
	}
	return false, ""
}

type benchRow struct {
	File         string `json:"file"`
	SizeKB       int64  `json:"size_kb"`
	HadWriteback bool   `json:"had_writeback"`
	Candidates   int    `json:"candidates"`
	Duplicates   int    `json:"duplicates"` // candidates that restate an existing vault note (dedup)
	Kept         int    `json:"kept"`       // fresh candidates the panel keeps (majority)
	Unanimous    int    `json:"unanimous"`  // of Kept, how many passed all 3 lenses (strictest)
	Err          string `json:"err,omitempty"`
}

// sampleTranscripts picks up to n transcripts to grade: skip trivially small ones
// (<50KB, no real work) and skip the huge in-progress current session (>40MB, also too
// slow to be representative), then take the largest remaining (most substantive work).
func sampleTranscripts(dir string, n int) ([]string, error) {
	all, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	type fi struct {
		path string
		size int64
	}
	var fis []fi
	for _, p := range all {
		st, e := os.Stat(p)
		if e != nil {
			continue
		}
		if st.Size() < 50<<10 || st.Size() > 40<<20 {
			continue
		}
		fis = append(fis, fi{p, st.Size()})
	}
	sort.Slice(fis, func(i, j int) bool { return fis[i].size > fis[j].size })
	if n > 0 && len(fis) > n {
		fis = fis[:n]
	}
	out := make([]string, len(fis))
	for i, f := range fis {
		out[i] = f.path
	}
	return out, nil
}

func runExtractBenchmark(ctx context.Context, client llm.Client, dir string, n, concurrency, maxChars int, asJSON bool, dedupVault string, repeat, samples int) error {
	files, err := sampleTranscripts(dir, n)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no usable transcripts in %s", dir)
	}
	// Optional dedup context: a retriever over an existing vault, so a candidate that
	// restates a note already there is counted separately from genuinely fresh ones.
	var rtr *retrieve.Retriever
	if dedupVault != "" {
		if vs, e := index.OpenReadOnly(dedupVault); e == nil {
			defer vs.Close()
			rtr, _ = buildVaultRetriever(vs, dedupVault)
		}
	}
	// Grade with a 3-lens judge PANEL, using SEPARATE judge models when MESH_JUDGE*_* is
	// configured. A single generalist judge (worse, a self-grade) rubber-stamps almost
	// everything (observed 100% keep); three lenses each stressing one qualifying criterion
	// (non-obvious / reusable / durable) with a majority-keep bar is the honest number.
	judges, independent := llm.NewJudgePanelFromEnv(client)
	if !asJSON {
		descs := make([]string, len(judges))
		for i, j := range judges {
			descs[i] = j.Describe()
		}
		fmt.Fprintf(os.Stderr, "grading %d transcripts: extractor %s, judge panel [%s] (independent=%v, majority keep)\n",
			len(files), client.Describe(), strings.Join(descs, ", "), independent)
		if !independent {
			fmt.Fprintf(os.Stderr, "WARNING: judge panel == extractor (self-grade). Set MESH_JUDGE_AGENT/MESH_JUDGE_MODEL (and MESH_JUDGE2_*/MESH_JUDGE3_*) for an honest, model-diverse number.\n")
		}
	}
	if concurrency < 1 {
		concurrency = 1
	}
	// Variance mode: extraction is non-deterministic (the LLM samples at ~temp 1), so a
	// single run's coverage is a noisy point. --repeat K runs the whole benchmark K times
	// and reports mean +/- stddev plus per-session stability, so coverage is never quoted
	// as one number again.
	if repeat > 1 {
		return runVarianceBenchmark(ctx, files, client, judges, independent, rtr, maxChars, concurrency, repeat, samples, asJSON)
	}
	rows := benchmarkRows(ctx, files, client, judges, rtr, maxChars, concurrency, samples, !asJSON)

	// Tally. "Fresh" = candidates that are not duplicates of an existing vault note;
	// precision is measured on the fresh set (what a reviewer actually sees).
	var manual, extractCov, totalC, totalDup, totalK, totalU, errs int
	for _, r := range rows {
		if r.Err != "" && r.Candidates == 0 {
			errs++
		}
		if r.HadWriteback {
			manual++
		}
		if r.Candidates-r.Duplicates > 0 {
			extractCov++
		}
		totalC += r.Candidates
		totalDup += r.Duplicates
		totalK += r.Kept
		totalU += r.Unanimous
	}
	N := len(rows)
	fresh := totalC - totalDup
	precision := 0.0
	if fresh > 0 {
		precision = float64(totalK) / float64(fresh) * 100
	}
	verdict := "STOP (precision < 60%: the review queue would be a spam filter)"
	switch {
	case precision >= 80:
		verdict = "SHIP the pending-review queue (precision >= 80%)"
	case precision >= 60:
		verdict = "SHIP behind review (precision 60-80%: human veto carries it)"
	}

	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"sampled": N, "errors": errs,
			"manual_writeback_sessions": manual, "extraction_coverage_sessions": extractCov,
			"candidates": totalC, "duplicates": totalDup, "fresh": fresh, "kept": totalK, "unanimous": totalU,
			"precision_pct": precision, "independent_judge": independent, "verdict": verdict, "rows": rows,
		}, "", "  ")
		fmt.Println(string(b))
		return nil
	}

	pct := func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b) * 100
	}
	fmt.Printf("\nMesh write-back benchmark: auto-extraction vs the current manual (nudge) algo\n")
	fmt.Printf("transcripts graded: %d   (LLM errors: %d)   dir: %s\n", N, errs, dir)
	fmt.Printf("\nCURRENT ALGO (manual write-back, Stop-hook nudge)\n")
	fmt.Printf("  sessions that wrote back:        %d / %d   (%.0f%% coverage)\n", manual, N, pct(manual, N))
	fmt.Printf("\nAUTO-EXTRACTION\n")
	fmt.Printf("  sessions yielding >=1 fresh note: %d / %d   (%.0f%% coverage)\n", extractCov, N, pct(extractCov, N))
	fmt.Printf("  candidate notes:                 %d   (%.1f per session)\n", totalC, float64(totalC)/float64(max1(N)))
	if dedupVault != "" {
		fmt.Printf("  already-known (deduped out):     %d   (%.0f%% of candidates)\n", totalDup, pct(totalDup, max1(totalC)))
		fmt.Printf("  fresh (reach review):            %d\n", fresh)
	}
	judgeKind := "self-grade, inflated"
	if independent {
		judgeKind = "independent"
	}
	fmt.Printf("  panel keep-worthy (of fresh):    %d / %d   (PRECISION %.0f%%, %s 3-lens panel, majority)\n", totalK, fresh, precision, judgeKind)
	fmt.Printf("  of those, unanimous (3/3 lenses): %d   (the strictest bar)\n", totalU)
	lift := "n/a"
	if manual > 0 {
		lift = fmt.Sprintf("%.1fx", pct(extractCov, N)/pct(manual, N))
	} else if extractCov > 0 {
		lift = "from zero"
	}
	fmt.Printf("\nCOVERAGE LIFT: %s (%.0f%% of sessions get a note vs %.0f%% manually)\n", lift, pct(extractCov, N), pct(manual, N))
	fmt.Printf("VERDICT: %s\n", verdict)
	return nil
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func pctOf(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}

// benchStats is the per-run tally of a benchmark pass.
type benchStats struct {
	N, manual, extractCov, totalC, totalDup, totalK, totalU, errs int
}

func (s benchStats) coveragePct() float64  { return pctOf(s.extractCov, s.N) }
func (s benchStats) precisionPct() float64 { return pctOf(s.totalK, s.totalC-s.totalDup) }

func tallyRows(rows []benchRow) benchStats {
	s := benchStats{N: len(rows)}
	for _, r := range rows {
		if r.Err != "" && r.Candidates == 0 {
			s.errs++
		}
		if r.HadWriteback {
			s.manual++
		}
		if r.Candidates-r.Duplicates > 0 {
			s.extractCov++
		}
		s.totalC += r.Candidates
		s.totalDup += r.Duplicates
		s.totalK += r.Kept
		s.totalU += r.Unanimous
	}
	return s
}

// benchmarkRows runs one benchmark pass over files: digest -> extract -> dedup -> judge
// panel, concurrently. Verbose prints a per-file line to stderr. This is the single-run
// unit that --repeat drives K times for a variance estimate.
func benchmarkRows(ctx context.Context, files []string, client llm.Client, judges []llm.Client, rtr *retrieve.Retriever, maxChars, concurrency, samples int, verbose bool) []benchRow {
	if concurrency < 1 {
		concurrency = 1
	}
	rows := make([]benchRow, len(files))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := benchRow{File: filepath.Base(f)}
			if st, e := os.Stat(f); e == nil {
				row.SizeKB = st.Size() >> 10
			}
			digest, st, e := extract.Digest(f, maxChars)
			if e != nil {
				row.Err = "digest: " + e.Error()
				rows[i] = row
				return
			}
			row.HadWriteback = st.HadWriteback
			cands, e := extract.ExtractConsistent(ctx, client, digest, samples)
			if e != nil {
				row.Err = "extract: " + e.Error()
				rows[i] = row
				return
			}
			row.Candidates = len(cands)
			for _, cnd := range cands {
				if known, _ := knownInVault(ctx, rtr, cnd); known {
					row.Duplicates++ // restates a note already in the vault; not judged as fresh
					continue
				}
				v, je := extract.JudgePanel(ctx, judges, cnd, extract.PanelMajority)
				if je != nil {
					row.Err = "judge: " + je.Error()
					continue
				}
				if v.Keep {
					row.Kept++
					if v.KeepN == v.Total {
						row.Unanimous++ // 3/3 lenses: the strictest bar, for the split
					}
				}
			}
			rows[i] = row
			if verbose {
				fmt.Fprintf(os.Stderr, "  %-40s writeback=%-5v cands=%d dup=%d kept=%d(u%d) %s\n", row.File, row.HadWriteback, row.Candidates, row.Duplicates, row.Kept, row.Unanimous, row.Err)
			}
		}(i, f)
	}
	wg.Wait()
	return rows
}

func meanStddev(xs []float64) (mean, sd float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	for _, x := range xs {
		d := x - mean
		sd += d * d
	}
	return mean, math.Sqrt(sd / float64(len(xs)))
}

// runVarianceBenchmark runs the benchmark K times and reports coverage + precision as
// mean +/- stddev, plus per-session stability (which transcripts ALWAYS, SOMETIMES, or
// NEVER yield a note across the K runs). The stability split is the real characterization
// of the non-determinism: a "sometimes" session is where the coverage variance lives.
func runVarianceBenchmark(ctx context.Context, files []string, client llm.Client, judges []llm.Client, independent bool, rtr *retrieve.Retriever, maxChars, concurrency, repeat, samples int, asJSON bool) error {
	var covs, precs []float64
	var candCounts []int
	yields := map[string]int{} // file base -> # of runs it yielded a fresh note
	for run := 0; run < repeat; run++ {
		rows := benchmarkRows(ctx, files, client, judges, rtr, maxChars, concurrency, samples, false)
		s := tallyRows(rows)
		covs = append(covs, s.coveragePct())
		precs = append(precs, s.precisionPct())
		candCounts = append(candCounts, s.totalC)
		for _, r := range rows {
			if r.Candidates-r.Duplicates > 0 {
				yields[r.File]++
			}
		}
		if !asJSON {
			fmt.Fprintf(os.Stderr, "  run %d/%d: coverage %.0f%%  precision %.0f%%  candidates %d\n", run+1, repeat, s.coveragePct(), s.precisionPct(), s.totalC)
		}
	}
	covMean, covSD := meanStddev(covs)
	precMean, precSD := meanStddev(precs)
	totCand := 0
	for _, c := range candCounts {
		totCand += c
	}
	candMean := float64(totCand) / float64(max1(repeat))

	// Per-session stability across the K runs.
	always, never := 0, 0
	var unstableNames []string
	for _, f := range files {
		switch yields[filepath.Base(f)] {
		case repeat:
			always++
		case 0:
			never++
		default:
			unstableNames = append(unstableNames, fmt.Sprintf("%s (%d/%d runs)", filepath.Base(f), yields[filepath.Base(f)], repeat))
		}
	}
	unstable := len(unstableNames)

	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"repeats": repeat, "sampled": len(files), "independent_judge": independent,
			"coverage_pct_mean": covMean, "coverage_pct_stddev": covSD, "coverage_pct_runs": covs,
			"precision_pct_mean": precMean, "precision_pct_stddev": precSD, "precision_pct_runs": precs,
			"candidates_per_run_mean": candMean,
			"sessions_always_yield":   always, "sessions_never_yield": never,
			"sessions_unstable": unstable, "unstable_sessions": unstableNames,
		}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("\nMesh extraction VARIANCE benchmark (%d repeats, %d transcripts, panel independent=%v)\n", repeat, len(files), independent)
	fmt.Printf("\n  coverage:   %.0f%% +/- %.0f%%   (per run: %s)\n", covMean, covSD, fmtPcts(covs))
	fmt.Printf("  precision:  %.0f%% +/- %.0f%%   (per run: %s)\n", precMean, precSD, fmtPcts(precs))
	fmt.Printf("  candidates: %.1f per run (mean)\n", candMean)
	fmt.Printf("\nper-session stability (of %d transcripts, across %d runs):\n", len(files), repeat)
	fmt.Printf("  ALWAYS yields a note:  %d\n", always)
	fmt.Printf("  NEVER yields a note:   %d\n", never)
	fmt.Printf("  UNSTABLE (flips):      %d   <- where the coverage variance lives\n", unstable)
	for _, u := range unstableNames {
		fmt.Printf("     - %s\n", u)
	}
	return nil
}

func fmtPcts(xs []float64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%.0f", x)
	}
	return strings.Join(parts, " ")
}

// judgeEvalCase is one labeled candidate: label "keep" means the panel SHOULD keep it,
// "reject" means it SHOULD reject it.
type judgeEvalCase struct {
	Label           string            `json:"label"`
	Type            string            `json:"type,omitempty"`
	Template        string            `json:"template"`
	TemplateVersion int               `json:"template_version"`
	Title           string            `json:"title"`
	Summary         string            `json:"summary"`
	Sections        map[string]string `json:"sections"`
	Confidence      string            `json:"confidence"`
}

// evalResult is the confusion matrix of a judge-eval run (label "keep" = positive class).
type evalResult struct {
	TP, FN, TN, FP, Errs  int
	Slipped, OverRejected []string
}

// evalCases runs the panel over labeled cases and tallies the confusion matrix. Pure over
// its judges, so a stub client makes it deterministic in tests.
func evalCases(ctx context.Context, judges []llm.Client, cases []judgeEvalCase, concurrency int) evalResult {
	if concurrency < 1 {
		concurrency = 4
	}
	kept := make([]struct {
		keep bool
		err  string
	}, len(cases))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, c := range cases {
		wg.Add(1)
		go func(i int, c judgeEvalCase) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cand := extract.Candidate{Type: c.Type, Template: c.Template, TemplateVersion: c.TemplateVersion, Title: c.Title, Summary: c.Summary, Sections: c.Sections, Confidence: c.Confidence}
			v, e := extract.JudgePanel(ctx, judges, cand, extract.PanelMajority)
			if e != nil {
				kept[i].err = e.Error()
				return
			}
			kept[i].keep = v.Keep
		}(i, c)
	}
	wg.Wait()

	var er evalResult
	for i, c := range cases {
		if kept[i].err != "" {
			er.Errs++
			continue
		}
		switch strings.ToLower(c.Label) {
		case "keep":
			if kept[i].keep {
				er.TP++
			} else {
				er.FN++
				er.OverRejected = append(er.OverRejected, c.Title)
			}
		case "reject":
			if kept[i].keep {
				er.FP++
				er.Slipped = append(er.Slipped, c.Title)
			} else {
				er.TN++
			}
		}
	}
	return er
}

// runJudgeEval measures the judge PANEL's DISCRIMINATION over a labeled fixture: does it
// keep the good notes (recall) AND reject the bad ones (specificity)? A raw keep-rate on
// live extractions cannot answer this (it has no negatives), so a pinned ~100% precision
// there could mean "everything's good" OR "the judge accepts anything". This benchmark
// adds the missing negative control: bad candidates the panel MUST reject.
func runJudgeEval(ctx context.Context, client llm.Client, path string, concurrency int, asJSON bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fx struct {
		Doc           string          `json:"_doc"`
		FormatVersion int             `json:"format_version"`
		Cases         []judgeEvalCase `json:"cases"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fx); err != nil {
		return fmt.Errorf("parse fixture (purpose-specific template format required): %w", err)
	}
	for _, c := range fx.Cases {
		if c.Template == "" {
			return fmt.Errorf("historical judge fixture requires a reviewed purpose-specific rewrite")
		}
		if _, err := vault.TemplateFor(c.Template, c.TemplateVersion); err != nil {
			return fmt.Errorf("invalid judge template: %w", err)
		}
	}
	if len(fx.Cases) == 0 {
		return fmt.Errorf("no cases in %s", path)
	}
	judges, independent := llm.NewJudgePanelFromEnv(client)
	if concurrency < 1 {
		concurrency = 4
	}
	if !asJSON {
		descs := make([]string, len(judges))
		for i, j := range judges {
			descs[i] = j.Describe()
		}
		fmt.Fprintln(os.Stderr, "purpose-specific fixture format: metrics require a new baseline and cannot be compared directly to the retired shorthand corpus")
		fmt.Fprintf(os.Stderr, "judge-eval: %d cases via panel [%s] (independent=%v)\n", len(fx.Cases), strings.Join(descs, ", "), independent)
	}

	er := evalCases(ctx, judges, fx.Cases, concurrency)
	tp, fn, tn, fp, errs := er.TP, er.FN, er.TN, er.FP, er.Errs
	slipped, overRejected := er.Slipped, er.OverRejected
	good, bad := tp+fn, tn+fp
	pct := func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b) * 100
	}
	recall := pct(tp, good)     // keeps the good (true-positive rate)
	specificity := pct(tn, bad) // rejects the bad (true-negative rate) <- the discrimination signal
	accuracy := pct(tp+tn, good+bad)

	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"cases": len(fx.Cases), "errors": errs, "independent_judge": independent, "fixture_format_version": fx.FormatVersion, "requires_rebaseline": true,
			"good_total": good, "bad_total": bad,
			"kept_good": tp, "rejected_good": fn, "rejected_bad": tn, "kept_bad": fp,
			"recall_pct": recall, "specificity_pct": specificity, "accuracy_pct": accuracy,
			"bad_that_slipped_through": slipped, "good_over_rejected": overRejected,
		}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("\nJudge-panel discrimination benchmark (majority keep, independent=%v)\n", independent)
	fmt.Printf("cases: %d   (LLM errors: %d)\n", len(fx.Cases), errs)
	fmt.Printf("\nKEEPS THE GOOD (recall):       %d / %d   (%.0f%%)\n", tp, good, recall)
	fmt.Printf("REJECTS THE BAD (specificity): %d / %d   (%.0f%%)   <- the discrimination signal\n", tn, bad, specificity)
	fmt.Printf("accuracy:                      %d / %d   (%.0f%%)\n", tp+tn, good+bad, accuracy)
	if len(slipped) > 0 {
		fmt.Printf("\nBAD notes the panel WRONGLY KEPT (%d):\n", len(slipped))
		for _, s := range slipped {
			fmt.Printf("  - %s\n", s)
		}
	}
	if len(overRejected) > 0 {
		fmt.Printf("\nGOOD notes the panel WRONGLY REJECTED (%d):\n", len(overRejected))
		for _, s := range overRejected {
			fmt.Printf("  - %s\n", s)
		}
	}
	return nil
}
