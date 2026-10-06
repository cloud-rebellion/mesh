// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
)

const authoringMaxBytes = 96 << 10

// A single wire format feeds preview, validation, drafts and durable publication.
// Content is authored once and rendered into the body by the vault package.
type authoringArgs struct {
	DraftID         string            `json:"draft_id,omitempty"`
	DraftRevision   string            `json:"draft_revision,omitempty"`
	UpdateID        string            `json:"update_id,omitempty"`
	UpdateRevision  string            `json:"update_revision,omitempty"`
	VerifiedAt      string            `json:"verified_at,omitempty"`
	Type            string            `json:"type,omitempty"`
	Title           string            `json:"title"`
	Template        string            `json:"template,omitempty"`
	TemplateVersion int               `json:"template_version,omitempty"`
	Summary         string            `json:"summary"`
	Sections        map[string]string `json:"sections,omitempty"`
	Blocks          []vault.BlockSpec `json:"blocks,omitempty"`
	Collections     []string          `json:"collections,omitempty"`
	Related         []string          `json:"related,omitempty"`
	Supersedes      []string          `json:"supersedes,omitempty"`
	Tags            []string          `json:"tags,omitempty"`
	Status          string            `json:"status,omitempty"`
	Severity        string            `json:"severity,omitempty"`
	Author          string            `json:"author,omitempty"`
	Source          string            `json:"source,omitempty"`
	SourceURL       string            `json:"source_url,omitempty"`
	Confidence      string            `json:"confidence,omitempty"`
	ReviewBy        string            `json:"review_by,omitempty"`
	Scope           string            `json:"scope,omitempty"`
}

func authoringProperties() map[string]any {
	str := map[string]any{"type": "string"}
	list := map[string]any{"type": "array", "items": str, "maxItems": 32}
	return map[string]any{
		"type": str, "title": str, "template": str, "draft_id": str, "draft_revision": str, "update_id": str, "update_revision": str, "verified_at": str,
		"template_version": map[string]any{"type": "integer", "minimum": 1},
		"summary":          str,
		"sections":         map[string]any{"type": "object", "additionalProperties": str, "description": "Authored Markdown keyed by the selected template's section keys. State unknowns explicitly; never invent evidence."},
		"blocks": map[string]any{"type": "array", "maxItems": 16, "items": map[string]any{
			"type": "object", "required": []string{"template", "id", "fields"}, "additionalProperties": false,
			"properties": map[string]any{"template": str, "version": map[string]any{"type": "integer", "minimum": 1}, "id": str, "fields": map[string]any{"type": "object", "additionalProperties": str}},
		}},
		"collections": list, "related": list, "supersedes": list, "tags": list,
		"status": str, "severity": str, "author": str, "source": str,
		"source_url": str, "confidence": str, "review_by": str, "scope": str,
	}
}

func authoringToolSpecs() []map[string]any {
	str := map[string]any{"type": "string"}
	lookup := map[string]any{"type": "object", "required": []string{"template"}, "additionalProperties": false,
		"properties": map[string]any{"template": str, "version": map[string]any{"type": "integer", "minimum": 1}}}
	tools := []map[string]any{
		{"name": "mesh_prepare_update", "description": "Prepare a writable published note with its content and revision. Edit, validate and publish through mesh_author_note; retain identity, template/version and scopes. Routine updates need no approval.", "inputSchema": map[string]any{"type": "object", "required": []string{"id"}, "additionalProperties": false, "properties": map[string]any{"id": str}}},
		{"name": "mesh_drafts", "description": "Browse the explicit draft inbox with current access checks. Returns revisions for safe draft completion; ordinary search excludes drafts.", "inputSchema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}, "offset": map[string]any{"type": "integer", "minimum": 0}}}},
		{"name": "mesh_templates", "description": "Compact versioned catalog of note templates and optional blocks. Choose a purpose, then fetch only its template and selected blocks.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
		{"name": "mesh_note_template", "description": "Fetch one note template, section keys and authoring guidance. Semantic type is separate from template choice.", "inputSchema": lookup},
		{"name": "mesh_block_template", "description": "Fetch one optional block's fields, including provenance, evidence and limitations where applicable.", "inputSchema": lookup},
	}

	tools = append(tools, map[string]any{
		"name": "mesh_author_note", "description": "Prepare, validate, save a draft or publish using one authoring format. Fetch mesh_note_template for the note schema. Existing templates need no note approval.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"action", "note"}, "additionalProperties": false, "properties": map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"prepare", "validate", "draft", "publish"}},
			"note":   map[string]any{"type": "object", "description": "Authoring object; its schema is returned by mesh_note_template."},
		}},
	})
	for _, item := range []struct{ name, description string }{
		{"mesh_append_note", "Publish complete template-based knowledge. Fetch mesh_note_template for fields and guidance; no per-note approval."},
		{"mesh_write_entity", "Publish an entity overview with the shared template authoring format; no per-note approval."},
	} {
		// Keep compatibility entry points compact too. Writers fetch the complete,
		// strict schema from mesh_note_template; duplicating it in two tool specs
		// makes every reader pay for a format it does not use. Runtime decoding
		// still rejects unknown and retired fields for every publication surface.
		properties := map[string]any{"title": str, "type": str, "template": str, "summary": str, "scope": str}
		tools = append(tools, map[string]any{"name": item.name, "description": item.description,
			"inputSchema": map[string]any{"type": "object", "required": []string{"title"}, "additionalProperties": true, "properties": properties}})
	}

	tools = append(tools, map[string]any{
		"name": "mesh_propose_template", "description": "Save a proposed reusable template or block and an example as a draft for human review. Does not register or activate it; library changes require the user's approval.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"kind", "title", "rationale", "definition", "example"}, "additionalProperties": false,
			"properties": map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"template", "block"}}, "title": str, "rationale": str, "definition": str, "example": str, "scope": str}},
	})
	return tools
}

func decodeAuthoring(raw json.RawMessage, dst any) error {
	if len(raw) > authoringMaxBytes {
		return fmt.Errorf("authoring arguments exceed %d bytes", authoringMaxBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return fmt.Errorf("authoring arguments must be an object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		// Do not echo a decoder error containing arbitrary supplied content.
		return fmt.Errorf("invalid authoring arguments; use the versioned template fields (legacy do/dont/why inputs are retired)")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one authoring object")
	}
	return nil
}

func templateCatalog() map[string]any {
	notes := make([]map[string]any, 0)
	for _, t := range vault.Templates() {
		notes = append(notes, map[string]any{"id": t.ID, "version": t.Version, "type": t.Type, "purpose": t.Purpose})
	}
	blocks := make([]map[string]any, 0)
	for _, b := range vault.BlockTemplates() {
		blocks = append(blocks, map[string]any{"id": b.ID, "version": b.Version, "purpose": b.Purpose})
	}
	return map[string]any{"templates": notes, "blocks": blocks, "authoring_version": 1,
		"workflow": "fetch template and selected blocks; prepare; fill known facts and explicit uncertainty; validate; publish or save draft",
		"quality":  "Validation establishes structure, not factual correctness. Library additions require human approval."}
}

func (s *Server) toolTemplate(raw json.RawMessage, block bool) (any, *rpcError) {
	var a struct {
		Template string `json:"template"`
		Version  int    `json:"version"`
	}
	if err := decodeAuthoring(raw, &a); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if block {
		t, err := vault.BlockTemplateFor(a.Template, a.Version)
		if err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		return textResult(t), nil
	}
	t, err := vault.TemplateFor(a.Template, a.Version)
	if err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	return textResult(map[string]any{"template": t, "authoring_schema": map[string]any{"type": "object", "properties": authoringProperties(), "required": []string{"title"}, "additionalProperties": false}}), nil
}

func (s *Server) authoringSpec(ctx context.Context, a authoringArgs, forceType string) (vault.NewNoteSpec, *rpcError) {
	if can, set := writeAllowed(ctx); set && !can {
		return vault.NewNoteSpec{}, &rpcError{Code: codeInvalidParams, Message: "forbidden: your role is read-only"}
	}
	var scopes []string
	want := strings.TrimSpace(a.Scope)
	var update *vault.NoteSnapshot
	if a.UpdateID != "" {
		var rerr *rpcError
		update, rerr = s.authorizedUpdate(ctx, a.UpdateID)
		if rerr != nil {
			return vault.NewNoteSpec{}, rerr
		}
		// Retain all existing scopes. New-note default scope selection must never
		// silently remove audiences from a published note (including multi-scope notes).
		scopes = append([]string{}, update.Frontmatter.EffectiveScopes()...)
		if want != "" {
			scopes = []string{want}
		}
	} else if sf := scopeFromCtx(ctx); sf != nil {
		if want == "" {
			want = sf.WriteScope
		}
		if want == "" {
			return vault.NewNoteSpec{}, &rpcError{Code: codeInvalidParams, Message: "your account is not in a single scope; pass an explicit `scope`"}
		}
		if sf.CanWrite == nil || !sf.CanWrite(want) {
			return vault.NewNoteSpec{}, &rpcError{Code: codeInvalidParams, Message: "forbidden: you cannot write notes in the requested scope"}
		}
	}
	if update == nil && want != "" {
		scopes = []string{want}
	}
	if forceType != "" {
		a.Type = forceType
	}
	s.mu.RLock()
	agent := s.agent
	s.mu.RUnlock()
	if agent == "" {
		agent = "mesh-mcp"
	}
	if caller, hosted := TeamCallerFromContext(ctx); hosted {
		a.Author, agent = caller.User, "mesh-hosted-mcp"
	}
	if strings.TrimSpace(a.Source) == "" {
		a.Source = "agent"
	}
	spec, err := vault.NormalizeSpec(vault.NewNoteSpec{
		Type: vault.NoteType(a.Type), Title: a.Title, Template: a.Template, TemplateVersion: a.TemplateVersion,
		DraftID: a.DraftID, DraftRevision: a.DraftRevision, VerifiedAt: a.VerifiedAt,
		UpdateID: a.UpdateID, UpdateRevision: a.UpdateRevision,
		Summary: a.Summary, Sections: a.Sections, Blocks: a.Blocks, Collections: a.Collections,
		Related: a.Related, Supersedes: a.Supersedes, Tags: a.Tags, Status: a.Status,
		Severity: a.Severity, Author: a.Author, Agent: agent, By: agent, Source: a.Source,
		SourceURL: a.SourceURL, Confidence: a.Confidence, ReviewBy: a.ReviewBy, Scope: scopes,
	})
	if err != nil {
		return spec, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if update != nil {
		spec.UpdatePath = update.Path
	}
	if spec.DraftID != "" {
		snapshot, _, rerr := s.authorizedDraft(ctx, spec.DraftID)
		if rerr != nil {
			return spec, rerr
		}
		spec.DraftPath = snapshot.Path
	}
	return spec, nil
}

func (s *Server) toolPrepareNote(ctx context.Context, raw json.RawMessage, validate bool) (any, *rpcError) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var a authoringArgs
	if err := decodeAuthoring(raw, &a); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if a.DraftID != "" && a.DraftRevision == "" && !validate {
		_, revision, rerr := s.authorizedDraft(ctx, a.DraftID)
		if rerr != nil {
			return nil, rerr
		}
		a.DraftRevision = revision
	}
	spec, rerr := s.authoringSpec(ctx, a, "")
	if rerr != nil {
		return nil, rerr
	}
	issues := vault.MissingContent(spec)
	if err := s.validateAuthoringLinks(ctx, spec); err != nil {
		issues = append(issues, "unknown, invalid or inaccessible authoring reference")
	}
	if validate {
		if err := vault.ValidateSpec(spec); err != nil && len(issues) == 0 {
			issues = append(issues, err.Error())
		}
		if spec.UpdateID != "" && len(issues) == 0 {
			if _, err := vault.PrepareNoteContext(ctx, s.vaultRoot, spec); err != nil {
				issues = append(issues, ScrubPathsUnder(err.Error(), s.vaultRoot))
			}
		}
		return textResult(map[string]any{"valid": len(issues) == 0, "issues": issues, "factual_correctness": "not assessed"}), nil
	}
	if spec.UpdateID == "" {
		spec.Status = "draft"
	}
	p, err := vault.PrepareNoteContext(ctx, s.vaultRoot, spec)
	if err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: ScrubPathsUnder(err.Error(), s.vaultRoot)}
	}
	source, sourceURL := frontmatterProvenance(string(p.Content))
	rel, _ := filepath.Rel(s.vaultRoot, p.Result.Path)
	return authoringContentResult(map[string]any{"id": p.Result.ID, "markdown": string(p.Content), "issues": issues,
		"saved": false, "identity_reserved": false, "draft_revision": a.DraftRevision, "update_revision": a.UpdateRevision, "template": spec.Template, "template_version": spec.TemplateVersion}, source, sourceURL, rel)
}

func (s *Server) toolSaveDraft(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a authoringArgs
	if err := decodeAuthoring(raw, &a); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	a.Status = "draft"
	data, _ := json.Marshal(a)
	return s.toolWrite(ctx, data, "")
}

func (s *Server) toolProposeTemplate(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct{ Kind, Title, Rationale, Definition, Example, Scope string }
	if err := decodeAuthoring(raw, &a); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if (a.Kind != "template" && a.Kind != "block") || vault.Unfilled(a.Rationale) || vault.Unfilled(a.Definition) || vault.Unfilled(a.Example) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "provide kind (template or block), rationale, proposed definition and a worked example"}
	}
	data, _ := json.Marshal(authoringArgs{Title: a.Title, Template: "finding", Status: "draft", Scope: a.Scope,
		Summary:  "Proposed " + a.Kind + " library addition; awaiting the user's approval. " + a.Rationale,
		Sections: map[string]string{"question": a.Rationale, "findings": a.Definition, "evidence": a.Example},
		Tags:     []string{"template-proposal"},
	})
	return s.toolWrite(ctx, data, "")
}

// Validate explicit knowledge relationships against both the current index and
// current file. A readable target must also be readable to every audience of the
// new note, preventing a broad note from disclosing a private reference.
func (s *Server) validateAuthoringLinks(ctx context.Context, spec vault.NewNoteSpec) error {
	if spec.UpdateID != "" {
		snapshot, rerr := s.authorizedUpdate(ctx, spec.UpdateID)
		if rerr != nil || snapshot.Revision != spec.UpdateRevision {
			return fmt.Errorf("unknown, inaccessible or changed update target")
		}
	}
	if spec.DraftID != "" {
		_, revision, rerr := s.authorizedDraft(ctx, spec.DraftID)
		if rerr != nil || revision != spec.DraftRevision {
			return fmt.Errorf("unknown, inaccessible or changed draft")
		}
	}
	refs, err := AuthoringReferences(spec)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		id, anchor, _ := strings.Cut(strings.TrimSpace(ref), "#")
		if id == "" || len(id) > 256 {
			return fmt.Errorf("invalid reference")
		}
		metadata, err := s.store.NoteMetadataFor(ctx, []string{"note:" + id})
		if err != nil {
			return err
		}
		m, ok := metadata["note:"+id]
		sf := scopeFromCtx(ctx)
		if !ok || !filepath.IsLocal(m.Path) || (sf != nil && !vault.ScopeAllowsCSV(m.Scope, sf.AllowedRead)) {
			return fmt.Errorf("inaccessible reference")
		}
		data, err := readFetchFile(ctx, s.vaultRoot, m.Path, batchFetchFileBytes)
		if err != nil || !fetchFileScopeAllowed(data, sf) {
			return fmt.Errorf("inaccessible reference")
		}
		fmText, _, _ := vault.SplitFrontmatter(string(data))
		fm, _, err := vault.ParseFrontmatter([]byte(fmText))
		if err != nil || vault.UnterminatedFrontmatter(string(data)) || fm.ID != id {
			return fmt.Errorf("invalid reference")
		}
		audiences := spec.Scope
		if len(audiences) == 0 {
			audiences = []string{vault.DefaultScope}
		}
		for _, audience := range audiences {
			if !vault.ScopeAllows(fm.EffectiveScopes(), map[string]bool{audience: true}) {
				return fmt.Errorf("reference audience mismatch")
			}
		}
		if anchor != "" {
			if !AuthoringReferenceAnchorValid(data, anchor) {
				return fmt.Errorf("invalid reference anchor")
			}
		}
	}
	return nil
}

func (s *Server) authorizedDraft(ctx context.Context, id string) (*vault.DraftSnapshot, string, *rpcError) {
	denied := &rpcError{Code: codeInvalidParams, Message: "unknown or inaccessible draft"}
	if id == "" || len(id) > 256 {
		return nil, "", denied
	}
	metadata, err := s.store.NoteMetadataFor(ctx, []string{"note:" + id})
	if err != nil {
		return nil, "", denied
	}
	m, ok := metadata["note:"+id]
	sf := scopeFromCtx(ctx)
	if !ok || !filepath.IsLocal(m.Path) || (sf != nil && !vault.ScopeAllowsCSV(m.Scope, sf.AllowedRead)) {
		return nil, "", denied
	}
	data, err := readFetchFile(ctx, s.vaultRoot, m.Path, batchFetchFileBytes)
	if err != nil || !fetchFileScopeAllowed(data, sf) {
		return nil, "", denied
	}
	header, _, had := vault.SplitFrontmatter(string(data))
	fm, _, err := vault.ParseFrontmatter([]byte(header))
	if err != nil || !had || fm.ID != id || !vault.IsDraft(fm) {
		return nil, "", denied
	}
	if sf != nil {
		for _, scope := range fm.EffectiveScopes() {
			if sf.CanWrite == nil || !sf.CanWrite(scope) {
				return nil, "", denied
			}
		}
	}
	return &vault.DraftSnapshot{Path: m.Path, Frontmatter: fm, Revision: vault.ContentRevision(data)}, vault.ContentRevision(data), nil
}

func (s *Server) toolDrafts(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	if err := decodeAuthoring(raw, &a); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if a.Offset < 0 {
		return nil, &rpcError{Code: codeInvalidParams, Message: "offset must be nonnegative"}
	}
	a.Limit = clampLimit(a.Limit, 20, 20)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sf := scopeFromCtx(ctx)
	var allowed map[string]bool
	if sf != nil {
		allowed = sf.AllowedRead
	}
	type current struct {
		fm       *vault.Frontmatter
		content  vault.AuthoringContent
		revision string
	}
	fresh := map[string]current{}
	visible := func(rel string) bool {
		data, err := readFetchFile(ctx, s.vaultRoot, rel, batchFetchFileBytes)
		if err != nil || !fetchFileScopeAllowed(data, sf) {
			return false
		}
		header, body, had := vault.SplitFrontmatter(string(data))
		fm, _, err := vault.ParseFrontmatter([]byte(header))
		if err != nil || !had || !vault.IsDraft(fm) {
			return false
		}
		content, _ := vault.ReadAuthoring(fm, body)
		fresh[rel] = current{fm, content, vault.ContentRevision(data)}
		return true
	}
	rows, err := s.store.DraftNotesContext(ctx, allowed, visible, a.Limit+1, a.Offset)
	if err != nil {
		return nil, internalErr(err)
	}
	more := len(rows) > a.Limit
	if more {
		rows = rows[:a.Limit]
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		f, ok := fresh[m.Path]
		if !ok || f.fm.ID != m.NoteID {
			continue
		}
		out = append(out, map[string]any{"id": m.NoteID, "title": f.fm.Title, "template": f.fm.Template, "template_version": f.fm.TemplateVersion,
			"summary": clipSectionContext(f.content.Summary, 1200), "status": "draft", "revision": f.revision, "missing": f.content.MissingSections})
	}
	return textResult(map[string]any{"drafts": out, "more": more, "next_offset": a.Offset + len(rows)}), nil
}

func (s *Server) toolAuthorNote(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Action string          `json:"action"`
		Note   json.RawMessage `json:"note"`
	}
	if err := decodeAuthoring(raw, &a); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	switch a.Action {
	case "prepare":
		return s.toolPrepareNote(ctx, a.Note, false)
	case "validate":
		return s.toolPrepareNote(ctx, a.Note, true)
	case "draft":
		return s.toolSaveDraft(ctx, a.Note)
	case "publish":
		var note authoringArgs
		if err := decodeAuthoring(a.Note, &note); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		// Publishing a previously prepared draft changes lifecycle explicitly;
		// the full content validator must run even if the object still says draft.
		if strings.EqualFold(strings.TrimSpace(note.Status), "draft") || note.Status == "" {
			note.Status = "active"
		}
		data, _ := json.Marshal(note)
		return s.toolWrite(ctx, data, "")
	default:
		return nil, &rpcError{Code: codeInvalidParams, Message: "action must be prepare, validate, draft or publish"}
	}
}
