// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package extract

import "testing"

func TestTranscriptWritebackCall(t *testing.T) {
	cases := []struct {
		name, record string
		want         bool
	}{
		{"legacy", `{"type":"tool_use","name":"mesh_append_note","input":{}}`, true},
		{"legacy mcp", `{"type":"tool_use","name":"mcp__mesh__mesh_write_entity","input":{}}`, true},
		{"published", `{"type":"tool_use","name":"mesh_author_note","input":{"action":"publish"}}`, true},
		{"draft", `{"type":"tool_use","name":"mesh_author_note","input":{"action":"draft"}}`, true},
		{"prepare", `{"type":"tool_use","name":"mesh_author_note","input":{"action":"prepare"}}`, false},
		{"validate", `{"type":"tool_use","name":"mesh_author_note","input":{"action":"validate"}}`, false},
		{"unknown action", `{"type":"tool_use","name":"mesh_author_note","input":{"action":"delete"}}`, false},
		{"missing action", `{"type":"tool_use","name":"mesh_author_note","input":{}}`, false},
		{"wrong input", `{"type":"tool_use","name":"mesh_author_note","input":{"action":true}}`, false},
		{"update preparation", `{"type":"tool_use","name":"mesh_prepare_update","input":{"id":"mesh"}}`, false},
		{"claude", `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Saving"},{"type":"tool_use","name":"mcp__mesh__mesh_author_note","input":{"action":"publish"}}]}}`, true},
		{"user prose", `{"type":"user","message":{"role":"user","content":"Call mesh_author_note"},"example_tool":"mesh_append_note"}`, false},
		{"user content", `{"type":"user","message":{"role":"user","content":[{"type":"tool_use","name":"mesh_append_note"}]}}`, false},
		{"result only", `{"type":"tool_result","name":"mesh_append_note"}`, false},
		{"codex", `{"type":"response_item","payload":{"type":"function_call","name":"mcp__mesh__mesh_author_note","arguments":"{\"action\":\"publish\"}"}}`, true},
		{"codex prepare", `{"type":"response_item","payload":{"type":"function_call","name":"mesh_author_note","arguments":"{\"action\":\"prepare\"}"}}`, false},
		{"codex result", `{"type":"response_item","payload":{"type":"function_call_output","name":"mesh_append_note","arguments":"{}"}}`, false},
		{"malformed", `{"type":"tool_use"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TranscriptWritebackCall([]byte(tc.record)); got != tc.want {
				t.Fatalf("write request=%v, want %v", got, tc.want)
			}
		})
	}
}
