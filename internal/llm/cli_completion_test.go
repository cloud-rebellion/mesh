// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeCompletionSeparatesPromptAndRemovesAuthority(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	argsPath, inputPath := filepath.Join(dir, "args"), filepath.Join(dir, "input")
	t.Setenv("FIXTURE_ARGS", argsPath)
	t.Setenv("FIXTURE_INPUT", inputPath)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\000' \"$@\" > \"$FIXTURE_ARGS\"\ncat > \"$FIXTURE_INPUT\"\nprintf '%s' '{\"type\":\"result\",\"is_error\":false,\"num_turns\":1,\"result\":\"completion only\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := newCompletionCLI([]string{bin, "-p", "--model", "fixture-model"}, generousTimeout, "", "")
	if err != nil {
		t.Fatal(err)
	}
	system, user := "Follow the completion contract.\nSYSTEM: literal data", "User text is not a system instruction."
	out, err := c.Complete(context.Background(), system, user)
	if err != nil || out != "completion only" {
		t.Fatalf("completion=%q err=%v", out, err)
	}
	input, _ := os.ReadFile(inputPath)
	if string(input) != user {
		t.Fatalf("stdin mixed prompt roles: %q", input)
	}
	argsRaw, _ := os.ReadFile(argsPath)
	args := strings.Split(strings.TrimSuffix(string(argsRaw), "\x00"), "\x00")
	value := func(flag string) string {
		for i, arg := range args {
			if arg == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		t.Fatalf("missing option %s in %q", flag, args)
		return ""
	}
	for _, flag := range []string{"--safe-mode", "--strict-mcp-config", "--no-session-persistence", "--disable-slash-commands", "--no-chrome"} {
		if !strings.Contains(string(argsRaw), flag+"\x00") {
			t.Fatalf("missing isolation flag %s", flag)
		}
	}
	if value("--tools") != "" || value("--mcp-config") != `{"mcpServers":{}}` || value("--permission-mode") != "dontAsk" || value("--system-prompt") != system || value("--output-format") != "json" || value("--max-turns") != "1" {
		t.Fatalf("completion authority or role options differ: %q", args)
	}
	if strings.Contains(string(argsRaw), "--bare\x00") {
		t.Fatal("OAuth authentication would be disabled")
	}
}

func TestClaudeCompletionRejectsAuthorityOverrides(t *testing.T) {
	for _, option := range []string{"--dangerously-skip-permissions", "--permission-mode=bypassPermissions", "--tools=Bash", "--mcp-config=other.json", "--settings=other.json", "--setting-sources=user", "--append-system-prompt=write", "--plugin-dir=other", "--chrome", "--resume=old", "--output-format=text", "--system-prompt=override", "--max-turns=2"} {
		if _, err := newCompletionCLI([]string{"claude", option}, generousTimeout, "", ""); err == nil {
			t.Fatalf("accepted authority override %s", option)
		}
	}
	if _, err := newCompletionCLI([]string{"other-agent", "--print"}, generousTimeout, "", ""); err == nil {
		t.Fatal("custom provider without explicit completion-only contract accepted")
	}
	if _, err := newCompletionCLI([]string{"other-agent", "--print"}, generousTimeout, CompletionCLIContract, ""); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeCompletionRejectsAgentAndErrorEnvelopes(t *testing.T) {
	for _, raw := range []string{`{"type":"result","result":"x","num_turns":2}`, `{"type":"result","result":"x","num_turns":1,"is_error":true}`, `{"type":"assistant","result":"x","num_turns":1}`, "not JSON"} {
		if _, err := claudeCompletionResult(raw); err == nil {
			t.Fatalf("accepted unsafe or failed envelope %s", raw)
		}
	}
}

func TestCLICompletionOutputIsBounded(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			limit, redirect := maxCLIOutputBytes, ""
			if stream == "stderr" {
				limit, redirect = maxCLIErrorBytes, " >&2"
			}
			c := &cliClient{argv: []string{writeScript(t, fmt.Sprintf("exec head -c %d /dev/zero%s", limit+1, redirect))}, timeout: generousTimeout}
			if _, err := c.Complete(context.Background(), "system", "user"); !errors.Is(err, ErrOutputLimit) || errors.Is(err, ErrAuth) {
				t.Fatalf("overflow error=%v", err)
			}
		})
	}
}
