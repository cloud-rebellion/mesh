// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package extract

import (
	"encoding/json"
	"strings"
)

// WritebackToolCall identifies a durable authoring request, including MCP names.
// It is a duplicate-extraction signal, not proof that the requested write succeeded;
// callers must still confirm publication through the tool result and readback.
func WritebackToolCall(name string, input json.RawMessage) bool {
	name = strings.TrimPrefix(name, "functions.")
	if strings.HasPrefix(name, "mcp__") {
		// MCP server names are configured by the user (for example mesh-corpus).
		// Match the exact tool name after its namespace, never a prose substring.
		separator := strings.LastIndex(name, "__")
		if separator <= len("mcp__") {
			return false // no server name or no tool separator
		}
		name = name[separator+2:]
	}
	switch name {
	case "mesh_append_note", "mesh_write_entity":
		return true
	case "mesh_author_note":
		var args struct {
			Action string `json:"action"`
		}
		return json.Unmarshal(input, &args) == nil && (args.Action == "publish" || args.Action == "draft")
	default:
		return false
	}
}

// TranscriptWritebackCall recognises structured Claude or Codex tool-call records.
// Quoted prose, tool results, preparation and validation do not count as writes.
func TranscriptWritebackCall(record []byte) bool {
	var event struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		Arguments json.RawMessage `json:"arguments"`
		Message   struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Payload struct {
			Type      string          `json:"type"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"payload"`
	}
	if json.Unmarshal(record, &event) != nil {
		return false
	}
	if event.Type == "tool_use" {
		return WritebackToolCall(event.Name, event.Input)
	}
	if event.Type == "function_call" {
		return functionWritebackCall(event.Name, event.Arguments)
	}
	if event.Type == "response_item" && event.Payload.Type == "function_call" {
		return functionWritebackCall(event.Payload.Name, event.Payload.Arguments)
	}
	if event.Message.Role != "assistant" {
		return false
	}
	var blocks []tBlock
	if json.Unmarshal(event.Message.Content, &blocks) != nil {
		return false
	}
	for _, block := range blocks {
		if block.Type == "tool_use" && WritebackToolCall(block.Name, block.Input) {
			return true
		}
	}
	return false
}

func functionWritebackCall(name string, raw json.RawMessage) bool {
	// Codex function arguments are encoded as a JSON string containing JSON.
	var args string
	if json.Unmarshal(raw, &args) != nil {
		return false
	}
	return WritebackToolCall(name, json.RawMessage(args))
}
