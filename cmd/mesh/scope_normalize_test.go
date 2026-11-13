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
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
)

func sharedScopeFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(root, "private-plan")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	data := "---\nid: legacy\ntype: note\ntitle: Legacy note\nscope: [Hash deployment reconciliation]\ncreated: 2026-01-02\nauthor: Original Author\nrelated: []\ncustom: {retained: true}\n---\nExact source body.\n"
	if err := os.WriteFile(filepath.Join(root, "legacy.md"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(private, "plan.json"), data
}

func runSharedScopeCommand(args ...string) error {
	cmd := rootCmd()
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(append([]string{"scope", "normalize-shared"}, args...))
	return cmd.Execute()
}

func TestSharedScopeMigrationRequiresExplicitFreshPinnedPlan(t *testing.T) {
	root, planFile, original := sharedScopeFixture(t)
	if err := runSharedScopeCommand(root, "--plan", planFile); err == nil {
		t.Fatal("migration accepted without explicit shared-vault attestation")
	}
	if err := runSharedScopeCommand(root, "--shared-vault", "--plan", planFile); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "legacy.md"))
	if err != nil || string(before) != original {
		t.Fatal("planning modified source", err)
	}
	manifest, err := os.ReadFile(planFile)
	if err != nil {
		t.Fatal(err)
	}
	digest := evidenceRevision(manifest)
	if err := runSharedScopeCommand(root, "--shared-vault", "--apply", "--manifest", planFile, "--manifest-sha256", strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong manifest digest accepted")
	}
	if err := runSharedScopeCommand(root, "--shared-vault", "--apply", "--manifest", planFile, "--manifest-sha256", digest); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, "legacy.md"))
	if err != nil {
		t.Fatal(err)
	}
	header, body, _ := vault.SplitFrontmatter(string(after))
	fm, raw, err := vault.ParseFrontmatter([]byte(header))
	if err != nil || len(fm.Scope) != 1 || fm.Scope[0] != "dev" || raw["scope_migration"] == nil || body != "Exact source body.\n" || fm.Author != "Original Author" {
		t.Fatalf("migration lost provenance/body/identity: %v %v %q", fm, err, body)
	}
	if err := runSharedScopeCommand(root, "--shared-vault", "--apply", "--manifest", planFile, "--manifest-sha256", digest); err == nil {
		t.Fatal("used plan applied a second time")
	}
}

func TestSharedScopeMigrationPreflightsWholeCohortBeforeWriting(t *testing.T) {
	root, planFile, original := sharedScopeFixture(t)
	other := strings.ReplaceAll(original, "legacy", "second")
	if err := os.WriteFile(filepath.Join(root, "second.md"), []byte(other), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runSharedScopeCommand(root, "--shared-vault", "--plan", planFile); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(planFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "second.md"), []byte(other+"Concurrent source change.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runSharedScopeCommand(root, "--shared-vault", "--apply", "--manifest", planFile, "--manifest-sha256", evidenceRevision(manifest)); err == nil {
		t.Fatal("changed cohort accepted")
	}
	first, err := os.ReadFile(filepath.Join(root, "legacy.md"))
	if err != nil || string(first) != original {
		t.Fatal("earlier cohort source was written before all entries passed", err)
	}
}

func TestSharedScopeMigrationRejectsStaleForeignDuplicateAndPublicPlans(t *testing.T) {
	root, planFile, _ := sharedScopeFixture(t)
	if err := runSharedScopeCommand(root, "--shared-vault", "--plan", planFile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(planFile)
	if err != nil {
		t.Fatal(err)
	}
	var valid sharedScopePlan
	if err := json.Unmarshal(data, &valid); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"stale", "future", "foreign", "duplicate", "public", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			plan := valid
			name := filepath.Join(filepath.Dir(planFile), kind+".json")
			mode := os.FileMode(0600)
			switch kind {
			case "stale":
				plan.CreatedAt = time.Now().Add(-6 * time.Minute)
			case "future":
				plan.CreatedAt = time.Now().Add(time.Minute)
			case "foreign":
				plan.Vault = filepath.Join(root, "different")
			case "duplicate":
				plan.Entries = append(append([]sharedScopeEntry{}, plan.Entries...), plan.Entries[0])
			case "public":
				mode = 0644
			}
			encoded, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Symlink(planFile, name); err != nil {
					t.Fatal(err)
				}
				encoded = data
			} else if err := os.WriteFile(name, encoded, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := readSharedScopePlan(root, name, evidenceRevision(encoded)); err == nil {
				t.Fatal("unsafe plan accepted")
			}
		})
	}
}

func TestSharedScopeMigrationPreflightsReadOnlyCohort(t *testing.T) {
	root, planFile, original := sharedScopeFixture(t)
	second := filepath.Join(root, "second.md")
	if err := os.WriteFile(second, []byte(strings.ReplaceAll(original, "legacy", "second")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runSharedScopeCommand(root, "--shared-vault", "--plan", planFile); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(planFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(second, 0400); err != nil {
		t.Fatal(err)
	}
	if err := runSharedScopeCommand(root, "--shared-vault", "--apply", "--manifest", planFile, "--manifest-sha256", evidenceRevision(manifest)); err == nil {
		t.Fatal("read-only later entry was not refused before first write")
	}
	first, err := os.ReadFile(filepath.Join(root, "legacy.md"))
	if err != nil || string(first) != original {
		t.Fatal("writable first entry changed before read-only entry was checked", err)
	}
}
