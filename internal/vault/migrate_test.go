// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func mustFM(s string) string {
	fm, _, _ := SplitFrontmatter(s)
	return fm
}

func TestMigrateFileAddsIdWhenRelatedIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "codeindex.md")
	original := "---\ntype: entity\nupdated: 2026-04-10\n---\n# Codeindex\nbody\n\n## Related\n- [[mesh]]\n- [[dockyard|the platform]]\n"
	if err := os.WriteFile(p, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := MigrateFile(dir, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("expected changes")
	}

	data, _ := os.ReadFile(p)
	s := string(data)
	for _, want := range []string{
		"id: codeindex", `when: "2026-04-10"`, "related:", "  - mesh", "  - dockyard",
		"type: entity", "updated: 2026-04-10", // existing keys preserved
	} {
		if !strings.Contains(s, want) {
			t.Errorf("after migrate missing %q\n---\n%s", want, s)
		}
	}

	fm, _, _ := ParseFrontmatter([]byte(mustFM(s)))
	if fm.ID != "codeindex" || fm.When != "2026-04-10" || len(fm.Related) != 2 {
		t.Errorf("reparse mismatch: id=%q when=%q related=%v", fm.ID, fm.When, fm.Related)
	}

	res2, err := MigrateFile(dir, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Changed {
		t.Errorf("second migrate should be a no-op, got actions %v", res2.Actions)
	}
}

func TestMigrateFileNoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "orphan.md")
	if err := os.WriteFile(p, []byte("# Orphan\njust text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := MigrateFile(dir, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("expected changes")
	}
	s, _ := os.ReadFile(p)
	for _, want := range []string{"id: orphan", "type: note", "when:", "# Orphan", "just text"} {
		if !strings.Contains(string(s), want) {
			t.Errorf("missing %q\n%s", want, string(s))
		}
	}
}

func TestMigrateDoesNotRequireAbsentLegacyTriad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "some-decision.md")
	if err := os.WriteFile(p, []byte("---\nid: some-decision\ntype: decision\nwhen: 2026-01-01\n---\n# D\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := MigrateFile(dir, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Error("already-keyed file should not change")
	}
	if len(res.Issues) != 0 {
		t.Errorf("absent legacy triad is not an authoring requirement: %v", res.Issues)
	}
}

func TestMigrateReportsExplicitLegacyPlaceholders(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "placeholder.md")
	content := "---\nid: placeholder\ntype: decision\nwhen: 2026-01-01\ndo: TODO\ndont: '<!-- pending -->'\nwhy: TODO\n---\n# Historical placeholders\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := MigrateFile(dir, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 3 || res.Changed {
		t.Fatalf("explicit historical placeholders not reported losslessly: %+v", res)
	}
	after, _ := os.ReadFile(p)
	if string(after) != content {
		t.Fatal("report-only historical check changed content")
	}
}

func TestBackfillRelatedFile(t *testing.T) {
	for _, tc := range []struct {
		name        string
		content     string
		related     []string
		wantChange  bool
		wantHas     []string
		wantRelated []string // what the note's related: must decode to after the rewrite
	}{
		{
			name:       "adds links to a note with none",
			content:    "---\nid: alpha\ntype: gotcha\ntags:\n    - mesh\n---\n# Alpha\nbody\n",
			related:    []string{"beta", "gamma"},
			wantChange: true,
			wantHas:    []string{"related:", "- beta", "- gamma"},
		},
		{
			name:       "never overwrites an author's own links",
			content:    "---\nid: alpha\nrelated:\n    - mine\n---\n# Alpha\n",
			related:    []string{"derived"},
			wantChange: false,
			wantHas:    []string{"- mine"},
		},
		{
			// The shape 1098 of the live vault's 1227 notes are in. A bare key parses to
			// nil, not to an empty list, and reading it as "the author declared related"
			// skipped every one of them: --wire-orphans reported "already declaring
			// related" for notes whose related list did not exist.
			name:        "a bare related: key is an empty scaffold, not a declaration",
			content:     "---\nid: alpha\ntype: gotcha\nrelated:\ntags:\n    - mesh\n---\n# Alpha\nbody\n",
			related:     []string{"beta", "gamma"},
			wantChange:  true,
			wantHas:     []string{"- beta", "- gamma"},
			wantRelated: []string{"beta", "gamma"},
		},
		{
			// The inverse, and the one the old condition got backwards: an explicit empty
			// list IS a decision ("I looked, there are none") and must survive.
			name:       "an explicit empty list is a decision and is left alone",
			content:    "---\nid: alpha\ntype: gotcha\nrelated: []\n---\n# Alpha\n",
			related:    []string{"derived"},
			wantChange: false,
			wantHas:    []string{"related: []"},
		},
		{
			// A scalar we cannot interpret is still the author having written something.
			name:       "a non-list related is left alone",
			content:    "---\nid: alpha\ntype: gotcha\nrelated: mine\n---\n# Alpha\n",
			related:    []string{"derived"},
			wantChange: false,
			wantHas:    []string{"related: mine"},
		},
		{
			name:       "deduplicates and drops blanks",
			content:    "---\nid: alpha\n---\n# Alpha\n",
			related:    []string{"beta", "  ", "beta"},
			wantChange: true,
			wantHas:    []string{"- beta"},
		},
		{
			name:       "no links means no rewrite",
			content:    "---\nid: alpha\n---\n# Alpha\n",
			related:    nil,
			wantChange: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "n.md")
			if err := os.WriteFile(p, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := BackfillRelatedFile(p, tc.related, false)
			if err != nil {
				t.Fatal(err)
			}
			if res.Changed != tc.wantChange {
				t.Fatalf("Changed = %v, want %v", res.Changed, tc.wantChange)
			}
			out, _ := os.ReadFile(p)
			for _, want := range tc.wantHas {
				if !strings.Contains(string(out), want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			// Whatever we wrote must still parse, or the note vanishes from the index.
			fmStr, _, had := SplitFrontmatter(string(out))
			if !had {
				t.Fatal("rewritten note lost its frontmatter block")
			}
			fm, _, err := ParseFrontmatter([]byte(fmStr))
			if err != nil {
				t.Fatalf("rewritten frontmatter does not parse (index would drop the note): %v", err)
			}
			// Prepending a related: block to frontmatter that already carries a bare
			// related: line yields two of them. YAML resolves that silently, so the note
			// keeps parsing while the graph reads whichever copy the decoder kept.
			if n := strings.Count("\n"+fmStr, "\nrelated:"); n > 1 {
				t.Errorf("frontmatter declares related: %d times, want at most 1:\n%s", n, fmStr)
			}
			if tc.wantRelated != nil {
				if got := []string(fm.Related); !slices.Equal(got, tc.wantRelated) {
					t.Errorf("related = %v, want %v\n%s", got, tc.wantRelated, out)
				}
			}
		})
	}
}

// Neither dry-run nor apply may project shorthand into unsupported cause or impact.
func TestRetiredBodyBackfillPreservesOriginal(t *testing.T) {
	for _, typ := range []string{"post-mortem", "gotcha", "decision", "entity", "note"} {
		for _, dryRun := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/dry=%v", typ, dryRun), func(t *testing.T) {
				original := "---\nid: original\ntype: " + typ + "\ntitle: Historical note\ndont: Avoid an untested deployment.\nwhy: The source does not establish the incident cause.\n---\n# Historical note\n## Impact\n<!-- TODO -->\n## Root cause\n<!-- TODO -->\n"
				path := filepath.Join(t.TempDir(), "note.md")
				if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
					t.Fatal(err)
				}
				res, err := BackfillBodyFile(path, dryRun)
				if err == nil || !strings.Contains(err.Error(), "migration-preview") || res.Changed {
					t.Fatalf("retired writer did not refuse: %+v %v", res, err)
				}
				got, err := os.ReadFile(path)
				if err != nil || string(got) != original {
					t.Fatalf("unreviewed content changed: %q %v", got, err)
				}
				info, _ := os.Stat(path)
				if info.Mode().Perm() != 0o600 {
					t.Fatal("retired writer altered access")
				}
			})
		}
	}
}
