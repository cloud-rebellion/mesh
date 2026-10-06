// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRequireRootRefusesWhatIsNotAVault covers the predicate itself, including the
// case the CLI test cannot reach cheaply: a --vault that names an existing FILE.
func TestRequireRootRefusesWhatIsNotAVault(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(file, []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "nonexistent-typo")

	if err := RequireRoot(dir); err != nil {
		t.Errorf("RequireRoot(%q) on an existing directory: %v, want nil", dir, err)
	}
	for _, tc := range []struct{ name, root string }{
		{"missing path", missing},
		{"a file, not a directory", file},
	} {
		err := RequireRoot(tc.root)
		if err == nil {
			t.Errorf("RequireRoot(%q) [%s] returned nil, want a refusal", tc.root, tc.name)
			continue
		}
		if !errors.Is(err, ErrNoVault) {
			t.Errorf("RequireRoot(%q) [%s] = %v, want an error matching ErrNoVault", tc.root, tc.name, err)
		}
	}
}

// TestCreateNoteWillNotInventTheVault is the engine-level half of the phantom-vault
// fix. The CLI is not the only caller: the MCP write-back tool (mesh_append_note) and
// the web pending API reach this same writer, so the check lives here where MkdirAll
// does the inventing, not only at the flag that happened to be measured.
func TestCreateNoteWillNotInventTheVault(t *testing.T) {
	ghost := filepath.Join(t.TempDir(), "nonexistent-typo")
	res, err := CreateNote(ghost, completeFixtureSpec(NewNoteSpec{Type: TypeDecision, Title: "Typo test", Do: "a", Dont: "b", Why: "c"}))
	if err == nil {
		t.Fatalf("CreateNote created a note at %s in a vault that does not exist", res.Path)
	}
	if !errors.Is(err, ErrNoVault) {
		t.Fatalf("CreateNote(%q) = %v, want an error matching ErrNoVault", ghost, err)
	}
	if _, statErr := os.Stat(ghost); statErr == nil {
		t.Fatalf("CreateNote refused but still created %s; nothing may reach the filesystem before the check", ghost)
	}
}
