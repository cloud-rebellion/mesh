// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/extract"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/llm"
	"github.com/bright-interaction/mesh/internal/mcp"
)

// knownInVault must return true for a candidate that restates an existing note (so the
// review queue does not re-surface knowledge the vault already has) and false for a
// genuinely new one. Exercises the full path: retriever over the vault + TitleSimilarity.
func TestKnownInVaultDedup(t *testing.T) {
	dir := t.TempDir()
	note := "---\nid: ssrf-tailscale\ntype: gotcha\n---\n# SSRF denylist must include 100.64.0.0/10 (Tailscale CGNAT)\nResolve the host and reject the Tailscale CGNAT range in SSRF denylists.\n"
	if err := os.WriteFile(filepath.Join(dir, "ssrf.md"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rtr, closeRtr := buildVaultRetriever(store, dir)
	defer closeRtr()
	if rtr == nil {
		t.Fatal("retriever failed to build over the vault")
	}
	ctx := context.Background()

	dup := extract.Candidate{Template: "troubleshooting", TemplateVersion: 1, Type: "gotcha", Title: "SSRF denylists must include 100.64.0.0/10 for Tailscale", Summary: "add the CGNAT range"}
	if known, of := knownInVault(ctx, rtr, dup); !known {
		t.Errorf("restatement not detected as known (matched %q)", of)
	}

	fresh := extract.Candidate{Template: "decision", TemplateVersion: 1, Type: "decision", Title: "Mollie webhooks require re-fetch, not signature verification", Summary: "re-fetch the payment by id"}
	if known, of := knownInVault(ctx, rtr, fresh); known {
		t.Errorf("a genuinely new candidate was wrongly deduped (matched %q)", of)
	}
}

// The production review queue must be the JUDGED set, not every raw extraction. The
// gate drops the extractor's low-confidence self-ratings outright and lets the judge
// veto weak notes, so only genuinely keep-worthy candidates reach a human. Without it
// the queue held every non-duplicate candidate (the ~64% precision production shipped
// while the benchmark judged). Deterministic via a stub judge.
func TestWriteToPendingQualityGate(t *testing.T) {
	dir := t.TempDir()
	store, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.Close() // writeToPending opens its own handle

	// Stub judge: keep everything except a candidate whose text says REJECT.
	judge := llm.Func(func(ctx context.Context, system, user string) (string, error) {
		if strings.Contains(user, "REJECT") {
			return `{"keep": false, "reason": "weak one-off"}`, nil
		}
		return `{"keep": true, "reason": "durable"}`, nil
	})

	cands := []extract.Candidate{
		{Template: "troubleshooting", TemplateVersion: 1, Type: "gotcha", Title: "KEEP a durable rule with a real mechanism", Summary: "do x", Confidence: "high"},
		{Template: "troubleshooting", TemplateVersion: 1, Type: "gotcha", Title: "REJECT a weak one-off with no mechanism", Summary: "do y", Confidence: "high"},
		{Template: "decision", TemplateVersion: 1, Type: "decision", Title: "a low-confidence guess dropped before judging", Summary: "do z", Confidence: "low"},
	}
	if err := writeToPending(dir, "session.jsonl", cands, []llm.Client{judge}); err != nil {
		t.Fatal(err)
	}

	store2, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	items, _ := store2.ListPending()
	if len(items) != 1 {
		t.Fatalf("only the judged-keep, non-low-confidence candidate should queue, got %d: %+v", len(items), items)
	}
	if !strings.Contains(items[0].Title, "KEEP") {
		t.Errorf("wrong candidate queued: %q", items[0].Title)
	}
}

func TestWriteToPendingRoutesThroughLiveMCPOwner(t *testing.T) {
	dir := t.TempDir()
	seed, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := index.ReindexFull(seed, dir); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	owner, err := mcp.NewOwningServer(dir, "mesh mcp --watch (test)")
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.WaitReady(); err != nil {
		t.Fatal(err)
	}
	afterPendingOpEnqueue = func() {
		if err := owner.Reconcile(); err != nil {
			t.Errorf("MCP owner did not drain extraction op: %v", err)
		}
	}
	t.Cleanup(func() { afterPendingOpEnqueue = nil })

	cand := extract.Candidate{Template: "troubleshooting", TemplateVersion: 1, Type: "gotcha", Title: "Stop hook survives a live MCP owner", Summary: "queue through the owner", Confidence: "high"}
	if err := writeToPending(dir, "session.jsonl", []extract.Candidate{cand}, nil); err != nil {
		t.Fatalf("automatic extraction failed beside its normal live MCP owner: %v", err)
	}
	reader, err := index.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := reader.GetPending(index.PendingID(cand.Type, cand.Title))
	if err != nil || got.Summary != cand.Summary {
		t.Fatalf("MCP owner did not persist queued extraction: got=%+v err=%v", got, err)
	}
}

// The discrimination harness must classify each labeled case correctly: a kept "keep" is
// a true positive, a kept "reject" is a bad note that SLIPPED THROUGH (false positive),
// and a rejected "keep" is a good note over-rejected. Deterministic via a stub panel that
// keeps everything except titles containing BAD.
func TestEvalCasesConfusionMatrix(t *testing.T) {
	stub := llm.Func(func(ctx context.Context, system, user string) (string, error) {
		if strings.Contains(user, "BAD") {
			return `{"keep": false, "reason": "bad"}`, nil
		}
		return `{"keep": true, "reason": "good"}`, nil
	})
	cases := []judgeEvalCase{
		{Label: "keep", Title: "GOOD one"},        // kept -> TP
		{Label: "keep", Title: "GOOD two"},        // kept -> TP
		{Label: "reject", Title: "BAD one"},       // rejected -> TN
		{Label: "reject", Title: "BAD two"},       // rejected -> TN
		{Label: "reject", Title: "sneaky filler"}, // KEPT (no BAD) -> FP, slipped through
		{Label: "keep", Title: "BAD-tagged good"}, // rejected -> FN, over-rejected
	}
	er := evalCases(context.Background(), []llm.Client{stub}, cases, 2)
	if er.TP != 2 || er.FN != 1 || er.TN != 2 || er.FP != 1 || er.Errs != 0 {
		t.Fatalf("confusion matrix wrong: TP=%d FN=%d TN=%d FP=%d errs=%d", er.TP, er.FN, er.TN, er.FP, er.Errs)
	}
	if len(er.Slipped) != 1 || er.Slipped[0] != "sneaky filler" {
		t.Errorf("slipped-through list wrong: %v", er.Slipped)
	}
	if len(er.OverRejected) != 1 || er.OverRejected[0] != "BAD-tagged good" {
		t.Errorf("over-rejected list wrong: %v", er.OverRejected)
	}
}

// The variance benchmark's stats must be correct: tallyRows derives coverage/precision
// from rows, and meanStddev summarizes the per-run spread. Deterministic, no LLM.
func TestVarianceStats(t *testing.T) {
	rows := []benchRow{
		{Candidates: 3, Duplicates: 1, Kept: 2}, // fresh 2, kept 2, yields
		{Candidates: 0},                         // no candidates, no yield
		{Candidates: 2, Duplicates: 2, Kept: 0}, // all dup, no fresh -> no yield
		{Candidates: 1, Duplicates: 0, Kept: 1}, // fresh 1, kept 1, yields
	}
	s := tallyRows(rows)
	if s.N != 4 || s.extractCov != 2 {
		t.Fatalf("coverage sessions: N=%d extractCov=%d, want 4 and 2", s.N, s.extractCov)
	}
	if got := s.coveragePct(); got != 50 {
		t.Errorf("coverage%% = %.0f, want 50", got)
	}
	// fresh = totalC - totalDup = 6 - 3 = 3; kept = 3; precision = 100.
	if got := s.precisionPct(); got != 100 {
		t.Errorf("precision%% = %.0f, want 100 (kept 3 / fresh 3)", got)
	}

	mean, sd := meanStddev([]float64{90, 100, 100})
	if mean < 96.6 || mean > 96.7 {
		t.Errorf("mean = %.2f, want ~96.67", mean)
	}
	if sd < 4.6 || sd > 4.8 {
		t.Errorf("stddev = %.2f, want ~4.71", sd)
	}
	if m, s := meanStddev(nil); m != 0 || s != 0 {
		t.Errorf("empty meanStddev = %v,%v want 0,0", m, s)
	}
}

func TestNearDuplicatePending(t *testing.T) {
	existing := []index.PendingNote{
		{Template: "troubleshooting", TemplateVersion: 1, Type: "gotcha", Title: "Verify new credential works BEFORE invalidating the old one"},
	}
	if of, dup := nearDuplicatePending("Verify new credentials work BEFORE retiring old ones", existing); !dup {
		t.Errorf("a reworded restatement of a queued item should be caught (matched %q)", of)
	}
	if _, dup := nearDuplicatePending("Bun is the only package manager allowed", existing); dup {
		t.Error("an unrelated title must not be treated as a duplicate")
	}
}

// writeToPending must not re-queue a candidate that restates an item already in the
// review queue, even when the LLM reworded it (so reruns and recurring sessions cannot
// flood the queue), while still queueing genuinely new candidates. This is the fix for
// the flood the live round-trip test surfaced (LLM rephrasing dodged the exact-id dedup).
func TestWriteToPendingSuppressesQueuedDuplicates(t *testing.T) {
	dir := t.TempDir()
	store, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddPending(index.PendingNote{Template: "troubleshooting", TemplateVersion: 1, Type: "gotcha", Title: "Verify new credential works BEFORE invalidating the old one", Summary: "probe first"}); err != nil {
		t.Fatal(err)
	}
	store.Close() // writeToPending opens its own handle

	cands := []extract.Candidate{
		{Template: "troubleshooting", TemplateVersion: 1, Type: "gotcha", Title: "Verify new credentials work BEFORE retiring old ones", Summary: "probe first"}, // reworded duplicate
		{Template: "decision", TemplateVersion: 1, Type: "decision", Title: "Bun is the only package manager allowed", Summary: "use bun"},                       // genuinely new
	}
	if err := writeToPending(dir, "session.jsonl", cands, nil); err != nil { // nil judge: exercise dedup only
		t.Fatal(err)
	}

	store2, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	items, _ := store2.ListPending()
	if len(items) != 2 {
		t.Fatalf("queue should hold the baseline + the one fresh candidate = 2, got %d", len(items))
	}
	for _, it := range items {
		if it.Title == "Verify new credentials work BEFORE retiring old ones" {
			t.Error("the reworded duplicate was queued despite matching an existing item")
		}
	}
}
