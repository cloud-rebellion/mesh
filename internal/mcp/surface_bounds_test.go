// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bulkVault builds a vault of n notes whose bodies all match one query, so a test can
// see what an unbounded tool would actually hand back.
func bulkVault(t *testing.T, n int) *Server {
	t.Helper()
	files := map[string]string{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("bulk-%04d", i)
		files["notes/"+id+".md"] = "---\nid: " + id + "\ntype: note\nwhen: 2026-01-01\n---\n# " + id + "\ndelta corpus filler\n"
	}
	return serverWithFiles(t, files)
}

// ---- mesh_changed_since: a millisecond timestamp must not read as "nothing changed".

// TestChangedSinceRejectsFutureTimestamp pins the worse of the two traps on `since`.
// The parameter is UNIX SECONDS, most runtimes hand out milliseconds by default, and a
// millisecond stamp is a year-57578 timestamp, so "SELECT ... WHERE mtime > ?" matched
// nothing and the tool answered {"changed":null}. That is not an error the caller can
// see: the agent reads it as "nothing changed since I left" and carries on against
// stale context. Refuse it, and name the likely cause so the fix is one edit away.
func TestChangedSinceRejectsFutureTimestamp(t *testing.T) {
	s := bulkVault(t, 3)
	now := time.Now().Unix()

	cases := []struct {
		name      string
		since     int64
		wantErr   bool
		wantInMsg string
	}{
		{"milliseconds instead of seconds, the reported case", now * 1000, true, "MILLISECONDS"},
		{"far future timestamp", now + 86400*365, true, "in the future"},
		{"a minute of clock skew is tolerated", now + 60, false, ""},
		{"a normal resume timestamp", now - 3600, false, ""},
		{"zero means everything", 0, false, ""},
		{"negative is treated as zero", -1, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"since": tc.since})
			out, rerr := s.toolChangedSince(context.Background(), raw)
			if !tc.wantErr {
				if rerr != nil {
					t.Fatalf("since=%d must be accepted: %v", tc.since, rerr.Message)
				}
				return
			}
			if rerr == nil {
				t.Fatalf("since=%d was accepted and answered %v; a future timestamp can only ever report nothing changed", tc.since, toolJSON(t, out))
			}
			if !strings.Contains(rerr.Message, tc.wantInMsg) {
				t.Fatalf("refusal does not name the cause (want %q): %s", tc.wantInMsg, rerr.Message)
			}
		})
	}
}

// TestChangedSinceIsBoundedAndFlagsTruncation pins the other half. ChangedSince is a
// bare "WHERE mtime > ? ORDER BY mtime DESC" with no LIMIT, so since=0 returned every
// note in the vault to the tool an agent calls on RESUME, when its context is already
// full. A cap alone is not enough: without an explicit truncated flag a capped delta is
// indistinguishable from a complete one, which is the same silent-wrong-answer shape as
// the millisecond case.
func TestChangedSinceIsBoundedAndFlagsTruncation(t *testing.T) {
	s := bulkVault(t, changedSinceLimitDefault+40)

	cases := []struct {
		name          string
		limit         any
		wantCount     int
		wantTruncated bool
	}{
		{"no limit falls back to the default cap", nil, changedSinceLimitDefault, true},
		{"an explicit small limit is honored", 5, 5, true},
		{"an unbounded limit is clamped to the max", 1000000, changedSinceLimitDefault + 40, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"since": 0}
			if tc.limit != nil {
				args["limit"] = tc.limit
			}
			raw, _ := json.Marshal(args)
			out, rerr := s.toolChangedSince(context.Background(), raw)
			if rerr != nil {
				t.Fatalf("changed_since: %v", rerr.Message)
			}
			res := toolJSON(t, out)
			if n := len(asList(res["changed"])); n != tc.wantCount {
				t.Fatalf("returned %d notes, want %d (res=%v)", n, tc.wantCount, res["count"])
			}
			if truncated, _ := res["truncated"].(bool); truncated != tc.wantTruncated {
				t.Fatalf("truncated = %v, want %v; a capped delta a caller cannot detect is a wrong answer", truncated, tc.wantTruncated)
			}
		})
	}
}

// TestChangedSineEmptyDeltaIsAnArray: an empty delta used to serialize as
// "changed": null, which reads as "no data" rather than "no changes". It is [] with an
// explicit count now, so "I have the complete delta and it is empty" is unambiguous.
func TestChangedSinceEmptyDeltaIsAnArray(t *testing.T) {
	s := bulkVault(t, 2)
	raw, _ := json.Marshal(map[string]any{"since": time.Now().Unix() + 60})
	out, rerr := s.toolChangedSince(context.Background(), raw)
	if rerr != nil {
		t.Fatalf("changed_since: %v", rerr.Message)
	}
	res := toolJSON(t, out)
	if res["changed"] == nil {
		t.Fatalf("empty delta serialized as null: %v", res)
	}
	if c, _ := res["count"].(float64); c != 0 {
		t.Fatalf("count = %v, want 0", res["count"])
	}
}

// ---- mesh_code_search: the caller-supplied limit had a floor but no ceiling.

// TestCodeSearchClampsLimit: index.SearchCode only FLOORS the limit (`if limit <= 0 {
// limit = 12 }`) and then over-fetches 4x for its test-code re-rank, so
// mesh_code_search{"limit":1000000} asked SQLite for four million rows and returned
// every matching symbol in the estate's code index straight into the caller's context.
func TestCodeSearchClampsLimit(t *testing.T) {
	s := newTestServer(t)
	root := t.TempDir()
	var src strings.Builder
	src.WriteString("package x\n")
	for i := 0; i < codeSearchLimitMax+50; i++ {
		fmt.Fprintf(&src, "\nfunc ClampableWidget%03d() {}\n", i)
	}
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte(src.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	seedCodeIndex(t, s.vaultRoot, root)

	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{"unbounded limit is clamped", 1000000, codeSearchLimitMax},
		{"omitted limit keeps the old floor", 0, codeSearchLimitDefault},
		{"negative limit falls back to the default", -5, codeSearchLimitDefault},
		{"a small explicit limit is honored", 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// "Clampable" and not "ClampableWidget": the code index splits identifiers, so
			// the whole camel-case name is not itself a token. Verified against the real
			// index: this query matches all 150 seeded symbols.
			raw, _ := json.Marshal(map[string]any{"query": "Clampable", "limit": tc.limit})
			out, rerr := s.toolCodeSearch(context.Background(), raw)
			if rerr != nil {
				t.Fatalf("code search: %v", rerr.Message)
			}
			if n := codeCount(t, out); n > tc.want {
				t.Fatalf("mesh_code_search returned %d symbols, want at most %d", n, tc.want)
			}
		})
	}
}

// ---- mesh_append_note: the error path leaked what the success path hides.

// TestAppendNoteErrorDoesNotLeakServerPath. The success path deliberately relativizes
// res.Path with a comment saying a hosted hub must not leak the server's absolute vault
// root; the error path 30 lines above returned err.Error() raw, so a too-long title came
// back as "open /private/tmp/.../vault2/gotchas/bbbb...: file name too long" and handed
// the caller the whole server layout. Titles in this vault already run near 200
// characters and the filename cliff is 255, so this is reachable by accident.
func TestAppendNoteErrorDoesNotLeakServerPath(t *testing.T) {
	s := newTestServer(t)
	raw, _ := json.Marshal(map[string]any{
		"type": "gotcha", "title": strings.Repeat("b", 400), "summary": "Test the filename size bound before publication.", "sections": fixtureSections("gotcha")})
	_, rerr := s.toolWrite(WithLocalOperator(context.Background()), raw, "gotcha")
	if rerr == nil {
		t.Fatal("a 400-character title must be refused")
	}
	if strings.Contains(rerr.Message, s.vaultRoot) {
		t.Fatalf("error leaks the server's vault root: %s", rerr.Message)
	}
	// Not even a scrubbed path fragment: the title is refused BEFORE anything touches the
	// filesystem, so there is no path in this message at all.
	if strings.Contains(rerr.Message, "/") {
		t.Fatalf("error still carries a path fragment, so the refusal is happening at the filesystem rather than up front: %s", rerr.Message)
	}
	if !strings.Contains(rerr.Message, "title too long") {
		t.Fatalf("error is not actionable, it should name the title length: %s", rerr.Message)
	}
}

// TestScrubPathsKeepsTheUsefulPart is the defense in depth behind the case above: any
// UNANTICIPATED filesystem error from CreateNote still reaches the caller, so its
// absolute paths are cut down to a base name. Matching on filepath.IsAbs rather than on
// the configured vault root is what makes it work when the kernel names a different
// prefix than the server was started with (/tmp resolving to /private/tmp on macOS).
func TestScrubPathsKeepsTheUsefulPart(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"open error with a trailing colon", "open /srv/hub/vault/gotchas/x.md: file name too long", "open <path>/x.md: file name too long"},
		{"a prefix the server never configured", "stat /private/tmp/vault2/notes/y.md: no such file", "stat <path>/y.md: no such file"},
		{"relative paths are left alone", "invalid type \"nonsense\"", "invalid type \"nonsense\""},
		{"no path at all", "title is required", "title is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scrubPaths(tc.in); got != tc.want {
				t.Fatalf("scrubPaths(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---- slug fold: reachable Swedish anchors WITHOUT moving anything that already exists.

// TestFetchResolvesFoldedAndLegacyAnchors. Heading anchors used to be ASCII-only, so
// "## Åtgärder" slugged to "tg-rder": listed nowhere a caller would look and guessable
// by nobody. It folds to "atgarder" now. The legacy slug must KEEP resolving, because
// the index stores one heading node per anchor and only rewrites it on reindex, so
// between a binary upgrade and the next reindex a caller can still be holding the old
// one. Both spellings hit the same section; a genuinely wrong anchor still misses.
func TestFetchResolvesFoldedAndLegacyAnchors(t *testing.T) {
	s := serverWithFiles(t, map[string]string{
		"notes/sv.md": "---\nid: sv\ntype: note\nwhen: 2026-01-01\n---\n" +
			"# Deploy\nintro\n\n## Åtgärder\nthe swedish section body\n\n## Nasta steg\nsomething else\n",
	})

	cases := []struct {
		name    string
		anchor  string
		wantHit bool
	}{
		{"the folded anchor an agent can now guess", "atgarder", true},
		{"the legacy ascii-only anchor still resolves", "tg-rder", true},
		{"an ascii heading is unaffected", "nasta-steg", true},
		{"a genuinely wrong anchor still misses", "atgarde", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"id": "sv", "anchor": tc.anchor})
			out, rerr := s.toolFetch(context.Background(), raw)
			if !tc.wantHit {
				if rerr == nil {
					t.Fatalf("anchor %q must miss, not silently return the whole note", tc.anchor)
				}
				return
			}
			if rerr != nil {
				t.Fatalf("anchor %q did not resolve: %s", tc.anchor, rerr.Message)
			}
			body := rawContent(t, out)
			if !strings.Contains(body, "the swedish section body") && tc.anchor != "nasta-steg" {
				t.Fatalf("anchor %q returned the wrong section:\n%s", tc.anchor, body)
			}
		})
	}

	// The miss message must list the anchors that DO work, in their current spelling.
	raw, _ := json.Marshal(map[string]any{"id": "sv", "anchor": "nope"})
	if _, rerr := s.toolFetch(context.Background(), raw); rerr == nil {
		t.Fatal("a missing anchor must be an error")
	} else if !strings.Contains(rerr.Message, "atgarder") {
		t.Fatalf("anchor miss does not offer the folded anchor: %s", rerr.Message)
	}
}

// TestExistingNoteIDsStillResolveAfterTheFold is the compatibility guarantee for the id
// path. Files on disk keep their names and their frontmatter ids; nothing re-derives an
// id from a title after creation (index.effectiveID reads FM.ID, and Store.NotePath is a
// lookup by that id). So a note already written under the OLD ascii-only slug, including
// one whose id could not be minted today, must still fetch by that exact id.
func TestExistingNoteIDsStillResolveAfterTheFold(t *testing.T) {
	s := serverWithFiles(t, map[string]string{
		// Written before the fold: a Swedish title whose id lost its diacritics.
		"gotchas/tg-rdsplan-f-r-deploy.md": "---\nid: tg-rdsplan-f-r-deploy\ntype: gotcha\nwhen: 2026-01-01\n" +
			"title: Åtgärdsplan för deploy\ndo: a\ndont: b\nwhy: c\n---\n# Plan\nlegacy body\n",
		// Written after the fold, same title, folded id.
		"gotchas/atgardsplan-for-deploy.md": "---\nid: atgardsplan-for-deploy\ntype: gotcha\nwhen: 2026-01-01\n" +
			"title: Åtgärdsplan för deploy\ndo: a\ndont: b\nwhy: c\n---\n# Plan\nfolded body\n",
	})
	for _, tc := range []struct{ id, want string }{
		{"tg-rdsplan-f-r-deploy", "legacy body"},
		{"atgardsplan-for-deploy", "folded body"},
	} {
		raw, _ := json.Marshal(map[string]any{"id": tc.id})
		out, rerr := s.toolFetch(context.Background(), raw)
		if rerr != nil {
			t.Fatalf("existing note id %q stopped resolving after the slug fold: %s", tc.id, rerr.Message)
		}
		if !strings.Contains(rawContent(t, out), tc.want) {
			t.Fatalf("id %q resolved to the wrong note", tc.id)
		}
	}
}

// rawContent pulls the plain text out of a rawText tool result (mesh_fetch returns the
// note markdown, not a JSON object).
func rawContent(t *testing.T, out any) string {
	t.Helper()
	m, _ := out.(map[string]any)
	content, ok := m["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content in result: %v", out)
	}
	s, _ := content[0]["text"].(string)
	return s
}
