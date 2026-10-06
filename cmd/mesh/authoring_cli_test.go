// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

func completeFindingSections() map[string]string {
	return map[string]string{
		"question":    "Does the local CLI publish a complete ordinary note?",
		"findings":    "The fixture exercises the actual CLI publication path.",
		"evidence":    "The verification reads the resulting local Markdown file.",
		"limitations": "This fixture does not establish deployment or remote availability.",
		"next_steps":  "Production acceptance remains a separate workflow.",
	}
}

func TestAuthoringCLIRejectsInvalidReferenceWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name, target, reference string
	}{
		{"unknown target", "", "[[missing-note]]"},
		{"unknown anchor", "---\nid: target\nscope: [dev]\n---\n# Target\n## Evidence\nObserved fixture.\n", "[[target#absent]]"},
		{"broader audience", "---\nid: target\nscope: [finance]\n---\n# Target\nPrivate fixture.\n", "[[target]]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.target != "" {
				if err := os.WriteFile(filepath.Join(root, "target.md"), []byte(tc.target), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := vault.Walk(root)
			sections := completeFindingSections()
			sections["evidence"] += " " + tc.reference
			raw, _ := json.Marshal(sections)
			sectionsFile := filepath.Join(t.TempDir(), "sections.json")
			if err := os.WriteFile(sectionsFile, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := rootCmd()
			cmd.SetArgs([]string{"new", "finding", "Invalid linked finding", "--vault", root, "--summary", "The fixture checks invalid link rejection.", "--sections-file", sectionsFile})
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "reference") {
				t.Fatalf("invalid reference accepted: %v", err)
			}
			after, _ := vault.Walk(root)
			if len(after) != len(before) {
				t.Fatalf("rejected note escaped publication gate: %v", after)
			}
		})
	}
}

func TestLocalAuthoringReferencesUseCurrentFilesWithoutIndex(t *testing.T) {
	root := t.TempDir()
	data := []byte("---\nid: target\nscope: [dev, finance]\n---\n# Target\n## Evidence\nObserved fixture.\n")
	if err := os.WriteFile(filepath.Join(root, "target.md"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	spec := vault.NewNoteSpec{Summary: "Supported by [[target#evidence]].", Scope: []string{"dev", "finance"}, Collections: []string{"target"}}
	if err := validateLocalAuthoringReferences(context.Background(), root, spec); err != nil {
		t.Fatalf("current unindexed reference refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".mesh", "mesh.db")); !os.IsNotExist(err) {
		t.Fatalf("reference validation opened or created an index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "target.md"), []byte("---\nid: target\nscope: [dev]\n---\n# Target\n## Evidence\nChanged audience.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateLocalAuthoringReferences(context.Background(), root, spec); err == nil {
		t.Fatal("current audience change was ignored")
	}
}

func TestAuthoringCLIOrdinaryNotePublishesWithoutApproval(t *testing.T) {
	root := t.TempDir()
	sectionFile := filepath.Join(t.TempDir(), "sections.json")
	raw, _ := json.Marshal(completeFindingSections())
	if err := os.WriteFile(sectionFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := rootCmd()
	cmd.SetArgs([]string{"new", "finding", "CLI authored finding", "--vault", root, "--summary", "The fixture checks ordinary CLI publication.", "--sections-file", sectionFile})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "notes", "cli-authored-finding.md"))
	if err != nil {
		t.Fatal(err)
	}
	fm, _, err := vault.ParseFrontmatter(data)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Template != "finding" || fm.TemplateVersion != 1 || fm.Status != "active" {
		t.Fatalf("wrong authored identity: %+v", fm)
	}
	_, body, _ := vault.SplitFrontmatter(string(data))
	content, err := vault.ReadAuthoring(fm, body)
	if err != nil || len(content.MissingSections) != 0 {
		t.Fatalf("incomplete CLI note: %+v %v", content, err)
	}
	if fm.Do != "" || fm.Dont != "" || fm.Why != "" {
		t.Fatal("ordinary CLI note stored retired shorthand")
	}
}

func TestAuthoringCLIIncompletePublishLeavesNoStub(t *testing.T) {
	root := t.TempDir()
	cmd := rootCmd()
	cmd.SetArgs([]string{"new", "finding", "Incomplete CLI finding", "--vault", root, "--status", "active", "--summary", "Only a summary was supplied."})
	if err := cmd.Execute(); err == nil {
		t.Fatal("incomplete ordinary note published")
	}
	files, _ := vault.Walk(root)
	if len(files) != 0 {
		t.Fatalf("stub escaped publication gate: %v", files)
	}
	cmd = rootCmd()
	cmd.SetArgs([]string{"new", "finding", "Incomplete CLI finding", "--vault", root, "--summary", "Only a summary was supplied."})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("incomplete draft refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "inbox", "incomplete-cli-finding.md")); err != nil {
		t.Fatalf("draft not isolated in inbox: %v", err)
	}
}

func TestAuthoringCLIRejectsRetiredInputFormat(t *testing.T) {
	root := t.TempDir()
	cmd := rootCmd()
	cmd.SetArgs([]string{"new", "decision", "Old format", "--vault", root, "--do", "action"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("retired input accepted: %v", err)
	}
	files, _ := vault.Walk(root)
	if len(files) != 0 {
		t.Fatal("retired input published a file")
	}
}

func TestAuthoringCLIDraftPublishRequiresCurrentRevision(t *testing.T) {
	root := t.TempDir()
	first, err := vault.CreateNote(root, vault.NewNoteSpec{Template: "finding", Title: "Revision guarded draft", Status: "draft", Summary: "The fixture records an incomplete draft."})
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	sectionsFile := filepath.Join(t.TempDir(), "sections.json")
	raw, _ := json.Marshal(completeFindingSections())
	if err := os.WriteFile(sectionsFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"new", "finding", "Revision guarded draft", "--vault", root, "--status", "active", "--draft-id", first.ID, "--summary", "The complete fixture resumes its original identity.", "--sections-file", sectionsFile}
	cmd := rootCmd()
	cmd.SetArgs(append(append([]string{}, base...), "--draft-revision", "stale"))
	if err := cmd.Execute(); err == nil {
		t.Fatal("stale draft revision published")
	}
	current, _ := os.ReadFile(first.Path)
	if string(current) != string(original) {
		t.Fatal("stale update altered draft")
	}
	cmd = rootCmd()
	cmd.SetArgs(append(append([]string{}, base...), "--draft-revision", first.Revision))
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	current, err = os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	fm, _, err := vault.ParseFrontmatter(current)
	if err != nil {
		t.Fatal(err)
	}
	if fm.ID != first.ID || fm.Status != "active" {
		t.Fatalf("draft identity/lifecycle lost: %+v", fm)
	}
	files, _ := vault.Walk(root)
	if len(files) != 1 {
		t.Fatalf("draft publication duplicated canonical note: %v", files)
	}
}

func TestAuthoringCLIMigrationPreviewAndExactReviewedApply(t *testing.T) {
	root := t.TempDir()
	original := []byte("---\nid: reviewed-finding\ntype: note\ntitle: Reviewed finding\nwhen: 2026-01-01\ncreated: 2026-01-01\nscope: [dev]\n---\n# Reviewed finding\n\nThe fixture records a bounded CLI observation.\n")
	notePath := filepath.Join(root, "reviewed-finding.md")
	if err := os.WriteFile(notePath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	requestPath := filepath.Join(work, "requests.json")
	previewPath := filepath.Join(work, "preview.json")
	requests := []vault.MigrationRequest{{Path: "reviewed-finding.md", ID: "reviewed-finding", Spec: vault.NewNoteSpec{Template: "finding", TemplateVersion: 1, Title: "Reviewed finding", Summary: "The fixture records a bounded CLI observation.", Sections: completeFindingSections()}}}
	raw, _ := json.Marshal(requests)
	if err := os.WriteFile(requestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := rootCmd()
	cmd.SetArgs([]string{"templates", "migration-preview", "--vault", root, "--requests", requestPath, "--out", previewPath})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(notePath)
	if string(current) != string(original) {
		t.Fatal("preview mutated original note")
	}
	var preview vault.MigrationPreview
	raw, err := os.ReadFile(previewPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &preview); err != nil {
		t.Fatal(err)
	}
	cmd = rootCmd()
	cmd.SetArgs([]string{"templates", "migration-apply", "--vault", root, "--preview", previewPath, "--preview-hash", preview.Hash, "--reviewed-ids", "other-id"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("unreviewed migration selection applied")
	}
	current, _ = os.ReadFile(notePath)
	if string(current) != string(original) {
		t.Fatal("rejected selection mutated original")
	}
	cmd = rootCmd()
	cmd.SetArgs([]string{"templates", "migration-apply", "--vault", root, "--preview", previewPath, "--preview-hash", preview.Hash, "--reviewed-ids", "reviewed-finding"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	current, _ = os.ReadFile(notePath)
	fm, _, err := vault.ParseFrontmatter(current)
	if err != nil {
		t.Fatal(err)
	}
	if fm.ID != "reviewed-finding" || fm.Template != "finding" {
		t.Fatalf("migration result=%+v", fm)
	}
}
