// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
)

// guardDeadline bounds one refusal. A guarded command returns in microseconds; the
// bound exists for the UNguarded case, and it is not decoration. `mesh watch` and
// `mesh mcp` are daemons: with the guard removed they do not fail on the phantom
// vault, they successfully START on it and block forever. Measured while ablating this
// fix, an unbounded version of this test hung the whole package past ten minutes
// instead of failing. A regression guard that hangs teaches the next person to
// disable it, so a command that does not refuse has to lose here, in seconds, saying
// why.
const guardDeadline = 20 * time.Second

// runRootExpectingError executes the real command tree and returns the error and the
// output. It goes through rootCmd() rather than calling the RunE closures directly
// because the defect is reachable only the way a user reaches it: by typing a --vault
// or a positional [vault] that is not there.
//
// The command runs on its own goroutine so a daemon that failed to refuse cannot take
// the suite with it. That goroutine is deliberately left running on the timeout path:
// there is no way to stop a `mesh watch` loop from out here, the test binary is going
// to fail and exit anyway, and hanging on to it would defeat the point of the bound.
func runRootExpectingError(t *testing.T, args ...string) (error, string) {
	t.Helper()
	var buf bytes.Buffer
	root := rootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)

	type outcome struct {
		err error
		out string
	}
	done := make(chan outcome, 1)
	go func() { done <- outcome{root.Execute(), buf.String()} }()

	timer := time.NewTimer(guardDeadline)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.err, res.out
	case <-timer.C:
		t.Fatalf("`mesh %s` did not return within %s. It was pointed at a vault that does not exist, "+
			"so it should have refused at once; instead it is still running, which means it accepted the path "+
			"and is now serving a vault it created itself.", strings.Join(args, " "), guardDeadline)
		return nil, ""
	}
}

// TestVaultTakingWriteCommandsRefuseAPathThatIsNotThere is the regression guard for the
// phantom vault.
//
// Measured before the fix, from an empty directory:
//
//	$ mesh new decision "Typo test" --vault ./nonexistent-typo --do a --dont b --why c
//	created nonexistent-typo/decisions/typo-test.md
//	  id: typo-test   when: 2026-08-13
//	  lint: clean
//	$ echo $?
//	0
//
// A whole vault built from a typo, with no .mesh, no index, and the user's note filed
// somewhere they will never look. os.MkdirAll invents every missing parent and nobody
// had asked whether the vault existed.
//
// `mesh new` was the loudest case but not the only one: index.Open MkdirAlls
// <root>/.mesh, so `mesh watch`, `mesh mcp --vault`, `mesh sync` and all three `mesh
// code` subcommands each minted their own empty vault from the same typo. Six twins of
// one missing question. They are covered together here on purpose, because fixing one
// of a set and calling it done is how this defect class survives a fix pass.
//
// The assertion is deliberately TWO things per command: a non-nil error that names the
// path, AND nothing on disk afterwards. Exit code alone would have passed for `mesh
// code reindex`, which failed with "no code roots" while having already created
// ghost/.mesh on the way there.
func TestVaultTakingWriteCommandsRefuseAPathThatIsNotThere(t *testing.T) {
	cases := []struct {
		name string
		args func(ghost string) []string
	}{
		{"new", func(g string) []string {
			return []string{"new", "decision", "Typo test", "--vault", g, "--status", "draft", "--summary", "An authored draft summary", "--section", "question=The fixture checks a vault root."}
		}},
		{"watch", func(g string) []string { return []string{"watch", g} }},
		{"mcp", func(g string) []string { return []string{"mcp", "--vault", g} }},
		{"sync", func(g string) []string { return []string{"sync", g} }},
		{"code reindex", func(g string) []string { return []string{"code", "reindex", g} }},
		{"code search", func(g string) []string { return []string{"code", "search", "foo", "--vault", g} }},
		{"code context", func(g string) []string { return []string{"code", "context", "foo", "--vault", g} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh parent per case, so "nothing was created" is unambiguous.
			ghost := filepath.Join(t.TempDir(), "nonexistent-typo")
			err, out := runRootExpectingError(t, tc.args(ghost)...)
			if err == nil {
				t.Fatalf("`mesh %s` accepted a vault path that does not exist and returned nil; "+
					"a typo in --vault must be an error naming the path, not a new vault. output: %q", tc.name, out)
			}
			if !errors.Is(err, vault.ErrNoVault) {
				t.Errorf("`mesh %s` refused with %v, which is not vault.ErrNoVault; "+
					"every entry point has to refuse for the same reason so the message stays one message", tc.name, err)
			}
			if !strings.Contains(err.Error(), ghost) {
				t.Errorf("`mesh %s` refused with %q, which does not name the path %q the user mistyped", tc.name, err, ghost)
			}
			if _, statErr := os.Stat(ghost); statErr == nil {
				t.Fatalf("`mesh %s` created %s even though it refused; the refusal has to happen BEFORE anything reaches the filesystem", tc.name, ghost)
			}
		})
	}
}

// TestVaultTakingCommandsStillAcceptAVaultWithNoMeshDir pins the OTHER half of the
// rule, the half that decides how much the guard is allowed to refuse.
//
// The rule is "an existing directory", not "a directory containing .mesh". A folder of
// markdown that has never been indexed is a supported, documented state (SPEC section
// 12 makes `mesh migrate <vault>` then `mesh index <vault>` over exactly such a folder
// the v1 gate, and .mesh is what that pass creates). A guard tightened to require .mesh
// would close the typo AND the documented first run, which is the over-reach this test
// exists to catch on the next pass.
func TestVaultTakingCommandsStillAcceptAVaultWithNoMeshDir(t *testing.T) {
	root := t.TempDir()
	if _, err := os.Stat(filepath.Join(root, ".mesh")); !os.IsNotExist(err) {
		t.Fatalf("fixture is wrong: %s should have no .mesh", root)
	}
	var buf bytes.Buffer
	cmd := rootCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"new", "finding", "Real note", "--vault", root, "--status", "draft", "--summary", "An authored draft summary", "--section", "question=The fixture checks a vault root."})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("`mesh new` refused an existing vault directory that simply has no .mesh yet: %v (output %q). "+
			"The guard is meant to refuse a path that is NOT THERE, not a vault that has not been indexed.", err, buf.String())
	}
	written := filepath.Join(root, "inbox", "real-note.md")
	if _, err := os.Stat(written); err != nil {
		t.Fatalf("`mesh new` reported success but %s is not there: %v", written, err)
	}
}

// TestInitStillCreatesTheVault guards the exemption. `mesh init` is one of the two
// commands whose job IS to create a vault (the other is `mesh join`, which the README
// documents as cloning into a fresh directory). Applying the guard everywhere without
// carving those out would have replaced a silent-creation bug with a bootstrap that
// cannot bootstrap.
func TestInitStillCreatesTheVault(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "brand-new-vault")
	var buf bytes.Buffer
	cmd := rootCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"init", fresh})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("`mesh init %s` failed: %v (output %q)", fresh, err, buf.String())
	}
	for _, want := range []string{filepath.Join(fresh, "index.md"), filepath.Join(fresh, ".mesh")} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("`mesh init` did not create %s: %v", want, err)
		}
	}
}
