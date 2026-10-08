// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bright-interaction/mesh/internal/llm"
	"github.com/spf13/cobra"
)

const (
	codexStopInputLimit  = 1 << 20
	codexStopIDLimit     = 512
	codexWritebackReason = "Mesh: first check whether this turn's task outcome already has a confirmed successful Mesh writeback receipt. If so, finish without duplicating it. Otherwise follow the current workspace's writeback instructions and preserve the required task outcome and useful, durable findings through your existing authenticated Mesh connection. Use mesh_note_template for the registered template that fits the subject, then mesh_author_note with substantive sections, supported facts and relevant evidence. Validate, publish and confirm the saved result; keep incomplete material in a draft. Choose relevant tags and supported collections and references you can access, and state uncertainty. If Mesh is unavailable, report the specific blocker and pending writeback; do not claim it was saved or retry indefinitely. Only finish without a new note if the current workspace's instructions permit it and no task outcome needs recording. This is one reminder to the current agent, not a request to launch another model or extractor."
)

// This is a same-agent reminder, not a transcript reader or independent extractor.
// In particular, an opaque Codex exec call cannot prove publication succeeded.
func hooksCodexStopCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "codex-stop-check",
		Short:  "Internal: remind the current Codex agent to write back once per turn",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCodexStopCheck(cmd.InOrStdin(), cmd.OutOrStdout(), os.TempDir())
		},
	}
}

func runCodexStopCheck(input io.Reader, output io.Writer, markerDir string) error {
	// Completion-only children must never consume input, nudge or spawn work.
	if os.Getenv(llm.ChildEnv) != "" {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(input, codexStopInputLimit+1))
	if err != nil || len(data) > codexStopInputLimit || !utf8.Valid(data) {
		return nil // Malformed hook input fails open, without echoing private data.
	}
	var in struct {
		SessionID      string `json:"session_id"`
		TurnID         string `json:"turn_id"`
		HookEventName  string `json:"hook_event_name"`
		StopHookActive *bool  `json:"stop_hook_active"`
	}
	if json.Unmarshal(data, &in) != nil || in.HookEventName != "Stop" || in.StopHookActive == nil || *in.StopHookActive ||
		!validCodexStopID(in.SessionID) || !validCodexStopID(in.TurnID) {
		return nil
	}
	// Unknown fields, including transcript_path and last_assistant_message, are
	// ignored. The unstable transcript and arbitrary tool text are not write receipts.
	marker := codexStopMarker(markerDir, in.SessionID, in.TurnID)
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil // Existing claims, symlinks and unavailable storage never loop.
	}
	if f.Close() != nil {
		return nil
	}
	response := struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}{Decision: "block", Reason: codexWritebackReason}
	if json.NewEncoder(output).Encode(response) != nil {
		return errors.New("write Codex Stop response")
	}
	return nil
}

func validCodexStopID(id string) bool {
	if len(id) == 0 || len(id) > codexStopIDLimit || strings.TrimSpace(id) == "" {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func codexStopMarker(dir, sessionID, turnID string) string {
	// NUL is excluded from each ID, so the separator makes the pair unambiguous.
	sum := sha256.Sum256([]byte(sessionID + "\x00" + turnID))
	return filepath.Join(dir, "mesh-codex-stop-"+hex.EncodeToString(sum[:]))
}
