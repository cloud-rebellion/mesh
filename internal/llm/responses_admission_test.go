// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// All numbers are deliberately synthetic accounting units, NOT market prices,
// real model limits, production budget advice or a provisioned API account.
func spendFixturePolicy() responsesSpendPolicy {
	now := time.Now().Unix()
	zero := int64(1)
	m := responsesSpendModel{Model: "unit-explicit-model", Tier: "default", ContextTokens: 2048, MaxOutputTokens: 512, MaxInputBytes: 512, InputTokensPerByte: 2, FramingTokens: 32, InputRate: 2, OutputRate: 3, FixedRate: &zero}
	return responsesSpendPolicy{Version: 2, BudgetID: "unit-budget", ScopeID: "unit-project", Currency: "USD", Budget: 1000000000, PeriodStart: now - 60, PeriodEnd: now + 3600, ReviewedAt: now - 30, ValidUntil: now + 600, MaxAgeSeconds: 900, PricingURL: "https://developers.openai.com/api/docs/pricing", ModelsURL: "https://developers.openai.com/api/docs/models", AllInputCharges: true, Prefixes: []string{"MESH_CURATOR", "MESH_JUDGE", "MESH_JUDGE2", "MESH_JUDGE3"}, Models: []responsesSpendModel{m}, MaxInFlight: 128, MinIntervalMS: 1}
}

func spendFixtureDir(t *testing.T, p responsesSpendPolicy) string {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("OpenAI spending admission has no supported filesystem implementation on this platform")
	}
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	b = append(b, '\n')
	hash := sha256.Sum256(b)
	s := responsesSpendState{Version: 2, BudgetID: p.BudgetID, ScopeID: p.ScopeID, PolicySHA: hex.EncodeToString(hash[:]), Tickets: []responsesDispatchTicket{}}
	state, _ := json.Marshal(s)
	for name, data := range map[string][]byte{"policy.json": b, "state.json": state, "lock": {}} {
		if err = os.WriteFile(filepath.Join(path, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func spendFixtureEnv(t *testing.T, p responsesSpendPolicy) string {
	t.Helper()
	dir := spendFixtureDir(t, p)
	t.Setenv("MESH_OPENAI_SPEND_DIR", dir)
	return dir
}

func spendFixtureState(t *testing.T, dir string) responsesSpendState {
	t.Helper()
	var s responsesSpendState
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || json.Unmarshal(b, &s) != nil {
		t.Fatal("invalid fixture state", err)
	}
	return s
}

func TestOpenAIAdmissionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*responsesSpendPolicy)
	}{
		{"missing rates", func(p *responsesSpendPolicy) { p.Models[0].InputRate = 0 }},
		{"missing output rates", func(p *responsesSpendPolicy) { p.Models[0].OutputRate = 0 }},
		{"missing fixed charge", func(p *responsesSpendPolicy) { p.Models[0].FixedRate = nil }},
		{"missing context", func(p *responsesSpendPolicy) { p.Models[0].ContextTokens = 0 }},
		{"missing envelope", func(p *responsesSpendPolicy) { p.Models[0].InputTokensPerByte = 0 }},
		{"inconsistent context", func(p *responsesSpendPolicy) { p.Models[0].ContextTokens = 1000 }},
		{"missing budget", func(p *responsesSpendPolicy) { p.Budget = 0 }},
		{"expired", func(p *responsesSpendPolicy) { p.ValidUntil = time.Now().Unix() - 1 }},
		{"stale", func(p *responsesSpendPolicy) { p.MaxAgeSeconds = 1 }},
		{"future review", func(p *responsesSpendPolicy) { p.ReviewedAt = time.Now().Unix() + 1 }},
		{"bad tier", func(p *responsesSpendPolicy) { p.Models[0].Tier = "auto" }},
		{"unattested cache charges", func(p *responsesSpendPolicy) { p.AllInputCharges = false }},
		{"wrong currency", func(p *responsesSpendPolicy) { p.Currency = "EUR" }},
		{"multiply overflow", func(p *responsesSpendPolicy) { p.Models[0].InputRate = math.MaxInt64 }},
		{"addition overflow", func(p *responsesSpendPolicy) { v := int64(math.MaxInt64); p.Models[0].FixedRate = &v }},
		{"envelope overflow", func(p *responsesSpendPolicy) { p.Models[0].InputTokensPerByte = math.MaxInt64 }},
		{"duplicate models", func(p *responsesSpendPolicy) { p.Models = append(p.Models, p.Models[0]) }},
		{"duplicate prefixes", func(p *responsesSpendPolicy) { p.Prefixes = append(p.Prefixes, "MESH_JUDGE") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := spendFixturePolicy()
			tc.change(&p)
			spendFixtureEnv(t, p)
			if _, err := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512); err == nil {
				t.Fatal("invalid policy admitted")
			}
		})
	}
	for _, tc := range []struct {
		prefix, model string
		max           int
	}{{"MESH_OTHER", "unit-explicit-model", 512}, {"MESH_CURATOR", "wrong-model", 512}, {"MESH_CURATOR", "unit-explicit-model", 0}, {"MESH_CURATOR", "unit-explicit-model", 513}} {
		t.Run(tc.prefix+tc.model, func(t *testing.T) {
			spendFixtureEnv(t, spendFixturePolicy())
			if _, err := newResponsesAdmission(tc.prefix, tc.model, tc.max); err == nil {
				t.Fatal("context/model selection admitted")
			}
		})
	}
	t.Run("no implicit path", func(t *testing.T) {
		t.Setenv("MESH_OPENAI_SPEND_DIR", "")
		if _, err := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512); err == nil {
			t.Fatal("path guessed")
		}
	})
}

func TestOpenAIAdmissionDurability(t *testing.T) {
	p := spendFixturePolicy()
	cost, _ := spendModelCost(p.Models[0], 512)
	p.Budget = cost * 2
	dir := spendFixtureEnv(t, p)
	for _, prefix := range []string{"MESH_CURATOR", "MESH_JUDGE"} {
		a, err := newResponsesAdmission(prefix, "unit-explicit-model", 512)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = a.reserve(context.Background(), 100); err != nil {
			t.Fatal(err)
		}
	}
	s := spendFixtureState(t, dir)
	if s.Reserved != cost*2 || s.Reservations != 2 {
		t.Fatal("shared accounting mismatch", s)
	}
	if _, err := newResponsesAdmission("MESH_JUDGE2", "unit-explicit-model", 512); err == nil {
		t.Fatal("restart reset budget")
	}
	t.Run("over input before reservation", func(t *testing.T) {
		dir := spendFixtureEnv(t, spendFixturePolicy())
		a, _ := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512)
		if _, err := a.reserve(context.Background(), 513); err == nil {
			t.Fatal("input envelope admitted")
		}
		if spendFixtureState(t, dir).Reservations != 0 {
			t.Fatal("invalid input reserved")
		}
	})
	t.Run("cancel before reservation", func(t *testing.T) {
		dir := spendFixtureEnv(t, spendFixturePolicy())
		a, _ := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := a.reserve(ctx, 1); err == nil {
			t.Fatal("cancel ignored")
		}
		if spendFixtureState(t, dir).Reservations != 0 {
			t.Fatal("cancelled input reserved")
		}
	})
	t.Run("durable halt", func(t *testing.T) {
		dir := spendFixtureEnv(t, spendFixturePolicy())
		a, _ := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512)
		a.reserve(context.Background(), 1)
		a.halt(context.Background())
		if !spendFixtureState(t, dir).Halted {
			t.Fatal("halt not persistent")
		}
		if _, err := newResponsesAdmission("MESH_JUDGE", "unit-explicit-model", 512); err == nil {
			t.Fatal("restart ignored halt")
		}
	})
}

func TestOpenAIAdmissionFilesystem(t *testing.T) {
	for _, kind := range []string{"missing state", "missing lock", "intent", "orphan temp", "policy mode", "state mode", "directory mode", "symlink state", "hardlink state", "symlink policy", "symlink lock", "corrupt state", "wrong hash", "rollback clock", "case alias", "unknown field"} {
		t.Run(kind, func(t *testing.T) {
			dir := spendFixtureEnv(t, spendFixturePolicy())
			a, err := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "state.json")
			switch kind {
			case "missing state":
				os.Remove(path)
			case "missing lock":
				os.Remove(filepath.Join(dir, "lock"))
			case "intent", "orphan temp":
				os.WriteFile(filepath.Join(dir, kind), []byte("unresolved write"), 0600)
			case "policy mode":
				os.Chmod(filepath.Join(dir, "policy.json"), 0644)
			case "state mode":
				os.Chmod(path, 0644)
			case "directory mode":
				os.Chmod(dir, 0755)
			case "symlink state", "symlink policy", "symlink lock":
				name := strings.TrimPrefix(kind, "symlink ")
				if name != "lock" {
					name += ".json"
				}
				original := filepath.Join(dir, name)
				target := filepath.Join(t.TempDir(), "target")
				os.Rename(original, target)
				os.Symlink(target, original)
			case "hardlink state":
				os.Link(path, filepath.Join(t.TempDir(), "other"))
			case "corrupt state":
				os.WriteFile(path, []byte("{"), 0600)
			case "wrong hash":
				s := spendFixtureState(t, dir)
				s.PolicySHA = strings.Repeat("0", 64)
				b, _ := json.Marshal(s)
				os.WriteFile(path, b, 0600)
			case "rollback clock":
				s := spendFixtureState(t, dir)
				s.LastAdmission = time.Now().Unix() + 1
				s.Reserved = 1
				s.Reservations = 1
				b, _ := json.Marshal(s)
				os.WriteFile(path, b, 0600)
			case "case alias":
				b, _ := os.ReadFile(path)
				b = []byte(strings.Replace(string(b), "\"reserved_nano_usd\":0", "\"reserved_nano_usd\":0,\"Reserved_nano_usd\":0", 1))
				os.WriteFile(path, b, 0600)
			case "unknown field":
				b, _ := os.ReadFile(path)
				b = append([]byte("{\"unknown\":1,"), b[1:]...)
				os.WriteFile(path, b, 0600)
			}
			if _, err := a.reserve(context.Background(), 1); err == nil {
				t.Fatal("unsafe state admitted")
			}
		})
	}
	t.Run("policy replacement cannot reset", func(t *testing.T) {
		p := spendFixturePolicy()
		dir := spendFixtureEnv(t, p)
		a, _ := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512)
		a.reserve(context.Background(), 1)
		p.Budget++
		b, _ := json.Marshal(p)
		os.WriteFile(filepath.Join(dir, "policy.json"), b, 0600)
		if _, err := a.reserve(context.Background(), 1); err == nil {
			t.Fatal("changed policy admitted")
		}
		if _, err := newResponsesAdmission("MESH_JUDGE", "unit-explicit-model", 512); err == nil {
			t.Fatal("new client reset accounting")
		}
	})
	t.Run("parent symlink", func(t *testing.T) {
		dir := spendFixtureDir(t, spendFixturePolicy())
		linked := filepath.Join(t.TempDir(), "linked")
		os.Symlink(dir, linked)
		t.Setenv("MESH_OPENAI_SPEND_DIR", linked)
		if _, err := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512); err == nil {
			t.Fatal("parent link admitted")
		}
	})
}

func TestOpenAIAdmissionConcurrent(t *testing.T) {
	p := spendFixturePolicy()
	cost, _ := spendModelCost(p.Models[0], 512)
	p.Budget = cost * 3
	dir := spendFixtureEnv(t, p)
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512)
			if err == nil {
				_, err = a.reserve(context.Background(), 1)
			}
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	s := spendFixtureState(t, dir)
	if success != 3 || s.Reservations != 3 || s.Reserved != p.Budget {
		t.Fatal("concurrent overspend/lost admission", success, s)
	}
}

func TestOpenAIAdmissionProcess(t *testing.T) {
	if os.Getenv("MESH_SPEND_TEST_CHILD") == "1" {
		os.Setenv("MESH_OPENAI_SPEND_DIR", os.Getenv("MESH_SPEND_TEST_DIR"))
		a, err := newResponsesAdmission("MESH_JUDGE", "unit-explicit-model", 512)
		if err == nil {
			_, err = a.reserve(context.Background(), 1)
		}
		if err != nil {
			os.Exit(10)
		}
		os.Exit(0)
	}
	p := spendFixturePolicy()
	cost, _ := spendModelCost(p.Models[0], 512)
	p.Budget = cost * 2
	dir := spendFixtureEnv(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	commands := make([]*exec.Cmd, 4)
	for i := range commands {
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOpenAIAdmissionProcess$")
		c.Env = []string{"MESH_SPEND_TEST_CHILD=1", "MESH_SPEND_TEST_DIR=" + dir, "GOMAXPROCS=2"}
		commands[i] = c
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
	}
	success := 0
	for _, c := range commands {
		err := c.Wait()
		if err == nil {
			success++
			continue
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 10 {
			t.Fatal("unexpected child result", err)
		}
	}
	s := spendFixtureState(t, dir)
	if success != 2 || s.Reservations != 2 || s.Reserved != p.Budget {
		t.Fatal("cross-process overspend", success, s)
	}
}

func TestOpenAIAdmissionJudgeNoDowngrade(t *testing.T) {
	t.Setenv("MESH_JUDGE_AGENT", "openai")
	t.Setenv("MESH_JUDGE_KEY", "")
	t.Setenv("MESH_JUDGE_MODEL", "unit-explicit-model")
	called := false
	fallback := Func(func(context.Context, string, string) (string, error) { called = true; return "fallback", nil })
	judge, independent := NewJudgeFromEnv(fallback)
	if independent {
		t.Fatal("unavailable judge labelled independent")
	}
	if _, err := judge.Complete(context.Background(), "s", "u"); !errors.Is(err, ErrRateLimited) {
		t.Fatal("judge did not fail closed", err)
	}
	panel, _ := NewJudgePanelFromEnv(fallback)
	if _, err := panel[0].Complete(context.Background(), "s", "u"); !errors.Is(err, ErrRateLimited) || called {
		t.Fatal("panel downgraded", err)
	}
}

func spendFixtureClientWithoutNetwork(t *testing.T, p responsesSpendPolicy, transport responsesRoundTrip) (*openAIResponses, string) {
	t.Helper()
	dir := spendFixtureEnv(t, p)
	t.Setenv("MESH_CURATOR_AGENT", "openai")
	t.Setenv("MESH_CURATOR_KEY", "unit-dedicated-key")
	t.Setenv("MESH_CURATOR_MODEL", "unit-explicit-model")
	t.Setenv("MESH_CURATOR_MAXTOK", "512")
	t.Setenv("MESH_CURATOR_ENDPOINT", "")
	t.Setenv("MESH_CURATOR_TEMP", "")
	c, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	client := c.(*openAIResponses)
	client.hc.Transport = transport
	return client, dir
}

func TestOpenAIAdmissionBeforeHTTPAndUncertainty(t *testing.T) {
	for _, outcome := range []string{"completed", "transport uncertain", "HTTP401", "HTTP429", "cancel after reservation", "malformed stream"} {
		t.Run(outcome, func(t *testing.T) {
			p := spendFixturePolicy()
			cost, _ := spendModelCost(p.Models[0], 512)
			p.Budget = cost
			var calls atomic.Int32
			var dir string
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, path := spendFixtureClientWithoutNetwork(t, p, func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				state := spendFixtureState(t, dir)
				if state.Reserved != cost || state.Reservations != 1 {
					t.Fatal("outbound before durable reservation")
				}
				if r.URL.String() != openAIResponsesURL {
					t.Fatal("unexpected outbound route")
				}
				if outcome == "transport uncertain" {
					return nil, errors.New("unit transport secret body")
				}
				if outcome == "cancel after reservation" {
					cancel()
					return nil, context.Canceled
				}
				code := 200
				if outcome == "HTTP401" {
					code = 401
				}
				if outcome == "HTTP429" {
					code = 429
				}
				stream := responseFixtureStream(responseFixtureEvents("ok"))
				if outcome == "malformed stream" {
					stream = "data: malformed secret\n\n"
				}
				return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
			})
			dir = path
			text, err := c.Complete(ctx, "trusted", "untrusted")
			if outcome == "completed" {
				if err != nil || text != "ok" {
					t.Fatal(text, err)
				}
			} else if err == nil || text != "" {
				t.Fatal("uncertain result admitted")
			}
			if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "unit-dedicated-key")) {
				t.Fatal("provider details leaked")
			}
			if calls.Load() != 1 || spendFixtureState(t, dir).Reserved != cost {
				t.Fatal("reservation refunded")
			}
			if _, err = c.Complete(context.Background(), "trusted", "untrusted"); !errors.Is(err, ErrRateLimited) || calls.Load() != 1 {
				t.Fatal("exhausted budget contacted provider", err)
			}
			if _, err = newResponsesAdmission("MESH_JUDGE", "unit-explicit-model", 512); err == nil {
				t.Fatal("restart reset uncertain reservation")
			}
		})
	}
	t.Run("no admission object", func(t *testing.T) {
		called := false
		c := &openAIResponses{model: "unit-explicit-model", maxTok: 512, hc: &http.Client{Transport: responsesRoundTrip(func(*http.Request) (*http.Response, error) { called = true; return nil, nil })}}
		if _, err := c.Complete(context.Background(), "s", "u"); err == nil || called {
			t.Fatal("nil admission bypass")
		}
	})
	t.Run("input envelope is job specific", func(t *testing.T) {
		calls := 0
		c, _ := spendFixtureClientWithoutNetwork(t, spendFixturePolicy(), func(*http.Request) (*http.Response, error) { calls++; return nil, nil })
		if _, err := c.Complete(context.Background(), strings.Repeat("s", 513), ""); err == nil || errors.Is(err, ErrAuth) || errors.Is(err, ErrRateLimited) || calls != 0 {
			t.Fatal("input overflow bypass/starvation", err)
		}
	})
}

func TestOpenAIAdmissionUsageMismatch(t *testing.T) {
	for _, kind := range []string{"missing usage", "model", "tier", "missing input", "missing output", "missing total", "negative input", "input over ceiling", "output over limit", "total mismatch", "sum overflow"} {
		t.Run(kind, func(t *testing.T) {
			p := spendFixturePolicy()
			var calls int
			c, dir := spendFixtureClientWithoutNetwork(t, p, func(*http.Request) (*http.Response, error) {
				calls++
				events := responseFixtureEvents("discard")
				r := events[len(events)-1]["response"].(map[string]any)
				u := r["usage"].(map[string]any)
				switch kind {
				case "missing usage":
					delete(r, "usage")
				case "model":
					r["model"] = "other-model"
				case "tier":
					r["service_tier"] = "priority"
				case "missing input":
					delete(u, "input_tokens")
				case "missing output":
					delete(u, "output_tokens")
				case "missing total":
					delete(u, "total_tokens")
				case "negative input":
					u["input_tokens"] = -1
				case "input over ceiling":
					u["input_tokens"] = 2049
					u["total_tokens"] = 2069
				case "output over limit":
					u["output_tokens"] = 513
					u["total_tokens"] = 523
				case "total mismatch":
					u["total_tokens"] = 31
				case "sum overflow":
					u["input_tokens"] = int64(math.MaxInt64)
					u["output_tokens"] = 1
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(responseFixtureStream(events)))}, nil
			})
			if text, err := c.Complete(context.Background(), "s", "u"); text != "" || !errors.Is(err, ErrRateLimited) {
				t.Fatal("billing mismatch admitted", text, err)
			}
			s := spendFixtureState(t, dir)
			if !s.Halted || s.Reservations != 1 || s.Reserved <= 0 {
				t.Fatal("billing mismatch not durably halted", s)
			}
			if _, err := c.Complete(context.Background(), "s", "u"); err == nil || calls != 1 {
				t.Fatal("halt contacted provider")
			}
			if _, err := newResponsesAdmission("MESH_JUDGE", "unit-explicit-model", 512); err == nil {
				t.Fatal("other process/prefix ignored halt")
			}
		})
	}
}

func TestOpenAIAdmissionExpiryDuringPersistence(t *testing.T) {
	p := spendFixturePolicy()
	p.ValidUntil = time.Now().Unix() + 2
	called := false
	c, dir := spendFixtureClientWithoutNetwork(t, p, func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not dispatch")
	})
	c.admission.persist = func(r *os.Root, s responsesSpendState) error {
		if err := admissionCommit(r, s); err != nil {
			return err
		}
		// Models a fsync/lock delay across the attestation's exact expiry.
		time.Sleep(time.Until(time.Unix(p.ValidUntil, 0)) + 10*time.Millisecond)
		return nil
	}
	if text, err := c.Complete(context.Background(), "s", "u"); err == nil || text != "" || called {
		t.Fatal("expired committed reservation dispatched", err)
	}
	if s := spendFixtureState(t, dir); s.Reservations != 1 || s.Reserved <= 0 {
		t.Fatal("expired reservation refunded", s)
	}
}

func TestOpenAIAdmissionMissingStateFields(t *testing.T) {
	for _, field := range []string{"reserved_nano_usd", "reservations", "last_admission_unix", "halted"} {
		for _, null := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-null-%v", field, null), func(t *testing.T) {
				dir := spendFixtureEnv(t, spendFixturePolicy())
				a, err := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = a.reserve(context.Background(), 1); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "state.json")
				b, _ := os.ReadFile(path)
				var state map[string]any
				json.Unmarshal(b, &state)
				if null {
					state[field] = nil
				} else {
					delete(state, field)
				}
				b, _ = json.Marshal(state)
				os.WriteFile(path, b, 0600)
				if _, err = a.reserve(context.Background(), 1); err == nil {
					t.Fatal("missing/null state gained zero authority")
				}
				if _, err = newResponsesAdmission("MESH_JUDGE", "unit-explicit-model", 512); err == nil {
					t.Fatal("restart admitted incomplete state")
				}
			})
		}
	}
}

func TestOpenAIAdmissionJudgeCaseSelection(t *testing.T) {
	for _, selection := range []string{"OPENAI", " OpenAI "} {
		t.Run(selection, func(t *testing.T) {
			t.Setenv("MESH_JUDGE_AGENT", selection)
			t.Setenv("MESH_JUDGE_KEY", "")
			t.Setenv("MESH_JUDGE_MODEL", "unit-explicit-model")
			called := false
			fallback := Func(func(context.Context, string, string) (string, error) { called = true; return "fallback", nil })
			judge, _ := NewJudgeFromEnv(fallback)
			if _, err := judge.Complete(context.Background(), "s", "u"); err == nil || called {
				t.Fatal("normalized selection downgraded")
			}
			panel, _ := NewJudgePanelFromEnv(fallback)
			if _, err := panel[0].Complete(context.Background(), "s", "u"); err == nil || called {
				t.Fatal("normalized panel downgraded")
			}
		})
	}
}
