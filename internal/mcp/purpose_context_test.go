// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bright-interaction/mesh/internal/vault"
)

// Publish through the real shared writer, rather than constructing a context map
// or a hand-rendered note that bypasses the versioned authoring contract.
func publishPurposeContextFixture(t *testing.T, s *Server, ctx context.Context, kind string, sections map[string]string, blocks []vault.BlockSpec) (string, string) {
	t.Helper()
	definition, err := vault.TemplateFor(kind, 1)
	if err != nil {
		t.Fatal(err)
	}
	complete := map[string]string{}
	for _, section := range definition.Sections {
		complete[section.Key] = "Synthetic fixture context; no production observation or owner is claimed."
	}
	for key, value := range sections {
		complete[key] = value
	}
	args := authoringArgs{
		Title: "Purpose context " + kind, Template: kind, TemplateVersion: 1,
		Summary:  "An illustrative note for testing partial retrieval, not production verification.",
		Sections: complete, Blocks: blocks,
	}
	saved, rpcErr := s.toolWrite(ctx, mustJSON(args), "")
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	receipt := toolJSON(t, saved)
	return receipt["id"].(string), filepath.Join(s.vaultRoot, receipt["path"].(string))
}

func TestPurposeSectionContextKeepsApplicableCaveats(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	ctx := WithLocalOperator(context.Background())
	// These are independently authored partial-answer scenarios: the requested
	// answer alone could suggest readiness or applicability that its caveat denies.
	cases := []struct {
		kind, anchor, answer, caveatKey, caveat string
	}{
		{"review", "assessment", "The synthetic storage design meets the reviewed encryption criterion.", "gaps", "Restore integrity has not been tested; this review cannot establish recovery readiness."},
		{"status", "changes", "The synthetic release has been assembled.", "blockers", "Release authorization remains missing; assembled bytes are not an installed update."},
		{"concept", "explanation", "A lease permits one synthetic worker to hold a resource.", "examples", "Illustrative only: expiry cannot terminate an already running remote request."},
		{"reference", "entries", "| Fixture version | Meaning |\n|---|---|\n| A | Example |", "interpretation", "These entries apply only to the dated fixture revision, not newer server behavior."},
		{"prd", "requirements", "REQ-1: show a saved note after offline reopening.", "open_questions", "Cross-device retention policy has not been agreed; REQ-1 does not decide it."},
		{"trd", "architecture", "Use an illustrative local engine behind a narrow native boundary.", "quality", "Performance and availability limits remain unagreed; the architecture is not their verification."},
		{"app-flow", "main_flows", "FLOW-1: confirm the synthetic invitation, then save the enrollment receipt.", "alternatives", "An interrupted enrollment may be uncertain; never redeem the same invitation automatically."},
		{"design-brief", "experience", "Expose a concise synthetic onboarding form.", "constraints", "Keyboard and screen-reader coverage remain requirements rather than checked compliance."},
		{"backend-schema", "structure", "The synthetic table has an account ID and an owned record ID.", "operations", "No restore drill is recorded; naming a backup field does not prove recovery."},
		{"implementation-plan", "sequence", "First introduce the compatible reader, then enable the proposed writer.", "rollout", "Rollback compatibility is unverified; deployment and live writeback remain pending."},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			id, path := publishPurposeContextFixture(t, s, ctx, tc.kind, map[string]string{tc.anchor: tc.answer, tc.caveatKey: tc.caveat}, nil)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			wire, _ := contextFetch(t, s, ctx, id, tc.anchor)
			envelope, section := decodeContext(t, wire)
			if !strings.Contains(section, tc.answer) || strings.Contains(section, tc.caveat) || envelope.Fields[tc.caveatKey] != tc.caveat {
				t.Fatalf("partial answer lost its separately authored caveat: %s", wire)
			}
			if envelope.ContextTruncated {
				t.Fatalf("short fixture context unexpectedly truncated: %s", wire)
			}
			full, _ := contextFetch(t, s, ctx, id, "")
			if full != string(before) || strings.Count(full, tc.answer) != 1 || strings.Count(full, tc.caveat) != 1 {
				t.Fatal("retrieval changed the full note or duplicated substantive body prose")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatal("partial retrieval mutated the published source")
			}
		})
	}
}

func TestPurposeBlockContextKeepsProofAndProvenance(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	ctx := WithLocalOperator(context.Background())
	blocks := []vault.BlockSpec{
		{Template: "evidence", ID: "observation", Fields: map[string]string{
			"claim":       "The synthetic reader returned the requested fixture revision.",
			"source":      "[Synthetic transcript](https://example.invalid/fixture/rev-a); revision fixture-a.",
			"observed_at": "2026-10-09; a synthetic fixture date, not a production observation.",
			"result":      "The returned revision was fixture-a; the trial did not include a second device.",
			"uncertainty": "Single-device fixture evidence cannot establish cross-device consistency.",
		}},
		{Template: "verification", ID: "check", Fields: map[string]string{
			"checks":  "Reopened the synthetic note and compared its recorded revision with fixture-a.",
			"context": "Fixture engine revision fixture-a, with no live account or provider.",
			"results": "The reopened bytes matched; live synchronization was not exercised.",
			"gaps":    "Provider delivery, remote cancellation and installed updates remain unverified.",
		}},
		{Template: "code", ID: "example", Fields: map[string]string{
			"purpose": "Illustrate a bounded loop without claiming a runnable program.", "language": "go",
			"code":   "for attempt := 0; attempt < 3; attempt++ {\n    retry()\n}",
			"source": "[Illustrative source](https://example.invalid/code/fixture-a)", "revision": "fixture-a, explicitly synthetic",
			"explanation": "The attempts are bounded, but retry is unspecified.", "verification": "No live dependency check was performed.",
			"limitations": "This example omits cancellation and backoff.", "illustrative": "true",
		}},
	}
	id, _ := publishPurposeContextFixture(t, s, ctx, "finding", nil, blocks)
	for _, tc := range []struct {
		anchor, body string
		want         map[string]string
	}{
		{"claim", blocks[0].Fields["claim"], map[string]string{
			"block:observation:source": blocks[0].Fields["source"], "block:observation:observed_at": blocks[0].Fields["observed_at"],
			"block:observation:result": blocks[0].Fields["result"], "block:observation:uncertainty": blocks[0].Fields["uncertainty"],
		}},
		{"version-or-context", blocks[1].Fields["context"], map[string]string{
			"block:check:checks": blocks[1].Fields["checks"], "block:check:results": blocks[1].Fields["results"], "block:check:gaps": blocks[1].Fields["gaps"],
		}},
		{"code", blocks[2].Fields["code"], map[string]string{
			"block:example:source": blocks[2].Fields["source"], "block:example:revision": blocks[2].Fields["revision"],
			"block:example:verification": blocks[2].Fields["verification"], "block:example:limitations": blocks[2].Fields["limitations"], "block:example:illustrative": "true",
		}},
	} {
		t.Run(tc.anchor, func(t *testing.T) {
			wire, _ := contextFetch(t, s, ctx, id, tc.anchor)
			envelope, section := decodeContext(t, wire)
			if !strings.Contains(section, tc.body) || envelope.ContextTruncated {
				t.Fatalf("requested child or short supporting context lost: %s", wire)
			}
			for key, value := range tc.want {
				if envelope.Fields[key] != value {
					t.Fatalf("partial proof lost %s: %s", key, wire)
				}
			}
			if _, duplicatedPayload := envelope.Fields["block:example:code"]; duplicatedPayload {
				t.Fatal("code payload duplicated into the added context")
			}
			if tc.anchor != "code" && strings.Contains(wire, blocks[2].Fields["code"]) {
				t.Fatal("unselected block code escaped the requested retrieval")
			}
		})
	}
}

func TestPurposeContextBoundsAndCurrentScope(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	ctx := WithLocalOperator(context.Background())
	answer := "This synthetic assessment covers only the described local fixture."
	caveat := strings.Repeat("限界: live recovery remains unverified. ", 300)
	id, path := publishPurposeContextFixture(t, s, ctx, "review", map[string]string{"assessment": answer, "gaps": caveat}, nil)
	wire, _ := contextFetch(t, s, ctx, id, "assessment")
	envelope, section := decodeContext(t, wire)
	if !envelope.ContextTruncated || !utf8.ValidString(wire) || !strings.Contains(section, answer) || envelope.Fields["gaps"] == "" || !strings.HasPrefix(caveat, envelope.Fields["gaps"]) {
		t.Fatal("long caveat must be a truthful bounded UTF-8 excerpt with an incomplete warning")
	}
	full, _ := contextFetch(t, s, ctx, id, "")
	if !strings.Contains(full, caveat) {
		t.Fatal("bounded context altered the complete source caveat")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "---\n", "---\nscope: [private]\n", 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	// The current file has tightened access; no reindex or owner restart is allowed
	// to be a prerequisite for refusing the formerly readable note's caveats.
	public := WithScopeFilter(ctx, &ScopeFilter{AllowedRead: map[string]bool{"dev": true}})
	out, rpcErr := s.toolFetch(public, mustJSON(map[string]string{"id": id, "anchor": "assessment"}))
	if out != nil || rpcErr == nil || rpcErr.Message != "unknown note id" {
		t.Fatal("context retrieval leaked a current-file scope change")
	}
}
