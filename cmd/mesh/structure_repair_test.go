// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

func TestRepairRelationsPreserveEverySourceAudience(t *testing.T) {
	for _, tc := range []struct {
		name           string
		source, target []string
		want           bool
	}{
		{"default scope", nil, nil, true},
		{"private target", []string{"dev"}, []string{"sales"}, false},
		{"shared target", []string{"dev"}, []string{"dev", "sales"}, true},
		{"one missing audience", []string{"dev", "sales"}, []string{"dev"}, false},
		{"all audiences", []string{"dev", "sales"}, []string{"sales", "dev"}, true},
		{"labels are literal", []string{"custom-project"}, []string{"dev"}, false},
		{"blank source defaults", []string{" "}, []string{"dev"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &vault.Frontmatter{Scope: tc.source}
			target := &vault.Frontmatter{Scope: tc.target}
			if got := relationAudienceAllows(source, target); got != tc.want {
				t.Fatalf("audience allowed=%v want=%v", got, tc.want)
			}
		})
	}
	if relationAudienceAllows(nil, &vault.Frontmatter{}) || relationAudienceAllows(&vault.Frontmatter{}, nil) {
		t.Fatal("unknown identity granted a relation")
	}
}

func TestRepairRelationsLinksUntaggedCanonicalEntitiesWithoutPrivateOrHiddenEvidence(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "capture-0099.md")
	source := "---\nid: untagged-source\ntype: note\ntitle: Capture without useful search terms\nscope: [dev, sales]\n---\nCloudRebellion owns this implementation. BrightInteraction is also discussed.\n<!-- GhostEntity is hidden metadata, not topic evidence. -->\n"
	files := map[string]string{
		sourcePath:                         source,
		filepath.Join(root, "cloud.md"):    "---\nid: cloud-rebellion\ntype: entity\ntitle: CloudRebellion\nscope: [dev, sales]\n---\nA canonical organization.\n",
		filepath.Join(root, "private.md"):  "---\nid: bright-interaction\ntype: entity\ntitle: BrightInteraction\nscope: [dev]\n---\nA private canonical organization.\n",
		filepath.Join(root, "ghost.md"):    "---\nid: ghost-entity\ntype: entity\ntitle: GhostEntity\nscope: [dev, sales]\n---\nA canonical entity that appears only inside the source comment.\n",
		filepath.Join(root, "generic.md"):  "---\nid: generic-source\ntype: note\ntitle: Generic capture\nscope: [dev, sales]\n---\nMesh Hash Stage. No specific corroboration.\n",
		filepath.Join(root, "explicit.md"): "---\nid: explicit-empty\ntype: note\ntitle: Explicit CloudRebellion capture\nscope: [dev, sales]\nrelated: []\n---\nCloudRebellion is explicitly mentioned, but the author chose no relations.\n",
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"index", root}, {"structure", root, "--wire-orphans", "--local-only", "--apply"}} {
		cmd := rootCmd()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(args, err)
		}
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	header, body, _ := vault.SplitFrontmatter(string(data))
	fm, _, err := vault.ParseFrontmatter([]byte(header))
	if err != nil {
		t.Fatal(err)
	}
	_, originalBody, _ := vault.SplitFrontmatter(source)
	if body != originalBody || !reflect.DeepEqual(fm.EffectiveScopes(), []string{"dev", "sales"}) || len(fm.Tags) != 0 {
		t.Fatal("repair changed body or access metadata", fm)
	}
	var linkedCanonical bool
	for _, id := range fm.Related {
		if id == "cloud-rebellion" {
			linkedCanonical = true
		}
		if id == "bright-interaction" || id == "ghost-entity" {
			t.Fatal("private/comment-only evidence granted a durable link", fm.Related)
		}
	}
	if !linkedCanonical {
		t.Fatal("untagged canonical evidence was ignored", fm.Related)
	}
	for _, name := range []string{"generic.md", "explicit.md"} {
		actual, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != files[filepath.Join(root, name)] {
			t.Fatal("uncorroborated or explicitly unlinked note was changed", name)
		}
	}
}

func TestRepairRelationsUsesDeclaredIdentityInsteadOfCaptureFilename(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "capture-0001.md")
	for path, content := range map[string]string{
		sourcePath:                             "---\nid: authored-source\ntitle: Mesh relation repair identity\ntype: note\nscope: [dev]\ntags: [mesh]\n---\n# Mesh relation repair identity\nA captured note with a stable declared identity.\n",
		filepath.Join(root, "capture-0002.md"): "---\nid: mesh-reference\ntitle: Mesh relation repair identity reference\ntype: note\nscope: [dev]\ntags: [mesh]\n---\n# Mesh relation repair identity reference\nA corroborating reference about the same repair.\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"index", root}, {"structure", root, "--wire-orphans", "--local-only", "--apply"}} {
		cmd := rootCmd()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(args, err)
		}
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	header, _, _ := vault.SplitFrontmatter(string(data))
	fm, _, err := vault.ParseFrontmatter([]byte(header))
	if err != nil || fm.ID != "authored-source" || len(fm.Related) != 1 || fm.Related[0] != "mesh-reference" {
		t.Fatalf("declared identity did not produce the corroborated relation: %+v %v", fm, err)
	}
}
