// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// fakeResolver resolves exactly the ids given to it, which is what a real repository does
// and what a hex-looking word in prose does not.
func fakeResolver(t *testing.T, known map[string]string) CommitResolver {
	t.Helper()
	return func(id string) (Commit, bool) {
		subj, ok := known[id]
		if !ok {
			return Commit{}, false
		}
		parts := strings.SplitN(subj, "|", 2)
		return Commit{ID: id, When: mustParse(t, parts[0]), Subject: parts[1]}, true
	}
}

const pmSkeleton = "---\nid: p\ntype: post-mortem\ntitle: P\nwhen: \"2026-07-16\"\ncreated: \"2026-07-16\"\nagent: claude-code\n" +
	"do: FOLLOW-UP\ndont: IMPACT\nwhy: ROOT-CAUSE\n---\n\n# P\n\n" +
	"## What happened\n<!-- TODO: the incident, plainly -->\n\n" +
	"## Impact\nIMPACT\n\n## Root cause\nROOT-CAUSE\n\n## Follow-ups\nFOLLOW-UP\n"

func TestBackfillTimelineFile(t *testing.T) {
	t.Run("builds a timeline from the commits the note names", func(t *testing.T) {
		src := strings.Replace(pmSkeleton, "## Root cause\nROOT-CAUSE",
			"## Root cause\nFixed at 4f6ad976 after 9f4f791f, see also 2026-07-11.", 1)
		p := filepath.Join(t.TempDir(), "p.md")
		os.WriteFile(p, []byte(src), 0o644)

		res, err := BackfillTimelineFile(p, fakeResolver(t, map[string]string{
			"4f6ad976": "2026-07-16T21:15:17+02:00|fix(dockyard): close the reports",
			"9f4f791f": "2026-07-16T19:12:11+02:00|chore: remove dead tiles",
		}), false)
		if err != nil || !res.Changed {
			t.Fatalf("Changed=%v err=%v", res.Changed, err)
		}
		out, _ := os.ReadFile(p)
		got := string(out)

		for _, want := range []string{
			"Recorded 2026-07-16 by claude-code.",
			"| 2026-07-16 19:12 +02:00 | `9f4f791f` | chore: remove dead tiles |",
			"| 2026-07-16 21:15 +02:00 | `4f6ad976` | fix(dockyard): close the reports |",
			"Other dates this note refers to: 2026-07-11.",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
		// Chronological, not the order they appear in the prose (4f6ad976 is named first).
		if strings.Index(got, "9f4f791f") > strings.Index(got, "4f6ad976") {
			t.Errorf("timeline is not in committer order:\n%s", got)
		}
	})

	t.Run("a hex-looking word that is not a commit never reaches the timeline", func(t *testing.T) {
		// This is the whole reason resolution is required rather than regex-only: prose is
		// full of hex-shaped words and truncated ids, and inventing a row for one would put
		// a fabricated event in an incident record.
		src := strings.Replace(pmSkeleton, "## Root cause\nROOT-CAUSE",
			"## Root cause\nThe deadbeef marker and abcdef0 both appear here.", 1)
		p := filepath.Join(t.TempDir(), "p.md")
		os.WriteFile(p, []byte(src), 0o644)

		if _, err := BackfillTimelineFile(p, fakeResolver(t, map[string]string{}), false); err != nil {
			t.Fatal(err)
		}
		out, _ := os.ReadFile(p)
		// Scope the check to the generated section: "deadbeef" legitimately stays in the
		// prose that mentions it, it just must not become a timeline entry.
		_, body, _ := SplitFrontmatter(string(out))
		happened := body[strings.Index(body, "## What happened"):]
		happened = happened[:strings.Index(happened, "\n## ")]
		if strings.Contains(happened, "deadbeef") || strings.Contains(happened, "| committed |") {
			t.Errorf("an unresolvable id produced a timeline row:\n%s", happened)
		}
		if !strings.Contains(string(out), "names no commit ids and no other dates") {
			t.Errorf("expected the honest dates-only wording:\n%s", out)
		}
	})

	t.Run("does not repeat the recorded date as an other date", func(t *testing.T) {
		src := strings.Replace(pmSkeleton, "## Root cause\nROOT-CAUSE",
			"## Root cause\nIt broke on 2026-07-16 and nothing else.", 1)
		p := filepath.Join(t.TempDir(), "p.md")
		os.WriteFile(p, []byte(src), 0o644)

		if _, err := BackfillTimelineFile(p, nil, false); err != nil {
			t.Fatal(err)
		}
		out, _ := os.ReadFile(p)
		if strings.Contains(string(out), "Other dates") {
			t.Errorf("the note's own recorded date was listed back as an other date:\n%s", out)
		}
	})

	t.Run("never touches an authored What happened", func(t *testing.T) {
		src := strings.Replace(pmSkeleton, "## What happened\n<!-- TODO: the incident, plainly -->",
			"## What happened\nA HUMAN WROTE THIS", 1)
		p := filepath.Join(t.TempDir(), "p.md")
		os.WriteFile(p, []byte(src), 0o644)

		res, err := BackfillTimelineFile(p, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := os.ReadFile(p)
		if res.Changed || string(out) != src {
			t.Errorf("an authored section was modified (Changed=%v)", res.Changed)
		}
	})

	t.Run("is idempotent and leaves other types alone", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "p.md")
		os.WriteFile(p, []byte(pmSkeleton), 0o644)
		BackfillTimelineFile(p, nil, false)
		first, _ := os.ReadFile(p)
		res, _ := BackfillTimelineFile(p, nil, false)
		second, _ := os.ReadFile(p)
		if res.Changed || string(first) != string(second) {
			t.Errorf("second run changed the file (Changed=%v)", res.Changed)
		}

		g := filepath.Join(t.TempDir(), "g.md")
		gotcha := "---\nid: g\ntype: gotcha\ntitle: G\n---\n\n# G\n\n## What happened\n<!-- TODO: the incident, plainly -->\n"
		os.WriteFile(g, []byte(gotcha), 0o644)
		res2, _ := BackfillTimelineFile(g, nil, false)
		outG, _ := os.ReadFile(g)
		if res2.Changed || string(outG) != gotcha {
			t.Errorf("a non post-mortem was rewritten (Changed=%v)", res2.Changed)
		}
	})

	t.Run("dry run reports without writing", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "p.md")
		os.WriteFile(p, []byte(pmSkeleton), 0o644)
		res, err := BackfillTimelineFile(p, nil, true)
		if err != nil || !res.Changed {
			t.Fatalf("dry run should report a change: Changed=%v err=%v", res.Changed, err)
		}
		out, _ := os.ReadFile(p)
		if string(out) != pmSkeleton {
			t.Error("dry run wrote to the file")
		}
	})
}

func TestFirstSentence(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{
			"takes the opening sentence",
			"Self-hosted newsletter service running on host. It is wired into the deploy path.",
			"Self-hosted newsletter service running on host.",
		},
		{
			"stops at the first paragraph",
			"A short but complete opening line here.\n\nA second paragraph that must not be reached.",
			"A short but complete opening line here.",
		},
		{
			"does not break on an abbreviation",
			"It runs on host, behind Caddy, etc. And then a real second sentence follows here.",
			"It runs on host, behind Caddy, etc. And then a real second sentence follows here.",
		},
		{"refuses something too short to be a summary", "Nope. Then more text follows here.", ""},
		{"refuses empty", "", ""},
		{"refuses the TODO sentinel being passed through", "TODO", ""},
	} {
		if got := FirstSentence(tc.in); got != tc.want {
			t.Errorf("%s: FirstSentence(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
