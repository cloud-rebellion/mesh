// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/bright-interaction/mesh/internal/vault"
)

// SectionAddress is navigation metadata, never a claim that its contents have
// been verified. The prose stays in the source note and is fetched separately.
type SectionAddress struct {
	Key     string `json:"key"`
	Heading string `json:"heading"`
	Anchor  string `json:"anchor"`
}

type authoringMetadata struct {
	Template          string           `json:"template,omitempty"`
	Version           int              `json:"version,omitempty"`
	Summary           string           `json:"summary,omitempty"`
	Sections          []SectionAddress `json:"sections,omitempty"`
	Missing           []string         `json:"missing,omitempty"`
	SectionsTruncated bool             `json:"sections_truncated,omitempty"`
}

func readerAuthoring(pn *ParsedNote) authoringMetadata {
	a, err := vault.ReadAuthoring(pn.FM, pn.Body)
	return readerAuthoringFrom(pn, a, err)
}

func readerAuthoringFrom(pn *ParsedNote, a vault.AuthoringContent, err error) authoringMetadata {
	out := authoringMetadata{Template: pn.FM.Template, Version: pn.FM.TemplateVersion}
	if err != nil {
		out.Missing = []string{"valid authored structure"}
		return out
	}
	out.Missing = a.MissingSections
	summary, _ := vault.StripComments(a.Summary)
	if !vault.Unfilled(summary) {
		out.Summary = boundedText(summary, 320)
	}
	for _, section := range a.OrderedSections {
		if vault.Unfilled(section.Text) {
			continue
		}
		// Templates are finite, but retain a bound if future templates expand.
		if len(out.Sections) >= 12 {
			out.SectionsTruncated = true
			break
		}
		out.Sections = append(out.Sections, SectionAddress{Key: boundedText(section.Key, 80), Heading: boundedText(section.Heading, 120), Anchor: boundedText(section.Anchor, 160)})
	}
	for _, block := range a.Blocks {
		if len(out.Sections) >= 12 {
			out.SectionsTruncated = true
			break
		}
		out.Sections = append(out.Sections, SectionAddress{Key: "block-" + block.ID, Heading: boundedText(block.Template+": "+block.ID, 120), Anchor: block.Anchor})
	}
	return out
}

func boundedText(text string, max int) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	return string([]rune(text)[:max])
}

// draftPredicate deliberately uses the notes-table snapshot, before FTS LIMIT.
// A stale graph or a high-ranked draft cannot hide a readable published match.
const draftPredicate = ` AND (CASE WHEN json_valid(n.frontmatter) THEN lower(trim(COALESCE(json_extract(n.frontmatter, '$.Status'), ''))) ELSE '' END) != 'draft'`

type behaviorGuidance struct {
	Recommended string `json:"recommended,omitempty"`
	Forbidden   string `json:"forbidden,omitempty"`
}

var authoredBullet = regexp.MustCompile(`^(?:[-*+]|[0-9]+[.)])\s+`)

// readerBehavior keeps the contradiction heuristic narrow: it reads explicit
// behavior statements in sections that prescribe actions. Descriptions of impact,
// causes, observations, timelines and code examples never become prohibitions.
// This is a review hint, not proof of equivalence or of a confirmed contradiction.
func readerBehavior(pn *ParsedNote) behaviorGuidance {
	a, err := vault.ReadAuthoring(pn.FM, pn.Body)
	return readerBehaviorFrom(pn, a, err)
}

func readerBehaviorFrom(pn *ParsedNote, a vault.AuthoringContent, err error) behaviorGuidance {
	if err != nil || pn.FM.Template == "" || vault.IsDraft(pn.FM) {
		return behaviorGuidance{}
	}
	keys := map[string]map[string]bool{
		"decision":        {"decision": true},
		"troubleshooting": {"remedy": true},
		"method":          {"approach": true, "limitations": true},
		"procedure":       {"steps": true, "recovery": true},
	}
	var recommended, forbidden []string
	for _, section := range a.OrderedSections {
		if !keys[a.Template][section.Key] {
			continue
		}
		clean, _ := vault.StripNonContent(section.Text)
		for _, raw := range strings.FieldsFunc(clean, func(r rune) bool { return r == '\n' || r == ';' }) {
			statement := strings.TrimSpace(raw)
			if strings.HasPrefix(statement, ">") || strings.HasPrefix(statement, "\"") || strings.HasPrefix(statement, "'") {
				continue
			}
			statement = authoredBullet.ReplaceAllString(statement, "")
			lower := strings.ToLower(statement)
			negative := false
			for _, prefix := range []string{"do not ", "don't ", "never ", "must not ", "avoid "} {
				if strings.HasPrefix(lower, prefix) {
					forbidden = append(forbidden, boundedText(statement[len(prefix):], 500))
					negative = true
					break
				}
			}
			if negative {
				continue
			}
			for _, prefix := range []string{"use ", "always ", "must ", "require ", "preserve ", "keep ", "verify "} {
				if strings.HasPrefix(lower, prefix) {
					if prefix == "must " || prefix == "always " {
						statement = statement[len(prefix):]
					}
					recommended = append(recommended, boundedText(statement, 500))
					break
				}
			}
		}
	}
	return behaviorGuidance{Recommended: boundedText(strings.Join(recommended, "\n"), 2000), Forbidden: boundedText(strings.Join(forbidden, "\n"), 2000)}
}
