// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import "strings"

// AuthoredHeadingAnchors assigns the note title and template block fields distinct addresses. Line numbers
// are one-based within the body. Invalid or legacy documents retain ordinary
// Markdown addressing; untrusted markers cannot invent an authoring contract.
func AuthoredHeadingAnchors(fm *Frontmatter, body string) map[int]string {
	if fm == nil || fm.Template == "" {
		return nil
	}
	content, err := ReadAuthoring(fm, body)
	if err != nil {
		return nil
	}
	blocks := map[string]ParsedBlock{}
	for _, b := range content.Blocks {
		blocks["Block "+b.ID] = b
	}
	markers, _ := StripNonContent(body)
	headings, _ := StripFencesAndComments(body)
	markerLines, headingLines := strings.Split(markers, "\n"), strings.Split(headings, "\n")
	anchors := map[int]string{}
	var block ParsedBlock
	for i, line := range markerLines {
		h, ok := ParseATXHeading(line, headingLines[i])
		if !ok {
			continue
		}
		if h.Level <= 2 {
			block = ParsedBlock{}
			if h.Level == 1 {
				anchors[i+1] = "note-title"
			}
			if h.Level == 2 {
				block = blocks[h.Text]
			}
			continue
		}
		if h.Level != 3 || block.ID == "" {
			continue
		}
		template, _ := BlockTemplateFor(block.Template, block.Version)
		for _, field := range template.Fields {
			if field.Heading == h.Text {
				anchors[i+1] = block.Anchor + "-" + Slugify(field.Key)
				break
			}
		}
	}
	return anchors
}
