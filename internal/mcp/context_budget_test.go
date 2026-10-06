// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"encoding/json"
	"testing"
)

func TestMCPContextBudget(t *testing.T) {
	specs := ToolSpecs()
	wire, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	// Some MCP clients repeat initialize instructions beside every tool. Budget
	// that conservative shape as well as Mesh's own tools/list payload.
	// The versioned authoring workflow adds six tools. Its full schema/library
	// remain fetch-on-demand; the shared action tool avoids three repeated schemas.
	repeatedContractBytes := len(wire) + len(specs)*len(contractText)
	t.Logf("contract=%d bytes tools=%d bytes repeated-contract-shape=%d bytes tools=%d",
		len(contractText), len(wire), repeatedContractBytes, len(specs))
	if len(contractText) > 700 {
		t.Errorf("MCP initialize contract is %d bytes, budget is 700", len(contractText))
	}
	if len(wire) > 9_000 {
		t.Errorf("MCP tools/list payload is %d bytes, budget is 9000", len(wire))
	}
	if repeatedContractBytes > 26_000 {
		t.Errorf("repeated-contract client shape is %d bytes, budget is 26000", repeatedContractBytes)
	}
	for _, spec := range specs {
		name, _ := spec["name"].(string)
		description, _ := spec["description"].(string)
		if len(description) > 200 {
			t.Errorf("tool %s description is %d bytes, budget is 200", name, len(description))
		}
	}
}
