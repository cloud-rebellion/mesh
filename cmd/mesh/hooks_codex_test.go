// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func codexStopInput(session, turn string, active bool) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": session, "turn_id": turn, "hook_event_name": "Stop",
		"stop_hook_active": active,
	})
	return string(b)
}

func invokeCodexStop(t *testing.T, input, dir string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runCodexStopCheck(strings.NewReader(input), &out, dir); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestCodexStopOneNudgePerTurn(t *testing.T) {
	t.Setenv("MESH_LLM_CHILD", "")
	dir := t.TempDir()
	input := codexStopInput("session", "turn-1", false)
	out := invokeCodexStop(t, input, dir)
	var response map[string]string
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatal(err)
	}
	if len(response) != 2 || response["decision"] != "block" || response["reason"] != codexWritebackReason {
		t.Fatalf("unexpected protocol response: %v", response)
	}
	reason := response["reason"]
	if !strings.HasPrefix(reason, "Mesh: first check") || strings.Index(reason, "confirmed successful Mesh writeback receipt") > strings.Index(reason, "mesh_author_note") ||
		!strings.Contains(reason, "finish without duplicating it") || !strings.Contains(reason, "mesh_note_template for the registered template") ||
		!strings.Contains(reason, "report the specific blocker and pending writeback") || !strings.Contains(reason, "do not claim it was saved or retry indefinitely") ||
		!strings.Contains(reason, "follow the current workspace's writeback instructions") || !strings.Contains(reason, "Only finish without a new note if the current workspace's instructions permit it and no task outcome needs recording") {
		t.Fatalf("reminder omitted receipt-first or pending-writeback completion contract: %q", reason)
	}
	if got := invokeCodexStop(t, input, dir); got != "" {
		t.Fatal("repeated Stop nudged the same turn")
	}
	if got := invokeCodexStop(t, codexStopInput("session", "turn-2", false), dir); got == "" {
		t.Fatal("a later turn could not receive its own reminder")
	}
	info, err := os.Stat(codexStopMarker(dir, "session", "turn-1"))
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != 0 {
		t.Fatalf("claim should be empty and private: info=%v err=%v", info, err)
	}
}

func TestCodexStopConcurrentSingleClaim(t *testing.T) {
	t.Setenv("MESH_LLM_CHILD", "")
	dir := t.TempDir()
	input := codexStopInput("concurrent-session", "concurrent-turn", false)
	var nudges, failures atomic.Int32
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			var out bytes.Buffer
			if err := runCodexStopCheck(strings.NewReader(input), &out, dir); err != nil {
				failures.Add(1)
			}
			if out.Len() != 0 {
				nudges.Add(1)
			}
		})
	}
	wg.Wait()
	if nudges.Load() != 1 || failures.Load() != 0 {
		t.Fatalf("nudges=%d errors=%d, want one nudge and no errors", nudges.Load(), failures.Load())
	}
}

func TestCodexStopActiveContinuationDoesNotClaim(t *testing.T) {
	t.Setenv("MESH_LLM_CHILD", "")
	dir := t.TempDir()
	if out := invokeCodexStop(t, codexStopInput("session", "turn", true), dir); out != "" {
		t.Fatal("active Stop continuation was blocked")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("active continuation created a claim: %v %v", entries, err)
	}
}

type codexForbiddenReader struct{ reads int }

func (r *codexForbiddenReader) Read([]byte) (int, error) {
	r.reads++
	return 0, errors.New("input must not be consumed")
}

func TestCodexStopChildGuardBeforeInput(t *testing.T) {
	t.Setenv("MESH_LLM_CHILD", "1")
	input := &codexForbiddenReader{}
	var out bytes.Buffer
	dir := t.TempDir()
	if err := runCodexStopCheck(input, &out, dir); err != nil || input.reads != 0 || out.Len() != 0 {
		t.Fatalf("child guard failed: reads=%d output=%q err=%v", input.reads, out.String(), err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("child created a claim")
	}
}

func TestCodexStopMalformedInputFailsOpen(t *testing.T) {
	t.Setenv("MESH_LLM_CHILD", "")
	cases := map[string]string{
		"empty":             "",
		"malformed":         `{"private":"not-to-be-echoed"`,
		"trailing":          codexStopInput("session", "turn", false) + `{}`,
		"missing-session":   codexStopInput("", "turn", false),
		"missing-turn":      codexStopInput("session", "", false),
		"oversized-id":      codexStopInput(strings.Repeat("x", codexStopIDLimit+1), "turn", false),
		"control-id":        codexStopInput("session\x00private", "turn", false),
		"blank-id":          codexStopInput("  \t", "turn", false),
		"wrong-event":       `{"session_id":"s","turn_id":"t","hook_event_name":"SessionStart","stop_hook_active":false}`,
		"missing-active":    `{"session_id":"s","turn_id":"t","hook_event_name":"Stop"}`,
		"null-active":       `{"session_id":"s","turn_id":"t","hook_event_name":"Stop","stop_hook_active":null}`,
		"wrong-active-type": `{"session_id":"s","turn_id":"t","hook_event_name":"Stop","stop_hook_active":"false"}`,
		"oversized-input":   strings.Repeat(" ", codexStopInputLimit+1),
		"invalid-utf8":      string([]byte{0xff}),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if out := invokeCodexStop(t, input, dir); out != "" {
				t.Fatalf("invalid input produced private output: %q", out)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("invalid input created a claim")
			}
		})
	}
}

type codexCountingReader struct {
	r    io.Reader
	read int
}

func (r *codexCountingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.read += n
	return n, err
}

func TestCodexStopInputReadAndIdentityBounds(t *testing.T) {
	t.Setenv("MESH_LLM_CHILD", "")
	dir := t.TempDir()
	input := &codexCountingReader{r: strings.NewReader(strings.Repeat(" ", codexStopInputLimit+100))}
	var out bytes.Buffer
	if err := runCodexStopCheck(input, &out, dir); err != nil || input.read != codexStopInputLimit+1 || out.Len() != 0 {
		t.Fatalf("input was not bounded: bytes=%d output=%d err=%v", input.read, out.Len(), err)
	}
	valid := codexStopInput(strings.Repeat("s", codexStopIDLimit), strings.Repeat("t", codexStopIDLimit), false)
	// An input exactly at the byte ceiling is still accepted.
	valid += strings.Repeat(" ", codexStopInputLimit-len(valid))
	if got := invokeCodexStop(t, valid, dir); got == "" || len(got) > 2048 {
		t.Fatalf("valid boundary input failed or response grew with input: bytes=%d", len(got))
	}
	failure := &codexForbiddenReader{}
	if err := runCodexStopCheck(failure, &out, dir); err != nil || failure.reads != 1 || out.Len() != 0 {
		t.Fatalf("input error did not fail open: reads=%d output=%q err=%v", failure.reads, out.String(), err)
	}
}

func TestCodexStopIgnoresOpaqueWritebackAndTranscript(t *testing.T) {
	t.Setenv("MESH_LLM_CHILD", "")
	dir := t.TempDir()
	// A directory cannot be parsed as a transcript. Both it and arbitrary tool
	// text must remain irrelevant; neither is a server-acknowledged write receipt.
	input := map[string]any{
		"session_id": "opaque-session", "turn_id": "opaque-turn",
		"hook_event_name": "Stop", "stop_hook_active": false,
		"transcript_path":        dir,
		"last_assistant_message": "I called mesh_author_note publish and saved everything",
		"response_item":          map[string]any{"type": "custom_tool_call", "name": "exec", "input": "await tools.mesh_author_note({action:'publish'}); PRIVATE_TRANSCRIPT"},
	}
	b, _ := json.Marshal(input)
	out := invokeCodexStop(t, string(b), dir)
	if !strings.Contains(out, `"decision":"block"`) || strings.Contains(out, "PRIVATE_TRANSCRIPT") || strings.Contains(out, dir) {
		t.Fatalf("opaque text suppressed a reminder or leaked input: %q", out)
	}
}

func TestCodexStopMarkerPairIsUnambiguous(t *testing.T) {
	dir := t.TempDir()
	seen := map[string]bool{}
	for _, pair := range [][2]string{{"s/a", "t"}, {"s-a", "t"}, {"s", "a/t"}, {"s/a/t", "t"}, {"s", "a/tt"}} {
		p := codexStopMarker(dir, pair[0], pair[1])
		if seen[p] || filepath.Dir(p) != dir || len(filepath.Base(p)) != len("mesh-codex-stop-")+64 {
			t.Fatalf("unsafe or colliding marker path: %q", p)
		}
		seen[p] = true
	}
}

func TestCodexStopUnavailableClaimFailsOpen(t *testing.T) {
	t.Setenv("MESH_LLM_CHILD", "")
	input := codexStopInput("session", "turn", false)
	dir := t.TempDir()
	target := filepath.Join(dir, "preserve")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, codexStopMarker(dir, "session", "turn")); err != nil {
		t.Fatal(err)
	}
	if out := invokeCodexStop(t, input, dir); out != "" {
		t.Fatal("existing symlink was followed or replaced")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "untouched" {
		t.Fatal("symlink target changed")
	}
	if out := invokeCodexStop(t, input, filepath.Join(dir, "absent")); out != "" {
		t.Fatal("unavailable marker storage blocked Stop")
	}
}

type codexOutputFailure struct{}

func (codexOutputFailure) Write([]byte) (int, error) { return 0, errors.New("PRIVATE_WRITER_ERROR") }

func TestCodexStopOutputErrorIsFixedAndClaimRetained(t *testing.T) {
	t.Setenv("MESH_LLM_CHILD", "")
	dir := t.TempDir()
	input := codexStopInput("session", "turn", false)
	err := runCodexStopCheck(strings.NewReader(input), codexOutputFailure{}, dir)
	if err == nil || err.Error() != "write Codex Stop response" {
		t.Fatalf("unsafe output error: %v", err)
	}
	if out := invokeCodexStop(t, input, dir); out != "" {
		t.Fatal("failed output discarded its claim and could loop")
	}
}

func TestCodexStopActualCommandProtocol(t *testing.T) {
	t.Setenv("MESH_LLM_CHILD", "")
	session := "cli-" + filepath.Base(t.TempDir())
	marker := codexStopMarker(os.TempDir(), session, "turn")
	t.Cleanup(func() { _ = os.Remove(marker) })
	var out bytes.Buffer
	cmd := hooksCmd()
	cmd.SetIn(strings.NewReader(codexStopInput(session, "turn", false)))
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"codex-stop-check"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"decision":"block"`) {
		t.Fatalf("registered command did not use Codex stdout protocol: %q", out.String())
	}
	for _, args := range [][]string{{"--extract"}, {"vault"}} {
		c := hooksCodexStopCheckCmd()
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		c.SetArgs(args)
		if err := c.Execute(); err == nil {
			t.Fatalf("unexpected extractor or vault interface accepted: %v", args)
		}
	}
}
