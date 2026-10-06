// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import "github.com/bright-interaction/mesh/internal/vault"

func contextualSectionText(fm *vault.Frontmatter, body, key string) string {
	content, err := vault.ReadAuthoring(fm, body)
	if err == nil {
		if key == "summary" {
			return content.Summary
		}
		if text := content.Sections[key]; text != "" {
			return text
		}
	}
	return vault.SectionText(body, key)
}

// A code/evidence field fetched by its child heading must carry its parent
// block's provenance and limits. Fetching the whole block already includes them.
func addSelectedBlockContext(fields map[string]string, fm *vault.Frontmatter, body string, anchors []string) {
	content, err := vault.ReadAuthoring(fm, body)
	if err != nil {
		return
	}
	for _, block := range content.Blocks {
		_, parentStart, parentEnd, matches := resolveAuthoredAnchorSpan(fm, body, block.Anchor)
		if matches != 1 {
			continue
		}
		needed := false
		for _, anchor := range anchors {
			_, start, end, matches := resolveAuthoredAnchorSpan(fm, body, anchor)
			needed = needed || (matches == 1 && start > parentStart && end <= parentEnd)
		}
		if !needed {
			continue
		}
		prefix := "block:" + block.ID + ":"
		fields[prefix+"template"] = block.Template
		for _, key := range []string{"purpose", "source", "revision", "illustrative", "explanation", "verification", "limitations", "claim", "observed_at", "uncertainty", "context", "gaps", "trigger", "prerequisites", "completion", "interpretation", "owner", "due", "status"} {
			if text := block.Fields[key]; text != "" {
				fields[prefix+key] = text
			}
		}
	}
}
