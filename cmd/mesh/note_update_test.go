// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

func TestCLIUpdatePreparesValidatesAndPublishesSameIdentity(t *testing.T) {
	root := t.TempDir()
	first, err := vault.CreateNote(root, vault.NewNoteSpec{Template: "finding", Title: "CLI schema fixture", Summary: "The fixture describes revision one.", Sections: completeFindingSections(), Author: "original"})
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(first.Path)
	// Model a reviewed migrated note. CLI JSON must retain a dotted stored tag
	// without depending on private in-memory normalization state.
	original = []byte(strings.Replace(string(original), "---\n", "---\ntags: [oauth-2.1]\n", 1))
	if err := os.WriteFile(first.Path, original, 0600); err != nil {
		t.Fatal(err)
	}
	first.Revision = vault.ContentRevision(original)
	var out bytes.Buffer
	cmd := rootCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"update", first.ID, "--vault", root})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var spec vault.NewNoteSpec
	if err := json.Unmarshal(out.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.UpdateID != first.ID || spec.UpdateRevision != first.Revision {
		t.Fatal("prepared object missing revision")
	}
	spec.Title, spec.Summary = "Renamed CLI schema fixture", "The fixture describes revision two; no live verification is asserted."
	raw, _ := json.Marshal(spec)
	input := filepath.Join(t.TempDir(), "edited.json")
	if err := os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, validate := range []bool{true, false} {
		cmd = rootCmd()
		out.Reset()
		cmd.SetOut(&out)
		args := []string{"update", first.ID, "--vault", root, "--spec", input, "--by", "editor"}
		if validate {
			args = append(args, "--validate")
		}
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(first.Path)
		if validate {
			if string(data) != string(original) {
				t.Fatal("CLI validation wrote source")
			}
			continue
		}
		fm, _, err := vault.ParseFrontmatter(data)
		if err != nil || fm.ID != first.ID || fm.Title != spec.Title || fm.Author != "original" || fm.UpdatedBy != "editor" || fm.UpdatedAgent != "mesh-cli" || len(fm.Tags) != 1 || fm.Tags[0] != "oauth-2.1" {
			t.Fatalf("CLI metadata incorrect: %+v %v", fm, err)
		}
		if !strings.Contains(out.String(), `"updated":true`) {
			t.Fatal("missing durable update receipt")
		}
	}
	cmd = rootCmd()
	cmd.SetArgs([]string{"update", first.ID, "--vault", root, "--spec", input})
	if err := cmd.Execute(); err == nil {
		t.Fatal("CLI accepted stale update")
	}
	files, _ := vault.Walk(root)
	if len(files) != 1 {
		t.Fatal("CLI replacement created another note")
	}
}

func TestCLIUpdateRefusesInventedLinksAndMismatchedIdentity(t *testing.T) {
	root := t.TempDir()
	first, err := vault.CreateNote(root, vault.NewNoteSpec{Template: "finding", Title: "CLI intact schema", Summary: "The fixture describes one schema.", Sections: completeFindingSections()})
	if err != nil {
		t.Fatal(err)
	}
	before, err := vault.NoteSnapshotContext(t.Context(), root, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "edited.json")
	for _, id := range []string{first.ID, "different-id"} {
		spec := before.Spec
		spec.UpdateID = id
		spec.Related = []string{"invented-decision"}
		raw, _ := json.Marshal(spec)
		if err := os.WriteFile(input, raw, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := rootCmd()
		cmd.SetArgs([]string{"update", first.ID, "--vault", root, "--spec", input})
		if err := cmd.Execute(); err == nil {
			t.Fatal("invalid update accepted")
		}
	}
	data, _ := os.ReadFile(first.Path)
	if string(data) != string(before.Content) {
		t.Fatal("refused CLI update changed source")
	}
}
