// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

const SharedScopeNormalizationReason = "shared-vault-topic-label-correction"

// ScopeNormalizationResult describes an operator-reviewed, hash-pinned file.
// Changed is also set for a preview; Written is set only after atomic publication.
type ScopeNormalizationResult struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	BeforeSHA string `json:"before_sha256"`
	AfterSHA  string `json:"after_sha256"`
	Changed   bool   `json:"changed"`
	Written   bool   `json:"written"`
}

// PlanSharedScopeNormalization prepares an explicitly approved shared-vault
// correction. It does NOT decide whether an audience may be changed. Callers must
// independently establish that authorization and preserve the reviewed originals.
// Published notes with non-dev scope declarations become [dev]; their original
// YAML scalar/list value is retained under scope_migration.original_scope. Other
// metadata values and comments survive, and body bytes are never re-rendered.
// Drafts and colliding migration metadata are refused rather than guessed.
func PlanSharedScopeNormalization(data []byte) ([]byte, bool, error) {
	opener, header, closer, body, err := scopeNormalizationParts(data)
	if err != nil {
		return nil, false, err
	}
	fm, original, err := ParseFrontmatter(header)
	if err != nil || fm == nil || fm.ID == "" || !fm.Type.Valid() {
		return nil, false, errors.New("scope normalization requires valid identified frontmatter")
	}
	if IsDraft(fm) {
		return nil, false, errors.New("scope normalization refuses drafts")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(header, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, errors.New("scope normalization requires a YAML mapping")
	}
	mapping := doc.Content[0]
	scopeIndex, migrationIndex := -1, -1
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Kind != yaml.ScalarNode || mapping.Content[i].Tag == "!!merge" {
			return nil, false, errors.New("scope normalization refuses merged or complex top-level metadata")
		}
		switch mapping.Content[i].Value {
		case "scope":
			scopeIndex = i
		case "scope_migration":
			migrationIndex = i
		}
	}
	nonDev := false
	if scopeIndex >= 0 {
		nonDev, err = scopeNormalizationNonDev(mapping.Content[scopeIndex+1])
		if err != nil {
			return nil, false, err
		}
	}
	if migrationIndex >= 0 {
		if nonDev || !validSharedScopeMigration(mapping.Content[migrationIndex+1]) {
			return nil, false, errors.New("scope_migration collision; refusing to overwrite provenance")
		}
		return data, false, nil
	}
	if !nonDev {
		return data, false, nil
	}
	oldScope := mapping.Content[scopeIndex+1]
	mapping.Content[scopeIndex+1] = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle,
		Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: DefaultScope}}}
	migration := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "schema"}, {Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "reason"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: SharedScopeNormalizationReason},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "original_scope"}, oldScope,
	}}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "scope_migration"}, migration)
	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := encoder.Close(); err != nil {
		return nil, false, err
	}
	newHeader := encoded.Bytes()
	if bytes.HasSuffix(opener, []byte("\r\n")) {
		newHeader = bytes.ReplaceAll(newHeader, []byte("\n"), []byte("\r\n"))
	}
	out := make([]byte, 0, len(data)+len(newHeader))
	out = append(out, opener...)
	out = append(out, newHeader...)
	out = append(out, closer...)
	out = append(out, body...)
	_, checkHeader, _, checkBody, err := scopeNormalizationParts(out)
	if err != nil || !bytes.Equal(body, checkBody) {
		return nil, false, errors.New("scope normalization changed body bytes")
	}
	currentFM, current, err := ParseFrontmatter(checkHeader)
	if err != nil || currentFM.ID != fm.ID || !reflect.DeepEqual(currentFM.EffectiveScopes(), []string{DefaultScope}) {
		return nil, false, errors.New("scope normalization failed frontmatter verification")
	}
	delete(original, "scope")
	delete(current, "scope")
	delete(current, "scope_migration")
	if !reflect.DeepEqual(original, current) {
		return nil, false, errors.New("scope normalization changed other metadata")
	}
	return out, true, nil
}

func scopeNormalizationParts(data []byte) (opener, header, closer, body []byte, err error) {
	if len(data) > 1<<20 {
		return nil, nil, nil, nil, ErrConfinedFileTooLarge
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) < 2 || !frontmatterDelimiter(strings.TrimSuffix(string(lines[0]), "\n"), true) {
		return nil, nil, nil, nil, errors.New("scope normalization requires frontmatter")
	}
	for i := 1; i < len(lines); i++ {
		if frontmatterDelimiter(strings.TrimSuffix(string(lines[i]), "\n"), false) {
			return lines[0], bytes.Join(lines[1:i], nil), lines[i], bytes.Join(lines[i+1:], nil), nil
		}
	}
	return nil, nil, nil, nil, ErrUnterminatedFrontmatter
}

// An original scope is a string or list of strings, never an alias/anchor whose
// meaning could change when it moves to the provenance record.
func scopeNormalizationNonDev(n *yaml.Node) (bool, error) {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return false, errors.New("scope normalization refuses scope aliases or anchors")
	}
	if n.Kind == yaml.ScalarNode {
		if n.Tag == "!!null" {
			return false, nil
		}
		if n.Tag != "!!str" {
			return false, errors.New("scope normalization requires string scope values")
		}
		s := strings.TrimSpace(n.Value)
		return s != "" && s != DefaultScope, nil
	}
	if n.Kind != yaml.SequenceNode {
		return false, errors.New("scope normalization requires a scalar or list scope")
	}
	nonDev := false
	for _, child := range n.Content {
		if child.Kind != yaml.ScalarNode || child.Tag != "!!str" {
			return false, errors.New("scope normalization requires a list of string scopes")
		}
		changed, err := scopeNormalizationNonDev(child)
		if err != nil {
			return false, err
		}
		nonDev = nonDev || changed
	}
	return nonDev, nil
}

func validSharedScopeMigration(n *yaml.Node) bool {
	if n.Kind != yaml.MappingNode || n.Anchor != "" || len(n.Content) != 6 {
		return false
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i].Value, n.Content[i+1]
		if seen[key] {
			return false
		}
		seen[key] = true
		switch key {
		case "schema":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!int" || value.Value != "1" {
				return false
			}
		case "reason":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" || value.Value != SharedScopeNormalizationReason {
				return false
			}
		case "original_scope":
			if nonDev, err := scopeNormalizationNonDev(value); err != nil || !nonDev {
				return false
			}
		default:
			return false
		}
	}
	return seen["schema"] && seen["reason"] && seen["original_scope"]
}

// NormalizeSharedScopeFile publishes one hash-pinned operator correction. The
// caller MUST hold an exclusive vault maintenance window: inode/hash checks detect
// drift but cannot serialize arbitrary external editors with an atomic rename.
// This is an operator primitive, not an MCP permission or audience policy decision.
func NormalizeSharedScopeFile(ctx context.Context, root, relativePath, expectedSHA string, dryRun bool) (ScopeNormalizationResult, error) {
	result := ScopeNormalizationResult{Path: relativePath}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(expectedSHA) != 64 || strings.ToLower(expectedSHA) != expectedSHA {
		return result, errors.New("scope normalization requires an exact SHA256")
	}
	if _, err := hex.DecodeString(expectedSHA); err != nil {
		return result, errors.New("scope normalization requires an exact SHA256")
	}
	parent, name, err := scopeNormalizationParent(root, relativePath)
	if err != nil {
		return result, err
	}
	defer parent.Close()
	data, info, err := scopeNormalizationRead(ctx, parent, name)
	if err != nil {
		return result, err
	}
	result.BeforeSHA = ContentRevision(data)
	if result.BeforeSHA != expectedSHA {
		return result, errors.New("scope normalization source changed after manifest")
	}
	header, _, _ := SplitFrontmatter(string(data))
	if fm, _, err := ParseFrontmatter([]byte(header)); err == nil {
		result.ID = fm.ID
	}
	out, changed, err := PlanSharedScopeNormalization(data)
	if err != nil {
		return result, err
	}
	result.Changed, result.AfterSHA = changed, ContentRevision(out)
	if info.Mode().Perm()&0o222 == 0 {
		return result, os.ErrPermission
	}
	if err := scopeNormalizationCallerOwns(info); err != nil {
		return result, err
	}
	if dryRun || !changed {
		return result, nil
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return result, err
	}
	temp := ".mesh-scope-" + hex.EncodeToString(random[:])
	file, err := parent.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return result, err
	}
	defer parent.Remove(temp)
	created, err := file.Stat()
	if err != nil {
		file.Close()
		return result, err
	}
	if err := scopeNormalizationSameOwnership(info, created); err != nil {
		file.Close()
		return result, err
	}
	if err := file.Chmod(info.Mode().Perm()); err != nil {
		file.Close()
		return result, err
	}
	if _, err := file.Write(out); err != nil {
		file.Close()
		return result, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return result, err
	}
	if err := file.Close(); err != nil {
		return result, err
	}
	current, currentInfo, err := scopeNormalizationRead(ctx, parent, name)
	if err != nil || !os.SameFile(info, currentInfo) || ContentRevision(current) != expectedSHA {
		return result, errors.New("scope normalization source changed before publication")
	}
	if err := scopeNormalizationSameState(info, currentInfo); err != nil {
		return result, err
	}
	// Reopen the named directory chain as well: publishing through a directory
	// handle that has since been moved would update an unreviewed vault path.
	namedParent, _, err := scopeNormalizationParent(root, relativePath)
	if err != nil {
		return result, err
	}
	oldDir, oldErr := parent.Stat(".")
	newDir, newErr := namedParent.Stat(".")
	namedParent.Close()
	if oldErr != nil || newErr != nil || !os.SameFile(oldDir, newDir) {
		return result, errors.New("scope normalization directory changed before publication")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := parent.Rename(temp, name); err != nil {
		return result, err
	}
	result.Written = true
	dir, err := parent.Open(".")
	if err != nil {
		return result, fmt.Errorf("scope normalization published; directory sync failed: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return result, fmt.Errorf("scope normalization published; directory sync failed: %w", err)
	}
	return result, nil
}

func scopeNormalizationParent(root, rel string) (*os.Root, string, error) {
	if !filepath.IsLocal(rel) || filepath.Clean(rel) != rel || filepath.Ext(rel) != ".md" {
		return nil, "", errors.New("invalid scope normalization note path")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil || canonical != abs {
		return nil, "", errors.New("scope normalization refuses a linked vault root")
	}
	dir, err := os.OpenRoot(abs)
	if err != nil {
		return nil, "", err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		info, err := dir.Lstat(part)
		if err != nil || !info.IsDir() {
			dir.Close()
			return nil, "", errors.New("scope normalization refuses a linked or missing directory")
		}
		next, err := dir.OpenRoot(part)
		if err != nil {
			dir.Close()
			return nil, "", err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			next.Close()
			dir.Close()
			return nil, "", errors.New("scope normalization directory changed")
		}
		dir.Close()
		dir = next
	}
	return dir, parts[len(parts)-1], nil
}

func scopeNormalizationRead(ctx context.Context, parent *os.Root, name string) ([]byte, os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	info, err := parent.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil, errors.New("scope normalization requires a regular, unlinked note")
	}
	file, err := parent.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, nil, errors.New("scope normalization note identity changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > 1<<20 {
		return nil, nil, ErrConfinedFileTooLarge
	}
	named, err := parent.Lstat(name)
	if err != nil || !os.SameFile(info, named) {
		return nil, nil, errors.New("scope normalization note changed during read")
	}
	if err := scopeNormalizationSameState(info, named); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return data, info, nil
}

// FileInfo.Sys is platform-specific. Linux and Darwin (the supported migration
// hosts) expose numeric Uid/Gid fields. Inspect those without an OS-specific type
// import so other builds still compile, and refuse publication when ownership
// cannot be established. No chown is performed and an operator must use the
// existing vault owner; a differently inherited temp-file group is also refused.
func scopeNormalizationOwnership(info os.FileInfo) (uint64, uint64, error) {
	if info == nil || info.Sys() == nil {
		return 0, 0, errors.New("scope normalization cannot verify file ownership on this platform")
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return 0, 0, errors.New("scope normalization cannot verify file ownership on this platform")
	}
	uid, gid := value.FieldByName("Uid"), value.FieldByName("Gid")
	if !uid.IsValid() || !gid.IsValid() || !uid.CanUint() || !gid.CanUint() {
		return 0, 0, errors.New("scope normalization cannot verify file ownership on this platform")
	}
	return uid.Uint(), gid.Uint(), nil
}

func scopeNormalizationCallerOwns(info os.FileInfo) error {
	uid, _, err := scopeNormalizationOwnership(info)
	if err != nil {
		return err
	}
	if os.Geteuid() < 0 || uid != uint64(os.Geteuid()) {
		return errors.New("scope normalization must run as the existing vault file owner")
	}
	return nil
}

func scopeNormalizationSameOwnership(before, after os.FileInfo) error {
	uid, gid, err := scopeNormalizationOwnership(before)
	if err != nil {
		return err
	}
	newUID, newGID, err := scopeNormalizationOwnership(after)
	if err != nil {
		return err
	}
	if uid != newUID || gid != newGID {
		return errors.New("scope normalization refuses a file ownership change")
	}
	return nil
}

func scopeNormalizationSameState(before, after os.FileInfo) error {
	if before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("scope normalization note metadata changed")
	}
	return scopeNormalizationSameOwnership(before, after)
}
