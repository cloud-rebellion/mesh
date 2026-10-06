// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"errors"
	"strings"
	"testing"
)

// TestSlugifyFoldsDiacritics pins the fix for the ASCII-only slug. The filter kept
// only [a-z0-9], so every Scandinavian letter was DELETED rather than folded: the
// heading "Åtgärder" slugged to "tg-rder", an anchor no agent guesses, and a
// Swedish note title minted an id nobody can type. This vault is Swedish-heavy, so
// that made whole sections unreachable in practice.
func TestSlugifyFoldsDiacritics(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"swedish heading, the reported case", "Åtgärder", "atgarder"},
		{"swedish sentence heading", "Åtgärdsplan för deploy", "atgardsplan-for-deploy"},
		{"a-ring, a-umlaut, o-umlaut in one word", "åäö", "aao"},
		{"norwegian and danish o-slash and ae", "Søren Æble", "soren-aeble"},
		{"french acute and cedilla", "Résumé français", "resume-francais"},
		{"german umlaut and sharp s", "Straße über", "strasse-uber"},
		{"icelandic eth and thorn", "Ðag þing", "dag-thing"},
		{"spanish tilde n", "Mañana", "manana"},
		{"folded letter still separates on punctuation", "Åtgärder / deploy", "atgarder-deploy"},
		{"leading diacritic does not produce a leading dash", "Åke", "ake"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Slugify(tc.in); got != tc.want {
				t.Fatalf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSlugifyIsUnchangedForASCII is the compatibility half of the fold. Folding runs
// BEFORE the [a-z0-9] filter and only ever fires on a non-ASCII rune, so every id and
// heading anchor Mesh has already minted (all ASCII, because the old function could not
// emit anything else) must still slug to exactly itself. If this ever fails, existing
// note ids and existing anchors have moved and the vault is retroactively wrong.
func TestSlugifyIsUnchangedForASCII(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain title", "Deploy system is CI", "deploy-system-is-ci"},
		{"already a slug", "mesh-sqlite-busy-solved", "mesh-sqlite-busy-solved"},
		{"dates and digits survive", "Dockyard audit 2026-07-16", "dockyard-audit-2026-07-16"},
		{"punctuation runs collapse to one dash", "git core.bare=true breaks EVERY go build", "git-core-bare-true-breaks-every-go-build"},
		{"trailing punctuation is trimmed", "Why not?", "why-not"},
		{"the legacy shape of a folded id still slugs to itself", "tg-rder", "tg-rder"},
		{"empty stays empty", "", ""},
		{"punctuation only stays empty", "---", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Slugify(tc.in); got != tc.want {
				t.Fatalf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSlugifyKeepsNonLatinHeadingsAddressable(t *testing.T) {
	tests := map[string]string{
		"日本語":        "日本語",
		"Привет мир": "привет-мир",
	}
	for in, want := range tests {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCreateNoteRefusesOverlongTitle pins the fix for the leak on mesh_append_note's
// error path. A title that slugs past NAME_MAX used to reach os.OpenFile and come back
// as "open /srv/hub/vault/gotchas/<slug>.md: file name too long", handing the caller
// the server's ABSOLUTE vault path, which is exactly what the success path relativizes
// away. It is refused up front now, with a message that says what to do and names no
// path at all. The at-the-limit case is here too: the longest real filename in the
// reference vault is a 247-byte base, so this must refuse only what the filesystem
// would refuse anyway.
func TestCreateNoteRefusesOverlongTitle(t *testing.T) {
	cases := []struct {
		name    string
		slugLen int
		wantErr bool
	}{
		{"at the limit, still written", maxSlugLen, false},
		{"one over the limit", maxSlugLen + 1, true},
		{"far over the limit", 400, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			res, err := CreateNote(root, completeFixtureSpec(NewNoteSpec{Type: TypeGotcha, Title: strings.Repeat("b", tc.slugLen)}))
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("a %d-character slug must still be writable: %v", tc.slugLen, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a %d-character slug was accepted (wrote %s)", tc.slugLen, res.Path)
			}
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("over-long title must be an ErrInvalidSpec so the MCP layer can echo it: %v", err)
			}
			if !strings.Contains(err.Error(), "title too long") {
				t.Fatalf("error does not say what is wrong: %v", err)
			}
			if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "/") {
				t.Fatalf("error leaks a filesystem path: %v", err)
			}
		})
	}
}

// TestCreateNoteMintsGuessableSwedishID is the end-to-end of the fold: the write-back
// tool has to produce an id an agent can retrieve later. Before the fold this title
// produced "tg-rdsplan-f-r-deploy".
func TestCreateNoteMintsGuessableSwedishID(t *testing.T) {
	root := t.TempDir()
	res, err := CreateNote(root, completeFixtureSpec(NewNoteSpec{Type: TypeGotcha, Title: "Åtgärdsplan för deploy"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "atgardsplan-for-deploy" {
		t.Fatalf("id = %q, want atgardsplan-for-deploy", res.ID)
	}
}

// TestCreateNoteValidationErrorsAreTagged: every caller-input error CreateNote raises
// must carry ErrInvalidSpec, because that sentinel is what the MCP layer uses to decide
// a message is safe to hand back verbatim. An untagged one gets scrubbed to a generic
// string, so the agent loses an actionable message it should have had.
func TestCreateNoteValidationErrorsAreTagged(t *testing.T) {
	cases := []struct {
		name string
		spec NewNoteSpec
	}{
		{"invalid type", completeFixtureSpec(NewNoteSpec{Type: NoteType("nonsense"), Title: "x"})},
		{"empty title", completeFixtureSpec(NewNoteSpec{Type: TypeGotcha, Title: "   "})},
		{"over-long title", completeFixtureSpec(NewNoteSpec{Type: TypeGotcha, Title: strings.Repeat("c", 400)})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateNote(t.TempDir(), tc.spec)
			if err == nil {
				t.Fatal("want an error")
			}
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("not tagged ErrInvalidSpec: %v", err)
			}
		})
	}
}
