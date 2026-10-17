// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package extract

import (
	"encoding/json"
)

// digestTranscriptRecord accepts the existing Claude message representation and
// Codex's persisted response_item envelope. Codex event_msg mirrors are skipped
// so the same user/assistant text is not counted twice. Metadata, reasoning,
// inter-agent messages, images, audio and tool results never enter the digest.
func digestTranscriptRecord(record []byte) (string, []tBlock) {
	var envelope struct {
		tLine
		Payload  json.RawMessage `json:"payload"`
		Metadata struct {
			CompactionOutput          bool    `json:"compaction_output"`
			DeliveredAssistantMessage *string `json:"delivered_assistant_message"`
		} `json:"metadata"`
	}
	if json.Unmarshal(record, &envelope) != nil {
		return "", nil
	}
	if envelope.Type != "response_item" {
		if envelope.Type != "user" && envelope.Type != "assistant" {
			return "", nil
		}
		if envelope.Message.Role != envelope.Type {
			return "", nil
		}
		return envelope.Message.Role, decodeContent(envelope.Message.Content)
	}
	// Codex also serializes messaging-tool delivery as ordinary assistant
	// output_text for older readers. Its envelope marker carries that distinct
	// provenance; it must not become original session narration here.
	if envelope.Metadata.CompactionOutput || envelope.Metadata.DeliveredAssistantMessage != nil {
		return "", nil
	}
	var item struct {
		Type      string   `json:"type"`
		Role      string   `json:"role"`
		Content   []tBlock `json:"content"`
		Name      string   `json:"name"`
		Namespace string   `json:"namespace"`
	}
	if json.Unmarshal(envelope.Payload, &item) != nil {
		return "", nil
	}
	switch item.Type {
	case "message":
		var expected string
		switch item.Role {
		case "user":
			expected = "input_text"
		case "assistant":
			expected = "output_text"
		default:
			return "", nil
		}
		var blocks []tBlock
		for _, block := range item.Content {
			if block.Type == expected {
				blocks = append(blocks, tBlock{Type: "text", Text: block.Text})
			}
		}
		return item.Role, blocks
	case "function_call", "custom_tool_call":
		name := item.Name
		if item.Namespace != "" {
			name = item.Namespace + "." + name
		}
		if !validDigestToolName(name) {
			return "", nil
		}
		// A tool name conveys context, without sending commands, patches,
		// credentials or other raw arguments to the completion provider.
		return "assistant", []tBlock{{Type: "tool_use", Name: name}}
	default:
		return "", nil
	}
}

// Names are bounded protocol labels; a transcript cannot introduce a new turn
// by putting a newline or a delimiter in a tool name.
func validDigestToolName(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	for _, ch := range name {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' || ch == '.' || ch == ':' {
			continue
		}
		return false
	}
	return true
}
