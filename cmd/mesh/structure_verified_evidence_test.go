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
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
)

func verifiedRepairManifest(t *testing.T, root string, rows []verifiedRelationEdge, when time.Time) (string, string) {
	t.Helper()
	dir := filepath.Join(root, "private-evidence")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(dir, "manifest-")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Chmod(0600); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(verifiedRelationDocument{Schema: 1, VerifiedAt: when, Candidates: rows})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file.Name(), evidenceRevision(body)
}

func verifiedRepairFixture(t *testing.T) (string, verifiedRelationEdge) {
	t.Helper()
	root := t.TempDir()
	data := map[string]string{
		"capture.md": "---\nid: captured-source\ntype: note\ntitle: Code reference capture\nscope: [dev]\n---\nThe implementation is `mesh/internal/mcp/authoring.go`.\n",
		"entity.md":  "---\nid: mesh\ntype: entity\ntitle: Mesh\nscope: [dev]\nrepo_path: automations/mesh/\n---\nCanonical knowledge product.\n",
	}
	for path, body := range data {
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	edge := verifiedRelationEdge{Source: "captured-source", SourcePath: "capture.md", SourceSHA: evidenceRevision([]byte(data["capture.md"])), Target: "mesh", TargetPath: "entity.md", TargetSHA: evidenceRevision([]byte(data["entity.md"])), Kind: "code-reference", Terms: []string{"mesh/internal/mcp/authoring.go"}, TargetRepoPath: "automations/mesh/"}
	return root, edge
}

func TestVerifiedRepairUsesPinnedCurrentScopeSafeSourceEvidence(t *testing.T) {
	root, edge := verifiedRepairFixture(t)
	name, digest := verifiedRepairManifest(t, root, []verifiedRelationEdge{edge}, time.Now().UTC())
	for _, args := range [][]string{{"index", root}, {"structure", root, "--wire-orphans", "--local-only", "--verified-evidence", name, "--evidence-sha256", digest, "--apply"}} {
		cmd := rootCmd()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(args, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, edge.SourcePath))
	if err != nil {
		t.Fatal(err)
	}
	header, body, _ := vault.SplitFrontmatter(string(data))
	fm, _, err := vault.ParseFrontmatter([]byte(header))
	if err != nil || len(fm.Related) != 1 || fm.Related[0] != "mesh" || body != "The implementation is `mesh/internal/mcp/authoring.go`.\n" {
		t.Fatal(fm, body, err)
	}
}

func TestVerifiedRepairDetectsUnexpectedDurableRelations(t *testing.T) {
	root, edge := verifiedRepairFixture(t)
	plan, err := captureRelatedRepair(context.Background(), root, edge.Source, edge.SourcePath, []string{edge.Target})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.BackfillRelatedFile(filepath.Join(root, edge.SourcePath), []string{"unexpected-target"}, false); err != nil {
		t.Fatal(err)
	}
	if err := plan.recordWrite(context.Background(), edge.Source); err == nil {
		t.Fatal("unselected durable relations accepted")
	}
}

func TestVerifiedRepairRejectsStalePrivateHiddenAndUnverifiedInputs(t *testing.T) {
	for _, scenario := range []string{"wrong-manifest-digest", "stale-attestation", "future-attestation", "changed-source", "changed-target", "private-audience", "comment-only", "frontmatter-only", "wrong-identity", "wrong-entity-root", "unqualified-tool", "unknown-kind", "linked-source"} {
		t.Run(scenario, func(t *testing.T) {
			root, edge := verifiedRepairFixture(t)
			when := time.Now().UTC()
			rewrite := func(path, before, after string) {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(root, path))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), before) {
					t.Fatal("fixture replacement missing")
				}
				updated := strings.Replace(string(data), before, after, 1)
				if err := os.WriteFile(filepath.Join(root, path), []byte(updated), 0600); err != nil {
					t.Fatal(err)
				}
				if path == edge.SourcePath {
					edge.SourceSHA = evidenceRevision([]byte(updated))
				} else {
					edge.TargetSHA = evidenceRevision([]byte(updated))
				}
			}
			switch scenario {
			case "stale-attestation":
				when = when.Add(-6 * time.Minute)
			case "future-attestation":
				when = when.Add(time.Minute)
			case "private-audience":
				rewrite(edge.SourcePath, "scope: [dev]", "scope: [dev, sales]")
			case "comment-only":
				rewrite(edge.SourcePath, "The implementation is `mesh/internal/mcp/authoring.go`.", "<!-- The implementation is `mesh/internal/mcp/authoring.go`. -->")
			case "frontmatter-only":
				rewrite(edge.SourcePath, "scope: [dev]", "scope: [dev]\nsource: mesh/internal/mcp/authoring.go")
				rewrite(edge.SourcePath, "The implementation is `mesh/internal/mcp/authoring.go`.", "A generic capture.")
			case "wrong-identity":
				edge.Source = "another-note"
			case "wrong-entity-root":
				edge.TargetRepoPath = "automations/hash/"
			case "unqualified-tool":
				edge.Kind = "native-tool-reference"
				edge.Terms = []string{"get_document"}
			case "unknown-kind":
				edge.Kind = "high-ranked-match"
			case "linked-source":
				link := filepath.Join(root, "linked.md")
				if err := os.Symlink(filepath.Join(root, edge.SourcePath), link); err != nil {
					t.Fatal(err)
				}
				edge.SourcePath = "linked.md"
			}
			name, digest := verifiedRepairManifest(t, root, []verifiedRelationEdge{edge}, when)
			switch scenario {
			case "wrong-manifest-digest":
				digest = strings.Repeat("f", 64)
			case "changed-source":
				if err := os.WriteFile(filepath.Join(root, edge.SourcePath), []byte("changed after attestation"), 0600); err != nil {
					t.Fatal(err)
				}
			case "changed-target":
				if err := os.WriteFile(filepath.Join(root, edge.TargetPath), []byte("changed after attestation"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadVerifiedRelationEvidence(context.Background(), root, name, digest); err == nil {
				t.Fatal("invalid repair evidence admitted")
			}
		})
	}
}

func TestVerifiedRepairRunPeerAcceptsOnlyTheSameVisibleNativeReference(t *testing.T) {
	root, edge := verifiedRepairFixture(t)
	runID := "run-20261111T212141e8dd03dd"
	for _, side := range []string{"source", "target"} {
		path, id := edge.SourcePath, edge.Source
		if side == "target" {
			path, id = edge.TargetPath, edge.Target
		}
		body := "---\nid: " + id + "\ntype: note\ntitle: Execution findings\nscope: [dev]\n---\nObserved " + runID + ".\n"
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if side == "source" {
			edge.SourceSHA = evidenceRevision([]byte(body))
		} else {
			edge.TargetSHA = evidenceRevision([]byte(body))
		}
	}
	edge.Kind = "native-run-peer"
	edge.Terms = []string{runID}
	edge.TargetRepoPath = ""
	name, digest := verifiedRepairManifest(t, root, []verifiedRelationEdge{edge}, time.Now().UTC())
	plan, err := loadVerifiedRelationEvidence(context.Background(), root, name, digest)
	if err != nil {
		t.Fatal(err)
	}
	if links, err := plan.links(context.Background(), edge.Source, edge.SourcePath); err != nil || len(links) != 1 || links[0] != edge.Target {
		t.Fatal(links, err)
	}
	data, err := os.ReadFile(filepath.Join(root, edge.TargetPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, edge.TargetPath), []byte(strings.ReplaceAll(string(data), runID, "another execution")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.links(context.Background(), edge.Source, edge.SourcePath); err == nil {
		t.Fatal("target changed after complete preflight")
	}
}
