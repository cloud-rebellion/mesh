// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func completeSpec(t *testing.T, typ NoteType, title string) NewNoteSpec {
	t.Helper()
	template, err := TemplateFor(DefaultTemplate(typ), 1)
	if err != nil {
		t.Fatal(err)
	}
	spec := NewNoteSpec{Type: typ, Title: title, Template: template.ID, TemplateVersion: 1, Summary: "A bounded fixture for " + title + ". No live environment was verified.", Sections: map[string]string{}}
	for _, s := range template.Sections {
		spec.Sections[s.Key] = "Fixture " + s.Key + " for " + title + "; unknown real-world facts remain unverified."
	}
	return spec
}

// completeFixtureSpec updates filesystem-boundary fixtures to a complete modern
// note without changing the cancellation/collision/durability behavior under test.
// Legacy field values are fixture payload only, never projected to semantic sections.
func completeFixtureSpec(spec NewNoteSpec) NewNoteSpec {
	payload := strings.Join([]string{spec.Do, spec.Dont, spec.Why}, " ")
	spec.Do, spec.Dont, spec.Why = "", "", ""
	template, err := TemplateFor(DefaultTemplate(spec.Type), 1)
	if err != nil {
		return spec
	}
	spec.Template, spec.TemplateVersion = template.ID, template.Version
	if spec.Summary == "" {
		spec.Summary = "Fixture payload for " + spec.Title + ": " + payload
	}
	if spec.Sections == nil {
		spec.Sections = map[string]string{}
	}
	for _, section := range template.Sections {
		if spec.Sections[section.Key] == "" {
			spec.Sections[section.Key] = "Explicit fixture " + section.Key + "; real-world state remains unknown."
		}
	}
	return spec
}

func TestAuthoringCatalogAndBodyOnlyRoundTrip(t *testing.T) {
	fixedNow(t)
	if len(Templates()) != 19 || len(BlockTemplates()) != 11 {
		t.Fatal("unexpected approved catalog")
	}
	copyCatalog := Templates()
	copyCatalog[0].Sections[0].Heading = "mutated"
	if Templates()[0].Sections[0].Heading == "mutated" {
		t.Fatal("catalog mutation escaped")
	}
	copyBlocks := BlockTemplates()
	copyBlocks[0].Fields[0].Heading = "mutated"
	if BlockTemplates()[0].Fields[0].Heading == "mutated" {
		t.Fatal("block catalog mutation escaped")
	}
	for _, template := range Templates() {
		t.Run(template.ID, func(t *testing.T) {
			spec := completeSpec(t, template.Type, "Fixture "+template.ID)
			spec.Template = template.ID
			spec.Sections = map[string]string{}
			for _, s := range template.Sections {
				spec.Sections[s.Key] = "Explicit " + s.Key + " content unique to " + template.ID + "."
			}
			spec.Collections = []string{"existing-collection"}
			spec.Tags = []string{"research", "research"}
			spec.Related = []string{"known-sibling"}
			spec.Supersedes = []string{"previous-guidance"}
			res, err := CreateNote(t.TempDir(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.TODOs) != 0 {
				t.Fatalf("complete note has gaps: %v", res.TODOs)
			}
			data, err := os.ReadFile(res.Path)
			if err != nil {
				t.Fatal(err)
			}
			yamlText, body, _ := SplitFrontmatter(string(data))
			for _, prose := range append([]string{spec.Summary}, mapValues(spec.Sections)...) {
				if strings.Contains(yamlText, prose) {
					t.Fatalf("prose duplicated in YAML: %q", prose)
				}
				if strings.Count(body, prose) != 1 {
					t.Fatalf("prose did not appear exactly once: %q", prose)
				}
			}
			for _, legacy := range []string{"do:", "dont:", "why:", "summary:"} {
				if strings.Contains(yamlText, "\n"+legacy) {
					t.Fatalf("new YAML contains %q", legacy)
				}
			}
			fm, _, err := ParseFrontmatter([]byte(yamlText))
			if err != nil {
				t.Fatal(err)
			}
			content, err := ReadAuthoring(fm, body)
			if err != nil {
				t.Fatal(err)
			}
			if content.Summary != spec.Summary || len(content.MissingSections) != 0 {
				t.Fatalf("reader: %+v", content)
			}
			for key, want := range spec.Sections {
				if content.Sections[key] != want || SectionText(body, key) != want {
					t.Fatalf("section %s did not roundtrip", key)
				}
			}
			if fm.Updated != "2026-06-16T09:00:00Z" || fm.Created != "2026-06-16T09:00:00Z" || fm.VerifiedAt != "" {
				t.Fatalf("timestamp semantics: %+v", fm)
			}
			if fm.Agent != "mesh" || fm.Source == "" || fm.TemplateVersion != 1 {
				t.Fatalf("automatic metadata missing: %+v", fm)
			}
		})
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	return out
}

func TestAuthoringDraftPrepareAndPublishCompleteness(t *testing.T) {
	root := t.TempDir()
	spec := NewNoteSpec{Template: "post-mortem", Title: "Unresolved incident", Status: "DRAFT", Sections: map[string]string{"root_cause": "Unknown; evidence is still being collected."}}
	if err := ValidateSpec(spec); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("draft validated for publication: %v", err)
	}
	prepared, err := PrepareNoteContext(context.Background(), root, spec)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("preparation mutated the vault")
	}
	if filepath.Base(filepath.Dir(prepared.Result.Path)) != "inbox" || len(prepared.Result.TODOs) == 0 {
		t.Fatalf("draft preparation: %+v", prepared.Result)
	}
	if !strings.Contains(string(prepared.Content), "Unknown; evidence is still being collected.") {
		t.Fatal("honest unknown lost")
	}
	res, err := CreateNote(root, spec)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(res.Path)
	fmText, body, _ := SplitFrontmatter(string(data))
	fm, _, _ := ParseFrontmatter([]byte(fmText))
	if !IsDraft(fm) || len(MissingSections(fm, body)) == 0 {
		t.Fatal("draft lost its gaps")
	}
	published := spec
	published.Status = ""
	root2 := t.TempDir()
	if _, err := CreateNote(root2, published); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("incomplete published: %v", err)
	}
	if _, err := PrepareNoteContext(context.Background(), root2, published); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("incomplete prepared as publication: %v", err)
	}
	entries, _ = os.ReadDir(root2)
	if len(entries) != 0 {
		t.Fatal("invalid publication mutated the vault")
	}
}

func TestAuthoringOptionalBlocksAndNestedCodeRoundTrip(t *testing.T) {
	spec := completeSpec(t, TypeDecision, "Block fixture")
	for _, template := range BlockTemplates() {
		block := BlockSpec{Template: template.ID, ID: template.ID, Fields: map[string]string{}}
		for _, f := range template.Fields {
			if f.Required {
				block.Fields[f.Key] = "Explicit " + f.Key + " context; this is a fixture."
			}
		}
		if template.ID == "code" {
			block.Fields["language"] = "markdown"
			block.Fields["illustrative"] = "true"
			block.Fields["code"] = strings.Join([]string{"~~~text", "## fake heading", "<!-- mesh:section impact -->", "~~~", "text with " + strings.Repeat(string(rune(96)), 4)}, "\n")
		}
		if template.ID == "table" {
			block.Fields["table"] = "| Observation | Result |\n| --- | --- |\n| Fixture only | Unknown; no live measurement |"
		}
		spec.Blocks = append(spec.Blocks, block)
	}
	res, err := CreateNote(t.TempDir(), spec)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(res.Path)
	fmText, body, _ := SplitFrontmatter(string(data))
	fm, _, err := ParseFrontmatter([]byte(fmText))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := ReadAuthoring(fm, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.Blocks) != 11 || len(reader.MissingSections) > 0 {
		t.Fatalf("blocks or gaps: %+v", reader)
	}
	for i, b := range reader.Blocks {
		if b.ID != spec.Blocks[i].ID || b.Version != 1 || b.Anchor != "block-"+b.ID {
			t.Fatalf("block identity: %+v", b)
		}
		for key, want := range spec.Blocks[i].Fields {
			if b.Fields[key] != want {
				t.Fatalf("%s:%s content changed: %q vs %q", b.ID, key, b.Fields[key], want)
			}
			if strings.Contains(fmText, want) {
				t.Fatalf("block prose in YAML: %q", want)
			}
		}
	}
	if strings.Contains(body, "## Block recovery\n") == false {
		t.Fatal("selected block missing")
	}
	plain, err := PrepareNoteContext(context.Background(), t.TempDir(), completeSpec(t, TypeNote, "No blocks"))
	if err != nil || strings.Contains(string(plain.Content), "## Block ") {
		t.Fatalf("unselected blocks scaffolded: %v", err)
	}
}

func TestAuthoringRejectsUnknownAmbiguousAndOversizedInput(t *testing.T) {
	base := completeSpec(t, TypePostMortem, "Validation fixture")
	cases := []struct {
		name string
		edit func(*NewNoteSpec)
	}{
		{"unknown version", func(s *NewNoteSpec) { s.TemplateVersion = 2 }},
		{"unknown template", func(s *NewNoteSpec) { s.Template = "imagined" }},
		{"type mismatch", func(s *NewNoteSpec) { s.Type = TypeDecision }},
		{"legacy input", func(s *NewNoteSpec) { s.Dont = "never infer impact from this" }},
		{"unknown section", func(s *NewNoteSpec) { s.Sections["do"] = "unsupported" }},
		{"escaped heading", func(s *NewNoteSpec) { s.Sections["impact"] = "## Foreign section\ntext" }},
		{"placeholder section", func(s *NewNoteSpec) { s.Sections["impact"] = "<!-- TODO: impact -->" }},
		{"bad collection", func(s *NewNoteSpec) { s.Collections = []string{"[[mesh]]"} }},
		{"oversized summary", func(s *NewNoteSpec) { s.Summary = strings.Repeat("å", MaxSummaryRunes+1) }},
		{"oversized body", func(s *NewNoteSpec) { s.Sections["impact"] = strings.Repeat("x", MaxAuthoringBytes+1) }},
		{"unfounded verification date", func(s *NewNoteSpec) { s.VerifiedAt = "2026-10-06" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := NormalizeSpec(base)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(&spec)
			if err := ValidateSpec(spec); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("invalid spec accepted: %v", err)
			}
		})
	}
	noMembership := completeSpec(t, TypeMap, "Empty index")
	if err := ValidateSpec(noMembership); err != nil {
		t.Fatalf("invented membership requirement: %v", err)
	}
	code := BlockSpec{Template: "code", ID: "example", Fields: map[string]string{"code": strings.Repeat("x", MaxCodeBytes+1)}}
	base.Blocks = []BlockSpec{code}
	if _, err := NormalizeSpec(base); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("oversized code accepted: %v", err)
	}
}

func TestAuthoringHistoricalAdapterDoesNotInventModernMeaning(t *testing.T) {
	fm := &Frontmatter{Type: TypePostMortem, Dont: "Do not restart the owner.", Do: "Check the reader.", Why: "Owner exclusivity protects the index."}
	legacy := ReadLegacy(fm)
	if legacy.Values()["dont"] != fm.Dont {
		t.Fatal("historical text changed")
	}
	reader, err := ReadAuthoring(fm, "# Original account\n\nOriginal impact is unknown.\n")
	if err != nil || len(reader.Sections) != 0 {
		t.Fatalf("legacy fields projected into modern sections: %+v %v", reader, err)
	}
	recommended, forbidden, _ := LegacyGuidance(fm)
	if recommended != fm.Do || forbidden != fm.Dont {
		t.Fatal("legacy safety guidance lost")
	}
	modern := &Frontmatter{Type: TypePostMortem, Template: "post-mortem", TemplateVersion: 1}
	_, forbidden, _ = LegacyGuidance(modern)
	if forbidden != "" {
		t.Fatal("modern incident impact inferred as prohibition")
	}
	if len(ReadLegacy(&Frontmatter{}).Missing()) != 0 {
		t.Fatal("absent universal triad is still required")
	}
}

func TestAuthoringReaderRejectsChangedAndDuplicateFixedHeadings(t *testing.T) {
	prepared, err := PrepareNoteContext(context.Background(), t.TempDir(), completeSpec(t, TypePostMortem, "Heading fixture"))
	if err != nil {
		t.Fatal(err)
	}
	fmText, body, _ := SplitFrontmatter(string(prepared.Content))
	fm, _, _ := ParseFrontmatter([]byte(fmText))
	for name, changed := range map[string]string{
		"renamed core":      strings.Replace(body, "## Impact\n", "## Invented impact heading\n", 1),
		"duplicate summary": body + "\n## Summary\n<!-- mesh:section summary -->\n\nDuplicate claim.\n",
		"unknown heading":   body + "\n## Unregistered primary heading\n\nContent.\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadAuthoring(fm, changed); err == nil {
				t.Fatal("ambiguous template body accepted")
			}
		})
	}
}

func TestActionBlocksRetainKnownOwnerAndCompletion(t *testing.T) {
	for _, id := range []string{"checklist", "recovery", "follow-up"} {
		t.Run(id, func(t *testing.T) {
			template, err := BlockTemplateFor(id, 1)
			if err != nil {
				t.Fatal(err)
			}
			fields := map[string]string{}
			hasOwner, hasCompletion := false, false
			for _, field := range template.Fields {
				if field.Key == "owner" {
					hasOwner = true
					if field.Required {
						t.Fatal("unknown owner must be omittable")
					}
					fields[field.Key] = "Recorded fixture owner"
				}
				if field.Key == "completion" {
					hasCompletion = true
				}
				if field.Required {
					fields[field.Key] = "Explicit illustrative " + field.Key + "; no live result is asserted."
				}
			}
			if !hasOwner || !hasCompletion {
				t.Fatal("action block lacks ownership or completion contract")
			}
			spec := completeSpec(t, TypeNote, "Action block "+id)
			spec.Blocks = []BlockSpec{{Template: id, ID: "action", Fields: fields}}
			result, err := CreateNote(t.TempDir(), spec)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(result.Path)
			header, body, _ := SplitFrontmatter(string(data))
			fm, _, _ := ParseFrontmatter([]byte(header))
			content, err := ReadAuthoring(fm, body)
			if err != nil || len(content.Blocks) != 1 || content.Blocks[0].Fields["owner"] != fields["owner"] || content.Blocks[0].Fields["completion"] != fields["completion"] {
				t.Fatalf("owner/completion lost: %v %+v", err, content)
			}
			delete(spec.Blocks[0].Fields, "owner")
			without, err := CreateNote(t.TempDir(), spec)
			if err != nil {
				t.Fatal(err)
			}
			data, _ = os.ReadFile(without.Path)
			if strings.Contains(string(data), "### Owner") {
				t.Fatal("unknown owner section was rendered")
			}
		})
	}
}
