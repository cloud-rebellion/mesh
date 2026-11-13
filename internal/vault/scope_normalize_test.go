// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sharedScopeFixture(scope string) []byte {
	return []byte("---\n# existing provenance comment\nid: scope-test\ntype: note\ntitle: Scope test\ncreated: 2026-01-02\nauthor: Original Author\nrelated: [existing-note]\ncustom:\n  nested: [one, 'two: three']\n  date: 2026-03-04\n" + scope + "---\n# Scope test\n\nBody\r\n<!-- comment is literal body -->\n\x00unchanged\n")
}

func TestPlanSharedScopeNormalizationPreservesProvenance(t *testing.T) {
	for _, scope := range []string{"scope: hash # exact original label\n", "scope: ['Hash stage', dev]\n", "scope:\n  - hash\n  - 'stage dev' # list comment\n"} {
		t.Run(strings.ReplaceAll(scope, "\n", "|"), func(t *testing.T) {
			original := sharedScopeFixture(scope)
			beforeHeader, beforeBody, _ := SplitFrontmatter(string(original))
			_, before, err := ParseFrontmatter([]byte(beforeHeader))
			if err != nil {
				t.Fatal(err)
			}
			out, changed, err := PlanSharedScopeNormalization(original)
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			afterHeader, afterBody, _ := SplitFrontmatter(string(out))
			fm, after, err := ParseFrontmatter([]byte(afterHeader))
			if err != nil || afterBody != beforeBody || !reflect.DeepEqual(fm.Scope, StringList{"dev"}) {
				t.Fatalf("body/scope changed incorrectly: %v", err)
			}
			migration := after["scope_migration"].(map[string]interface{})
			if migration["schema"] != 1 || migration["reason"] != SharedScopeNormalizationReason || !reflect.DeepEqual(migration["original_scope"], before["scope"]) {
				t.Fatalf("original scope provenance differs: %#v", migration)
			}
			delete(before, "scope")
			delete(after, "scope")
			delete(after, "scope_migration")
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("unrelated metadata changed: %#v vs %#v", before, after)
			}
			for _, comment := range []string{"# existing provenance comment", "# exact original label", "# list comment"} {
				if bytes.Contains(original, []byte(comment)) && !bytes.Contains(out, []byte(comment)) {
					t.Fatalf("lost comment %q", comment)
				}
			}
			second, changed, err := PlanSharedScopeNormalization(out)
			if err != nil || changed || !bytes.Equal(second, out) {
				t.Fatalf("not idempotent: changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestPlanSharedScopeNormalizationCRLFAndDelimiters(t *testing.T) {
	original := bytes.ReplaceAll(sharedScopeFixture("scope: hash\n"), []byte("\n"), []byte("\r\n"))
	original = bytes.Replace(original, []byte("---\r\n"), []byte("\xef\xbb\xbf--- \t\r\n"), 1)
	_, _, closer, body, err := scopeNormalizationParts(original)
	if err != nil {
		t.Fatal(err)
	}
	out, changed, err := PlanSharedScopeNormalization(original)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	opener, header, newCloser, newBody, err := scopeNormalizationParts(out)
	if err != nil || !bytes.Equal(body, newBody) || !bytes.Equal(closer, newCloser) || !bytes.Equal(opener, []byte("\xef\xbb\xbf--- \t\r\n")) {
		t.Fatalf("delimiter/body preservation failed: %v", err)
	}
	if bytes.Contains(bytes.ReplaceAll(header, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatal("header contains bare LF")
	}
}

func TestPlanSharedScopeNormalizationNoOpAndRefusals(t *testing.T) {
	for _, scope := range []string{"", "scope: dev\n", "scope: [dev]\n", "scope: null\n", "scope: []\n", "scope: ''\n"} {
		original := sharedScopeFixture(scope)
		out, changed, err := PlanSharedScopeNormalization(original)
		if err != nil || changed || !bytes.Equal(out, original) {
			t.Fatalf("no-op %q: changed=%v err=%v", scope, changed, err)
		}
	}
	for name, data := range map[string][]byte{
		"draft":         bytes.Replace(sharedScopeFixture("scope: hash\n"), []byte("type: note"), []byte("type: note\nstatus: draft"), 1),
		"missing id":    bytes.Replace(sharedScopeFixture("scope: hash\n"), []byte("id: scope-test\n"), nil, 1),
		"invalid type":  bytes.Replace(sharedScopeFixture("scope: hash\n"), []byte("type: note"), []byte("type: spaceship"), 1),
		"unterminated":  []byte("---\nid: test\n"),
		"absent":        []byte("No header\n"),
		"bad yaml":      sharedScopeFixture("scope: [hash\n"),
		"duplicate":     sharedScopeFixture("scope: hash\nscope: stage\n"),
		"number":        sharedScopeFixture("scope: 42\n"),
		"nested list":   sharedScopeFixture("scope: [[hash]]\n"),
		"anchor":        sharedScopeFixture("scope: &s hash\n"),
		"alias":         sharedScopeFixture("saved: &s hash\nscope: *s\n"),
		"merge":         sharedScopeFixture("saved: &s {scope: hash}\n<<: *s\n"),
		"collision":     sharedScopeFixture("scope: hash\nscope_migration: {schema: 1}\n"),
		"dev collision": sharedScopeFixture("scope: dev\nscope_migration: {schema: 1, reason: unknown, original_scope: hash}\n"),
		"bad original":  sharedScopeFixture("scope: dev\nscope_migration: {schema: 1, reason: shared-vault-topic-label-correction, original_scope: dev}\n"),
		"extra field":   sharedScopeFixture("scope: dev\nscope_migration: {schema: 1, reason: shared-vault-topic-label-correction, original_scope: hash, extra: true}\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, changed, err := PlanSharedScopeNormalization(data); err == nil || changed {
				t.Fatalf("unsafe source accepted: changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestNormalizeSharedScopeFilePinnedPreviewAndPublication(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("notes", "scope-test.md")
	path := filepath.Join(root, rel)
	original := sharedScopeFixture("scope: hash\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	digest := ContentRevision(original)
	preview, err := NormalizeSharedScopeFile(t.Context(), root, rel, digest, true)
	if err != nil || !preview.Changed || preview.Written || preview.ID != "scope-test" {
		t.Fatalf("preview: %+v err=%v", preview, err)
	}
	if current, _ := os.ReadFile(path); !bytes.Equal(current, original) {
		t.Fatal("preview wrote the note")
	}
	applied, err := NormalizeSharedScopeFile(t.Context(), root, rel, digest, false)
	if err != nil || !applied.Written || applied.AfterSHA != preview.AfterSHA {
		t.Fatalf("apply: %+v err=%v", applied, err)
	}
	current, _ := os.ReadFile(path)
	if ContentRevision(current) != applied.AfterSHA {
		t.Fatal("published bytes differ from receipt")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatal("note permissions changed")
	}
	if _, err := NormalizeSharedScopeFile(t.Context(), root, rel, digest, false); err == nil {
		t.Fatal("stale original SHA accepted")
	}
	again, err := NormalizeSharedScopeFile(t.Context(), root, rel, applied.AfterSHA, false)
	if err != nil || again.Changed || again.Written {
		t.Fatalf("idempotent apply: %+v err=%v", again, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatal("left temporary files")
	}
}

func TestNormalizeSharedScopeFileRefusesUnsafeFilesystem(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := sharedScopeFixture("scope: hash\n")
	digest := ContentRevision(original)
	path := filepath.Join(root, "note.md")
	if err := os.WriteFile(path, original, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeSharedScopeFile(t.Context(), root, "note.md", digest, false); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("read-only note: %v", err)
	}
	if _, err := NormalizeSharedScopeFile(t.Context(), root, "note.md", digest, true); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("read-only preview must fail full-cohort admission: %v", err)
	}
	if err := os.Symlink(path, filepath.Join(root, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"linked.md", "linked-dir/note.md", "../note.md", "a/../note.md", "note.txt"} {
		if _, err := NormalizeSharedScopeFile(t.Context(), root, rel, digest, false); err == nil {
			t.Fatalf("unsafe path %q accepted", rel)
		}
	}
	if _, err := NormalizeSharedScopeFile(t.Context(), filepath.Join(root, "linked-dir"), "note.md", digest, true); err == nil {
		t.Fatal("linked root accepted")
	}
	if _, err := NormalizeSharedScopeFile(t.Context(), root, "note.md", "not-a-sha", true); err == nil {
		t.Fatal("invalid manifest SHA accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NormalizeSharedScopeFile(ctx, root, "note.md", digest, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context: %v", err)
	}
	if current, _ := os.ReadFile(path); !bytes.Equal(current, original) {
		t.Fatal("refused operation changed original")
	}
}

type sharedScopeFileInfo struct {
	os.FileInfo
	metadata any
}

func (f sharedScopeFileInfo) Sys() any { return f.metadata }

func TestSharedScopeNormalizationOwnershipGuard(t *testing.T) {
	metadata := struct{ Uid, Gid uint32 }{uint32(os.Geteuid()), uint32(os.Getegid())}
	owner := sharedScopeFileInfo{metadata: &metadata}
	if err := scopeNormalizationCallerOwns(owner); err != nil {
		t.Fatal(err)
	}
	otherUID := metadata
	otherUID.Uid++
	if err := scopeNormalizationCallerOwns(sharedScopeFileInfo{metadata: &otherUID}); err == nil {
		t.Fatal("another file owner accepted")
	}
	otherGroup := metadata
	otherGroup.Gid++
	if err := scopeNormalizationSameOwnership(owner, sharedScopeFileInfo{metadata: &otherGroup}); err == nil {
		t.Fatal("a differently inherited temporary-file group accepted")
	}
	if err := scopeNormalizationSameOwnership(owner, owner); err != nil {
		t.Fatal(err)
	}
	for _, metadata := range []any{nil, "unsupported", struct{ Other uint32 }{1}} {
		if err := scopeNormalizationCallerOwns(sharedScopeFileInfo{metadata: metadata}); err == nil {
			t.Fatalf("unverifiable ownership accepted: %#v", metadata)
		}
	}
}
