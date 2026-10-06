// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"strings"
	"testing"
	"time"
)

func fixedNow(t *testing.T) {
	t.Helper()
	orig := Now
	Now = func() time.Time { return time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { Now = orig })
}

func TestCreateNoteRoundTripsColonValues(t *testing.T) {
	fixedNow(t)
	spec := completeSpec(t, TypeGotcha, "Mollie: no HMAC on webhooks, so re-fetch by id")
	spec.Sections["remedy"] = "Re-fetch GET /v2/payments/{id}: the webhook body is not signed."
	spec.SourceURL = "https://example.invalid/fixture?reason=colon"
	res, err := CreateNote(t.TempDir(), spec)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	fmText, body, had := SplitFrontmatter(string(data))
	fm, _, err := ParseFrontmatter([]byte(fmText))
	if err != nil || !had || fm.ID != res.ID || fm.Type != TypeGotcha {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if SectionText(body, "remedy") != spec.Sections["remedy"] {
		t.Fatal("colon-bearing prose changed")
	}
}

func TestValidateRoundTrip(t *testing.T) {
	valid := "---\nid: ok-note\ntype: gotcha\ntitle: \"a: b\"\n---\n# body\n"
	if err := validateRoundTrip(valid, "ok-note"); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{
		"---\nid: bad-note\ntitle: foo: bar\n---\n# body\n",
		"---\nid: other\ntype: gotcha\n---\n# body\n",
		"# just a body\n",
	} {
		if err := validateRoundTrip(candidate, "expected"); err == nil {
			t.Fatalf("invalid candidate accepted: %s", candidate)
		}
	}
}

func TestCreateNoteCollisionSuffixes(t *testing.T) {
	root := t.TempDir()
	a, err := CreateNote(root, completeSpec(t, TypeNote, "Same Title"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CreateNote(root, completeSpec(t, TypeNote, "Same Title"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || b.ID != "same-title-2" {
		t.Fatalf("collision ids %s, %s", a.ID, b.ID)
	}
}

func TestModernMapRendersExplicitEntryPoints(t *testing.T) {
	spec := completeSpec(t, TypeMap, "Mesh operations map")
	spec.Summary = "Start here for the fixture's runtime, release and retrieval operations."
	spec.Sections["scope"] = "A bounded fixture collection; no live runtime claim."
	spec.Sections["start_here"] = "- [[mesh]]: fixture overview."
	spec.Sections["grouped_links"] = "- [[mesh-release]]: release guidance.\n- [[mesh-retrieval]]: retrieval guidance."
	res, err := CreateNote(t.TempDir(), spec)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(res.Path)
	_, body, _ := SplitFrontmatter(string(data))
	for _, want := range []string{spec.Summary, "[[mesh]]", "[[mesh-release]]", "[[mesh-retrieval]]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("explicit map content missing: %q", want)
		}
	}
	if strings.Contains(body, "TODO") {
		t.Fatal("authored map retained placeholders")
	}
}

func TestEmptyMapDraftDoesNotInventLinks(t *testing.T) {
	res, err := CreateNote(t.TempDir(), NewNoteSpec{Template: "index", Title: "Unpopulated collection", Status: "draft"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(res.Path)
	if strings.Contains(string(data), "[[") {
		t.Fatal("draft fabricated targets")
	}
	if len(res.TODOs) != 4 {
		t.Fatalf("expected summary plus three core gaps: %v", res.TODOs)
	}
}
