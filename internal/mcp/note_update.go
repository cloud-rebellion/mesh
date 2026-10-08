// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
)

func (s *Server) authorizedUpdate(ctx context.Context, id string) (*vault.NoteSnapshot, *rpcError) {
	denied := &rpcError{Code: codeInvalidParams, Message: "unknown or inaccessible published note"}
	if can, set := writeAllowed(ctx); set && !can {
		return nil, &rpcError{Code: codeInvalidParams, Message: "forbidden: your role is read-only"}
	}
	if id == "" || len(id) > 256 {
		return nil, denied
	}
	metadata, err := s.store.NoteMetadataFor(ctx, []string{"note:" + id})
	if err != nil {
		return nil, denied
	}
	m, ok := metadata["note:"+id]
	sf := scopeFromCtx(ctx)
	if !ok || !filepath.IsLocal(m.Path) || (sf != nil && !vault.ScopeAllowsCSV(m.Scope, sf.AllowedRead)) {
		return nil, denied
	}
	data, err := readFetchFile(ctx, s.vaultRoot, m.Path, batchFetchFileBytes)
	if err != nil || !fetchFileScopeAllowed(data, sf) {
		return nil, denied
	}
	header, _, had := vault.SplitFrontmatter(string(data))
	fm, _, err := vault.ParseFrontmatter([]byte(header))
	if err != nil || !had || fm.ID != id || vault.IsDraft(fm) {
		return nil, denied
	}
	if sf != nil {
		for _, scope := range fm.EffectiveScopes() {
			if sf.CanWrite == nil || !sf.CanWrite(scope) {
				return nil, denied
			}
		}
	}
	snapshot, err := vault.PublishedNoteSnapshot(m.Path, id, data)
	if err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: ScrubPathsUnder(err.Error(), s.vaultRoot)}
	}
	return snapshot, nil
}

func (s *Server) toolPrepareUpdate(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var a struct {
		ID    string `json:"id"`
		Draft bool   `json:"draft,omitempty"`
	}
	if err := decodeAuthoring(raw, &a); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	var snapshot *vault.NoteSnapshot
	var rerr *rpcError
	if a.Draft {
		if can, set := writeAllowed(ctx); set && !can {
			return nil, &rpcError{Code: codeInvalidParams, Message: "forbidden: your role is read-only"}
		}
		original, _, denied := s.authorizedDraft(ctx, a.ID)
		if denied != nil {
			return nil, denied
		}
		// Reread under current authorization; the response's exact revision is what
		// the normal publication path fences. No clipped/raw replacement parser.
		data, err := readFetchFile(ctx, s.vaultRoot, original.Path, batchFetchFileBytes)
		if err != nil || !fetchFileScopeAllowed(data, scopeFromCtx(ctx)) || vault.ContentRevision(data) != original.Revision {
			return nil, &rpcError{Code: codeInvalidParams, Message: "draft changed during preparation"}
		}
		snapshot, err = vault.DraftNoteSnapshot(original.Path, a.ID, data)
		if err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: ScrubPathsUnder(err.Error(), s.vaultRoot)}
		}
	} else {
		snapshot, rerr = s.authorizedUpdate(ctx, a.ID)
	}
	if rerr != nil {
		return nil, rerr
	}
	v, fm := snapshot.Spec, snapshot.Frontmatter
	note := authoringArgs{UpdateID: v.UpdateID, UpdateRevision: v.UpdateRevision, DraftID: v.DraftID, DraftRevision: v.DraftRevision,
		Type: string(v.Type), Title: v.Title, Template: v.Template, TemplateVersion: v.TemplateVersion,
		Summary: v.Summary, Sections: v.Sections, Blocks: v.Blocks,
		Collections: v.Collections, Related: v.Related, Supersedes: v.Supersedes, Tags: v.Tags,
		Status: v.Status, Severity: v.Severity, Confidence: v.Confidence, ReviewBy: v.ReviewBy,
	}
	// Omitted scope on an update inherits the target's complete current audience.
	payload := map[string]any{"id": a.ID, "revision": snapshot.Revision, "note": note,
		"saved": false, "draft": a.Draft, "created": fm.Created, "original_author": fm.Author,
		"scopes": fm.EffectiveScopes(), "previous_verified_at": fm.VerifiedAt,
		"verification": "Historical evidence remains in the body. verified_at is not prefilled; set it only after recording checks for the edited content.",
		"workflow":     "Edit the complete note object, retaining relevant evidence and links; validate and publish through mesh_author_note. Stale revisions require rereading and reconciliation. Incomplete edits use a separate linked draft.",
	}
	return authoringContentResult(payload, fm.Source, fm.SourceURL, snapshot.Path)
}

// Prefilled prose keeps the same external-content boundary as ordinary fetch.
// Bound the complete editable response; silently clipped content cannot be a
// safe basis for full replacement.
func authoringContentResult(payload any, source, sourceURL, rel string) (any, *rpcError) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, internalErr(err)
	}
	if len(encoded) > authoringMaxBytes {
		return nil, &rpcError{Code: codeInvalidParams, Message: "complete authoring response exceeds the byte limit; reconcile a bounded note before editing"}
	}
	if !strings.HasPrefix(source, importSourcePrefix) {
		if ps, imported := importedSource(rel); imported {
			source = ps
		}
	}
	if strings.HasPrefix(source, importSourcePrefix) {
		return rawText(wrapUntrusted(source, sourceURL, string(encoded))), nil
	}
	return rawText(string(encoded)), nil
}
