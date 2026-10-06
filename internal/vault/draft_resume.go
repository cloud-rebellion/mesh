// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type DraftSnapshot struct {
	Path        string
	Revision    string
	Content     []byte
	Frontmatter *Frontmatter
}

func ContentRevision(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// DraftSnapshotContext reads one canonical modern draft. It is a storage helper;
// remote callers must enforce their current read/write scopes before calling it.
func DraftSnapshotContext(ctx context.Context, root, id string) (*DraftSnapshot, error) {
	if !authoringID(id) {
		return nil, fmt.Errorf("%w: invalid draft id", ErrInvalidSpec)
	}
	claims, err := ClaimedIDsContext(ctx, root)
	if err != nil {
		return nil, err
	}
	rel, ok := claims[id]
	if !ok || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("%w: unknown draft", ErrInvalidSpec)
	}
	data, err := ReadConfinedFileContext(ctx, root, rel, 1<<20)
	if err != nil {
		return nil, err
	}
	header, _, had := SplitFrontmatter(string(data))
	fm, _, err := ParseFrontmatter([]byte(header))
	if err != nil || !had || fm.ID != id || !IsDraft(fm) || fm.Template == "" {
		return nil, fmt.Errorf("%w: target must be a versioned draft", ErrInvalidSpec)
	}
	if fm.Summary != "" || fm.Do != "" || fm.Dont != "" || fm.Why != "" {
		return nil, fmt.Errorf("%w: mixed historical prose requires reviewed migration before draft completion", ErrInvalidSpec)
	}
	return &DraftSnapshot{Path: rel, Revision: ContentRevision(data), Content: data, Frontmatter: fm}, nil
}

// A draft keeps its canonical file and ID when published. The draft inbox is a
// status-filtered view, so completion does not introduce a two-file transaction,
// duplicate IDs or broken path links. Preparation performs no writes.
func prepareDraftContext(ctx context.Context, root string, spec NewNoteSpec) (*PreparedNote, error) {
	before, err := DraftSnapshotContext(ctx, root, spec.DraftID)
	if err != nil {
		return nil, err
	}
	if spec.DraftPath != "" && filepath.Clean(spec.DraftPath) != filepath.Clean(before.Path) {
		return nil, fmt.Errorf("%w: draft path differs from its authorized identity", ErrInvalidSpec)
	}
	if spec.DraftRevision == "" || spec.DraftRevision != before.Revision {
		return nil, fmt.Errorf("%w: draft changed or revision missing; read the current draft revision before updating", ErrInvalidSpec)
	}
	plan, err := planNoteContext(ctx, root, spec, ClaimedIDsContext)
	if err != nil {
		return nil, err
	}
	old := before.Frontmatter
	a, b := append([]string{}, old.EffectiveScopes()...), append([]string{}, plan.fm.EffectiveScopes()...)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		return nil, fmt.Errorf("%w: draft completion cannot change its access scopes", ErrInvalidSpec)
	}
	if old.Template != plan.fm.Template || old.TemplateVersion != plan.fm.TemplateVersion {
		return nil, fmt.Errorf("%w: draft completion must retain its template and version", ErrInvalidSpec)
	}
	plan.fm.ID, plan.fm.When, plan.fm.Created = old.ID, old.When, old.Created
	// Creation provenance remains historical. The publication surface's audit
	// receipt records the authenticated editor independently.
	plan.fm.Author, plan.fm.Agent, plan.fm.Source = old.Author, old.Agent, old.Source
	plan.fm.SourceURL, plan.fm.ImportedAt = old.SourceURL, old.ImportedAt
	content, err := renderNote(plan.fm, spec.By)
	if err != nil {
		return nil, err
	}
	// Preserve unknown metadata; declared new fields remain authoritative. Legacy
	// prose cannot appear in a versioned draft written by the modern authoring path.
	originalHeader, _, _ := SplitFrontmatter(string(before.Content))
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
		if _, isKnown := known[key]; !isKnown {
			current[key] = value
		}
	}
	encoded, err := yaml.Marshal(current)
	if err != nil {
		return nil, err
	}
	content = "---\n" + string(encoded) + "---\n" + body
	if err := validateRoundTrip(content, old.ID); err != nil {
		return nil, err
	}
	return &PreparedNote{Result: CreateResult{Path: filepath.Join(root, before.Path), ID: old.ID, When: old.When, TODOs: plan.todos, Revision: ContentRevision([]byte(content))}, Content: []byte(content), PreviousPath: before.Path}, nil
}

func resumeDraftContext(ctx context.Context, root string, spec NewNoteSpec) (*CreateResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !authoringID(spec.DraftID) {
		return nil, fmt.Errorf("%w: invalid draft id", ErrInvalidSpec)
	}
	// Serialize cooperating Mesh processes without taking the index owner's lock.
	// External editors do not honor this claim; they must coordinate publication.
	// A leftover claim after a crash fails closed for operator recovery.
	dir := filepath.Join(root, ".mesh", "draft-locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	claim := filepath.Join(dir, spec.DraftID+".lock")
	f, err := os.OpenFile(claim, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		return nil, fmt.Errorf("%w: draft is being updated; inspect any abandoned draft lock before retrying", ErrInvalidSpec)
	}
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	defer os.Remove(claim)
	p, err := prepareDraftContext(ctx, root, spec)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Check the source again immediately before the durable publication boundary.
	data, err := ReadConfinedFileContext(ctx, root, p.PreviousPath, 1<<20)
	if err != nil {
		return nil, err
	}
	if ContentRevision(data) != spec.DraftRevision {
		return nil, fmt.Errorf("%w: draft changed during preparation", ErrInvalidSpec)
	}
	if err := WriteNoteAtomic(p.Result.Path, p.Content); err != nil {
		return nil, err
	}
	return &p.Result, nil
}
