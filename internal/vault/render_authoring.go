// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"fmt"
	"strings"
)

func renderBody(fm *Frontmatter) string {
	if fm.Template == "" {
		return renderLegacyBody(fm)
	}
	t, err := TemplateFor(fm.Template, fm.TemplateVersion)
	if err != nil {
		return ""
	} // planning rejects unknown templates before rendering
	var b strings.Builder
	renderAuthoredSection(&b, "Summary", "summary", fm.BodySummary, "Summarize the substance, scope and important uncertainty.")
	for _, section := range t.Sections {
		renderAuthoredSection(&b, section.Heading, section.Key, fm.Sections[section.Key], section.Guidance)
	}
	for _, block := range fm.BlockContents {
		t, _ := BlockTemplateFor(block.Template, block.Version)
		fmt.Fprintf(&b, "## Block %s\n<!-- mesh:block %s v%d %s -->\n\n", block.ID, block.Template, block.Version, block.ID)
		for _, f := range t.Fields {
			text, provided := block.Fields[f.Key]
			if !f.Required && (!provided || missingProse(text)) {
				continue
			}
			fmt.Fprintf(&b, "### %s\n\n", f.Heading)
			if missingProse(text) {
				fmt.Fprintf(&b, "<!-- TODO: %s -->\n\n", f.Guidance)
			} else if block.Template == "code" && f.Key == "code" {
				b.WriteString(fencedCode(text, block.Fields["language"]))
				b.WriteString("\n\n")
			} else {
				b.WriteString(text)
				b.WriteString("\n\n")
			}
		}
	}
	if len(fm.Related) > 0 {
		b.WriteString("## Related\n\n")
		for _, id := range fm.Related {
			fmt.Fprintf(&b, "- [[%s]]\n", id)
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func renderAuthoredSection(b *strings.Builder, heading, key, text, guidance string) {
	fmt.Fprintf(b, "## %s\n<!-- mesh:section %s -->\n\n", heading, key)
	if missingProse(text) {
		fmt.Fprintf(b, "<!-- TODO: %s -->\n\n", guidance)
	} else {
		b.WriteString(text)
		b.WriteString("\n\n")
	}
}

func fencedCode(code, language string) string {
	// Make the outer fence longer than any run in the source, preserving nested
	// Markdown examples and preventing code from escaping into authored headings.
	run, maxRun := 0, 2
	for _, r := range code {
		if r == rune(96) {
			run++
			if run > maxRun {
				maxRun = run
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat(string(rune(96)), maxRun+1)
	return fence + strings.TrimSpace(language) + "\n" + code + "\n" + fence
}
