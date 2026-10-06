// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allSectionHeadings is every heading the migrator can be asked to locate: the sections of
// all three flywheel types, the reference-page sections, and the entity headings the repair
// drops. The guard tests walk this list so a section added to a template later is covered
// the day it is added, not the day someone remembers to extend a test.
func allSectionHeadings(t *testing.T) []bodySection {
	t.Helper()
	var out []bodySection
	seen := map[string]bool{}
	for _, nt := range []NoteType{TypeDecision, TypeGotcha, TypePostMortem, TypeEntity, TypeNote} {
		sections := bodySections(nt)
		if sections == nil {
			sections = referenceSections(nt)
		}
		if sections == nil {
			t.Fatalf("type %s maps to no body sections", nt)
		}
		for _, s := range sections {
			if seen[s.heading] {
				continue
			}
			seen[s.heading] = true
			out = append(out, s)
		}
	}
	for _, h := range unsourcedEntityHeadings {
		if !seen[h] {
			seen[h] = true
			out = append(out, bodySection{heading: h, placeholder: "<!-- TODO: nothing sources this -->"})
		}
	}
	return out
}

// headingLines counts the lines of body that ARE the "## <heading>" heading line, tolerating
// the \r of a CRLF note and trailing spaces. It counts what a reader sees as a section, so a
// second appended copy of a heading the note already had shows up as 2.
func headingLines(body, heading string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimRight(line, " \t\r") == "## "+heading {
			n++
		}
	}
	return n
}

// TestSectionLocatorIsLineAnchored walks every body section the migrator knows about and
// asserts the projection finds it as a heading LINE: level-exact, tolerant of a CRLF note's
// \r and of trailing whitespace, and blind to a heading that only appears inside a fence or
// an HTML comment.
//
// The needle it replaces was strings.Contains("## "+heading+"\n"), which got all of that
// wrong: a CRLF section read as ABSENT and was appended a second time, "### Cause" satisfied
// a "## Cause" test, and an example inside a fenced block was rewritten as if it were the
// note's own section.
func TestSectionLocatorIsLineAnchored(t *testing.T) {
	fm := &Frontmatter{
		Do:      "VALUE-DO",
		Dont:    "VALUE-DONT",
		Why:     "VALUE-WHY",
		Related: StringList{"other-note"},
	}
	shapes := []struct {
		name    string
		body    func(heading, ph string) string
		visible bool // whether the locator must see a real section here
	}{
		{"lf", func(h, ph string) string { return "## " + h + "\n" + ph + "\n" }, true},
		{"crlf", func(h, ph string) string { return "## " + h + "\r\n" + ph + "\r\n" }, true},
		{"trailing space", func(h, ph string) string { return "## " + h + "  \n" + ph + "\n" }, true},
		{"h3 of the same name", func(h, ph string) string { return "### " + h + "\n" + ph + "\n" }, false},
		{"inside a fence", func(h, ph string) string { return "```md\n## " + h + "\n" + ph + "\n```\n" }, false},
		{"inside a comment", func(h, ph string) string { return "<!--\n## " + h + "\nnot a section\n-->\n" }, false},
		{"longer heading", func(h, ph string) string { return "## " + h + " notes\n" + ph + "\n" }, false},
	}
	for _, s := range allSectionHeadings(t) {
		if s.field == nil {
			continue // no frontmatter feeds it, so the projection never asks where it is
		}
		want := strings.TrimSpace(s.field(fm))
		for _, sh := range shapes {
			t.Run(s.heading+"/"+sh.name, func(t *testing.T) {
				body := sh.body(s.heading, s.placeholder)
				out, filled := fillBodySections(body, fm, []bodySection{s})
				if sh.visible {
					if strings.Join(filled, ",") != s.heading {
						t.Fatalf("filled = %v, want the section filled IN PLACE:\n%q", filled, out)
					}
					if n := headingLines(out, s.heading); n != 1 {
						t.Errorf("%d copies of %q, want 1:\n%q", n, "## "+s.heading, out)
					}
					if strings.Contains(out, s.placeholder) || !strings.Contains(out, want) {
						t.Errorf("placeholder not replaced by the frontmatter field:\n%q", out)
					}
					return
				}
				if strings.Join(filled, ",") != s.heading+" (added)" {
					t.Fatalf("filled = %v, want the section ADDED (nothing here is one):\n%q", filled, out)
				}
				if !strings.Contains(out, strings.TrimRight(body, "\n")) {
					t.Errorf("rewrote text that is not a section heading:\n%q", out)
				}
				if !strings.Contains(out, "## "+s.heading+"\n"+want) {
					t.Errorf("no real section was added:\n%q", out)
				}
			})
		}
	}
}

// TestDropPlaceholderSectionIsLineAnchored is the same guard for the entity repair, which
// DELETES a section and so has more to lose from a loose match: a fenced example that
// happens to quote the heading is a section it would cut out of the note.
func TestDropPlaceholderSectionIsLineAnchored(t *testing.T) {
	const ph = "<!-- TODO: nothing sources this -->"
	for _, s := range allSectionHeadings(t) {
		cases := []struct {
			name string
			body string
			want bool
		}{
			{"lf", "## " + s.heading + "\n" + ph + "\n", true},
			{"crlf", "## " + s.heading + "\r\n" + ph + "\r\n", true},
			{"trailing space", "## " + s.heading + "  \n" + ph + "\n", true},
			{"h3 of the same name", "### " + s.heading + "\n" + ph + "\n", false},
			{"inside a fence", "```md\n## " + s.heading + "\n" + ph + "\n```\n", false},
			{"longer heading", "## " + s.heading + " notes\n" + ph + "\n", false},
		}
		for _, tc := range cases {
			t.Run(s.heading+"/"+tc.name, func(t *testing.T) {
				out, ok := dropPlaceholderSection(tc.body, s.heading)
				if ok != tc.want {
					t.Fatalf("dropped = %v, want %v:\n%q", ok, tc.want, out)
				}
				if !ok && out != tc.body {
					t.Errorf("body changed on a no-op:\n%q", out)
				}
			})
		}
	}
}

// TestLastTrailingRelatedIsLineAnchored covers the third reader of the same needle. It
// decides where an added section lands, so a Related it cannot see puts the note's own
// prose after its link list.
func TestLastTrailingRelatedIsLineAnchored(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"lf", "## Overview\nx\n\n## Related\n- [[a]]\n", true},
		{"crlf", "## Overview\r\nx\r\n\r\n## Related\r\n- [[a]]\r\n", true},
		{"trailing space", "## Overview\nx\n\n## Related  \n- [[a]]\n", true},
		{"h3", "## Overview\nx\n\n### Related\n- [[a]]\n", false},
		{"only inside a fence", "## Overview\n```md\n## Related\n- [[a]]\n```\n", false},
		{"followed by another section", "## Related\n- [[a]]\n\n## Fix\nx\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastTrailingRelated(tc.body) >= 0; got != tc.want {
				t.Errorf("lastTrailingRelated found = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMigrateFileKeepsCRLF pins the line endings the frontmatter merge hands back. The
// merged block is always encoded with \n, so a CRLF note came back half CRLF and half LF:
// bytes no author wrote, and the shape that made every section test in migrate.go read a
// CRLF note as having no sections at all.
func TestMigrateFileKeepsCRLF(t *testing.T) {
	for _, tc := range []struct {
		name   string
		src    string
		wantLF int // bare LF endings allowed in the output
	}{
		{"crlf note", strings.ReplaceAll("---\ntitle: T\nupdated: 2026-01-01\n---\n\n# T\n\nbody\n", "\n", "\r\n"), 0},
		{"lf note", "---\ntitle: T\nupdated: 2026-01-01\n---\n\n# T\n\nbody\n", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "n.md")
			if err := os.WriteFile(p, []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := MigrateFile(filepath.Dir(p), p, false)
			if err != nil || !res.Changed {
				t.Fatalf("Changed=%v err=%v", res.Changed, err)
			}
			out, _ := os.ReadFile(p)
			bare := strings.Count(string(out), "\n") - strings.Count(string(out), "\r\n")
			switch {
			case tc.wantLF == 0 && bare != 0:
				t.Errorf("%d bare LF line endings in a CRLF note:\n%q", bare, out)
			case tc.wantLF < 0 && strings.Contains(string(out), "\r"):
				t.Errorf("an LF note came back with CR bytes:\n%q", out)
			}
			if !strings.Contains(string(out), "type: note") {
				t.Errorf("migration did not run:\n%q", out)
			}
		})
	}
}
