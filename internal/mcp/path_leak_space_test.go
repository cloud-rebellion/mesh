// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

// A vault root WITH A SPACE in it, which is the real deployment shape here: this estate's
// own vault lives under "Automation HQ". Every path-leak test in this file uses one,
// because the whitespace-tokenizing scrubber passed every space-free fixture while
// leaking most of the layout for a root like this one.
func spacedRootServer(t *testing.T) *Server { return spacedRootServerMode(t, false) }

// spacedRootServerWritable builds the server the way the HUB runs it: owning its own
// index, so write-back still reconciles in-process and a broken vault walk still surfaces
// a filesystem error through index_error. The scrub tests need that, because a read-only
// server never walks the vault (its owner does), so the error they exist to scrub can no
// longer arise there. Same coverage, pointed at where the code path now lives.
func spacedRootServerWritable(t *testing.T) *Server { return spacedRootServerMode(t, true) }

func spacedRootServerMode(t *testing.T, writable bool) *Server {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Automation HQ", "vault")
	if err := os.MkdirAll(filepath.Join(dir, "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "decisions", "seed.md"),
		[]byte("---\nid: seed\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: seed the index\n---\n# Seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	newSrv := NewServer
	if writable {
		newSrv = func(root string) (*Server, error) { return NewServerAt(root, filepath.Join(root, ".mesh")) }
	}
	srv, err := newSrv(dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.WaitReady(); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// TestScrubPathsWithARootThatContainsASpace. scrubPaths only ever split the message on
// whitespace, so for a root like "/Users/x/Desktop/Automation HQ/vault" it rewrote the
// first token to "<path>/Automation" and left "HQ/vault/gotchas/note.md" standing: the
// interesting half of the layout shipped anyway. The known-root pass fixes that by
// matching the root as a substring and scanning on from its END.
func TestScrubPathsWithARootThatContainsASpace(t *testing.T) {
	// Deliberately NOT a home-shaped path. This file ships to the public mirror,
	// whose blob-sanity gate refuses anything matching /(Users|home)/<name>/ in
	// published blob content, and it refuses on the SHAPE, not on the name, so
	// swapping the maintainer's real home for a neutral placeholder under
	// /Users/ trips it just the same. Naming a concrete offending path here
	// would re-trip the gate from inside the comment explaining it. The property
	// under test is only "the root contains a space", so a path with a space and
	// no home prefix satisfies both the test and the gate.
	const root = "/srv/data/Automation HQ/vault"
	cases := []struct {
		name, in, want string
		roots          []string
	}{
		{
			name:  "a filesystem error under a root with a space",
			roots: []string{root},
			in:    "open " + root + "/gotchas/note.md: file name too long",
			want:  "open <path>/note.md: file name too long",
		},
		{
			name:  "the root named on its own",
			roots: []string{root},
			in:    "open " + root + ": permission denied",
			want:  "open <path>: permission denied",
		},
		{
			// A trailing separator names the same directory, so it must not be allowed to
			// turn into "<path>/vault" and hand back the root's own last segment.
			name:  "the root with a trailing separator",
			roots: []string{root},
			in:    "mkdir " + root + "/: file exists",
			want:  "mkdir <path>: file exists",
		},
		{
			name:  "a quoted path",
			roots: []string{root},
			in:    `stat "` + root + `/notes/y.md": no such file`,
			want:  `stat "<path>/y.md": no such file`,
		},
		{
			name:  "both sides of a rename",
			roots: []string{root},
			in:    "rename " + root + "/a.md " + root + "/b.md: cross-device link",
			want:  "rename <path>/a.md <path>/b.md: cross-device link",
		},
		{
			name:  "a prefix this process was never told about still goes through the fallback",
			roots: []string{root},
			in:    "stat /private/tmp/vault2/notes/y.md: no such file",
			want:  "stat <path>/y.md: no such file",
		},
		{
			name:  "the resolved spelling of the root is scrubbed too",
			roots: []string{root, "/private" + root},
			in:    "open /private" + root + "/gotchas/note.md: permission denied",
			want:  "open <path>/note.md: permission denied",
		},
		{
			name:  "no root configured at all keeps the old behaviour",
			roots: nil,
			in:    "open /srv/hub/vault/gotchas/x.md: file name too long",
			want:  "open <path>/x.md: file name too long",
		},
		{
			name:  "a message with no path is left alone",
			roots: []string{root},
			in:    "title is required",
			want:  "title is required",
		},
		{
			name:  "an empty root is refused rather than allowed to swallow the message",
			roots: []string{""},
			in:    "title is required",
			want:  "title is required",
		},
		{
			name:  "a root of / is refused for the same reason",
			roots: []string{"/"},
			in:    "invalid type \"nonsense\"",
			want:  "invalid type \"nonsense\"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubPaths(tc.in, tc.roots...)
			if got != tc.want {
				t.Fatalf("scrubPaths(%q, %q) = %q, want %q", tc.in, tc.roots, got, tc.want)
			}
			// The regression itself: no fragment of a root with a space may survive, and
			// "HQ/vault" is precisely the tail the whitespace scan used to leave behind.
			for _, leak := range []string{"Automation", "HQ/vault", "tom.isgren"} {
				if strings.Contains(got, leak) {
					t.Fatalf("scrubbed message still carries %q from the server's vault root: %q", leak, got)
				}
			}
		})
	}
}

// TestAppendNoteDoesNotLeakTheVaultRootOnEitherFailure. Two failures inside one write,
// both of which reached the agent with an absolute path in it. The index-staleness one is
// the sibling that survived the last fix: it sits directly beside a "path" field that IS
// relativized, so it read as already handled.
func TestAppendNoteDoesNotLeakTheVaultRootOnEitherFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("both fixtures make a directory unreadable/unwritable, which root ignores")
	}
	cases := []struct {
		name string
		// break sets up the failure and returns the base name the message should keep.
		breakIt func(t *testing.T, s *Server) string
		// wantRPCError is true when the write itself must fail, false when the write
		// succeeds and only the index refresh fails.
		wantRPCError bool
	}{
		{
			name: "the note cannot be written",
			breakIt: func(t *testing.T, s *Server) string {
				dir := filepath.Join(s.vaultRoot, "gotchas")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0o555); err != nil { // readable, not writable
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
				return "leak-check.md"
			},
			wantRPCError: true,
		},
		{
			name: "the note is written but the index refresh fails",
			breakIt: func(t *testing.T, s *Server) string {
				// Sabotage the exact file after its durable creation. The write-back
				// path is targeted now, so an unrelated unreadable directory correctly
				// cannot make this note's publication fail anymore.
				s.notePublisher = func(ctx context.Context, spec vault.NewNoteSpec) (*vault.CreateResult, error) {
					res, err := vault.CreateNoteContext(ctx, s.vaultRoot, spec)
					if err == nil {
						if chmodErr := os.Chmod(res.Path, 0o000); chmodErr != nil {
							t.Fatal(chmodErr)
						}
						t.Cleanup(func() { _ = os.Chmod(res.Path, 0o644) })
					}
					return res, err
				}
				return "leak-check.md"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := spacedRootServerWritable(t)
			keep := tc.breakIt(t, s)
			raw, _ := json.Marshal(map[string]any{
				"type": "gotcha", "title": "Leak check", "summary": "Verify publication errors do not disclose the vault location.", "sections": fixtureSections("gotcha")})
			out, rerr := s.toolWrite(WithLocalOperator(context.Background()), raw, "gotcha")

			var msg string
			switch {
			case tc.wantRPCError:
				if rerr == nil {
					t.Fatalf("the write should have failed on an unwritable note directory, got %#v", out)
				}
				msg = rerr.Message
			default:
				if rerr != nil {
					t.Fatalf("a failed index refresh must not fail the write: %s", rerr.Message)
				}
				res, ok := out.(map[string]any)
				if !ok {
					t.Fatalf("tool result is not an object: %#v", out)
				}
				w := toolText(t, res)
				if w["index_stale"] != true {
					t.Fatalf("index_stale is not set, so the scrub cannot be what is being tested: %#v", w)
				}
				msg, _ = w["index_error"].(string)
				if msg == "" {
					t.Fatalf("no index_error in the response: %#v", w)
				}
			}

			assertNoVaultRoot(t, msg, s.vaultRoot)
			// Scrubbed, not blanked: the base name is the part that makes the message
			// actionable, and deleting it would be a different bug.
			if !strings.Contains(msg, keep) {
				t.Fatalf("message lost the useful part (%q): %q", keep, msg)
			}
		})
	}
}

// assertNoVaultRoot fails when msg carries the server's vault root or any recognisable
// piece of it. The piece-wise check is the point: the old scrubber removed the first
// whitespace-delimited chunk of a spaced root and left the rest, which passed a naive
// strings.Contains(msg, vaultRoot) test.
func assertNoVaultRoot(t *testing.T, msg, vaultRoot string) {
	t.Helper()
	if strings.Contains(msg, vaultRoot) {
		t.Fatalf("message carries the whole vault root: %q", msg)
	}
	for _, seg := range strings.Split(vaultRoot, string(filepath.Separator)) {
		if len(seg) < 4 { // short segments ("T", "001") collide with ordinary prose
			continue
		}
		if strings.Contains(msg, seg) {
			t.Fatalf("message carries the vault-root segment %q, so the layout leaked anyway: %q", seg, msg)
		}
	}
}
