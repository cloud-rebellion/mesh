// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTranscriptHasWriteback(t *testing.T) {
	dir := t.TempDir()
	// An actual tool call: the quoted tool name appears in a tool_use entry.
	call := filepath.Join(dir, "call.jsonl")
	os.WriteFile(call, []byte(`{"type":"tool_use","name":"mesh_append_note","input":{}}`+"\n"), 0o644)
	if !transcriptHasWriteback(call) {
		t.Error("a real mesh_append_note tool call should be detected")
	}
	// A mere mention (the injected contract text says: call mesh_append_note ...) must
	// NOT be mistaken for a write-back, or the Stop nudge would never fire.
	mention := filepath.Join(dir, "mention.jsonl")
	os.WriteFile(mention, []byte(`{"role":"user","content":"call mesh_append_note when done"}`+"\n"), 0o644)
	if transcriptHasWriteback(mention) {
		t.Error("an unquoted mention of the tool must NOT count as a write-back")
	}
	if transcriptHasWriteback(filepath.Join(dir, "missing.jsonl")) {
		t.Error("a missing transcript must not count as a write-back")
	}
}

func TestModernAuthoringStopsWithoutFallbackExtraction(t *testing.T) {
	for _, action := range []string{"publish", "draft"} {
		t.Run(action, func(t *testing.T) {
			sid := "modern-" + filepath.Base(t.TempDir())
			transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
			record := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"mcp__mesh__mesh_author_note","input":{"action":"` + action + `","note":{"title":"A supported finding"}}}]}}`
			if err := os.WriteFile(transcript, []byte(record+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			oldSpawn := spawnExtractionFn
			var spawns atomic.Int32
			spawnExtractionFn = func(string, string) { spawns.Add(1) }
			t.Cleanup(func() { spawnExtractionFn = oldSpawn })
			for _, prefix := range []string{"mesh-stop-", "mesh-extracted-"} {
				marker := filepath.Join(os.TempDir(), prefix+sanitizeID(sid))
				_ = os.Remove(marker)
				t.Cleanup(func() { _ = os.Remove(marker) })
			}
			input := map[string]string{"session_id": sid, "transcript_path": transcript}
			for attempt := 0; attempt < 2; attempt++ {
				if out := runStopCheck(t, "", input, "--extract", "--vault", t.TempDir()); out != "" {
					t.Fatalf("durable %s request was nudged again: %s", action, out)
				}
			}
			if spawns.Load() != 0 {
				t.Fatalf("durable %s request spawned %d fallback extractions", action, spawns.Load())
			}
		})
	}
}

func TestQuotedWriteToolInUserTextDoesNotSuppressStopNudge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mention.jsonl")
	record := `{"type":"user","message":{"role":"user","content":"The example says \"mesh_append_note\" but no tool was called."}}`
	if err := os.WriteFile(p, []byte(record+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if transcriptHasWriteback(p) {
		t.Fatal("quoted user text was mistaken for an actual write request")
	}
}

func runStopCheck(t *testing.T, env string, input map[string]string, args ...string) string {
	t.Helper()
	t.Setenv("MESH_LLM_CHILD", env)
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(t.TempDir(), "stdin.json")
	if err := os.WriteFile(in, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old })
	c := hooksStopCheckCmd()
	c.SetArgs(args)
	out, err := captureStdout(t, c.Execute)
	os.Stdin = old
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStopCheckSkipsMeshLLMChild(t *testing.T) {
	sid := "child-" + filepath.Base(t.TempDir())
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(os.TempDir(), "mesh-stop-"+sanitizeID(sid))
	_ = os.Remove(marker)
	t.Cleanup(func() { _ = os.Remove(marker) })

	oldSpawn := spawnExtractionFn
	var spawns atomic.Int32
	spawnExtractionFn = func(string, string) { spawns.Add(1) }
	t.Cleanup(func() { spawnExtractionFn = oldSpawn })

	input := map[string]string{"session_id": sid, "transcript_path": transcript}
	if out := runStopCheck(t, "1", input, "--extract", "--vault", t.TempDir()); out != "" {
		t.Fatalf("child stop-check output = %q, want empty", out)
	}
	if out := runStopCheck(t, "1", input, "--extract", "--vault", t.TempDir()); out != "" {
		t.Fatalf("second child stop-check output = %q, want empty", out)
	}
	if spawns.Load() != 0 {
		t.Fatalf("child spawned %d extractions, want 0", spawns.Load())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child created stop marker %s", marker)
	}
}

func TestStopCheckPreservesOncePerSessionExtraction(t *testing.T) {
	sid := "normal-" + filepath.Base(t.TempDir())
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"mesh-stop-", "mesh-extracted-"} {
		p := filepath.Join(os.TempDir(), prefix+sanitizeID(sid))
		_ = os.Remove(p)
		t.Cleanup(func() { _ = os.Remove(p) })
	}
	vault := t.TempDir()
	oldSpawn := spawnExtractionFn
	var spawns atomic.Int32
	spawnExtractionFn = func(string, string) { spawns.Add(1) }
	t.Cleanup(func() { spawnExtractionFn = oldSpawn })
	input := map[string]string{"session_id": sid, "transcript_path": transcript}

	if out := runStopCheck(t, "", input, "--extract", "--extract-cap", "20", "--vault", vault); out == "" {
		t.Fatal("first stop-check did not block")
	}
	if out := runStopCheck(t, "", input, "--extract", "--extract-cap", "20", "--vault", vault); out != "" {
		t.Fatalf("second stop-check output = %q, want empty", out)
	}
	if out := runStopCheck(t, "", input, "--extract", "--extract-cap", "20", "--vault", vault); out != "" {
		t.Fatalf("third stop-check output = %q, want empty", out)
	}
	if spawns.Load() != 1 {
		t.Fatalf("spawn count = %d, want 1", spawns.Load())
	}
}

func TestExtractionDailyCap(t *testing.T) {
	vault := t.TempDir()
	day := time.Date(2026, 9, 10, 12, 0, 0, 0, time.Local)
	for i := 0; i < 3; i++ {
		if !claimExtractionSlot(vault, 3, day) {
			t.Fatalf("claim %d refused before cap", i+1)
		}
	}
	if claimExtractionSlot(vault, 3, day) {
		t.Fatal("claim above cap succeeded")
	}
	if !claimExtractionSlot(vault, 3, day.AddDate(0, 0, 1)) {
		t.Fatal("new local day did not reset cap")
	}
	if claimExtractionSlot(vault, 0, day) {
		t.Fatal("cap 0 claimed a slot")
	}
}

func TestExtractionDailyCapConcurrent(t *testing.T) {
	vault := t.TempDir()
	day := time.Date(2026, 9, 10, 12, 0, 0, 0, time.Local)
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if claimExtractionSlot(vault, 10, day) {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 10 {
		t.Fatalf("concurrent claims = %d, want 10", won.Load())
	}
}
