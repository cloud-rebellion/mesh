// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every flywheel type must end with its links, RENDERED. A note whose last section is a
// comment explaining that its links live in the frontmatter is useless to both readers
// that matter: an agent reading it through mesh_fetch, and a human reading the raw file.
func TestEveryFlywheelTypeRendersItsRelatedLinks(t *testing.T) {
	for _, ty := range []NoteType{TypeDecision, TypeGotcha, TypePostMortem} {
		t.Run(string(ty), func(t *testing.T) {
			sections := bodySections(ty)
			if len(sections) == 0 {
				t.Fatalf("%s has no body sections", ty)
			}
			last := sections[len(sections)-1]
			if last.heading != "Related" {
				t.Fatalf("%s ends with %q, not Related: a note has to end with where to go next", ty, last.heading)
			}
			if last.field == nil {
				t.Fatalf("%s's Related section renders no field, so it can only ever be a placeholder", ty)
			}
			fm := &Frontmatter{Related: []string{"alpha", "beta"}}
			got := renderSections(fm, sections)
			for _, want := range []string{"[[alpha]]", "[[beta]]"} {
				if !strings.Contains(got, want) {
					t.Errorf("%s body does not contain %s:\n%s", ty, want, got)
				}
			}
		})
	}
}

// An empty related: list must fall back to the placeholder rather than emitting an empty
// heading or, worse, a broken link.
func TestRelatedSectionWithNoLinksStaysAPlaceholder(t *testing.T) {
	got := renderSections(&Frontmatter{}, bodySections(TypeGotcha))
	if !strings.Contains(got, relatedPlaceholder) {
		t.Fatalf("an empty related: list did not fall back to the placeholder:\n%s", got)
	}
	if strings.Contains(got, "[[") {
		t.Fatalf("an empty related: list emitted a link:\n%s", got)
	}
}

// The backfill has to reach the notes that predate a heading, and it has to leave
// authored prose alone. Both in one fixture, because the danger is a pass that gets one
// right by getting the other wrong.
func TestRetiredBackfillKeepsAuthoredProseAndRelatedMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.md")
	// A gotcha written before Related existed: Symptom is authored, Cause is still the
	// placeholder, and there is no Related heading at all.
	original := `---
id: g
type: gotcha
title: A gotcha
when: "2026-01-01"
related:
    - alpha
do: run the thing
dont: do not run the other thing
why: because the other thing eats the index
---

# A gotcha

## Symptom
The operator's own words about how this shows up.

## Cause
<!-- TODO: the root cause -->

## Fix
<!-- TODO: the resolution or workaround -->

<!-- authored by claude-code -->
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := BackfillBodyFile(path, false)
	if err == nil || res.Changed {
		t.Fatalf("retired operation accepted: %+v %v", res, err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != original {
		t.Fatalf("authored prose or links changed: %q %v", got, readErr)
	}
}

// Historical placeholders and references require deliberate reviewed conversion.
func TestRetiredBackfillRequiresReviewedPlaceholderConversion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.md")
	original := `---
id: d
type: decision
title: A decision
when: "2026-01-01"
related:
    - alpha
do: do the thing
dont: do not do the other
why: because
---

# A decision

## Context
because

## Decision
do the thing

## Consequences
do not do the other

## Related
<!-- linked notes from the related: field render in the graph -->
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := BackfillBodyFile(path, false)
	if err == nil || res.Changed {
		t.Fatalf("retired operation accepted: %+v %v", res, err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != original {
		t.Fatalf("historical placeholder or references changed: %q %v", got, readErr)
	}
}
