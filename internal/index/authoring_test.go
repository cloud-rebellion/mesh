// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

func TestAuthoringBlockAddressesAvoidCollisionsAndDeclareCardTruncation(t *testing.T) {
	template, _ := vault.TemplateFor("method", 1)
	spec := vault.NewNoteSpec{Template: "method", Title: "Summary", Summary: "A fixture distinguishes method verification from independent illustrative code checks.", Sections: map[string]string{}}
	for _, s := range template.Sections {
		spec.Sections[s.Key] = "Illustrative fixture context; no live behavior is established."
	}
	for i := 0; i < 9; i++ {
		spec.Blocks = append(spec.Blocks, vault.BlockSpec{Template: "code", ID: fmt.Sprintf("example-%d", i), Fields: map[string]string{
			"purpose": "Demonstrate a source-labelled fixture.", "language": "go", "code": "return 1", "source": "Illustrative fixture.", "revision": "No live revision applies.",
			"explanation": "The example returns a constant.", "verification": "Not run against a live application.", "limitations": "A fixture, not a complete program.", "illustrative": "true",
		}})
	}
	prepared, err := vault.PrepareNoteContext(context.Background(), t.TempDir(), spec)
	if err != nil {
		t.Fatal(err)
	}
	pn, err := Parse("method.md", prepared.Content)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := Parse("reader.md", []byte("---\nid: reader\ntype: note\nwhen: 2026-10-06\n---\n[["+pn.FM.ID+"#applicability]] [["+pn.FM.ID+"#verification]] [["+pn.FM.ID+"#block-example-0-verification]]\n"))
	g, issues := BuildGraph([]*ParsedNote{pn, ref})
	for _, issue := range issues {
		if issue.Kind == "duplicate-anchor" || issue.Kind == "broken-anchor" {
			t.Fatalf("valid template addresses collide or fail to resolve: %+v", issue)
		}
	}
	for _, anchor := range []string{"summary", "note-title", "verification", "block-example-0-verification", "block-example-1-verification"} {
		if _, ok := g.Node("note:" + pn.FM.ID + "#" + anchor); !ok {
			t.Fatalf("missing unique indexed address %s", anchor)
		}
	}
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.IndexVault([]*ParsedNote{pn, ref}, g); err != nil {
		t.Fatal(err)
	}
	metadata, err := store.NoteMetadataFor(context.Background(), []string{"note:" + pn.FM.ID})
	if err != nil {
		t.Fatal(err)
	}
	m := metadata["note:"+pn.FM.ID]
	if len(m.Sections) != 12 || !m.SectionsTruncated || len(m.MissingGuidance) != 0 {
		t.Fatalf("bounded addresses lost truncation or acquired false incompleteness: %+v", m)
	}
	if m.Sections[5].Anchor != "block-example-0" {
		t.Fatalf("selected block missing from reader card: %+v", m.Sections)
	}
}

func templateNote(t *testing.T, id, template, extra string, sections map[string]string) *ParsedNote {
	t.Helper()
	spec, err := vault.TemplateFor(template, 1)
	if err != nil {
		t.Fatal(err)
	}
	text := "---\nid: " + id + "\ntitle: " + id + "\ntype: " + string(spec.Type) + "\ntemplate: " + template + "\ntemplate_version: 1\nwhen: 2026-10-06\n" + extra + "---\n# " + id + "\n\n## Summary\nA concise body synopsis.\n"
	for _, section := range spec.Sections {
		content := "Supported context and explicit limits."
		if value, ok := sections[section.Key]; ok {
			content = value
		}
		text += "\n## " + section.Heading + "\n" + content + "\n"
	}
	pn, err := Parse(id+".md", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return pn
}

func TestAuthoringMetadataPersistenceAndHash(t *testing.T) {
	pn := templateNote(t, "a", "decision", "collections: [mesh]\nverified_at: 2026-10-05\n", nil)
	g, _ := BuildGraph([]*ParsedNote{pn})
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.IndexVault([]*ParsedNote{pn}, g); err != nil {
		t.Fatal(err)
	}
	metadata, err := store.NoteMetadataFor(context.Background(), []string{"note:a"})
	if err != nil {
		t.Fatal(err)
	}
	m := metadata["note:a"]
	if m.Template != "decision" || m.TemplateVersion != 1 || m.Summary != "A concise body synopsis." || len(m.MissingGuidance) != 0 || len(m.Sections) == 0 || m.VerifiedAt != "2026-10-05" {
		t.Fatalf("metadata=%+v", m)
	}
	loaded, err := store.LoadGraph()
	if err != nil {
		t.Fatal(err)
	}
	n, ok := loaded.Node("note:a")
	if !ok || n.Attrs["template"] != "decision" || n.Attrs["reader_authoring"] == nil {
		t.Fatalf("reader attrs lost: %+v", n)
	}
	if !strings.Contains(ChunkText(pn)[0], "concise body synopsis") {
		t.Fatalf("embedding lacks body summary: %v", ChunkText(pn))
	}
	base := retrievalHash(pn)
	for _, edit := range []struct{ from, to string }{
		{"collections: [mesh]", "collections: [stage]"},
		{"verified_at: 2026-10-05", "verified_at: 2026-10-06"},
		{"source_url", "https://example.test/updated"},
		{"concise body synopsis", "corrected body synopsis"},
		{"Supported context", "Updated supported context"},
	} {
		copy := *pn
		fm := *pn.FM
		copy.FM = &fm
		switch edit.from {
		case "collections: [mesh]":
			copy.FM.Collections = vault.StringList{"stage"}
		case "verified_at: 2026-10-05":
			copy.FM.VerifiedAt = "2026-10-06"
		case "source_url":
			copy.FM.SourceURL = edit.to
		default:
			copy.Body = strings.Replace(copy.Body, edit.from, edit.to, 1)
		}
		if retrievalHash(&copy) == base {
			t.Fatalf("edit %q did not invalidate indexed reader/vector metadata", edit.from)
		}
	}
}

func TestDraftCannotSupersedePublishedKnowledge(t *testing.T) {
	old := templateNote(t, "old", "decision", "", nil)
	current := templateNote(t, "current", "decision", "supersedes: [old]\n", nil)
	draft := templateNote(t, "draft", "decision", "status: draft\nsupersedes: [old]\n", nil)
	g, _ := BuildGraph([]*ParsedNote{old, current, draft})
	n, _ := g.Node("note:old")
	if n.Attrs["superseded_by"] != "current" {
		t.Fatalf("draft overrode published replacement: %+v", n.Attrs)
	}
	for _, edge := range g.Neighbors("note:draft") {
		if edge.Source == "note:draft" && edge.Relation == "supersedes" {
			t.Fatalf("draft mutated semantic supersession: %+v", edge)
		}
	}
}

func TestModernContradictionUsesExplicitBehaviorOnly(t *testing.T) {
	incident := templateNote(t, "incident", "post-mortem", "tags: [release]\n", map[string]string{"impact": "Never use unsafe mutable releases.", "root_cause": "Use unsafe mutable releases.", "follow_up": "Review the incident evidence."})
	if claims := readerBehavior(incident); claims.Recommended != "" || claims.Forbidden != "" {
		t.Fatalf("impact/cause became guidance: %+v", claims)
	}
	a := templateNote(t, "a", "decision", "tags: [release]\n", map[string]string{"decision": "Use unsafe mutable releases."})
	b := templateNote(t, "b", "troubleshooting", "tags: [release]\n", map[string]string{"cause": "Use unsafe mutable releases.", "remedy": "Do not use unsafe mutable releases."})
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	notes := []*ParsedNote{incident, a, b}
	g, _ := BuildGraph(notes)
	if _, err := store.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	rows, err := store.tier0Guidance()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected only explicit behavior guidance: %+v", rows)
	}
	findings := contradictionFindings(rows)
	if len(findings) != 1 {
		t.Fatalf("explicit opposing behavior did not produce a review hint: %+v", findings)
	}
}

func TestLegacyContentAndLinksSurviveReaderAdapter(t *testing.T) {
	legacy, err := Parse("old.md", []byte("---\nid: old\ntitle: Original institutional note\ntype: gotcha\ndo: Prefer the supported [[target]] route\ndont: TODO review [[target]] before promotion\nwhy: Original historical rationale\ncollections: [target]\n---\n# Original institutional note\nHistorical body remains.\n"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := Parse("target.md", []byte("---\nid: target\ntitle: Existing entity\ntype: entity\n---\n# Existing entity\n"))
	if err != nil {
		t.Fatal(err)
	}
	g, _ := BuildGraph([]*ParsedNote{legacy, target})
	refs := 0
	for _, edge := range g.Neighbors("note:old") {
		if edge.Relation == "references" && edge.Target == "note:target" {
			refs++
		}
	}
	if refs != 1 {
		t.Fatalf("legacy guidance links were lost or collection membership became a duplicate semantic edge: %v", g.Neighbors("note:old"))
	}
	if text := searchText(legacy); !strings.Contains(text, "Original historical rationale") || !strings.Contains(text, "Historical body remains") || strings.Contains(text, "TODO") {
		t.Fatalf("legacy prose was lost or scaffolding indexed: %q", text)
	}
	n, _ := g.Node("note:old")
	before := n.KnowledgeDegree
	legacy.FM.Collections = vault.StringList{"unassigned", "target"}
	updated, _ := BuildGraph([]*ParsedNote{legacy, target})
	n, _ = updated.Node("note:old")
	if n.KnowledgeDegree != before {
		t.Fatalf("discovery membership changed semantic graph degree: before=%d after=%d", before, n.KnowledgeDegree)
	}
}

func TestDraftExcludedFromPublishedWritebackAttribution(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	draft := templateNote(t, "draft", "finding", "status: draft\nsource: agent\n", nil)
	published := templateNote(t, "published", "finding", "source: agent\n", nil)
	notes := []*ParsedNote{draft, published}
	g, _ := BuildGraph(notes)
	if _, err := store.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	if count, err := store.BackfillWritebacks(); err != nil || count != 1 {
		t.Fatalf("backfill included draft: %d %v", count, err)
	}
	if store.IsAgentAuthoredNote("draft") || !store.IsAgentAuthoredNote("published") {
		t.Fatal("draft fetch can fabricate published reuse attribution")
	}
	if err := store.RecordWriteback("draft", "agent"); err != nil {
		t.Fatal(err)
	}
	stats, err := store.FlywheelStats()
	if err != nil || stats.Authored != 1 {
		t.Fatalf("draft counted as published writeback: %+v %v", stats, err)
	}
}
