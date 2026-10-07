// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// CompletionCLIContract is the explicit operator contract for non-Claude CLI
// adapters: JSON {protocol,system,user} on stdin, completion text on stdout, no
// agent tools, MCP servers, inherited instructions or model-driven execution.
// The custom executable remains operator-owned; this is not an OS sandbox.
const CompletionCLIContract = "mesh-completion-v1"

const (
	maxCLIOutputBytes = 4 << 20 // permits large curator merges, bounded before parsing
	maxCLIErrorBytes  = 64 << 10
)

var ErrOutputLimit = errors.New("llm: command output exceeds completion limit")

// newCompletionCLI cannot be configured into a tool-enabled Claude agent. Custom
// providers must explicitly attest the completion-only protocol; there is no
// fallback from a rejected adapter to an unrestricted agent.
func newCompletionCLI(argv []string, timeout time.Duration, contract, model string) (*cliClient, error) {
	base := filepath.Base(argv[0])
	if base != "claude" && base != "claude.exe" {
		if contract != CompletionCLIContract {
			return nil, fmt.Errorf("custom CLI requires CLI_CONTRACT=%s and a completion-only adapter", CompletionCLIContract)
		}
		return &cliClient{argv: argv, timeout: timeout}, nil
	}
	clean := []string{argv[0]}
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "-p" || arg == "--print":
			// The adapter supplies print mode itself.
		case arg == "--model" || arg == "--effort":
			if i+1 == len(argv) || strings.HasPrefix(argv[i+1], "-") {
				return nil, fmt.Errorf("Claude completion option %s needs a value", arg)
			}
			clean = append(clean, arg, argv[i+1])
			i++
		case strings.HasPrefix(arg, "--model=") || strings.HasPrefix(arg, "--effort="):
			key, value, _ := strings.Cut(arg, "=")
			if value == "" {
				return nil, fmt.Errorf("Claude completion option %s needs a value", key)
			}
			clean = append(clean, key, value)
		default:
			return nil, fmt.Errorf("unsupported Claude completion option %q; only print, model and effort are configurable", arg)
		}
	}
	if model != "" {
		clean = append(clean, "--model", model)
	}
	return &cliClient{argv: clean, timeout: timeout, claude: true}, nil
}

func (c *cliClient) completionCommand(system, user string) ([]string, string) {
	if !c.claude {
		payload, _ := json.Marshal(struct {
			Protocol string `json:"protocol"`
			System   string `json:"system"`
			User     string `json:"user"`
		}{CompletionCLIContract, system, user})
		return c.argv, string(payload) + "\n"
	}
	argv := append([]string(nil), c.argv...)
	argv = append(argv,
		"--print", "--safe-mode", "--tools", "",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--permission-mode", "dontAsk", "--no-session-persistence",
		"--disable-slash-commands", "--no-chrome",
		"--max-turns", "1",
		"--output-format", "json", "--system-prompt", system)
	return argv, user
}

// Claude JSON output lets the adapter reject error results and multi-turn agent
// responses, while preserving Complete's plain completion-text contract.
func claudeCompletionResult(raw string) (string, error) {
	var result struct {
		Type     string `json:"type"`
		IsError  bool   `json:"is_error"`
		NumTurns int    `json:"num_turns"`
		Result   string `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return "", fmt.Errorf("llm: invalid Claude completion envelope")
	}
	if result.Type != "result" || result.IsError || result.NumTurns != 1 || strings.TrimSpace(result.Result) == "" {
		return "", fmt.Errorf("llm: Claude did not return one successful completion turn")
	}
	return strings.TrimSpace(result.Result), nil
}

// The provider can be untrusted or misconfigured. Bound both output streams and
// cancel immediately on overflow, instead of waiting for a blocked pipe timeout.
type boundedCLIOutput struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
	cancel   context.CancelFunc
}

func (b *boundedCLIOutput) Write(p []byte) (int, error) {
	room := b.limit - b.buffer.Len()
	if len(p) <= room {
		return b.buffer.Write(p)
	}
	n, _ := b.buffer.Write(p[:room])
	b.overflow = true
	b.cancel()
	return n, ErrOutputLimit
}

func (b *boundedCLIOutput) String() string { return b.buffer.String() }
