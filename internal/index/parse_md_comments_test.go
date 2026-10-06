// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

func linkTargets(pn *ParsedNote) []string {
	var out []string
	for _, l := range pn.Links {
		out = append(out, l.Target)
	}
	return out
}

func tagNames(pn *ParsedNote) []string {
	var out []string
	for _, t := range pn.Tags {
		out = append(out, t.Name)
	}
	return out
}

func headingTexts(pn *ParsedNote) []string {
	var out []string
	for _, h := range pn.Headings {
		out = append(out, h.Text)
	}
	return out
}

// TestParseIgnoresHTMLComments: an HTML comment hides its content from every reader, so
// the wikilinks and tags inside one are not real. They were extracted anyway, which is
// how the commented-out [[note-id]] guidance in the scaffold skeleton turned into a
// broken link in the graph of every note Mesh created.
func TestParseIgnoresHTMLComments(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		links []string
		tags  []string
	}{
		{
			name: "link inside a single line comment",
			body: "<!-- link the note it explains: [[hidden]] -->\n",
		},
		{
			name: "link inside a multi line comment",
			body: "<!--\nlink it: [[hidden]]\nand [[also-hidden]]\n-->\n",
		},
		{
			name:  "link outside a same line comment is still extracted",
			body:  "real [[visible]] <!-- fake [[hidden]] --> and [[visible-too]]\n",
			links: []string{"visible", "visible-too"},
		},
		{
			name:  "text after a multi line comment closes is parsed",
			body:  "<!--\n[[hidden]]\n--> tail [[visible]]\n",
			links: []string{"visible"},
		},
		{
			name: "tag inside a comment",
			body: "<!-- #hidden-tag -->\nplain #visible-tag here\n",
			tags: []string{"visible-tag"},
		},
		{
			name:  "comment marker inside a code span is an example, not markup",
			body:  "an `<!--` in prose still leaves [[visible]] alone\n",
			links: []string{"visible"},
		},
		{
			name:  "comment marker inside a fence does not open a comment",
			body:  "```\n<!--\n```\n[[visible]]\n",
			links: []string{"visible"},
		},
		{
			name:  "fenced and inline code are still ignored",
			body:  "```\n[[fenced]] #fenced-tag\n```\ninline `[[spanned]]` then [[visible]] #visible-tag\n",
			links: []string{"visible"},
			tags:  []string{"visible-tag"},
		},
		{
			name:  "adjacent comments on one line",
			body:  "<!-- a [[h1]] --><!-- b [[h2]] --> [[visible]]\n",
			links: []string{"visible"},
		},
		{
			name:  "comments do not nest, the first close wins",
			body:  "<!-- outer <!-- inner --> [[visible]] -->\n",
			links: []string{"visible"},
		},
		{
			name:  "a stray close with no open is literal text",
			body:  "a --> b [[visible]] #visible-tag\n--> [[visible-two]]\n",
			links: []string{"visible", "visible-two"},
			tags:  []string{"visible-tag"},
		},
		{
			name:  "triple dash comment",
			body:  "<!--- hidden [[h]] ---> [[visible]]\n",
			links: []string{"visible"},
		},
		{
			name:  "empty comment is not an opener",
			body:  "<!----> [[visible]]\n",
			links: []string{"visible"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pn := parse(t, "note.md", tc.body)
			if got := linkTargets(pn); !slices.Equal(got, tc.links) {
				t.Errorf("links = %v, want %v", got, tc.links)
			}
			if got := tagNames(pn); !slices.Equal(got, tc.tags) {
				t.Errorf("tags = %v, want %v", got, tc.tags)
			}
		})
	}
}

func TestParseUnicodeBodyTags(t *testing.T) {
	pn := parse(t, "tags.md", "# Tags\n#räddning #åtgärd #café #日本語\n")
	if got := tagNames(pn); !slices.Equal(got, []string{"räddning", "åtgärd", "café", "日本語"}) {
		t.Fatalf("Unicode tags = %q, want all four intact", got)
	}
}

// TestParseBacktickNearACommentDoesNotSwallowTheFile pins the interaction between the
// two strippers, which is where this whole area goes wrong. Blanking code spans in a
// pass of their own erases the --> out of "<!-- the ` char -->" (an odd backtick blanked
// to end of line), and the comment then ran to the end of the file: every link, tag and
// heading below it vanished from the index with nothing reported. Stripping comments
// first has the mirror bug, hiding the opener instead and leaking the links a genuine
// comment contains. One scan that knows about both is the only ordering that holds.
func TestParseBacktickNearACommentDoesNotSwallowTheFile(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		links    []string
		tags     []string
		headings []string
	}{
		{
			name:     "odd backtick inside a one line comment",
			body:     "# Title\n<!-- prefer the ` char here -->\nreal link [[visible]] and #visible-tag\n## Second heading\nmore [[visible-two]]\n",
			links:    []string{"visible", "visible-two"},
			tags:     []string{"visible-tag"},
			headings: []string{"Title", "Second heading"},
		},
		{
			name:  "code span wrapping across a multi line comment",
			body:  "<!-- TODO: run `mesh index\n     --workers 4` and confirm it is clean -->\n[[visible]]\n",
			links: []string{"visible"},
		},
		{
			name:  "odd backtick before an opener does not hide the opener",
			body:  "see the ` char <!--\n[[hidden]]\n-->\n[[visible]]\n",
			links: []string{"visible"},
		},
		{
			name:  "an unpaired backtick is literal, not a span to end of line",
			body:  "the ` char, then [[visible]] and #visible-tag\n",
			links: []string{"visible"},
			tags:  []string{"visible-tag"},
		},
		{
			name:  "even backticks still hide their span",
			body:  "`[[hidden]]` and [[visible]]\n",
			links: []string{"visible"},
		},
		{
			name:  "matching double backticks hide the whole span",
			body:  "``[[hidden]]`` and [[visible]]\n",
			links: []string{"visible"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pn := parse(t, "note.md", tc.body)
			if got := linkTargets(pn); !slices.Equal(got, tc.links) {
				t.Errorf("links = %v, want %v", got, tc.links)
			}
			if got := tagNames(pn); !slices.Equal(got, tc.tags) {
				t.Errorf("tags = %v, want %v", got, tc.tags)
			}
			if tc.headings != nil {
				if got := headingTexts(pn); !slices.Equal(got, tc.headings) {
					t.Errorf("headings = %v, want %v", got, tc.headings)
				}
			}
			if len(pn.Issues) != 0 {
				t.Errorf("unexpected issues %v (no comment here is unterminated)", pn.Issues)
			}
		})
	}
}

// A backslash before Markdown punctuation escapes it. In particular, \[[note]] is a
// syntax example, not a graph edge. Two backslashes leave the opener unescaped and the
// wikilink real (the first backslash escapes the second).
func TestParseHonorsEscapedWikilinkOpeners(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{name: "one backslash escapes", body: "show \\[[hidden]] then [[visible]]\n", want: []string{"visible"}},
		{name: "two backslashes leave a link", body: "show \\\\[[visible]]\n", want: []string{"visible"}},
		{name: "three backslashes escape", body: "show \\\\\\[[hidden]]\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pn := parse(t, "note.md", tc.body)
			if got := linkTargets(pn); !slices.Equal(got, tc.want) {
				t.Errorf("links = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseHeadingsWithComments states what hiding comments does to headings, which is
// not only a link-extraction change: a heading whose text is entirely a comment has no
// text left, so it stops being a heading. It used to enter the graph as a node labelled
// "<!-- TODO: a section per sub-area -->", straight out of the map skeleton.
func TestParseHeadingsWithComments(t *testing.T) {
	pn := parse(t, "note.md", "## <!-- TODO: a section per sub-area -->\n## Real\n## Also real <!-- keep the text, drop the note -->\n")
	if got := headingTexts(pn); !slices.Equal(got, []string{"Real", "Also real"}) {
		t.Errorf("headings = %v, want [Real, Also real]", got)
	}
	if pn.Headings[0].Line != 2 {
		t.Errorf("heading line = %d, want 2: blanking must not shift line numbers", pn.Headings[0].Line)
	}
}

// TestParseAbruptClosingComments: <!--> and <!---> are complete, empty comments in the
// HTML spec and in CommonMark. Reading them as an opener left a comment nothing could
// close, so the rest of the file disappeared.
func TestParseAbruptClosingComments(t *testing.T) {
	for _, body := range []string{"<!-->\n[[visible]]\n", "<!--->\n[[visible]]\n"} {
		pn := parse(t, "note.md", body)
		if got := linkTargets(pn); !slices.Equal(got, []string{"visible"}) {
			t.Errorf("body %q: links = %v, want [visible]", body, got)
		}
		if len(pn.Issues) != 0 {
			t.Errorf("body %q: issues %v, want none (the comment is closed)", body, pn.Issues)
		}
	}
}

// TestParseUnterminatedHTMLCommentRunsToEndOfFile pins the chosen behaviour for a <!--
// that opens a line and is never closed: it comments out the rest of the file, exactly
// like an unterminated fence. CommonMark starts an HTML block on a line that begins with
// <!-- and ends it at --> or at the end of the document, so this is what the author sees
// rendered; extracting the links below the marker would put edges in the graph that no
// reader of the note can see. It is reported, because losing half a note silently is
// worse than either behaviour.
func TestParseUnterminatedHTMLCommentRunsToEndOfFile(t *testing.T) {
	pn := parse(t, "note.md", "before [[visible]] #visible-tag\n<!-- oops, never closed\n[[hidden]] #hidden-tag\nstill hidden [[also-hidden]]\n")
	if got := linkTargets(pn); !slices.Equal(got, []string{"visible"}) {
		t.Errorf("links = %v, want only the link above the unterminated comment", got)
	}
	if got := tagNames(pn); !slices.Equal(got, []string{"visible-tag"}) {
		t.Errorf("tags = %v, want only the tag above the unterminated comment", got)
	}
	if len(pn.Issues) != 1 || pn.Issues[0].Kind != "unterminated-comment" {
		t.Fatalf("issues = %v, want one unterminated-comment", pn.Issues)
	}
	if !strings.Contains(pn.Issues[0].Msg, "line 2") {
		t.Errorf("issue %q does not name line 2, where the marker is", pn.Issues[0].Msg)
	}
	// BuildGraph is where lint, doctor and health read issues from.
	if _, issues := BuildGraph([]*ParsedNote{pn}); len(issues) == 0 {
		t.Error("BuildGraph dropped the parse issue, so nothing surfaces it")
	}
}

// TestParseUnterminatedMidLineCommentIsLiteralText is the other half of that rule. A
// <!-- in the middle of a line is inline raw HTML, which CommonMark only matches when it
// is terminated: unterminated, it renders as the literal text it is and everything below
// it is plainly visible to the reader. Hiding it anyway contradicted the very principle
// the comment stripping rests on, and cost the note its tail.
func TestParseUnterminatedMidLineCommentIsLiteralText(t *testing.T) {
	pn := parse(t, "note.md", "use the <!-- marker to hide a note\n\n## Real heading\n[[visible]] #visible-tag\n")
	if got := linkTargets(pn); !slices.Equal(got, []string{"visible"}) {
		t.Errorf("links = %v, want [visible]", got)
	}
	if got := tagNames(pn); !slices.Equal(got, []string{"visible-tag"}) {
		t.Errorf("tags = %v, want [visible-tag]", got)
	}
	if got := headingTexts(pn); !slices.Equal(got, []string{"Real heading"}) {
		t.Errorf("headings = %v, want [Real heading]", got)
	}
	if len(pn.Issues) != 0 {
		t.Errorf("issues = %v, want none: nothing is hidden, so there is nothing to report", pn.Issues)
	}
	// A terminated mid-line comment still hides its content: that is the common case in
	// the vault and the one the whole change is for.
	pn = parse(t, "note.md", "**One-liner.** <!-- link it: [[hidden]] -->\n[[visible]]\n")
	if got := linkTargets(pn); !slices.Equal(got, []string{"visible"}) {
		t.Errorf("terminated mid-line comment: links = %v, want [visible]", got)
	}
}

// TestSearchTextAgreesWithTheParser: FTS used a `(?s)<!--.*?-->` regex, which cannot
// match an unterminated comment. The graph dropped that text and search kept it, so the
// two halves of the product disagreed about whether it existed. Code spans are the
// deliberate exception: they are content people search for.
func TestSearchTextAgreesWithTheParser(t *testing.T) {
	pn := parse(t, "note.md", "head\n<!-- secretword\ntail\n")
	if got := searchText(pn); strings.Contains(got, "secretword") || strings.Contains(got, "tail") {
		t.Errorf("searchText = %q, still indexes text the graph treats as hidden", got)
	}
	pn = parse(t, "note.md", "run `mesh index --workers 4` now <!-- hidden-note -->\n")
	got := searchText(pn)
	if !strings.Contains(got, "workers") {
		t.Errorf("searchText = %q, dropped code that people search for", got)
	}
	if strings.Contains(got, "hidden-note") {
		t.Errorf("searchText = %q, kept comment text", got)
	}
}

// TestScaffoldedNoteHasNoWikilinks runs the real CreateNote path for every note type,
// then the real migrate path on top of it. Mesh writes these skeletons itself, so a
// link-shaped placeholder in one is a broken link the product manufactures for the
// author, on every single note it creates. Migrating is in the same binary and used to
// undo the whole fix: it lifted the commented-out placeholder into frontmatter
// related:, which is authoritative, so the link came back as a real edge.
func TestScaffoldedNoteHasNoWikilinks(t *testing.T) {
	types := []vault.NoteType{
		vault.TypeNote, vault.TypePostMortem, vault.TypeDecision,
		vault.TypeGotcha, vault.TypeEntity, vault.TypeConcept, vault.TypeMap,
	}
	for _, nt := range types {
		t.Run(string(nt), func(t *testing.T) {
			root := t.TempDir()
			res, err := vault.CreateNote(root, vault.NewNoteSpec{Type: nt, Title: "Fresh " + string(nt), Status: "draft"})
			if err != nil {
				t.Fatalf("CreateNote(%s): %v", nt, err)
			}
			for _, stage := range []string{"scaffolded", "migrated"} {
				if stage == "migrated" {
					if _, err := vault.MigrateFile(root, res.Path, false); err != nil {
						t.Fatalf("MigrateFile(%s): %v", res.Path, err)
					}
				}
				pn, err := ParseFile(res.Path)
				if err != nil {
					t.Fatalf("ParseFile(%s): %v", res.Path, err)
				}
				if got := linkTargets(pn); len(got) != 0 {
					t.Errorf("%s %s note ships wikilinks %v", stage, nt, got)
				}
				if got := pn.FM.Related; len(got) != 0 {
					t.Errorf("%s %s note ships related: %v", stage, nt, got)
				}
				// The graph is where it hurt: every scaffolded note raised a broken-link
				// issue against a note nobody had written, in lint, doctor and health.
				_, issues := BuildGraph([]*ParsedNote{pn})
				for _, is := range issues {
					if is.Kind == "broken-link" || is.Kind == "unterminated-comment" {
						t.Errorf("%s %s note raises %s: %s", stage, nt, is.Kind, is.Msg)
					}
				}
			}
			// Nothing was lifted, so the frontmatter must not have grown a related: key at
			// all (the body still mentions the word inside its guidance comment).
			out, err := os.ReadFile(res.Path)
			if err != nil {
				t.Fatal(err)
			}
			fmText, _, _ := vault.SplitFrontmatter(string(out))
			if strings.Contains(fmText, "related:") {
				t.Errorf("%s note gained a related: block from migrate:\n%s", nt, out)
			}
		})
	}
}
