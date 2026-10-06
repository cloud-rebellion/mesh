// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
package mcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/vault"
)

// AuthoringReferences shares the writer's fence-aware reference scan with review
// publication. Code contents remain literal examples; surrounding context is checked.
func AuthoringReferences(spec vault.NewNoteSpec) ([]string, error) {
	refs := append(append(append([]string{}, spec.Collections...), spec.Related...), spec.Supersedes...)
	var body strings.Builder
	body.WriteString("# Authoring reference scan\n\n")
	body.WriteString(spec.Summary)
	keys := make([]string, 0, len(spec.Sections))
	for key := range spec.Sections {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		body.WriteString("\n\n")
		body.WriteString(spec.Sections[key])
	}
	for _, block := range spec.Blocks {
		keys = keys[:0]
		for key := range block.Fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if block.Template == "code" && key == "code" {
				continue
			}
			body.WriteString("\n\n")
			body.WriteString(block.Fields[key])
		}
	}
	parsed, err := index.Parse("authoring.md", []byte(body.String()))
	if err != nil {
		return nil, err
	}
	for _, link := range parsed.Links {
		refs = append(refs, link.Target)
	}
	if len(refs) > 128 {
		return nil, fmt.Errorf("too many references")
	}
	return refs, nil
}

// AuthoringReferenceAnchorValid uses the same exact/ambiguous heading resolution
// as MCP section fetches, including stable authored-section and block identities.
func AuthoringReferenceAnchorValid(data []byte, anchor string) bool {
	if anchor == "" {
		return true
	}
	_, matches := resolveAnchorSection(string(data), anchor)
	return matches == 1
}
