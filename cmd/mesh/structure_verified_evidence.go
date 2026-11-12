// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bright-interaction/mesh/internal/vault"
)

// This is a local operator attestation, not evidence supplied by a note or an
// HTTP client. The operator independently verifies the source objects/native
// records, selects a private fresh manifest and explicitly pins its SHA256.
// Filesystem bytes, identity, lifecycle, audiences and visible citations are
// checked again here. A vault with folder ACLs needs a filtered-vault repair or
// an independent all-audience ACL proof. Writers must remain quiesced throughout.
type verifiedRelationDocument struct {
	Schema     int                    `json:"schema"`
	VerifiedAt time.Time              `json:"verified_at"`
	Candidates []verifiedRelationEdge `json:"candidates"`
}

type verifiedRelationEdge struct {
	Source         string   `json:"source"`
	SourcePath     string   `json:"source_path"`
	SourceSHA      string   `json:"source_sha256"`
	Target         string   `json:"target"`
	TargetPath     string   `json:"target_path"`
	TargetSHA      string   `json:"target_sha256"`
	Kind           string   `json:"evidence_kind"`
	Terms          []string `json:"terms"`
	TargetRepoPath string   `json:"target_repo_path,omitempty"`
}

type verifiedRelationNote struct {
	id, path, revision, body string
	fm                       *vault.Frontmatter
	raw                      map[string]any
}

type verifiedRelationEvidence struct {
	root      string
	rows      map[string][]verifiedRelationEdge
	notes     map[string]*verifiedRelationNote
	completed map[string]string
}

var evidenceSHA = regexp.MustCompile(`^[a-f0-9]{64}$`)
var evidenceRun = regexp.MustCompile(`^run-[0-9]{8}T[0-9]{6}[a-f0-9]{8}(?:-[a-z][a-z-]*)?$`)
var evidenceTool = regexp.MustCompile(`^(?:mcp__[a-z0-9_]+__[a-z0-9_]+|(?:mesh|stage)_[a-z0-9_]+|(?:mesh|stage|hash|flare)\.[a-z0-9_]+)$`)

func evidenceRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func loadVerifiedRelationEvidence(ctx context.Context, root, name, digest string) (*verifiedRelationEvidence, error) {
	refused := func() (*verifiedRelationEvidence, error) {
		return nil, fmt.Errorf("verified relation evidence is invalid, stale or does not match current notes")
	}
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || !evidenceSHA.MatchString(digest) {
		return refused()
	}
	st, err := os.Lstat(name)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 2<<20 {
		return refused()
	}
	parent, err := os.Lstat(filepath.Dir(name))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return refused()
	}
	file, err := os.Open(name)
	if err != nil {
		return refused()
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return refused()
	}
	data, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	if err != nil || len(data) > 2<<20 || evidenceRevision(data) != digest {
		return refused()
	}
	named, err := os.Lstat(name)
	if err != nil || !os.SameFile(st, named) || !named.Mode().IsRegular() {
		return refused()
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document verifiedRelationDocument
	if decoder.Decode(&document) != nil || decoder.Decode(new(any)) != io.EOF || document.Schema != 1 || len(document.Candidates) > 2000 {
		return refused()
	}
	age := time.Since(document.VerifiedAt)
	if document.VerifiedAt.IsZero() || age > 5*time.Minute || age < -30*time.Second {
		return refused()
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return refused()
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return refused()
	}
	plan := &verifiedRelationEvidence{root: canonical, rows: map[string][]verifiedRelationEdge{}, notes: map[string]*verifiedRelationNote{}, completed: map[string]string{}}
	seen := map[string]bool{}
	for _, edge := range document.Candidates {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if edge.Source == "" || edge.Target == "" || edge.Source == edge.Target || !evidenceSHA.MatchString(edge.SourceSHA) || !evidenceSHA.MatchString(edge.TargetSHA) || len(edge.Terms) == 0 || len(edge.Terms) > 8 {
			return refused()
		}
		source, err := plan.note(ctx, edge.Source, edge.SourcePath, edge.SourceSHA)
		if err != nil {
			return refused()
		}
		target, err := plan.note(ctx, edge.Target, edge.TargetPath, edge.TargetSHA)
		if err != nil {
			return refused()
		}
		if !relationAudienceAllows(source.fm, target.fm) {
			return refused()
		}
		peer := edge.Kind == "native-run-peer"
		if !peer && (target.fm.Type != vault.TypeEntity || target.fm.RepoPath == "" || target.fm.RepoPath != edge.TargetRepoPath) {
			return refused()
		}
		if peer && edge.TargetRepoPath != "" {
			return refused()
		}
		visibleSource, _ := vault.StripComments(source.fm.Title + "\n" + source.body)
		visibleTarget, _ := vault.StripComments(target.fm.Title + "\n" + target.body)
		valid := false
		for _, term := range edge.Terms {
			if term == "" || len(term) > 240 || strings.ContainsAny(term, "\x00\r\n") {
				return refused()
			}
			switch edge.Kind {
			case "code-reference":
				if !strings.Contains(term, "/") || strings.Contains(term, "..") {
					return refused()
				}
			case "native-run-reference", "native-run-peer":
				if !evidenceRun.MatchString(term) {
					return refused()
				}
			case "native-tool-reference":
				if !evidenceTool.MatchString(term) {
					return refused()
				}
			default:
				return refused()
			}
			if visibleEvidenceTerm(visibleSource, term) && (!peer || visibleEvidenceTerm(visibleTarget, term)) {
				valid = true
			}
		}
		if !valid {
			return refused()
		}
		if declaredRelationValue(source.raw["related"]) {
			continue
		}
		key := edge.Source + "\x00" + edge.Target
		if seen[key] {
			continue
		}
		seen[key] = true
		plan.rows[edge.Source] = append(plan.rows[edge.Source], edge)
	}
	for id, rows := range plan.rows {
		sort.Slice(rows, func(i, j int) bool { return rows[i].Target < rows[j].Target })
		if len(rows) > 3 {
			return refused()
		}
		plan.rows[id] = rows
	}
	return plan, nil
}

func declaredRelationValue(value any) bool {
	if value == nil {
		return false
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text) != ""
	}
	return true
}

func visibleEvidenceTerm(text, term string) bool {
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' }
	for offset := 0; offset < len(text); {
		rel := strings.Index(text[offset:], term)
		if rel < 0 {
			return false
		}
		start := offset + rel
		end := start + len(term)
		left, right := false, false
		if start > 0 {
			r, _ := utf8.DecodeLastRuneInString(text[:start])
			left = word(r)
		}
		if end < len(text) {
			r, _ := utf8.DecodeRuneInString(text[end:])
			right = word(r)
		}
		if !left && !right {
			return true
		}
		offset = start + 1
	}
	return false
}

func (p *verifiedRelationEvidence) note(ctx context.Context, id, path, revision string) (*verifiedRelationNote, error) {
	if cached := p.notes[id]; cached != nil {
		if cached.path != path || cached.revision != revision {
			return nil, fmt.Errorf("conflicting evidence identity")
		}
		return cached, nil
	}
	if !filepath.IsLocal(path) || filepath.Clean(path) != path || filepath.Ext(path) != ".md" {
		return nil, fmt.Errorf("invalid evidence note path")
	}
	full := filepath.Join(p.root, path)
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil || resolved != full {
		return nil, fmt.Errorf("linked evidence note")
	}
	st, err := os.Lstat(full)
	if err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("invalid evidence note")
	}
	data, err := vault.ReadConfinedFileContext(ctx, p.root, path, 1<<20)
	if err != nil || evidenceRevision(data) != revision || vault.UnterminatedFrontmatter(string(data)) {
		return nil, fmt.Errorf("changed evidence note")
	}
	header, body, had := vault.SplitFrontmatter(string(data))
	fm, raw, err := vault.ParseFrontmatter([]byte(header))
	if err != nil || !had || vault.IsDraft(fm) || !fm.Type.Valid() {
		return nil, fmt.Errorf("invalid evidence lifecycle")
	}
	if fm.ID == "" {
		fm.ID = strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	}
	if fm.ID != id {
		return nil, fmt.Errorf("changed evidence identity")
	}
	note := &verifiedRelationNote{id: id, path: path, revision: revision, body: body, fm: fm, raw: raw}
	p.notes[id] = note
	return note, nil
}

func (p *verifiedRelationEvidence) links(ctx context.Context, id, path string) ([]string, error) {
	if p == nil {
		return nil, nil
	}
	rows := p.rows[id]
	if len(rows) == 0 {
		return nil, nil
	}
	source := p.notes[id]
	if source == nil || source.path != path {
		return nil, fmt.Errorf("verified source path changed")
	}
	check := func(note *verifiedRelationNote) error {
		data, err := vault.ReadConfinedFileContext(ctx, p.root, note.path, 1<<20)
		if err != nil {
			return err
		}
		expected := note.revision
		if completed := p.completed[note.id]; completed != "" {
			expected = completed
		}
		if evidenceRevision(data) != expected {
			return fmt.Errorf("verified note changed after repair preflight")
		}
		return nil
	}
	if err := check(source); err != nil {
		return nil, err
	}
	var ids []string
	for _, row := range rows {
		if err := check(p.notes[row.Target]); err != nil {
			return nil, err
		}
		ids = append(ids, row.Target)
	}
	return ids, nil
}

// Use the same preservation proof for ordinary lexical/tag/entity repairs.
// Capture occurs during dry runs too, so unsupported source metadata is found
// before an operator chooses to apply. Target evidence is checked by the normal
// current-file audience filter; this snapshot verifies this source's write.
func captureRelatedRepair(ctx context.Context, root, id, path string, targets []string) (*verifiedRelationEvidence, error) {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	data, err := vault.ReadConfinedFileContext(ctx, canonical, path, 1<<20)
	if err != nil {
		return nil, err
	}
	plan := &verifiedRelationEvidence{root: canonical, notes: map[string]*verifiedRelationNote{}, rows: map[string][]verifiedRelationEdge{}, completed: map[string]string{}}
	if _, err := plan.note(ctx, id, path, evidenceRevision(data)); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target != "" && !seen[target] {
			plan.rows[id] = append(plan.rows[id], verifiedRelationEdge{Target: target})
			seen[target] = true
		}
	}
	return plan, nil
}

func (p *verifiedRelationEvidence) recordWrite(ctx context.Context, id string) error {
	if p == nil {
		return nil
	}
	before := p.notes[id]
	if before == nil {
		return fmt.Errorf("unknown verified repair source")
	}
	data, err := vault.ReadConfinedFileContext(ctx, p.root, before.path, 1<<20)
	if err != nil {
		return err
	}
	header, body, had := vault.SplitFrontmatter(string(data))
	fm, raw, err := vault.ParseFrontmatter([]byte(header))
	if err != nil || !had || body != before.body {
		return fmt.Errorf("durable repair requires review: body changed")
	}
	original := map[string]any{}
	for k, v := range before.raw {
		if k != "related" {
			original[k] = v
		}
	}
	current := map[string]any{}
	for k, v := range raw {
		if k != "related" {
			current[k] = v
		}
	}
	if !reflect.DeepEqual(original, current) {
		return fmt.Errorf("durable repair requires review: other metadata changed")
	}
	var expected []string
	for _, row := range p.rows[id] {
		expected = append(expected, row.Target)
	}
	if fm == nil || !reflect.DeepEqual([]string(fm.Related), expected) {
		return fmt.Errorf("durable repair requires review: written relations differ from selected evidence")
	}
	p.completed[id] = evidenceRevision(data)
	return nil
}
