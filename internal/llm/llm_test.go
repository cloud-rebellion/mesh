// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sanitizedEnv must strip mesh's own secrets + credential-shaped vars before they reach
// a third-party BYOAI subprocess, while keeping the child's own auth (ANTHROPIC_*) and
// the basics it needs to run (PATH/HOME).
func TestSanitizedEnvStripsSecrets(t *testing.T) {
	t.Setenv("MESH_UI_TOKEN", "hubsecret")
	t.Setenv("MESH_INGEST_GITHUB_TOKEN", "ghtok")
	t.Setenv("MESH_UI_COOKIE_SECRET", "cookiesecret")
	t.Setenv("GITHUB_TOKEN", "othertok")
	t.Setenv("STRIPE_SECRET_KEY", "sk_live")
	t.Setenv("ANTHROPIC_API_KEY", "childkey") // the child legitimately needs this
	t.Setenv("PATH", "/usr/bin")

	got := map[string]string{}
	for _, kv := range sanitizedEnv() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			got[kv[:i]] = kv[i+1:]
		}
	}
	for _, dropped := range []string{"MESH_UI_TOKEN", "MESH_INGEST_GITHUB_TOKEN", "MESH_UI_COOKIE_SECRET", "GITHUB_TOKEN", "STRIPE_SECRET_KEY"} {
		if _, ok := got[dropped]; ok {
			t.Errorf("sanitizedEnv leaked %s to the subprocess", dropped)
		}
	}
	if got["ANTHROPIC_API_KEY"] != "childkey" {
		t.Error("sanitizedEnv dropped ANTHROPIC_API_KEY; the child agent can no longer authenticate")
	}
	if got["PATH"] != "/usr/bin" {
		t.Error("sanitizedEnv dropped PATH; the child cannot run")
	}
}

func TestTruncationDetected(t *testing.T) {
	// Anthropic stop_reason=max_tokens -> ErrTruncated.
	ats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]}`))
	}))
	defer ats.Close()
	a := &anthropic{key: "k", model: "m", maxTok: 10, hc: &http.Client{Timeout: 5 * time.Second}, baseOverride: ats.URL}
	if _, err := a.Complete(context.Background(), "s", "u"); !errors.Is(err, ErrTruncated) {
		t.Fatalf("anthropic truncation: want ErrTruncated, got %v", err)
	}
	// OpenAI-compat finish_reason=length -> ErrTruncated.
	ots := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`))
	}))
	defer ots.Close()
	o := &openaiCompat{endpoint: ots.URL, model: "m", maxTok: 10, hc: &http.Client{Timeout: 5 * time.Second}}
	if _, err := o.Complete(context.Background(), "s", "u"); !errors.Is(err, ErrTruncated) {
		t.Fatalf("local truncation: want ErrTruncated, got %v", err)
	}
}

func TestAnthropicComplete(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "sekret" {
			t.Errorf("missing x-api-key")
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing anthropic-version")
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"merged "},{"type":"text","text":"note"}]}`))
	}))
	defer ts.Close()
	c := &anthropic{base: "https://unused", key: "sekret", model: "claude-sonnet-4-6", maxTok: 1024, hc: &http.Client{Timeout: 5 * time.Second}, baseOverride: ts.URL}
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if got != "merged note" {
		t.Fatalf("got %q", got)
	}
	if c.Describe() != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("describe = %q", c.Describe())
	}
}

// Temperature is sent only when configured (nil = provider default), so a reproducible
// run can pin it to 0 without changing the default behavior.
func TestAnthropicTemperature(t *testing.T) {
	var seen map[string]any
	handler := func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}
	// With temp set, the body carries "temperature".
	ts := httptest.NewServer(http.HandlerFunc(handler))
	defer ts.Close()
	zero := 0.0
	c := &anthropic{key: "k", model: "m", maxTok: 10, temp: &zero, hc: &http.Client{Timeout: 5 * time.Second}, baseOverride: ts.URL}
	if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if v, ok := seen["temperature"]; !ok || v.(float64) != 0.0 {
		t.Errorf("temperature not sent when set: %v", seen["temperature"])
	}
	// With temp nil, the body omits it (provider default).
	seen = nil
	ts2 := httptest.NewServer(http.HandlerFunc(handler))
	defer ts2.Close()
	c2 := &anthropic{key: "k", model: "m", maxTok: 10, hc: &http.Client{Timeout: 5 * time.Second}, baseOverride: ts2.URL}
	if _, err := c2.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if _, ok := seen["temperature"]; ok {
		t.Error("temperature must be omitted when unset")
	}
}

func TestAnthropicRateLimited(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()
	c := &anthropic{key: "k", model: "m", maxTok: 10, hc: &http.Client{Timeout: 5 * time.Second}, baseOverride: ts.URL}
	_, err := c.Complete(context.Background(), "s", "u")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited (errors.Is), got %v", err)
	}
}

func TestOpenAICompatComplete(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"local merged"}}]}`))
	}))
	defer ts.Close()
	c := &openaiCompat{endpoint: ts.URL, model: "llama", maxTok: 1024, hc: &http.Client{Timeout: 5 * time.Second}}
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if got != "local merged" {
		t.Fatalf("got %q", got)
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv("MESH_CURATOR_AGENT", "anthropic")
	t.Setenv("MESH_ANTHROPIC_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := NewFromEnv(); err == nil {
		t.Fatal("anthropic without a key should error")
	}
	t.Setenv("ANTHROPIC_API_KEY", "abc")
	c, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Describe(), "anthropic/") {
		t.Fatalf("describe = %q", c.Describe())
	}

	t.Setenv("MESH_CURATOR_AGENT", "local")
	t.Setenv("MESH_CURATOR_ENDPOINT", "")
	if _, err := NewFromEnv(); err == nil {
		t.Fatal("local without an endpoint should error")
	}
	t.Setenv("MESH_CURATOR_ENDPOINT", "http://localhost:11434/v1")
	t.Setenv("MESH_CURATOR_MODEL", "nomic")
	c, err = NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Describe(), "local/") {
		t.Fatalf("describe = %q", c.Describe())
	}

	// cli is the default when no agent is set, and reads MESH_CURATOR_CMD.
	t.Setenv("MESH_CURATOR_AGENT", "")
	t.Setenv("MESH_CURATOR_CMD", "myagent --print")
	t.Setenv("MESH_CURATOR_CLI_CONTRACT", "")
	if _, err = NewFromEnv(); err == nil {
		t.Fatal("uncontracted custom agent must not receive transcript data")
	}
	t.Setenv("MESH_CURATOR_CLI_CONTRACT", CompletionCLIContract)
	c, err = NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Describe() != "cli/myagent --print" {
		t.Fatalf("describe = %q", c.Describe())
	}
}

// generousTimeout is for the tests that are NOT about timing. They spawn a trivial /bin/sh
// script, so any deadline is incidental to what they assert, and a tight one only buys
// flakes: on a loaded CI runner or a laptop mid-build, forking a shell can take seconds.
// Both of these were observed failing with "context deadline exceeded" against a 5s cap on
// a freshly cloned mirror while other builds were running, then passing on a rerun. A test
// that fails for reasons unrelated to its subject teaches people to rerun until green.
// TestCLITimeout below deliberately keeps a 50ms deadline, because there the deadline IS
// the subject.
const generousTimeout = 60 * time.Second

// writeScript writes an executable shell script to a temp dir and returns its path.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCLIComplete(t *testing.T) {
	// A custom completion adapter receives distinct system/user JSON fields.
	c := &cliClient{argv: []string{writeScript(t, "cat")}, timeout: generousTimeout}
	out, err := c.Complete(context.Background(), "SYS-INSTRUCTIONS", "USER-PAYLOAD")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]string
	if json.Unmarshal([]byte(out), &request) != nil || request["protocol"] != CompletionCLIContract || request["system"] != "SYS-INSTRUCTIONS" || request["user"] != "USER-PAYLOAD" {
		t.Fatalf("separate completion fields did not reach the adapter: %q", out)
	}
}

func TestCLICompleteMarksMeshChild(t *testing.T) {
	t.Setenv(ChildEnv, "untrusted-parent-value")
	c := &cliClient{argv: []string{writeScript(t, `printf '%s' "$MESH_LLM_CHILD"`)}, timeout: generousTimeout}
	out, err := c.Complete(context.Background(), "system", "user")
	if err != nil {
		t.Fatal(err)
	}
	if out != "1" {
		t.Fatalf("%s = %q, want 1", ChildEnv, out)
	}
}

// The CLI error taxonomy decides whether the curator halts the whole pass (ErrAuth,
// no attempt charged) or charges THIS job an attempt. Only a CLI that could not start
// or that reports a login problem is global; anything that depends on the job's own
// input must be job-specific, else one poison job at the head of the pending queue
// starves every other conflict forever.
func TestCLIErrorTaxonomy(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-installed")
	cases := []struct {
		name    string
		argv    []string
		script  string
		wantErr bool // ErrAuth expected
	}{
		{name: "stderr names a login problem", script: "echo 'Not logged in. Run /login' >&2; exit 1", wantErr: true},
		{name: "stderr is a 401", script: "echo 'request failed: 401 Unauthorized' >&2; exit 1", wantErr: true},
		{name: "binary is missing", argv: []string{missing}, wantErr: true},
		{name: "oversize prompt rejected", script: "echo 'prompt is too long: 2100000 chars exceeds the limit' >&2; exit 1"},
		{name: "digits that merely look like a status", script: "echo 'prompt is 403000 tokens, over the limit at line 401' >&2; exit 1"},
		{name: "plain non-zero exit", script: "exit 2"},
		{name: "refusal with empty stdout", script: "exit 0"},
		{name: "stderr only, exit 0, no stdout", script: "echo 'warning: cache miss' >&2; exit 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv := tc.argv
			if argv == nil {
				argv = []string{writeScript(t, tc.script)}
			}
			c := &cliClient{argv: argv, timeout: generousTimeout}
			_, err := c.Complete(context.Background(), "s", "u")
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, ErrAuth); got != tc.wantErr {
				t.Fatalf("errors.Is(err, ErrAuth) = %v, want %v (err: %v)", got, tc.wantErr, err)
			}
		})
	}
}

func TestCLITimeout(t *testing.T) {
	c := &cliClient{argv: []string{writeScript(t, "sleep 5")}, timeout: 50 * time.Millisecond}
	_, err := c.Complete(context.Background(), "s", "u")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}
