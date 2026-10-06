// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

// Complete inputs for infrastructure tests whose subject is durability, timing
// or permissions. The independently authored acceptance examples below exercise
// the actual purpose-specific content contracts.
func fixtureSections(kind string) map[string]string {
	var keys []string
	switch kind {
	case "gotcha":
		keys = []string{"symptoms", "diagnosis", "cause", "remedy", "verification"}
	case "decision":
		keys = []string{"context", "options", "decision", "rationale", "consequences"}
	case "entity":
		keys = []string{"purpose", "current_state", "interfaces", "entry_points"}
	default:
		keys = []string{"question", "findings", "evidence", "limitations", "next_steps"}
	}
	sections := map[string]string{}
	for _, key := range keys {
		sections[key] = "Synthetic publication fixture for " + key + "; no production fact or verification is asserted."
	}
	return sections
}

func fixtureNote(title string) map[string]any {
	return map[string]any{"title": title, "type": "note", "summary": "Synthetic finding used to exercise publication infrastructure.", "sections": fixtureSections("note")}
}

func TestPurposeSpecificAuthoringJourneys(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	ctx := WithLocalOperator(context.Background())
	examples := []authoringArgs{
		{Title: "Engineering incident example", Template: "post-mortem", Summary: "Synthetic incident: concurrent refreshes caused session rejection. Serialization resolved the reproduced case; other adopters still need review.", Tags: []string{"engineering", "authentication"}, Sections: map[string]string{
			"what_happened": "In this illustrative incident, two requests refreshed the same account token concurrently; the second invalidated the first session.",
			"impact":        "The fixture affects one account. Real customer volume and duration are unknown.",
			"timeline":      "The fixture orders refresh A, refresh B, then rejection of A. No real timestamps are claimed.",
			"root_cause":    "The reproduction supports concurrent token rotation as the cause. Provider-specific behavior remains unverified.",
			"resolution":    "Serialize refreshes per account. This example has not been exercised against a live provider.",
			"follow_up":     "Review other recorded adopters; completion requires a concurrency check for each applicable integration. Ownership is unassigned.",
		}},
		{Title: "Marketing strategy example", Template: "plan", Summary: "Propose a limited onboarding campaign for new trial customers, measuring qualified activation before expanding spend.", Tags: []string{"marketing"}, Sections: map[string]string{
			"objective":        "Help new trial customers complete their first useful task; this illustrative plan targets first-week trials.",
			"context":          "The example assumes onboarding drop-off; the baseline must be measured before launch.",
			"approach":         "Test one concrete use-case message and a guided example with a small eligible cohort.",
			"milestones":       "Measure baseline, review messaging, launch the pilot, then assess the results. No owners or dates are assigned.",
			"success_measures": "Compare qualified activation and unsubscribe rates against the recorded baseline. No improvement has yet been observed.",
			"dependencies":     "Requires an agreed audience, consent checks and reliable activation measurement; seasonality may confound the result.",
		}},
		{Title: "Sales procedure example", Template: "procedure", Summary: "An illustrative lead handoff procedure preserves qualification evidence and requires recipient confirmation before the handoff is marked complete.", Tags: []string{"sales"}, Sections: map[string]string{
			"prerequisites":   "An accessible CRM record, recorded customer requirements and an identified receiving team.",
			"steps":           "1. Review the qualification evidence.\n2. Record outstanding questions.\n3. Send the agreed handoff through the team's approved workflow.\n4. Obtain recipient confirmation.",
			"expected_result": "The receiving team can find the record, requirements and unresolved questions.",
			"verification":    "Check record access and the actual recipient confirmation; this example does not claim a real handoff occurred.",
			"recovery":        "If access or confirmation is missing, keep the handoff open and resolve the specific blocker.",
		}},
	}
	for _, example := range examples {
		t.Run(example.Template, func(t *testing.T) {
			payload, _ := json.Marshal(example)
			preview, err := s.toolPrepareNote(ctx, payload, false)
			if err != nil {
				t.Fatal(err)
			}
			p := toolJSON(t, preview)
			if p["saved"] != false || !strings.Contains(p["markdown"].(string), "## Summary") {
				t.Fatalf("invalid preview: %v", p)
			}
			validation, err := s.toolPrepareNote(ctx, payload, true)
			if err != nil || toolJSON(t, validation)["valid"] != true {
				t.Fatalf("validation: %v %v", validation, err)
			}
			written, err := s.toolWrite(ctx, payload, "")
			if err != nil {
				t.Fatal(err)
			}
			receipt := toolJSON(t, written)
			data, fileErr := os.ReadFile(filepath.Join(s.vaultRoot, receipt["path"].(string)))
			if fileErr != nil {
				t.Fatal(fileErr)
			}
			fmText, body, _ := vault.SplitFrontmatter(string(data))
			fm, raw, parseErr := vault.ParseFrontmatter([]byte(fmText))
			if parseErr != nil || fm.ID == "" || fm.Created == "" || fm.Updated == "" || fm.Agent == "" {
				t.Fatalf("automatic metadata missing: %s (%v)", data, parseErr)
			}
			for _, key := range []string{"do", "dont", "why", "summary", "verified_at"} {
				if _, ok := raw[key]; ok {
					t.Fatalf("unexpected metadata %q", key)
				}
			}
			if strings.Count(body, example.Summary) != 1 || strings.Contains(body, "## Block") || strings.Contains(body, "TODO") {
				t.Fatalf("body duplication or unused structure: %s", body)
			}
			search, err := s.toolSearch(ctx, mustJSON(map[string]any{"query": example.Title}))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, v := range toolJSON(t, search)["cards"].([]any) {
				card := v.(map[string]any)
				if card["NoteID"] == fm.ID {
					found = true
					if _, incomplete := card["MissingGuidance"]; incomplete {
						t.Fatalf("complete note flagged incomplete: %v", card)
					}
				}
			}
			if !found {
				t.Fatal("published note absent from search")
			}
			fetched, err := s.toolFetch(ctx, mustJSON(map[string]any{"id": fm.ID, "anchor": "summary"}))
			if err != nil || !strings.Contains(rawContentText(t, fetched), example.Summary) {
				t.Fatalf("summary fetch: %v %v", fetched, err)
			}
		})
	}
}

func rawContentText(t *testing.T, result any) string {
	t.Helper()
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result %T", result)
	}
	items, ok := m["content"].([]map[string]any)
	if !ok || len(items) == 0 {
		t.Fatalf("missing content: %v", result)
	}
	return items[0]["text"].(string)
}

func TestDraftInboxAndRetiredInputs(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	ctx := WithLocalOperator(context.Background())
	args := mustJSON(map[string]any{"title": "Incomplete investigation uniqueprobe", "template": "post-mortem", "summary": "Impact and cause are still being investigated."})
	if _, err := s.toolWrite(ctx, args, ""); err == nil {
		t.Fatal("incomplete publication accepted")
	}
	if _, err := s.toolAuthorNote(ctx, mustJSON(map[string]any{"action": "publish", "note": map[string]any{"title": "Incomplete copied draft", "template": "finding", "status": "draft"}})); err == nil {
		t.Fatal("publish action bypassed completeness through draft status")
	}
	saved, err := s.toolSaveDraft(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	receipt := toolJSON(t, saved)
	if receipt["status"] != "draft" || !strings.HasPrefix(receipt["path"].(string), "inbox/") {
		t.Fatalf("not saved to draft inbox: %v", receipt)
	}
	out, err := s.toolSearch(ctx, mustJSON(map[string]any{"query": "uniqueprobe"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range toolJSON(t, out)["cards"].([]any) {
		if v.(map[string]any)["NoteID"] == receipt["id"] {
			t.Fatal("draft returned as ordinary guidance")
		}
	}
	if _, err := s.toolFetch(ctx, mustJSON(map[string]any{"id": receipt["id"]})); err != nil {
		t.Fatalf("saved draft unavailable by id: %v", err)
	}
	if _, err := s.toolWrite(ctx, mustJSON(map[string]any{"title": "Old framing", "do": "x", "dont": "y", "why": "z"}), ""); err == nil {
		t.Fatal("retired authoring accepted")
	}
	viewer := WithWriteCapability(ctx, false)
	if _, err := s.toolSaveDraft(viewer, args); err == nil {
		t.Fatal("read-only caller saved a draft")
	}
}

func TestTemplateCatalogIsVersionedAndStatic(t *testing.T) {
	c := templateCatalog()
	if len(c["templates"].([]map[string]any)) != 13 || len(c["blocks"].([]map[string]any)) != 11 {
		t.Fatalf("catalog: %v", c)
	}
	s := &Server{}
	if _, err := s.toolTemplate(mustJSON(map[string]any{"template": "procedure", "version": 1}), false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.toolTemplate(mustJSON(map[string]any{"template": "procedure", "version": 999}), false); err == nil {
		t.Fatal("unknown template version silently changed")
	}
	for _, spec := range authoringToolSpecs() {
		props, _ := spec["inputSchema"].(map[string]any)["properties"].(map[string]any)
		for _, key := range []string{"do", "dont", "why"} {
			if _, ok := props[key]; ok {
				t.Fatalf("retired key %s on %s", key, spec["name"])
			}
		}
	}
}

func TestAuthoringBlocksKeepEvidenceAndMarkdownInBoundedRetrieval(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	ctx := WithLocalOperator(context.Background())
	args := authoringArgs{Title: "Source-aware examples", Template: "finding", Summary: "Synthetic examples distinguish source provenance from actual verification.", Sections: fixtureSections("note"), Blocks: []vault.BlockSpec{
		{Template: "code", ID: "illustration", Fields: map[string]string{"purpose": "Show bounded retries.", "language": "go", "code": "for attempt := 0; attempt < 3; attempt++ {\n    retry()\n}", "source": "Illustrative code, created for this test.", "revision": "No repository revision applies.", "explanation": "The loop bounds attempts; retry behavior is unspecified.", "verification": "Not executed against a real dependency.", "limitations": "No backoff or cancellation is implemented.", "illustrative": "true"}},
		{Template: "evidence", ID: "observation", Fields: map[string]string{"claim": "This is a synthetic observation.", "source": "Test fixture scenario.", "observed_at": "Unknown; no real observation is claimed.", "result": "Fixture outcome only.", "uncertainty": "Cannot establish production behavior."}},
		{Template: "table", ID: "lookup", Fields: map[string]string{"purpose": "Compare fixture states.", "table": "| State | Meaning |\n|---|---|\n| Draft | Incomplete |", "source": "Illustrative test data.", "interpretation": "One example row.", "limitations": "No measured results."}},
	}}
	args.Sections["findings"] = "- Keep the source.\n- Explain the limits.\n\n1. Read the context.\n2. Inspect the evidence."
	raw, _ := json.Marshal(args)
	saved, err := s.toolWrite(ctx, raw, "")
	if err != nil {
		t.Fatal(err)
	}
	receipt := toolJSON(t, saved)
	id := receipt["id"].(string)
	code, _ := contextFetch(t, s, ctx, id, "code")
	envelope, section := decodeContext(t, code)
	if !strings.Contains(section, "for attempt") || envelope.Fields["block:illustration:source"] != args.Blocks[0].Fields["source"] || envelope.Fields["block:illustration:limitations"] != args.Blocks[0].Fields["limitations"] || envelope.Fields["block:illustration:illustrative"] != "true" {
		t.Fatalf("code lost contextual evidence: %s", code)
	}
	evidence, _ := contextFetch(t, s, ctx, id, "result")
	e, _ := decodeContext(t, evidence)
	if e.Fields["block:observation:uncertainty"] != args.Blocks[1].Fields["uncertainty"] || e.Fields["block:observation:source"] != args.Blocks[1].Fields["source"] {
		t.Fatalf("evidence lost source/uncertainty: %s", evidence)
	}
	for anchor, want := range map[string]string{"findings": args.Sections["findings"], "block-lookup": args.Blocks[2].Fields["table"]} {
		got, _ := contextFetch(t, s, ctx, id, anchor)
		if !strings.Contains(got, want) {
			t.Fatalf("Markdown lost: %s", got)
		}
	}
	batch, err := s.toolFetchMany(ctx, mustJSON(map[string]any{"items": []map[string]string{{"id": id, "anchor": "code"}, {"id": id, "anchor": "result"}}, "budget": 4000}))
	if err != nil {
		t.Fatal(err)
	}
	b := toolJSON(t, batch)
	if b["tokens"].(float64) > 4000 || len(b["omitted"].([]any)) != 0 {
		t.Fatalf("bounded fetch omitted contextual blocks: %v", b)
	}
	// Tightening the source scope must take effect before a subsequent reindex.
	path := filepath.Join(s.vaultRoot, receipt["path"].(string))
	data, _ := os.ReadFile(path)
	data = []byte(strings.Replace(string(data), "---\n", "---\nscope: [private]\n", 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	public := WithScopeFilter(ctx, &ScopeFilter{AllowedRead: map[string]bool{"dev": true}})
	if out, err := s.toolFetch(public, mustJSON(map[string]string{"id": id, "anchor": "code"})); out != nil || err == nil {
		t.Fatal("current-file scope tightening leaked code")
	}
}

func TestSectionContextBoundsLongBlockKeys(t *testing.T) {
	fields := map[string]string{}
	for i := 0; i < 32; i++ {
		fields[strings.Repeat("long-block-id-", 32)+string(rune('A'+i))] = "x"
	}
	encoded := encodeSectionContext(fields)
	if len(encoded) > sectionContextMaxBytes || !strings.Contains(encoded, `"context_truncated":true`) {
		t.Fatal("long keys escaped contextual budget")
	}
}

func TestRecoveryStepFetchRetainsConditions(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	ctx := WithLocalOperator(context.Background())
	args := authoringArgs{Title: "Recovery conditions", Template: "finding", Summary: "Recovery steps apply only to the documented condition.", Sections: fixtureSections("note"), Blocks: []vault.BlockSpec{
		{Template: "recovery", ID: "response", Fields: map[string]string{"trigger": "Only after confirming a failed fixture worker.", "prerequisites": "Requires operator permission and a recorded checkpoint.", "steps": "1. Inspect the checkpoint.\n2. Resume the fixture worker.", "verification": "Verify progress from the recorded checkpoint.", "completion": "Recovery ends when the fixture worker advances past the checkpoint without a repeated failure.", "limitations": "Illustrative procedure; live recovery remains unverified."}},
	}}
	raw, _ := json.Marshal(args)
	saved, err := s.toolWrite(ctx, raw, "")
	if err != nil {
		t.Fatal(err)
	}
	id := toolJSON(t, saved)["id"].(string)
	got, _ := contextFetch(t, s, ctx, id, "steps")
	envelope, _ := decodeContext(t, got)
	for _, key := range []string{"trigger", "prerequisites", "verification", "limitations"} {
		if envelope.Fields["block:response:"+key] != args.Blocks[0].Fields[key] {
			t.Fatalf("recovery lost %s: %s", key, got)
		}
	}
}

func TestHostedPublicationHonorsReaderRolloutGate(t *testing.T) {
	s := newTestServer(t)
	ctx := WithLocalOperator(context.Background())
	calls := 0
	s.SetNotePublisher(func(context.Context, vault.NewNoteSpec) (*vault.CreateResult, error) { calls++; return nil, nil })
	t.Setenv("MESH_AUTHORING_MODE", "readers-only")
	if _, err := s.toolWrite(ctx, mustJSON(fixtureNote("Reader-phase write")), ""); err == nil {
		t.Fatal("MCP writer ignored rollout gate")
	}
	if calls != 0 {
		t.Fatal("hosted publisher crossed the rollout boundary")
	}
	if _, err := s.toolPrepareNote(ctx, mustJSON(fixtureNote("Reader-phase preview")), false); err != nil {
		t.Fatalf("safe preparation unavailable: %v", err)
	}
}

func TestMethodVerificationAndNamedCodeFieldRemainDistinct(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	ctx := WithLocalOperator(context.Background())
	args := authoringArgs{
		Title: "Summary", Type: "concept", Template: "method",
		Summary: "A synthetic example keeps method checks distinct from code checks.",
	}
	args.Sections = map[string]string{
		"applicability": "A synthetic method used to check independent retrieval addresses.",
		"approach":      "Read method checks separately from code-block checks.",
		"variations":    "No other fixture variation is recorded.",
		"verification":  "Method checks have not been run against a live service.",
		"limitations":   "This synthetic method does not establish operational behavior.",
	}
	args.Blocks = []vault.BlockSpec{{Template: "code", ID: "sample", Fields: map[string]string{
		"purpose": "Show a constant return.", "language": "go", "code": "return 1", "source": "Illustrative fixture.", "revision": "No live revision applies.",
		"explanation": "The code returns a constant.", "verification": "Code checks remain unperformed.", "limitations": "Incomplete program; no production behavior is established.", "illustrative": "true",
	}}}
	saved, err := s.toolWrite(ctx, mustJSON(args), "")
	if err != nil {
		t.Fatal(err)
	}
	id := toolJSON(t, saved)["id"].(string)
	method, _ := contextFetch(t, s, ctx, id, "verification")
	_, text := decodeContext(t, method)
	if !strings.Contains(text, args.Sections["verification"]) || strings.Contains(text, args.Blocks[0].Fields["verification"]) {
		t.Fatalf("block check shadowed method check: %s", method)
	}
	code, _ := contextFetch(t, s, ctx, id, "block-sample-verification")
	envelope, text := decodeContext(t, code)
	if !strings.Contains(text, args.Blocks[0].Fields["verification"]) || envelope.Fields["block:sample:source"] != args.Blocks[0].Fields["source"] || envelope.Fields["block:sample:limitations"] != args.Blocks[0].Fields["limitations"] {
		t.Fatalf("named code field lost its block context: %s", code)
	}
	summary, _ := contextFetch(t, s, ctx, id, "summary")
	_, text = decodeContext(t, summary)
	if !strings.Contains(text, args.Summary) {
		t.Fatal("note title shadowed the summary section")
	}
}
