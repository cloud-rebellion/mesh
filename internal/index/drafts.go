// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"fmt"
)

// DraftNotesContext browses the explicitly requested inbox. It returns metadata
// only. Scope and folder filters run before pagination, so unreadable drafts
// neither appear nor consume slots. MCP must still check current source-file ACLs.
func (s *Store) DraftNotesContext(ctx context.Context, allowedScopes map[string]bool, allowPath func(string) bool, limit, offset int) ([]NoteMetadata, error) {
	if offset < 0 {
		return nil, fmt.Errorf("draft offset must be nonnegative")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	scopeSQL, args, readable := scopePredicate(allowedScopes)
	if !readable {
		return nil, nil
	}
	ctx, cancel := withSearchDeadline(ctx)
	defer cancel()
	rows, err := s.readDB.QueryContext(ctx, `
SELECT 'note:' || n.id, n.id, n.path, n.type, n.title, n.scope,
       n.frontmatter, COALESCE(gn.attrs, '{}')
FROM notes n
LEFT JOIN nodes gn ON gn.id = 'note:' || n.id
WHERE (CASE WHEN json_valid(n.frontmatter) THEN lower(trim(COALESCE(json_extract(n.frontmatter, '$.Status'), ''))) ELSE '' END) = 'draft'`+scopeSQL+`
ORDER BY n.updated DESC, n.id, n.path`, args...)
	if err != nil {
		return nil, searchErr(ctx, err)
	}
	defer rows.Close()
	var out []NoteMetadata
	for rows.Next() {
		var m NoteMetadata
		var fmJSON, attrsJSON string
		if err := rows.Scan(&m.NodeID, &m.NoteID, &m.Path, &m.Type, &m.Title, &m.Scope, &fmJSON, &attrsJSON); err != nil {
			return nil, searchErr(ctx, err)
		}
		if allowPath != nil && !allowPath(m.Path) {
			continue
		}
		if offset > 0 {
			offset--
			continue
		}
		applyReaderMetadata(&m, fmJSON, attrsJSON)
		out = append(out, m)
		if len(out) == limit {
			break
		}
	}
	return out, searchErr(ctx, rows.Err())
}
