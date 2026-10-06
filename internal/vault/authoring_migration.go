// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const MaxMigrationPreviewBytes = 8 << 20

// MigrationRequest names an explicit source and an authored reconstruction.
// Templates are never selected by interpreting historical do/dont/why fields.
type MigrationRequest struct {
	Path string      `json:"path"`
	ID   string      `json:"id"`
	Spec NewNoteSpec `json:"spec"`
}

type MigrationEntry struct {
	Path                 string   `json:"path"`
	ID                   string   `json:"id"`
	OriginalHash         string   `json:"original_hash"`
	ResultHash           string   `json:"result_hash"`
	Original             string   `json:"original"`
	Content              string   `json:"content"`
	Actions              []string `json:"actions"`
	Issues               []string `json:"issues,omitempty"`
	CaveatsRequireReview bool     `json:"caveats_require_review"`
}

type MigrationPreview struct {
	Version int              `json:"version"`
	Hash    string           `json:"hash"`
	Entries []MigrationEntry `json:"entries"`
}

// MigrationApproval is an operator-provided selection, not proof of human review.
// Both the exact preview hash and explicit note IDs are required.
type MigrationApproval struct {
	PreviewHash string   `json:"preview_hash"`
	IDs         []string `json:"ids"`
}

type MigrationReceipt struct {
	ID             string `json:"id"`
	Path           string `json:"path"`
	PreviewHash    string `json:"preview_hash"`
	OriginalHash   string `json:"original_hash"`
	ResultHash     string `json:"result_hash"`
	OriginalPath   string `json:"original_path"`
	ReceiptPath    string `json:"receipt_path"`
	State          string `json:"state"`
	AppliedAt      string `json:"applied_at,omitempty"`
	AlreadyApplied bool   `json:"already_applied,omitempty"`
}

func contentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func previewHash(preview *MigrationPreview) (string, error) {
	copy := *preview
	copy.Hash = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	if len(data) > MaxMigrationPreviewBytes {
		return "", fmt.Errorf("migration preview exceeds %d bytes", MaxMigrationPreviewBytes)
	}
	return contentHash(data), nil
}

// PreviewAuthoringMigration is read-only and bounded. Original bytes, unknown
// metadata and caveats remain available for review; no source is reconstructed
// from semantic guesses. A complete authored Spec is required for each candidate.
func PreviewAuthoringMigration(ctx context.Context, root string, requests []MigrationRequest) (*MigrationPreview, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := RequireRoot(root); err != nil {
		return nil, err
	}
	if len(requests) == 0 || len(requests) > MaxAuthoringList {
		return nil, fmt.Errorf("choose 1..%d explicit notes for migration", MaxAuthoringList)
	}
	preview := &MigrationPreview{Version: 1}
	seenIDs, seenPaths := map[string]bool{}, map[string]bool{}
	for _, request := range requests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel, err := migrationPath(request.Path)
		if err != nil {
			return nil, err
		}
		data, err := ReadConfinedFile(ctx, root, rel, MaxMigrationPreviewBytes)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(data) {
			return nil, fmt.Errorf("%s: migration requires UTF-8 Markdown; original was not changed", rel)
		}
		if UnterminatedFrontmatter(string(data)) {
			return nil, ErrUnterminatedFrontmatter
		}
		fmText, _, had := SplitFrontmatter(string(data))
		if !had {
			return nil, fmt.Errorf("%s: migration requires existing canonical identity metadata", rel)
		}
		fm, _, err := ParseFrontmatter([]byte(fmText))
		if err != nil {
			return nil, err
		}
		if !authoringID(fm.ID) || request.ID != fm.ID || seenIDs[fm.ID] || seenPaths[rel] {
			return nil, fmt.Errorf("%s: migration requires its exact existing unique note id", rel)
		}
		if !fm.Type.Valid() || fm.When == "" {
			return nil, fmt.Errorf("%s: fix missing identity/type/date before authoring migration", rel)
		}
		seenIDs[fm.ID], seenPaths[rel] = true, true
		spec := request.Spec
		if spec.DraftID != "" {
			return nil, errors.New("migration cannot resume or promote a draft")
		}
		if spec.Title == "" {
			spec.Title = fm.Title
		}
		if spec.Title != fm.Title {
			return nil, fmt.Errorf("%s: migration cannot change title or meaning", rel)
		}
		if spec.Type == "" {
			spec.Type = fm.Type
		}
		if spec.Type != fm.Type {
			return nil, fmt.Errorf("%s: migration cannot relabel note type", rel)
		}
		spec.Status = fm.Status
		// Verification and provenance are preserved exactly, never inferred from formatting.
		if spec.VerifiedAt != "" && spec.VerifiedAt != fm.VerifiedAt {
			return nil, fmt.Errorf("%s: migration cannot bump verification", rel)
		}
		spec.VerifiedAt = ""
		normalized, err := NormalizeSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		if err := ValidateSpec(normalized); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		originalHash := contentHash(data)
		// Retain original normative fields under their original labels. The full
		// historical source lives in the immutable archive, not duplicated in the current page.
		if legacy := ReadLegacy(fm); len(legacy.AuthoredText()) > 0 {
			for _, block := range normalized.Blocks {
				if block.ID == "legacy-evidence" {
					return nil, fmt.Errorf("%s: legacy-evidence block id is reserved by migration", rel)
				}
			}
			var result strings.Builder
			for _, item := range []struct{ key, value string }{{"do", legacy.Do}, {"dont", legacy.Dont}, {"why", legacy.Why}} {
				if item.value == "" {
					continue
				}
				fmt.Fprintf(&result, "#### Original %s\n\n%s\n\n", item.key, fencedCode(item.value, "text"))
			}
			normalized.Blocks = append(normalized.Blocks, BlockSpec{Template: "evidence", Version: 1, ID: "legacy-evidence", Fields: map[string]string{
				"claim":       "Historical authoring is retained under its original labels without semantic relabeling.",
				"source":      rel + "; original SHA256 " + originalHash + ". The receipt links the exact archived original.",
				"observed_at": "Unknown; the original record date is " + fm.When + ". Formatting does not establish a new observation.",
				"result":      strings.TrimSpace(result.String()),
				"uncertainty": "Historical claims, warnings and caveats have not been independently re-verified. Review the archived original against the reconstructed sections.",
			}})
			if err := ValidateSpec(normalized); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
		}
		candidate, err := renderMigrationCandidate(fmText, fm, data, normalized)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		entry := MigrationEntry{Path: rel, ID: fm.ID, OriginalHash: originalHash, ResultHash: contentHash([]byte(candidate)), Original: string(data), Content: candidate,
			Actions: []string{"apply explicitly authored " + normalized.Template + " v1 body", "archive exact original and durable receipt", "preserve identity, metadata, references and verification date"},
			Issues:  []string{"Review reconstructed claims and caveats against the archived original; formatting is not verification."}, CaveatsRequireReview: true}
		preview.Entries = append(preview.Entries, entry)
		if _, err := previewHash(preview); err != nil {
			return nil, err
		}
	}
	hash, err := previewHash(preview)
	if err != nil {
		return nil, err
	}
	preview.Hash = hash
	return preview, nil
}

func migrationPath(path string) (string, error) {
	if !filepath.IsLocal(path) || !strings.EqualFold(filepath.Ext(path), ".md") {
		return "", fmt.Errorf("migration path must be a vault-relative Markdown file: %q", path)
	}
	rel := filepath.Clean(path)
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(part, ".") {
			return "", errors.New("migration cannot target hidden or archived state")
		}
	}
	return rel, nil
}

func renderMigrationCandidate(fmText string, original *Frontmatter, data []byte, spec NewNoteSpec) (string, error) {
	fm := *original
	fm.Template, fm.TemplateVersion = spec.Template, spec.TemplateVersion
	fm.BodySummary, fm.Sections, fm.BlockContents = spec.Summary, spec.Sections, spec.Blocks
	fm.Blocks = nil
	for _, block := range spec.Blocks {
		fm.Blocks = append(fm.Blocks, BlockMetadata{Template: block.Template, Version: block.Version, ID: block.ID})
	}
	fm.Collections = StringList(spec.Collections)
	fm.Updated = Now().UTC().Format(time.RFC3339)
	references := append([]string(nil), original.Related...)
	_, body, _ := SplitFrontmatter(string(data))
	searchable, _ := StripNonContent(body)
	for _, s := range ReadLegacy(original).AuthoredText() {
		searchable += "\n" + s
	}
	for _, match := range wikilinkTarget.FindAllStringSubmatch(searchable, -1) {
		if id := strings.TrimSpace(match[1]); id != "" {
			references = append(references, id)
		}
	}
	fm.Related = normalizeLinks(uniqueStrings(references))
	add := &yaml.Node{Kind: yaml.MappingNode}
	for _, pair := range []struct {
		key   string
		value any
	}{
		{"template", fm.Template}, {"template_version", fm.TemplateVersion}, {"updated", fm.Updated},
		{"collections", fm.Collections}, {"blocks", fm.Blocks}, {"related", fm.Related},
	} {
		if err := addKey(add, pair.key, pair.value); err != nil {
			return "", err
		}
	}
	old, err := dropTopLevelKeys(fmText, "do", "dont", "why", "summary")
	if err != nil {
		return "", err
	}
	header, err := mergeFrontmatter(add, old)
	if err != nil {
		return "", err
	}
	content := "---\n" + header + "\n---\n\n# " + fm.Title + "\n\n" + renderBody(&fm)
	content = matchEOL(content, string(data))
	if err := validateMigrationInvariant(string(data), content); err != nil {
		return "", err
	}
	return content, nil
}

func validateMigrationInvariant(original, candidate string) error {
	oldText, oldBody, _ := SplitFrontmatter(original)
	newText, newBody, had := SplitFrontmatter(candidate)
	if !had {
		return errors.New("migration candidate has no frontmatter")
	}
	old, oldRaw, err := ParseFrontmatter([]byte(oldText))
	if err != nil {
		return err
	}
	modern, newRaw, err := ParseFrontmatter([]byte(newText))
	if err != nil {
		return err
	}
	if old.ID != modern.ID || old.Type != modern.Type || old.VerifiedAt != modern.VerifiedAt {
		return errors.New("migration changed identity, type or verification date")
	}
	// Also reject additions where the original omitted a scope/provenance/caveat
	// field. Iterating oldRaw alone would miss that exposure-changing case.
	oldKnown, newKnown := *old, *modern
	for _, f := range []*Frontmatter{&oldKnown, &newKnown} {
		f.Template, f.TemplateVersion, f.Updated, f.Summary = "", 0, "", ""
		f.Collections, f.Related, f.Blocks = nil, nil, nil
		f.Do, f.Dont, f.Why = "", "", ""
		f.BodySummary, f.Sections, f.BlockContents = "", nil, nil
	}
	if !reflect.DeepEqual(oldKnown, newKnown) {
		return errors.New("migration changed preserved identity, audience, provenance or caveat metadata")
	}
	allowed := map[string]bool{"do": true, "dont": true, "why": true, "summary": true, "template": true, "template_version": true, "updated": true, "collections": true, "blocks": true, "related": true}
	for key, value := range oldRaw {
		if !allowed[key] && !reflect.DeepEqual(value, newRaw[key]) {
			return fmt.Errorf("migration changed preserved metadata %q", key)
		}
	}
	for _, key := range []string{"do", "dont", "why", "summary"} {
		if declaredKey(newRaw, key) {
			return fmt.Errorf("migration left prose field %q in YAML", key)
		}
	}
	content, err := ReadAuthoring(modern, newBody)
	if err != nil {
		return err
	}
	if len(content.MissingSections) > 0 {
		return fmt.Errorf("migration candidate has content gaps: %v", content.MissingSections)
	}
	spec := NewNoteSpec{Type: modern.Type, Title: modern.Title, Template: modern.Template, TemplateVersion: modern.TemplateVersion,
		Summary: content.Summary, Sections: content.Sections, Collections: []string(modern.Collections), Tags: []string(modern.Tags)}
	for _, block := range content.Blocks {
		spec.Blocks = append(spec.Blocks, BlockSpec{Template: block.Template, Version: block.Version, ID: block.ID, Fields: block.Fields})
	}
	if err := ValidateSpec(spec); err != nil {
		return fmt.Errorf("migration candidate violates authoring contract: %w", err)
	}
	// Preserve every existing ordinary wiki reference, including historical guidance.
	required := append([]string(nil), old.Related...)
	clean, _ := StripNonContent(oldBody)
	for _, s := range ReadLegacy(old).AuthoredText() {
		clean += "\n" + s
	}
	for _, match := range wikilinkTarget.FindAllStringSubmatch(clean, -1) {
		required = append(required, strings.TrimSpace(match[1]))
	}
	present := map[string]bool{}
	for _, id := range modern.Related {
		present[id] = true
	}
	for _, id := range normalizeLinks(required) {
		if id != "" && !present[id] {
			return fmt.Errorf("migration dropped original reference %q", id)
		}
	}
	return nil
}

// ApplyAuthoringMigration applies only explicitly selected, unchanged previews.
// Callers must serialize note replacement with other in-place vault writers.
// Each note is atomic, backed up first and receipted; a batch is not one transaction.
// Partial receipts are returned alongside an error so completed work is never hidden.
func ApplyAuthoringMigration(ctx context.Context, root string, preview *MigrationPreview, approval MigrationApproval) ([]MigrationReceipt, error) {
	if err := RequireAuthoringWrites(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if preview == nil || preview.Version != 1 || len(preview.Entries) == 0 || len(preview.Entries) > MaxAuthoringList {
		return nil, errors.New("invalid migration preview")
	}
	hash, err := previewHash(preview)
	if err != nil {
		return nil, err
	}
	if hash != preview.Hash || approval.PreviewHash != hash || len(approval.IDs) == 0 {
		return nil, errors.New("migration requires the exact reviewed preview hash and explicit note IDs")
	}
	selected := map[string]bool{}
	for _, id := range approval.IDs {
		if selected[id] {
			return nil, errors.New("duplicate reviewed migration ID")
		}
		selected[id] = true
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	var entries []MigrationEntry
	seenIDs, seenPaths := map[string]bool{}, map[string]bool{}
	// Check the entire reviewed selection before any mutation.
	for _, entry := range preview.Entries {
		if seenIDs[entry.ID] || seenPaths[entry.Path] {
			return nil, errors.New("duplicate migration preview source")
		}
		seenIDs[entry.ID], seenPaths[entry.Path] = true, true
		if !selected[entry.ID] {
			continue
		}
		rel, err := migrationPath(entry.Path)
		if err != nil {
			return nil, err
		}
		if !authoringID(entry.ID) || contentHash([]byte(entry.Original)) != entry.OriginalHash || contentHash([]byte(entry.Content)) != entry.ResultHash {
			return nil, errors.New("migration artifact integrity mismatch")
		}
		originalFM, _, _ := SplitFrontmatter(entry.Original)
		fm, _, err := ParseFrontmatter([]byte(originalFM))
		if err != nil || fm.ID != entry.ID {
			return nil, errors.New("migration artifact identity mismatch")
		}
		if err := validateMigrationInvariant(entry.Original, entry.Content); err != nil {
			return nil, err
		}
		current, err := ReadConfinedFile(ctx, root, rel, MaxMigrationPreviewBytes)
		if err != nil {
			return nil, err
		}
		currentHash := contentHash(current)
		if currentHash != entry.OriginalHash && currentHash != entry.ResultHash {
			return nil, fmt.Errorf("%s changed after preview; create a new preview", rel)
		}
		if info, err := handle.Lstat(rel); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o222 == 0 {
			return nil, fmt.Errorf("%s: migration source is not an editable regular file", rel)
		}
		entries = append(entries, entry)
		delete(selected, entry.ID)
	}
	if len(selected) != 0 {
		return nil, errors.New("reviewed ID is absent from the preview")
	}
	var receipts []MigrationReceipt
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return receipts, err
		}
		current, err := ReadConfinedFile(ctx, root, entry.Path, MaxMigrationPreviewBytes)
		if err != nil {
			return receipts, err
		}
		currentHash := contentHash(current)
		if currentHash != entry.OriginalHash && currentHash != entry.ResultHash {
			return receipts, fmt.Errorf("%s changed before publication", entry.Path)
		}
		dir := filepath.Join(".mesh", "migrations", hash)
		backup := filepath.Join(dir, entry.OriginalHash+"-original.md")
		receiptPath := filepath.Join(dir, entry.OriginalHash+"-receipt.json")
		if err := handle.MkdirAll(dir, 0o700); err != nil {
			return receipts, err
		}
		receipt := MigrationReceipt{ID: entry.ID, Path: entry.Path, PreviewHash: hash, OriginalHash: entry.OriginalHash, ResultHash: entry.ResultHash, OriginalPath: backup, ReceiptPath: receiptPath, State: "prepared"}
		if err := writeMigrationOriginal(handle, backup, []byte(entry.Original)); err != nil {
			return receipts, err
		}
		if err := writeMigrationReceipt(handle, receiptPath, receipt); err != nil {
			return receipts, err
		}
		if currentHash == entry.ResultHash {
			receipt.AlreadyApplied = true
		} else {
			if err := ctx.Err(); err != nil {
				return receipts, err
			}
			if err := writeMigrationAtomic(handle, entry.Path, []byte(entry.Content)); err != nil {
				return receipts, err
			}
		}
		receipt.State, receipt.AppliedAt = "applied", Now().UTC().Format(time.RFC3339)
		if err := writeMigrationReceipt(handle, receiptPath, receipt); err != nil {
			return append(receipts, receipt), fmt.Errorf("note applied; final receipt update failed: %w", err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func writeMigrationOriginal(root *os.Root, path string, data []byte) error {
	f, err := root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := root.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if contentHash(existing) != contentHash(data) {
			return errors.New("migration backup conflicts with the original")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		root.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		root.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		root.Remove(path)
		return err
	}
	syncMigrationDir(root, filepath.Dir(path))
	return nil
}

func writeMigrationReceipt(root *os.Root, path string, receipt MigrationReceipt) error {
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	return writeMigrationAtomic(root, path, append(data, '\n'))
}

func writeMigrationAtomic(root *os.Root, path string, data []byte) error {
	dir := filepath.Dir(path)
	mode := os.FileMode(0o600)
	if info, err := root.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("migration destination is not a regular file")
		}
		mode = info.Mode().Perm()
		if mode&0o222 == 0 {
			return os.ErrPermission
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Exclusive temp claims and rooted rename keep replacement inside this vault.
	name := filepath.Join(dir, ".mesh-migration-"+contentHash(data)[:16]+"-"+fmt.Sprint(Now().UnixNano()))
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer root.Remove(name)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Rename(name, path); err != nil {
		return err
	}
	syncMigrationDir(root, dir)
	return nil
}

func syncMigrationDir(root *os.Root, path string) {
	if dir, err := root.Open(path); err == nil {
		dir.Sync()
		dir.Close()
	}
}
