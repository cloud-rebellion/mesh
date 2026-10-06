// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"encoding/json"
	"strings"

	"github.com/bright-interaction/mesh/internal/vault"
)

// GotchaRow carries authored contents, rather than reinterpreting incident impact
// or root cause as a prohibition. Legacy fields retain their original meaning.
type GotchaRow struct {
	ID               string               `json:"id"`
	Title            string               `json:"title"`
	Legacy           *vault.LegacyContent `json:"legacy,omitempty"`
	Confidence       string               `json:"confidence"`
	Template         string               `json:"template,omitempty"`
	TemplateVersion  int                  `json:"template_version,omitempty"`
	Summary          string               `json:"summary,omitempty"`
	Content          string               `json:"content,omitempty"`
	MissingSections  []string             `json:"missing_sections,omitempty"`
	ContentTruncated bool                 `json:"content_truncated,omitempty"`
}

// Gotchas returns published authored troubleshooting notes and historical notes
// with an explicit anti-pattern. highOnly requires confidence=high; it is not a
// verification badge or permission to install the generated guard.
func (s *Store) Gotchas(highOnly bool) ([]GotchaRow, error) {
	rows, err := s.readDB.Query(`SELECT n.id, n.title, n.frontmatter,
 COALESCE(gn.attrs, '{}'), substr(COALESCE(si.body, ''), 1, 6001)
 FROM notes n
 LEFT JOIN nodes gn ON gn.id = 'note:' || n.id
 LEFT JOIN search_index si ON si.node_id = 'note:' || n.id
 WHERE n.type = 'gotcha'` + draftPredicate + ` ORDER BY n.title, n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GotchaRow
	for rows.Next() {
		var id, title, fmJSON, attrsJSON, body string
		if err := rows.Scan(&id, &title, &fmJSON, &attrsJSON, &body); err != nil {
			return nil, err
		}
		var fm vault.Frontmatter
		if json.Unmarshal([]byte(fmJSON), &fm) != nil {
			continue
		}
		if highOnly && strings.ToLower(strings.TrimSpace(fm.Confidence)) != "high" {
			continue
		}
		_, forbidden, _ := vault.LegacyGuidance(&fm)
		modern := fm.Template != "" || fm.TemplateVersion != 0
		if !modern && forbidden == "" {
			continue // no authored legacy anti-pattern to enforce
		}
		m := NoteMetadata{Type: string(fm.Type)}
		applyReaderMetadata(&m, fmJSON, attrsJSON)
		content := boundedText(body, 6000)
		var legacy *vault.LegacyContent
		if !modern {
			value := vault.ReadLegacy(&fm)
			legacy = &value
		}
		out = append(out, GotchaRow{ID: id, Title: title, Legacy: legacy,
			Confidence: fm.Confidence, Template: fm.Template, TemplateVersion: fm.TemplateVersion,
			Summary: m.Summary, Content: content, MissingSections: m.MissingGuidance, ContentTruncated: content != strings.TrimSpace(body)})
	}
	return out, rows.Err()
}
