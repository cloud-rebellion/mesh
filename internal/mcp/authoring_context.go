// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import "github.com/bright-interaction/mesh/internal/vault"

type purposeContextVersion struct {
	template string
	version  int
}

// These are contextual sections of the approved versioned templates, not a new
// authoring schema. Keep only applicability, supporting evidence and limits that
// qualify a partial answer. Payload sections stay in the requested body; the
// existing serialized-context bound and incomplete warning still apply.
var purposeContextSections = map[purposeContextVersion][]string{
	{"entity", 1}:              {"current_state", "interfaces"},
	{"method", 1}:              {"applicability", "verification", "limitations"},
	{"procedure", 1}:           {"prerequisites", "expected_result", "verification", "recovery"},
	{"post-mortem", 1}:         {"impact", "root_cause", "resolution"},
	{"decision", 1}:            {"context", "rationale", "consequences"},
	{"finding", 1}:             {"evidence", "limitations"},
	{"plan", 1}:                {"context", "dependencies"},
	{"troubleshooting", 1}:     {"symptoms", "cause", "verification"},
	{"concept", 1}:             {"applicability", "examples"},
	{"reference", 1}:           {"scope", "interpretation", "sources"},
	{"review", 1}:              {"criteria", "evidence", "gaps"},
	{"status", 1}:              {"current_state", "verification", "blockers"},
	{"index", 1}:               {"scope"},
	{"prd", 1}:                 {"scope", "dependencies", "acceptance", "open_questions"},
	{"trd", 1}:                 {"context", "quality", "dependencies", "verification", "open_questions"},
	{"app-flow", 1}:            {"actors", "alternatives", "acceptance"},
	{"design-brief", 1}:        {"constraints", "references", "acceptance"},
	{"backend-schema", 1}:      {"scope", "access", "evolution", "operations", "verification"},
	{"implementation-plan", 1}: {"scope", "verification", "rollout", "open_questions"},
}

func addPurposeSectionContext(fields map[string]string, fm *vault.Frontmatter, body string) {
	content, err := vault.ReadAuthoring(fm, body)
	if err != nil {
		return
	}
	if content.Summary != "" {
		fields["summary"] = content.Summary
	}
	if fm.Template == "" && fm.TemplateVersion == 0 {
		// Preserve the historical reader's heading lookups. Never reinterpret its
		// shorthand as a modern section or infer a purpose-specific migration.
		for _, key := range []string{"summary", "applicability", "prerequisites", "limitations", "evidence", "verification", "dependencies", "cause", "root_cause", "resolution", "impact", "consequences", "recovery"} {
			if text := contextualSectionText(fm, body, key); text != "" {
				fields[key] = text
			}
		}
		return
	}
	// ReadAuthoring resolves the exact catalog version and rejects foreign keys.
	// An unreviewed future version receives only summary and the existing warning,
	// rather than inheriting assumptions from a previous template contract.
	for _, key := range purposeContextSections[purposeContextVersion{content.Template, content.Version}] {
		if text := content.Sections[key]; text != "" {
			fields[key] = text
		}
	}
}

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
		definition, err := vault.BlockTemplateFor(block.Template, block.Version)
		if err != nil {
			continue
		}
		// Select context from this block's actual approved fields. Do not duplicate
		// a large primary payload (code/table/diagram/etc.) in the header. Evidence
		// results and verification checks/results are small contextual proof, not
		// a universal prose field that every kind of block must pretend to have.
		payload := map[string]string{
			"code": "code", "table": "table", "diagram": "diagram",
			"timeline": "events", "comparison": "comparison", "worked-example": "walkthrough",
			"checklist": "items", "recovery": "steps",
		}[block.Template]
		for _, field := range definition.Fields {
			if field.Key == payload {
				continue
			}
			if text := block.Fields[field.Key]; text != "" {
				fields[prefix+field.Key] = text
			}
		}
	}
}
