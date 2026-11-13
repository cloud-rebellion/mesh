// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
)

const reviewedSourceExcerpt = "The TipTap editor autosave feedback loop is reproduced locally."
const reviewedTargetExcerpt = "document authoring and e-signing"
const reviewedContextReason = "The reviewed capture concerns the actual TipTap editor autosave feedback loop, while the target describes this platform's document authoring and signing responsibility. This is subject context, not a release acceptance claim."

func reviewedRepairFixture(t *testing.T, targetType string) (string, verifiedRelationEdge) {
	t.Helper()
	root := t.TempDir()
	source := "---\n# retained historical provenance\nid: legacy-capture\ntype: status\ntitle: Raw legacy editor capture\ncreated: 2026-01-02\nauthor: Original Author\nscope: [dev]\nrelated: null\ntemplate: finding\ncustom_extended: {retained: [one, 'two: three']}\nscope_migration: {schema: 1, reason: shared-vault-topic-label-correction, original_scope: hash}\n---\n# Legacy capture\n\n" + reviewedSourceExcerpt + "\n\n## Historical non-template section\nNo runtime result was established.\n<!-- Preserve this literal body comment. -->\n"
	target := "---\nid: canonical-target\ntype: " + targetType + "\ntitle: Canonical platform context\nscope: [dev]\n---\nThe platform combines " + reviewedTargetExcerpt + " with native workflow controls.\n"
	for path, content := range map[string]string{"capture.md": source, "target.md": target} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root, verifiedRelationEdge{Source: "legacy-capture", SourcePath: "capture.md", SourceSHA: evidenceRevision([]byte(source)),
		Target: "canonical-target", TargetPath: "target.md", TargetSHA: evidenceRevision([]byte(target)), Kind: "reviewed-context",
		Terms: []string{reviewedSourceExcerpt}, TargetTerms: []string{reviewedTargetExcerpt}, ReviewReason: reviewedContextReason}
}

func TestReviewedContextCLIRepairsLegacyEntityAndPeerWithoutRewritingContent(t *testing.T) {
	for _, targetType := range []string{"entity", "note"} {
		t.Run(targetType, func(t *testing.T) {
			root, edge := reviewedRepairFixture(t, targetType)
			original, err := os.ReadFile(filepath.Join(root, edge.SourcePath))
			if err != nil {
				t.Fatal(err)
			}
			target, err := os.ReadFile(filepath.Join(root, edge.TargetPath))
			if err != nil {
				t.Fatal(err)
			}
			name, digest := verifiedRepairManifest(t, root, []verifiedRelationEdge{edge}, time.Now().UTC())
			for _, args := range [][]string{
				{"index", root},
				{"structure", root, "--wire-orphans", "--local-only", "--verified-evidence", name, "--evidence-sha256", digest},
			} {
				cmd := rootCmd()
				cmd.SetArgs(args)
				if err := cmd.Execute(); err != nil {
					t.Fatal(args, err)
				}
			}
			if data, err := os.ReadFile(filepath.Join(root, edge.SourcePath)); err != nil || !bytes.Equal(data, original) {
				t.Fatal("reviewed-context dry run changed the source", err)
			}
			cmd := rootCmd()
			cmd.SetArgs([]string{"structure", root, "--wire-orphans", "--local-only", "--verified-evidence", name, "--evidence-sha256", digest, "--apply"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, edge.SourcePath))
			if err != nil {
				t.Fatal(err)
			}
			beforeHeader, beforeBody, _ := vault.SplitFrontmatter(string(original))
			afterHeader, afterBody, _ := vault.SplitFrontmatter(string(data))
			_, before, err := vault.ParseFrontmatter([]byte(beforeHeader))
			if err != nil {
				t.Fatal(err)
			}
			fm, after, err := vault.ParseFrontmatter([]byte(afterHeader))
			if err != nil || !reflect.DeepEqual(fm.Related, vault.StringList{edge.Target}) || beforeBody != afterBody {
				t.Fatal("reviewed-context publication changed body or selected relations", fm, err)
			}
			delete(before, "related")
			delete(after, "related")
			if !reflect.DeepEqual(before, after) || !bytes.Contains(data, []byte("# retained historical provenance")) {
				t.Fatal("reviewed-context publication changed historical metadata or provenance")
			}
			if current, err := os.ReadFile(filepath.Join(root, edge.TargetPath)); err != nil || !bytes.Equal(current, target) {
				t.Fatal("reviewed-context publication changed its target", err)
			}
		})
	}
}

func TestReviewedContextRejectsUnreviewedOrInvisibleEvidence(t *testing.T) {
	for _, scenario := range []string{
		"missing-reason", "short-reason", "padded-reason", "oversized-reason", "oversized-padded-reason", "placeholder-reason", "missing-target-terms",
		"short-source-term", "generic-long-word", "short-unicode-source-term", "padded-source-term", "second-source-term-missing", "second-target-term-missing", "multiline-target-term", "oversized-target-term",
		"hidden-source", "hidden-target", "frontmatter-source", "frontmatter-target", "private-target", "source-draft", "target-draft", "explicit-empty-related", "existing-related", "repo-path-authority", "wrong-source-id", "stale-source", "stale-target",
	} {
		t.Run(scenario, func(t *testing.T) {
			root, edge := reviewedRepairFixture(t, "entity")
			rewrite := func(path, before, after string) {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(root, path))
				if err != nil || !strings.Contains(string(data), before) {
					t.Fatal("missing rewrite fixture", err)
				}
				data = []byte(strings.Replace(string(data), before, after, 1))
				if err := os.WriteFile(filepath.Join(root, path), data, 0600); err != nil {
					t.Fatal(err)
				}
				if path == edge.SourcePath {
					edge.SourceSHA = evidenceRevision(data)
				} else {
					edge.TargetSHA = evidenceRevision(data)
				}
			}
			switch scenario {
			case "missing-reason":
				edge.ReviewReason = ""
			case "short-reason":
				edge.ReviewReason = "These two notes discuss the same product and belong together."
			case "padded-reason":
				edge.ReviewReason = "Hash" + strings.Repeat(" ", 100)
			case "oversized-reason":
				edge.ReviewReason = strings.Repeat("Reviewed concrete context. ", 60)
			case "oversized-padded-reason":
				edge.ReviewReason += strings.Repeat(" ", 1200)
			case "placeholder-reason":
				edge.ReviewReason = "TODO: " + reviewedContextReason
			case "missing-target-terms":
				edge.TargetTerms = nil
			case "short-source-term":
				edge.Terms = []string{"Hash"}
			case "generic-long-word":
				rewrite(edge.SourcePath, reviewedSourceExcerpt, "interoperability")
				edge.Terms = []string{"interoperability"}
			case "short-unicode-source-term":
				term := strings.Repeat("界", 8)
				rewrite(edge.SourcePath, reviewedSourceExcerpt, term)
				edge.Terms = []string{term}
			case "padded-source-term":
				edge.Terms = []string{"Hash                "}
			case "second-source-term-missing":
				edge.Terms = append(edge.Terms, "A second concrete excerpt is missing")
			case "second-target-term-missing":
				edge.TargetTerms = append(edge.TargetTerms, "Another concrete target excerpt is missing")
			case "multiline-target-term":
				edge.TargetTerms = []string{reviewedTargetExcerpt + "\nwith native workflow controls"}
			case "oversized-target-term":
				edge.TargetTerms = []string{strings.Repeat("x", 241)}
			case "hidden-source":
				rewrite(edge.SourcePath, reviewedSourceExcerpt, "<!-- "+reviewedSourceExcerpt+" -->")
			case "hidden-target":
				rewrite(edge.TargetPath, "The platform combines "+reviewedTargetExcerpt+" with native workflow controls.", "<!-- "+reviewedTargetExcerpt+" -->")
			case "frontmatter-source":
				rewrite(edge.SourcePath, "scope: [dev]", "scope: [dev]\nwhy: "+reviewedSourceExcerpt)
				rewrite(edge.SourcePath, "\n"+reviewedSourceExcerpt+"\n", "\nA generic visible capture.\n")
			case "frontmatter-target":
				rewrite(edge.TargetPath, "scope: [dev]", "scope: [dev]\nwhy: "+reviewedTargetExcerpt)
				rewrite(edge.TargetPath, "The platform combines "+reviewedTargetExcerpt+" with native workflow controls.", "A generic visible target.")
			case "private-target":
				rewrite(edge.TargetPath, "scope: [dev]", "scope: [sales]")
			case "source-draft":
				rewrite(edge.SourcePath, "scope: [dev]", "scope: [dev]\nstatus: draft")
			case "target-draft":
				rewrite(edge.TargetPath, "scope: [dev]", "scope: [dev]\nstatus: draft")
			case "explicit-empty-related":
				rewrite(edge.SourcePath, "related: null", "related: []")
			case "existing-related":
				rewrite(edge.SourcePath, "related: null", "related: [authored-choice]")
			case "repo-path-authority":
				edge.TargetRepoPath = "automations/mesh/"
			case "wrong-source-id":
				edge.Source = "different-source"
			}
			name, digest := verifiedRepairManifest(t, root, []verifiedRelationEdge{edge}, time.Now().UTC())
			if scenario == "stale-source" || scenario == "stale-target" {
				path := edge.SourcePath
				if scenario == "stale-target" {
					path = edge.TargetPath
				}
				rewrite(path, "\n---\n", "\n---\nChanged after review.\n")
			}
			before, err := os.ReadFile(filepath.Join(root, edge.SourcePath))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loadVerifiedRelationEvidence(context.Background(), root, name, digest); err == nil {
				t.Fatal("unreviewed, stale, private or invisible context admitted")
			}
			// Exercise the actual operator apply path for access, visibility and
			// authored-intent refusals, including sources the orphan list skips.
			if scenario == "private-target" || scenario == "hidden-target" || scenario == "explicit-empty-related" {
				cmd := rootCmd()
				cmd.SetArgs([]string{"index", root})
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
				cmd = rootCmd()
				cmd.SetArgs([]string{"structure", root, "--wire-orphans", "--local-only", "--verified-evidence", name, "--evidence-sha256", digest, "--apply"})
				if err := cmd.Execute(); err == nil {
					t.Fatal("invalid reviewed evidence reached CLI apply")
				}
			}
			if after, err := os.ReadFile(filepath.Join(root, edge.SourcePath)); err != nil || !bytes.Equal(after, before) {
				t.Fatal("refused reviewed-context operation changed source bytes", err)
			}
		})
	}
}

func TestReviewedContextFieldsCannotChangeNativeEvidenceContracts(t *testing.T) {
	for _, field := range []string{"review_reason", "REVIEW_REASON", "target_terms", "TARGET_TERMS", "unexpected_review_data"} {
		for _, value := range []any{nil, "", []string{}} {
			t.Run(field+"-"+string(mustReviewJSON(t, value)), func(t *testing.T) {
				root, edge := verifiedRepairFixture(t)
				name, _ := verifiedRepairManifest(t, root, []verifiedRelationEdge{edge}, time.Now().UTC())
				data, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				var document map[string]any
				if err := json.Unmarshal(data, &document); err != nil {
					t.Fatal(err)
				}
				document["candidates"].([]any)[0].(map[string]any)[field] = value
				data = mustReviewJSON(t, document)
				if err := os.WriteFile(name, data, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := loadVerifiedRelationEvidence(context.Background(), root, name, evidenceRevision(data)); err == nil {
					t.Fatal("native evidence accepted review-only or unknown fields")
				}
			})
		}
	}
}

func mustReviewJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
