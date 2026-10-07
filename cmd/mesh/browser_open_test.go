// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/bright-interaction/mesh/internal/syncproto"
	"github.com/bright-interaction/mesh/pkg/meshclient"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const cliBrowserCode = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func cliBrowserFixture() meshclient.BrowserLogin {
	return meshclient.BrowserLogin{URL: "https://fixture.example/team#signin=mesh_login_" + cliBrowserCode, ExpiresAt: time.Now().Add(time.Minute).Unix(), ClientID: 7, User: "fixture user", Role: "member"}
}
func TestBrowserCommandsDefaultReceiptAndExplicitPrintOnly(t *testing.T) {
	for _, destination := range []string{"vault", "team"} {
		for _, printURL := range []bool{false, true} {
			t.Run(destination+map[bool]string{false: "-default", true: "-print"}[printURL], func(t *testing.T) {
				var prepared, opened int
				deps := browserCommandDeps{prepare: func(_ context.Context, dir, dest string) (meshclient.BrowserLogin, error) {
					prepared++
					if dir != "vault with spaces" || dest != destination {
						t.Error("selected vault/destination lost")
					}
					return cliBrowserFixture(), nil
				}, launch: func(_ context.Context, u string) error {
					opened++
					if u != cliBrowserFixture().URL {
						t.Error("wrong handoff")
					}
					return nil
				}}
				cmd := browserCmdWith(destination, deps)
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				args := []string{"vault with spaces"}
				if printURL {
					args = append(args, "--print-url")
				}
				cmd.SetArgs(args)
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
				if prepared != 1 {
					t.Fatal("handoff count")
				}
				if printURL {
					if opened != 0 || strings.TrimSpace(out.String()) != cliBrowserFixture().URL {
						t.Fatal("manual fallback")
					}
				} else {
					if opened != 1 || strings.Contains(out.String(), cliBrowserCode) || !strings.Contains(out.String(), "client 7") {
						t.Fatal("default receipt privacy")
					}
				}
			})
		}
	}
}
func TestBrowserOpenErrorNeverEchoesCapability(t *testing.T) {
	deps := browserCommandDeps{prepare: func(context.Context, string, string) (meshclient.BrowserLogin, error) {
		return cliBrowserFixture(), nil
	}, launch: func(context.Context, string) error { return errors.New("opener echoed " + cliBrowserFixture().URL) }}
	cmd := browserCmdWith("vault", deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"vault with spaces"})
	err := cmd.Execute()
	if err == nil || strings.Contains(err.Error(), cliBrowserCode) || strings.Contains(out.String(), cliBrowserCode) || !strings.Contains(err.Error(), "--print-url") {
		t.Fatal("opener error privacy/fallback")
	}
}
func TestJoinBrowserPolicyPreservesCompletionAndSkipsHeadlessHandoffs(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interactive bool
		flags       []string
		opens       int
		fail        bool
		indexFail   bool
		invalid     bool
	}{{"headless", false, nil, 0, false, false, false}, {"interactive", true, nil, 1, false, false, false}, {"explicit-open", false, []string{"--open"}, 1, false, false, false}, {"explicit-no-open", true, []string{"--no-open"}, 0, false, false, false}, {"open-fails", true, nil, 1, true, false, false}, {"index-fails", true, nil, 0, false, true, false}, {"conflicting", true, []string{"--open", "--no-open"}, 0, false, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var joined, prepared, opened int
			indexErr := errors.New("synthetic index stale")
			deps := joinBrowserDeps{join: func(string, string, string) (meshclient.Summary, error) {
				joined++
				return meshclient.Summary{Head: "fixture-head", Pulled: 2}, nil
			}, reconcile: func(context.Context, string, string) error {
				if tc.indexFail {
					return indexErr
				}
				return nil
			}, interactive: func() bool { return tc.interactive }, browser: browserCommandDeps{prepare: func(context.Context, string, string) (meshclient.BrowserLogin, error) {
				prepared++
				return cliBrowserFixture(), nil
			}, launch: func(context.Context, string) error {
				opened++
				if tc.fail {
					return errors.New(cliBrowserFixture().URL)
				}
				return nil
			}}}
			out, err := capture(t, func() error {
				cmd := joinCmdWithBrowser(deps)
				args := append([]string{"https://fixture.example", "synthetic-invite", "vault with spaces"}, tc.flags...)
				cmd.SetArgs(args)
				return cmd.Execute()
			})
			if tc.invalid {
				if err == nil || joined != 0 {
					t.Fatal("invalid flags redeemed invite")
				}
				return
			}
			if joined != 1 || prepared != tc.opens || opened != tc.opens {
				t.Fatalf("join%d prepare%d opened%d", joined, prepared, opened)
			}
			if !strings.Contains(out, "joined and cloned") || !strings.Contains(out, "synced:") {
				t.Fatal("lost durable receipt")
			}
			if tc.indexFail {
				if !errors.Is(err, indexErr) || !strings.Contains(out, "index stale") {
					t.Fatal("lost index failure semantics")
				}
			} else if tc.fail {
				if err == nil || !strings.Contains(err.Error(), "join and sync are complete") || !strings.Contains(err.Error(), "Retry mesh open") || strings.Contains(err.Error(), cliBrowserCode) || strings.Contains(out, cliBrowserCode) {
					t.Fatal("handoff failure suggests repeating join or exposes code")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestJoinBrowserFailureLeavesActualPrivateJoinIntact(t *testing.T) {
	dir := t.TempDir()
	srv := fakeHub(t, syncproto.SyncResponse{HeadSHA: "abcdef1234567890", FullReconcile: true})
	var joins int
	original := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/join" {
			joins++
		}
		original.ServeHTTP(w, r)
	})
	deps := defaultJoinBrowserDeps()
	deps.interactive = func() bool { return true }
	deps.reconcile = func(context.Context, string, string) error { return nil }
	deps.browser.launch = func(context.Context, string) error { t.Fatal("HTTP origin reached opener"); return nil }
	out, err := capture(t, func() error {
		cmd := joinCmdWithBrowser(deps)
		cmd.SetArgs([]string{srv.URL, "synthetic-invite", dir})
		return cmd.Execute()
	})
	if err == nil || joins != 1 || !strings.Contains(out, "joined and cloned") || !strings.Contains(err.Error(), "join and sync are complete") {
		t.Fatal("actual join durability receipt")
	}
	credentials := filepath.Join(dir, ".mesh", "credentials")
	state := filepath.Join(dir, ".mesh", "sync.json")
	beforeCreds, e := os.ReadFile(credentials)
	if e != nil {
		t.Fatal(e)
	}
	beforeState, e := os.ReadFile(state)
	if e != nil {
		t.Fatal(e)
	}
	cmd := browserCmd("vault")
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{dir})
	if cmd.Execute() == nil {
		t.Fatal("insecure hub accepted")
	}
	afterCreds, _ := os.ReadFile(credentials)
	afterState, _ := os.ReadFile(state)
	if joins != 1 || !bytes.Equal(beforeCreds, afterCreds) || !bytes.Equal(beforeState, afterState) {
		t.Fatal("open retried join or rewrote private pair")
	}
	info, _ := os.Stat(credentials)
	if info.Mode().Perm() != 0600 {
		t.Fatal("credential mode")
	}
}
func TestBrowserNativeArgvAndEnvironmentRemainPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable fixture; Windows native API cross-compiled separately")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "fake browser with spaces")
	script := []byte("#!/bin/sh\nprintf '%s\\n' \"$#\" \"$1\" \"${MESH_SYNTHETIC_SECRET-unset}\" > \"$XDG_RUNTIME_DIR/argv\"\n")
	if os.WriteFile(exe, script, 0700) != nil {
		t.Fatal("fixture")
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("MESH_SYNTHETIC_SECRET", "SYNTHETIC_MUST_NOT_REACH_OPENER")
	argument := "https://example.test/a path;$(touch should-not-execute)"
	if err := runBrowserCommand(context.Background(), exe, []string{argument}); err != nil {
		t.Fatal(err)
	}
	result, e := os.ReadFile(filepath.Join(dir, "argv"))
	if e != nil {
		t.Fatal(e)
	}
	if string(result) != "1\n"+argument+"\nunset\n" {
		t.Fatal("argv split, shell evaluation or secret environment inheritance")
	}
	for _, platform := range []string{"linux", "darwin"} {
		path, args, e := unixBrowserCommand(platform, cliBrowserFixture().URL)
		if e != nil || !filepath.IsAbs(path) || len(args) != 1 || args[0] != cliBrowserFixture().URL {
			t.Fatal("native browser argv")
		}
	}
}
func TestBrowserCommandsRegisteredAndCompletionDoesNotReadCredentials(t *testing.T) {
	root := rootCmd()
	for _, name := range []string{"open", "team"} {
		cmd, _, e := root.Find([]string{name})
		if e != nil || cmd.Name() != name || cmd.Flags().Lookup("print-url") == nil {
			t.Fatal("missing command")
		}
	}
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"__complete", "open", "--"})
	if e := root.Execute(); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "print-url") {
		t.Fatal("completion missing manual fallback")
	}
}
