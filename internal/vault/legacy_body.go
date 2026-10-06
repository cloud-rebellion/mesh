// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import "strings"

// Historical rendering helpers support legacy repair only. New notes use the
// versioned body contract in render_authoring.go.

// renderBody builds a note's body. For the three flywheel types (decision, gotcha,
// post-mortem) the body is PROJECTED from the same do/dont/why the author already put in
// frontmatter, via bodySections below, so a filled note reads as prose under real
// headings instead of a TODO skeleton sitting under a wall of YAML. Frontmatter is left
// untouched by this: retrieve.Card and internal/index read do/dont/why from there, so
// those fields stay exactly as CreateNote set them regardless of what the body ends up
// saying. Every other type (entity/concept/map/note) is not fed by do/dont/why at all
// (see NewNoteSpec/CreateNote), so its body is still the fixed bodyTemplate skeleton.
func renderLegacyBody(fm *Frontmatter) string {
	if sections := bodySections(fm.Type); sections != nil {
		return renderSections(fm, sections)
	}
	// An agent-authored map arrives with an overview in why and its front-door
	// entries in related. Rendering the generic human TODO template here stranded
	// both behind frontmatter: the graph was technically connected, but a teammate
	// opening the map saw an empty page. Keep the skeleton for an empty `mesh new
	// map`, while turning supplied, attributable facts into a useful body.
	if fm.Type == TypeMap && (!Unfilled(fm.Why) || len(fm.Related) > 0) {
		return renderAuthoredMap(fm)
	}
	body := bodyTemplate(fm.Type)
	// A reference page keeps its full skeleton (its extra prompts are the point), but any
	// heading that DOES have a frontmatter field behind it gets the prose instead of the
	// prompt. Same helper the backfill uses, so create-time and repair-time agree.
	if sections := referenceSections(fm.Type); sections != nil {
		body, _ = fillBodySections(body, fm, sections)
	}
	// Same entity repair the backfill applies, so a page written today and a page repaired
	// later are byte-identical. Without this an agent-written entity was born with a lead
	// prompt it would never fill and two sections nothing feeds, which is precisely the
	// state 11 live pages were in.
	if fm.Type == TypeEntity {
		body, _ = repairEntityPage(body, fm)
	}
	return body
}

func renderAuthoredMap(fm *Frontmatter) string {
	var b strings.Builder
	overview := strings.TrimSpace(fm.Why)
	if !Unfilled(overview) {
		lead := FirstSentence(overview)
		if lead == "" {
			lead = overview
		}
		b.WriteString("**")
		b.WriteString(lead)
		b.WriteString("**\n")
		if rest := strings.TrimSpace(strings.TrimPrefix(overview, lead)); rest != "" {
			b.WriteString("\n")
			b.WriteString(rest)
			b.WriteString("\n")
		}
	} else {
		b.WriteString(entityLeadPlaceholder)
		b.WriteString("\n")
	}
	b.WriteString("\n## Start here\n")
	if len(fm.Related) == 0 {
		b.WriteString("<!-- TODO: link the domain's entry points -->\n")
		return b.String()
	}
	for _, id := range fm.Related {
		b.WriteString("- [[")
		b.WriteString(id)
		b.WriteString("]]\n")
	}
	return b.String()
}

func bodyTemplate(t NoteType) string {
	if sections := bodySections(t); sections != nil {
		// Total for every type: an empty *Frontmatter reproduces the plain skeleton,
		// so this stays a single source of truth instead of a second copy of the text.
		return renderSections(&Frontmatter{}, sections)
	}
	switch t {
	case TypeEntity:
		return tmplEntity
	case TypeConcept:
		return tmplConcept
	case TypeMap:
		return tmplMap
	default:
		return tmplNote
	}
}

// bodySection is one heading of a flywheel note's body: its title, the placeholder shown
// while nothing is authored, and the frontmatter field (if any) that fills it once the
// author supplies real content.
type bodySection struct {
	heading     string
	placeholder string
	field       func(fm *Frontmatter) string
}

func fieldDo(fm *Frontmatter) string   { return fm.Do }
func fieldDont(fm *Frontmatter) string { return fm.Dont }
func fieldWhy(fm *Frontmatter) string  { return fm.Why }

// bodySections returns the ordered body sections for a type that is fed by do/dont/why,
// or nil for a type that is not (entity/concept/map/note keep the fixed skeleton from
// bodyTemplate).
//
// The mapping is the SAME shape for all three flywheel types, because do/dont/why mean
// the same three things everywhere in Mesh (ORGANIZATION.md: a decision/gotcha's
// "required body" is literally "**Do:** ... **Don't:** ... **Why:** ..."):
//
//   - why is the causal, explanatory field -> the type's causal section
//     (Context / Cause / Root cause).
//   - do is the actionable recommendation -> the type's action section
//     (Decision / Fix / Follow-ups).
//   - dont is the cautionary field, which in practice describes what goes wrong when the
//     advice is ignored -> the type's consequence section
//     (Consequences / Symptom / Impact).
//
// post-mortem has a fourth section, "What happened", with no corresponding field: it is
// the plain narrative of the incident, and do/dont/why carry judgments (a recommendation,
// a warning, a reason), not a timeline. It keeps its TODO placeholder even when the other
// three sections are filled, on purpose: inventing a fourth field's content from the
// other three would be fabrication, and an honest gap is better than invented history
// (see the post-mortem on the 65 title-only note stubs: guidance that cannot be sourced
// from the note itself must never be authored on the note's behalf).
func bodySections(t NoteType) []bodySection {
	switch t {
	case TypeDecision:
		return []bodySection{
			{"Context", "<!-- TODO: what forced a decision; the situation and constraints -->", fieldWhy},
			{"Decision", "<!-- TODO: what was decided, stated plainly -->", fieldDo},
			{"Consequences", "<!-- TODO: trade-offs accepted; what this makes easy or hard -->", fieldDont},
			relatedSection,
		}
	case TypeGotcha:
		return []bodySection{
			{"Symptom", "<!-- TODO: how the problem shows up -->", fieldDont},
			{"Cause", "<!-- TODO: the root cause -->", fieldWhy},
			{"Fix", "<!-- TODO: the resolution or workaround -->", fieldDo},
			relatedSection,
		}
	case TypePostMortem:
		return []bodySection{
			{"What happened", "<!-- TODO: the incident, plainly -->", nil},
			{"Impact", "<!-- TODO: who or what was affected and for how long -->", fieldDont},
			{"Root cause", "<!-- TODO: the underlying cause, not just the trigger -->", fieldWhy},
			{"Follow-ups", "<!-- TODO: concrete actions, owners, dates -->", fieldDo},
			relatedSection,
		}
	default:
		return nil
	}
}

// referenceSections maps frontmatter onto the body of a REFERENCE page (entity, note).
// These types are not fed by do/dont/why when a human scaffolds one with `mesh new`, which
// is why they are absent from bodySections and keep their type-specific skeleton. But an
// agent writing one through mesh_write_entity supplies the page's substance as `why`, and
// mesh_append_note carries do/dont/why on a plain note, so for those the prose exists and
// simply never reached the body: 18 pages in the live vault (11 entities, 7 notes) held
// full frontmatter and a body of nothing but TODO comments, with their related: links
// described rather than rendered.
//
// Only headings with a field to source them from appear here. The rest of each template
// (an entity's "How it works" and "Key facts", the "**One-liner.**" lead) has no
// corresponding field and keeps its TODO prompt, the same honest gap post-mortem's
// "What happened" keeps: a prompt for a human beats a heading filled with a guess.
func referenceSections(t NoteType) []bodySection {
	switch t {
	case TypeEntity:
		return []bodySection{
			{"What it does", "<!-- TODO: the core function, plainly -->", fieldWhy},
			relatedSection,
		}
	case TypeNote:
		return []bodySection{
			{"Overview", "<!-- TODO: the substance of this note -->", fieldWhy},
			{"Do", "<!-- TODO: what to do, concretely -->", fieldDo},
			{"Don't", "<!-- TODO: what to avoid, and why -->", fieldDont},
			relatedSection,
		}
	default:
		return nil
	}
}

// entityLeadPlaceholder is the "**One-liner.**" prompt tmplEntity opens with. It is a
// lead line rather than a heading, so the section machinery cannot reach it.
const entityLeadPlaceholder = "**One-liner.** <!-- TODO: what this is and why it matters, in one sentence -->"

// unsourcedEntityHeadings are the entity sections no frontmatter field can fill. On a page
// an agent wrote through mesh_write_entity they are dead weight forever: the whole payload
// is `why`, nothing else is ever supplied, and a heading followed by a TODO comment renders
// as an empty section. Writing architecture prose on the page's behalf is the fabrication
// the 65-stub post-mortem forbids, so the repair drops them instead. `mesh new entity`
// still emits them, because a human scaffolding a page IS going to fill them in and the
// prompt is the point there.
var unsourcedEntityHeadings = []string{"How it works", "Key facts"}

// FirstSentence returns the first sentence of s, or "" when there is not a clean one. It
// is deliberately conservative: it stops at ". ", "! " or "? " followed by a capital or a
// digit, refuses a result under 20 or over 400 characters, and refuses one that ends on a
// common abbreviation, so a page gets a real one-line summary or none at all.
func FirstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Only consider the first paragraph: a lead sentence never spans a blank line.
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.Join(strings.Fields(s), " ")
	for i := 0; i < len(s)-1; i++ {
		if s[i] != '.' && s[i] != '!' && s[i] != '?' {
			continue
		}
		if s[i+1] != ' ' {
			continue
		}
		rest := strings.TrimLeft(s[i+1:], " ")
		if rest == "" {
			break
		}
		c := rest[0]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			continue
		}
		cand := strings.TrimSpace(s[:i+1])
		if endsOnAbbreviation(cand) {
			continue
		}
		if len(cand) < 20 || len(cand) > 400 {
			return ""
		}
		return cand
	}
	// No sentence break at all: the whole first paragraph counts if it is short enough.
	if len(s) >= 20 && len(s) <= 400 {
		return s
	}
	return ""
}

// endsOnAbbreviation reports whether cand ends on something that looks like an
// abbreviation rather than a sentence, so "runs on host, port 8080, etc." does not
// become a one-liner ending mid-thought.
func endsOnAbbreviation(cand string) bool {
	low := strings.ToLower(cand)
	for _, a := range []string{" e.g.", " i.e.", " etc.", " vs.", " no.", " approx.", " cf."} {
		if strings.HasSuffix(low, a) {
			return true
		}
	}
	// A single letter before the dot is an initial, not a sentence end.
	if n := len(cand); n >= 2 && cand[n-1] == '.' {
		c := cand[n-2]
		if (c >= 'A' && c <= 'Z') && (n == 2 || cand[n-3] == ' ') {
			return true
		}
	}
	return false
}

// relatedSection closes every flywheel note with its links, RENDERED, not described.
//
// It used to be a comment saying the related: field renders in the graph, which is true
// and useless to the two readers that matter. An agent reading the note through
// mesh_fetch, and a human reading the raw markdown in an editor, both got a note whose
// last line explained that its links exist somewhere else. Now the links are in the note.
//
// This creates no duplicate edges: the indexer already emits a "references" edge per
// related: entry, Graph.AddEdge dedups on source+target+relation, and a body wikilink to
// the same target lands on the same key. What it adds is the ability to FOLLOW the thread
// from the text itself, which is the whole point of a linked corpus.
var relatedSection = bodySection{"Related", relatedPlaceholder, fieldRelated}

const relatedPlaceholder = "<!-- no related notes yet; add ids to the related: field and they render here -->"

// fieldRelated renders the related: ids as a wikilink list. Unfilled (the empty list)
// falls back to the placeholder like every other section.
func fieldRelated(fm *Frontmatter) string {
	if len(fm.Related) == 0 {
		return ""
	}
	lines := make([]string, 0, len(fm.Related))
	for _, r := range fm.Related {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		lines = append(lines, "- [["+r+"]]")
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// renderSections fills each heading with its mapped frontmatter field when the author
// supplied real content, and its placeholder comment otherwise. Unfilled is the SAME
// predicate lint and the indexer use for "the author left this blank" (empty, or the
// literal "TODO" sentinel the scaffold writes), so a section and its frontmatter field can
// never disagree about whether they are filled.
func renderSections(fm *Frontmatter, sections []bodySection) string {
	parts := make([]string, len(sections))
	for i, s := range sections {
		content := s.placeholder
		if s.field != nil {
			if v := strings.TrimSpace(s.field(fm)); !Unfilled(v) {
				content = v
			}
		}
		parts[i] = "## " + s.heading + "\n" + content
	}
	return strings.Join(parts, "\n\n") + "\n"
}

const tmplNote = `## Overview
<!-- TODO: the substance of this note -->

## Related
<!-- linked notes from the related: field render in the graph -->
`

// linkPlaceholder shows the wikilink syntax in backticks, so it reads as the example it
// is rather than as a link. A bare [[note-id]] in a template made every note Mesh
// scaffolds ship a real link to a note nobody had written, so the author inherited a
// broken-link notice from the skeleton itself.
const linkPlaceholder = "`[[note-id]]`"

// tmplEntity / tmplConcept / tmplMap follow the LLM-wiki (Karpathy) shape: a bold
// one-line card up top (the atomic summary that also becomes the retrieval snippet),
// then a few dense, type-specific sections, then Related links. The AI fills the
// <!-- TODO --> guidance; everything else is derived.

const tmplEntity = `**One-liner.** <!-- TODO: what this is and why it matters, in one sentence -->

## What it does
<!-- TODO: the core function, plainly -->

## How it works
<!-- TODO: architecture, the moving parts, where it runs -->

## Key facts
<!-- TODO: the non-obvious things a teammate needs (constraints, gotchas, status, repo) -->

## Related
<!-- link the concepts it uses and the decisions that shaped it: ` + linkPlaceholder + ` (also fill related: above) -->
`

const tmplConcept = `**One-liner.** <!-- TODO: the idea in one sentence -->

## The idea
<!-- TODO: what it is and why it matters -->

## How it works
<!-- TODO: the mechanism, the model, the steps -->

## How it applies to our work
<!-- TODO: where we use it, with ` + linkPlaceholder + ` links to the entities that apply it -->

## Related
<!-- ` + linkPlaceholder + ` -->
`

const tmplMap = `**One-liner.** <!-- TODO: the domain this maps, in one sentence -->

<!-- One paragraph of context, then grouped links. A map is mostly links: if you are
     writing long prose here it wants to be a concept instead. -->

## <!-- TODO: a section per sub-area -->
- ` + linkPlaceholder + ` - <!-- why it matters -->
`
