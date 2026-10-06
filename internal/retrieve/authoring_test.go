// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/vault"
)

func authoredSource(t *testing.T, id, template, status, missing string) noteSrc {
	t.Helper()
	spec, err := vault.TemplateFor(template, 1)
	if err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("---\nid: %s\ntype: %s\ntitle: TemplateNeedle %s\nwhen: 2026-10-06\nverified_at: 2026-10-05\nscope: [public]\ntemplate: %s\ntemplate_version: 1\nstatus: %s\n---\n# TemplateNeedle %s\n\n## Summary\nA compact body-authored synopsis.\n", id, spec.Type, id, template, status, id)
	for _, section := range spec.Sections {
		if section.Key == missing {
			continue
		}
		text += "\n## " + section.Heading + "\nSupported context for " + section.Key + ".\n"
	}
	return noteSrc{id + ".md", text}
}

func TestTemplateCardsUseCurrentBodyAndSections(t *testing.T) {
	r := buildVaultFrom(t, []noteSrc{authoredSource(t, "a", "decision", "accepted", "")})
	opt := scopedFTSOnlyOptions(map[string]bool{"public": true})
	opt.NoRerank = true
	check := func(wantMissing string) Card {
		t.Helper()
		cards, err := r.Retrieve(context.Background(), "templateneedle", opt)
		if err != nil || len(cards) != 1 {
			t.Fatalf("cards=%+v err=%v", cards, err)
		}
		c := cards[0]
		if c.Template != "decision" || c.TemplateVersion != 1 || c.State != "accepted" || c.Summary != "A compact body-authored synopsis." || c.VerifiedAt != "2026-10-05" {
			t.Fatalf("missing template identity/body summary: %+v", c)
		}
		for _, field := range c.MissingGuidance {
			if field == "do" || field == "dont" || field == "why" {
				t.Fatalf("modern note requires historical triad: %+v", c)
			}
		}
		if wantMissing == "" && len(c.MissingGuidance) != 0 {
			t.Fatalf("complete modern note flagged: %+v", c)
		}
		if wantMissing != "" && !strings.Contains(strings.Join(c.MissingGuidance, " "), wantMissing) {
			t.Fatalf("missing section invisible: %+v", c)
		}
		if len(c.Sections) == 0 || c.Sections[0].Anchor == "" {
			t.Fatalf("sections not addressable: %+v", c)
		}
		return c
	}
	check("")
	// Ranking graph remains stale while the persisted body/metadata is replaced.
	source := authoredSource(t, "a", "decision", "accepted", "rationale")
	pn, err := index.Parse(source.path, []byte(source.body))
	if err != nil {
		t.Fatal(err)
	}
	g, _ := index.BuildGraph([]*index.ParsedNote{pn})
	if _, err := r.store.IndexVault([]*index.ParsedNote{pn}, g); err != nil {
		t.Fatal(err)
	}
	c := check("rationale")
	for budget := 1; budget < 700; budget++ {
		packed := PackToBudget([]Card{c}, budget, nil)
		if len(packed) == 0 {
			continue
		}
		if TotalTokens(packed) > budget || packed[0].Template != c.Template || len(packed[0].MissingGuidance) != len(c.MissingGuidance) {
			t.Fatalf("metadata lost or budget exceeded at %d: %+v", budget, packed)
		}
	}
}

func TestDraftsHiddenBeforeLimitsAndExplicitlyBrowsable(t *testing.T) {
	sources := []noteSrc{authoredSource(t, "published", "decision", "accepted", "")}
	for _, id := range []string{"draft-a", "draft-b", "draft-c"} {
		sources = append(sources, authoredSource(t, id, "decision", "DRAFT", ""))
	}
	r := buildVaultFrom(t, sources)
	for _, opt := range []Options{
		{Limit: 1, WeightFTS: 1, NoRerank: true},
		{Limit: 1, WeightGraph: 1, NoRerank: true},
		{Limit: 1, NoRerank: true},
	} {
		cards, err := r.Retrieve(context.Background(), "templateneedle", opt)
		if err != nil || len(cards) != 1 || cards[0].NoteID != "published" {
			t.Fatalf("draft consumed candidate limit: %+v err=%v", cards, err)
		}
	}
	hits, err := r.store.Search(context.Background(), "templateneedle", 1)
	if err != nil || len(hits) != 1 || hits[0].NodeID != "note:published" {
		t.Fatalf("FTS draft filtering after LIMIT: %+v err=%v", hits, err)
	}
	drafts, err := r.store.DraftNotesContext(context.Background(), map[string]bool{"public": true}, func(path string) bool { return path != "draft-a.md" }, 1, 1)
	if err != nil || len(drafts) != 1 || drafts[0].NoteID != "draft-c" {
		t.Fatalf("draft filters must precede pagination: %+v err=%v", drafts, err)
	}
	if fenced, err := r.store.DraftNotesContext(context.Background(), map[string]bool{"secret": true}, nil, 10, 0); err != nil || len(fenced) != 0 {
		t.Fatalf("draft scope leak: %+v err=%v", fenced, err)
	}
	meta, err := r.store.NoteMetadataFor(context.Background(), []string{"note:draft-a"})
	if err != nil || meta["note:draft-a"].NoteID != "draft-a" {
		t.Fatalf("draft lost direct-fetch identity: %+v err=%v", meta, err)
	}
	// A current status change must be respected with a stale in-memory graph.
	published := authoredSource(t, "published", "decision", "draft", "")
	pn, err := index.Parse(published.path, []byte(published.body))
	if err != nil {
		t.Fatal(err)
	}
	g, _ := index.BuildGraph([]*index.ParsedNote{pn})
	if _, err := r.store.IndexVault([]*index.ParsedNote{pn}, g); err != nil {
		t.Fatal(err)
	}
	if cards, err := r.Retrieve(context.Background(), "templateneedle", Options{Limit: 10, NoRerank: true}); err != nil || len(cards) != 0 {
		t.Fatalf("stale graph exposed current draft: %+v err=%v", cards, err)
	}
}

func TestImportedCardCarriesProvenanceAfterMove(t *testing.T) {
	source := authoredSource(t, "moved-import", "finding", "", "")
	source.body = strings.Replace(source.body, "A compact body-authored synopsis.", "A compact body-authored synopsis. <!-- hidden-comment-needle -->", 1)
	source.body = strings.Replace(source.body, "scope: [public]\n", "scope: [public]\nsource: import:reference\nsource_url: https://example.test/original\n", 1)
	// The vault path no longer carries the imported prefix. Provenance must follow
	// the current metadata, so the MCP wrapper can still label both prose fields.
	r := buildVaultFrom(t, []noteSrc{source})
	cards, err := r.Retrieve(context.Background(), "templateneedle", Options{Limit: 1, NoRerank: true})
	if err != nil || len(cards) != 1 || cards[0].Source != "import:reference" || cards[0].SourceURL != "https://example.test/original" {
		t.Fatalf("moved import lost provenance: %+v %v", cards, err)
	}
	if strings.Contains(cards[0].Summary, "hidden-comment-needle") {
		t.Fatalf("hidden summary comment became retrieval prose: %+v", cards[0])
	}
	c := cards[0]
	c.Summary, c.Snippet = strings.Repeat("Imported fixture prose. ", 100), strings.Repeat("Matching fixture prose. ", 100)
	for budget := 1; budget <= 500; budget++ {
		packed := PackToBudget([]Card{c}, budget, nil)
		if len(packed) == 0 {
			continue
		}
		if TotalTokens(packed) > budget || packed[0].Source != c.Source || packed[0].SourceURL != c.SourceURL {
			t.Fatalf("provenance was omitted or not priced at budget %d: %+v", budget, packed)
		}
	}
}
