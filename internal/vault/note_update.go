// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// NoteSnapshot contains one published, versioned note and its editable content.
// Storage helpers do not grant access: remote callers must check current scopes.
type NoteSnapshot struct {
	Path        string
	Revision    string
	Content     []byte
	Frontmatter *Frontmatter
	Spec        NewNoteSpec
}

func NoteSnapshotContext(ctx context.Context, root, id string) (*NoteSnapshot, error) {
	if !authoringID(id) {
		return nil, fmt.Errorf("%w: invalid update id", ErrInvalidSpec)
	}
	claims, err := ClaimedIDsContext(ctx, root)
	if err != nil {
		return nil, err
	}
	rel, ok := claims[id]
	if !ok || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("%w: unknown published note", ErrInvalidSpec)
	}
	data, err := ReadConfinedFileContext(ctx, root, rel, 1<<20)
	if err != nil {
		return nil, err
	}
	return PublishedNoteSnapshot(rel, id, data)
}

// PublishedNoteSnapshot decodes already-authorized bytes without filesystem IO.
// Only losslessly representable authoring bodies can be edited by this API.
func PublishedNoteSnapshot(rel, id string, data []byte) (*NoteSnapshot, error) {
	return editableNoteSnapshot(rel, id, data, false)
}

// DraftNoteSnapshot returns a lossless editable object from already-authorized
// bytes. It never converts prose or fabricates missing draft substance.
func DraftNoteSnapshot(rel, id string, data []byte) (*NoteSnapshot, error) {
	return editableNoteSnapshot(rel, id, data, true)
}

func editableNoteSnapshot(rel, id string, data []byte, draft bool) (*NoteSnapshot, error) {
	header, body, had := SplitFrontmatter(string(data))
	fm, _, err := ParseFrontmatter([]byte(header))
	if err != nil || !had || UnterminatedFrontmatter(string(data)) || fm.ID != id || IsDraft(fm) != draft || fm.Template == "" {
		return nil, fmt.Errorf("%w: editable target must match its published/draft lifecycle and versioned template", ErrInvalidSpec)
	}
	mixedHistoricalProse := fm.Summary != ""
	for _, value := range ReadLegacy(fm).Values() {
		mixedHistoricalProse = mixedHistoricalProse || value != ""
	}
	if mixedHistoricalProse {
		return nil, fmt.Errorf("%w: historical prose requires reviewed migration before updating", ErrInvalidSpec)
	}
	authored, err := readAuthoring(fm, body, true)
	if err != nil {
		return nil, fmt.Errorf("%w: existing body cannot be safely decoded for editing", ErrInvalidSpec)
	}
	fm.BodySummary, fm.Sections = authored.Summary, authored.Sections
	for _, block := range authored.Blocks {
		fm.BlockContents = append(fm.BlockContents, BlockSpec{Template: block.Template, Version: block.Version, ID: block.ID, Fields: block.Fields})
	}
	// The reader tolerates presentation variations, but a full replacement must
	// not silently discard extra prose, a manual preamble or Related annotations.
	canonical := "# " + fm.Title + "\n\n" + renderBody(fm)
	if strings.TrimSpace(body) != strings.TrimSpace(canonical) {
		return nil, fmt.Errorf("%w: body has content outside the canonical authoring representation; reconcile it in a reviewed draft before updating", ErrInvalidSpec)
	}
	revision := ContentRevision(data)
	before := &NoteSnapshot{Path: rel, Revision: revision, Content: data, Frontmatter: fm}
	input := NewNoteSpec{
		UpdateID: id, UpdateRevision: revision, UpdatePath: rel,
		Type: fm.Type, Title: fm.Title, Template: fm.Template, TemplateVersion: fm.TemplateVersion,
		Summary: authored.Summary, Sections: authored.Sections, Blocks: fm.BlockContents,
		Collections: fm.Collections, Related: fm.Related, Supersedes: fm.Supersedes, Tags: fm.Tags,
		Status: fm.Status, Severity: fm.Severity, Confidence: fm.Confidence, ReviewBy: fm.ReviewBy,
		Scope: fm.EffectiveScopes(),
	}
	var spec NewNoteSpec
	if draft {
		input.UpdateID, input.UpdateRevision, input.UpdatePath = "", "", ""
		input.DraftID, input.DraftRevision, input.DraftPath = id, revision, rel
		spec, err = NormalizeSpec(input)
	} else {
		spec, err = NormalizeUpdateSpec(input, before)
	}
	if err != nil {
		return nil, err
	}
	// Verification blocks remain historical evidence. verified_at is deliberately
	// not prefilled: an edited revision requires an explicit recorded verification.
	before.Spec = spec
	return before, nil
}

func prepareUpdateContext(ctx context.Context, root string, spec NewNoteSpec) (*PreparedNote, error) {
	before, err := NoteSnapshotContext(ctx, root, spec.UpdateID)
	if err != nil {
		return nil, err
	}
	if spec.UpdatePath != "" && filepath.Clean(spec.UpdatePath) != filepath.Clean(before.Path) {
		return nil, fmt.Errorf("%w: update path differs from its authorized identity", ErrInvalidSpec)
	}
	if spec.UpdateRevision == "" || spec.UpdateRevision != before.Revision {
		return nil, fmt.Errorf("%w: note changed or revision missing; prepare the current note again and reconcile edits", ErrInvalidSpec)
	}
	spec, err = NormalizeUpdateSpec(spec, before)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(spec.Status), "draft") {
		return nil, fmt.Errorf("%w: published notes cannot be replaced with drafts; save incomplete work as a separate linked draft", ErrInvalidSpec)
	}
	plan, err := planNoteContext(ctx, root, spec, ClaimedIDsContext)
	if err != nil {
		return nil, err
	}
	old := before.Frontmatter
	if spec.retainsHistoricalTags() {
		// General new-note rendering normalizes tags. An unchanged historical
		// list must retain its exact spelling, order and duplicate entries.
		plan.fm.Tags = slices.Clone(old.Tags)
	}
	a, b := append([]string{}, old.EffectiveScopes()...), append([]string{}, plan.fm.EffectiveScopes()...)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		return nil, fmt.Errorf("%w: note updates cannot change access scopes", ErrInvalidSpec)
	}
	if old.Template != plan.fm.Template || old.TemplateVersion != plan.fm.TemplateVersion {
		return nil, fmt.Errorf("%w: note updates must retain the template and version", ErrInvalidSpec)
	}
	plan.fm.ID, plan.fm.When, plan.fm.Created = old.ID, old.When, old.Created
	plan.fm.Author, plan.fm.Agent, plan.fm.Source = old.Author, old.Agent, old.Source
	plan.fm.SourceURL, plan.fm.ImportedAt = old.SourceURL, old.ImportedAt
	plan.fm.Role, plan.fm.Stack, plan.fm.RepoPath = old.Role, old.Stack, old.RepoPath
	plan.fm.ExpectDeadRefs, plan.fm.ExpectDeadRefPaths = old.ExpectDeadRefs, old.ExpectDeadRefPaths
	plan.fm.UpdatedBy, plan.fm.UpdatedAgent = strings.TrimSpace(spec.Author), plan.fm.Agent
	if strings.TrimSpace(spec.Agent) != "" {
		plan.fm.UpdatedAgent = strings.TrimSpace(spec.Agent)
	} else if strings.TrimSpace(spec.By) != "" {
		plan.fm.UpdatedAgent = strings.TrimSpace(spec.By)
	}
	content, err := renderWithOriginalMetadata(before.Content, plan.fm, spec.By)
	if err != nil {
		return nil, err
	}
	return &PreparedNote{Result: CreateResult{Path: filepath.Join(root, before.Path), ID: old.ID, When: old.When, Revision: ContentRevision(content)}, Content: content, PreviousPath: before.Path, OriginalContent: before.Content}, nil
}

func renderWithOriginalMetadata(original []byte, fm *Frontmatter, by string) ([]byte, error) {
	content, err := renderNote(fm, by)
	if err != nil {
		return nil, err
	}
	originalHeader, _, _ := SplitFrontmatter(string(original))
	_, raw, err := ParseFrontmatter([]byte(originalHeader))
	if err != nil {
		return nil, err
	}
	header, body, _ := SplitFrontmatter(content)
	_, current, err := ParseFrontmatter([]byte(header))
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	typ := reflect.TypeOf(Frontmatter{})
	for i := 0; i < typ.NumField(); i++ {
		key := strings.Split(typ.Field(i).Tag.Get("yaml"), ",")[0]
		if key != "" && key != "-" {
			known[key] = true
		}
	}
	for key, value := range raw {
		if !known[key] {
			current[key] = value
		}
	}
	encoded, err := yaml.Marshal(current)
	if err != nil {
		return nil, err
	}
	content = "---\n" + string(encoded) + "---\n" + body
	if err := validateRoundTrip(content, fm.ID); err != nil {
		return nil, err
	}
	return []byte(content), nil
}

func updateNoteContext(ctx context.Context, root string, spec NewNoteSpec) (*CreateResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !authoringID(spec.UpdateID) {
		return nil, fmt.Errorf("%w: invalid update id", ErrInvalidSpec)
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	// Cooperating local Mesh writers serialize each published note. A crash
	// leaves a claim requiring operator recovery; external editors must coordinate.
	lockDir := filepath.Join(".mesh", "note-locks")
	if err := handle.MkdirAll(lockDir, 0700); err != nil {
		return nil, err
	}
	claim := filepath.Join(lockDir, spec.UpdateID+".lock")
	f, err := handle.OpenFile(claim, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		return nil, fmt.Errorf("%w: note is being updated; inspect any abandoned note lock before retrying", ErrInvalidSpec)
	}
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	defer handle.Remove(claim)
	p, err := prepareUpdateContext(ctx, root, spec)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	archiveDir := filepath.Join(".mesh", "note-history", spec.UpdateID)
	if err := handle.MkdirAll(archiveDir, 0700); err != nil {
		return nil, err
	}
	if err := writeMigrationOriginal(handle, filepath.Join(archiveDir, spec.UpdateRevision+".md"), p.OriginalContent); err != nil {
		return nil, err
	}
	// Recheck immediately before replacement, after preserving the old bytes.
	data, err := ReadConfinedFile(ctx, root, p.PreviousPath, 1<<20)
	if err != nil {
		return nil, err
	}
	if ContentRevision(data) != spec.UpdateRevision {
		return nil, fmt.Errorf("%w: note changed during preparation", ErrInvalidSpec)
	}
	// Rooted atomic write keeps the replacement confined and completes durability
	// before acknowledging success; cancellation cannot strand a half-written note.
	if err := writeMigrationAtomic(handle, p.PreviousPath, p.Content); err != nil {
		return nil, err
	}
	return &p.Result, nil
}
