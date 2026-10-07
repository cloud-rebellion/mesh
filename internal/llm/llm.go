// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package llm is the Mesh BYOAI chat boundary for the sync-curator (S2.1). It is
// stdlib-only (no SDK) and speaks three backends behind one Complete interface:
//
//   - cli (the default): the team's existing coding-agent CLI (e.g. `claude -p`),
//     already authenticated in the dev's IDE, invoked without agent tools or
//     discovered MCP/configuration. System and user prompts remain separate.
//     Custom adapters require an explicit completion-only JSON contract.
//   - anthropic: Anthropic Messages API with an explicit key (best when running
//     headless/always-on where no IDE CLI is logged in).
//   - local: any OpenAI-compatible /v1/chat/completions endpoint (e.g. a local
//     Ollama), so a sovereign vault never egresses.
//
// Keys and vault content are never logged.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/safehttp"
)

// maxLLMResponseBytes bounds an LLM endpoint's response so a hostile/misconfigured
// (operator-editable) endpoint cannot OOM the process; a chat completion is small.
const maxLLMResponseBytes = 32 << 20

// ChildEnv marks every command-line LLM subprocess started by Mesh. Session
// hooks inherit the agent process environment, so Mesh's Stop hook uses this
// marker to avoid nudging or auto-extracting its own strict-output children.
const ChildEnv = "MESH_LLM_CHILD"

// ErrRateLimited (429) is transient: the caller should back off and retry the
// whole pass later, not hammer the rest of the batch.
var ErrRateLimited = errors.New("llm: rate limited")

// ErrTruncated means the model hit its output budget (stop_reason max_tokens /
// finish_reason length), so the completion is partial and must not be trusted as
// a complete merged note.
var ErrTruncated = errors.New("llm: output truncated at max_tokens")

// ErrAuth (401/403, a CLI that cannot start or reports a login problem) is an operator
// config problem, not a poison job: the caller should wait for the key to be fixed, NOT
// burn the attempt cap. It is GLOBAL by contract, so a failure that depends on the job's
// own input (an oversize prompt, a refusal, a non-zero exit with unrelated stderr) must
// never be reported as ErrAuth: the curator halts the whole pass on it, and one such job
// sitting at the head of the queue would starve every other conflict.
var ErrAuth = errors.New("llm: authentication failed (check the API key)")

// Client turns a (system, user) prompt into completion text.
type Client interface {
	Complete(ctx context.Context, system, user string) (string, error)
	Describe() string // agent + model, no secrets; for status/logging
}

// Func adapts a function to a Client (used by tests + the e2e stub curator).
type Func func(ctx context.Context, system, user string) (string, error)

func (f Func) Complete(ctx context.Context, system, user string) (string, error) {
	return f(ctx, system, user)
}
func (f Func) Describe() string { return "stub" }

// ---- CLI backend (BYOAI via the dev's coding-agent CLI, no API key) ----

// cliClient invokes the authenticated provider as a completion-only subprocess.
// Claude uses an actual system-prompt argument and user-only stdin; custom
// adapters receive separate JSON fields. Output is bounded before parsing.
type cliClient struct {
	argv    []string
	timeout time.Duration
	claude  bool
}

func (c *cliClient) Describe() string { return "cli/" + strings.Join(c.argv, " ") }

func (c *cliClient) Complete(ctx context.Context, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	argv, input := c.completionCommand(system, user)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(input)
	// The child (default `claude -p`) is a third party that phones home, and note/
	// connector content flows into its prompt (a prompt-injection reach). Never hand it
	// the parent's full environment: strip mesh's own secrets (MESH_*) and any other
	// credential-shaped var so ingest tokens, the cookie/OIDC secrets, and the embed/
	// rerank keys can't be exfiltrated. The child's OWN auth (ANTHROPIC_*/CLAUDE_*) and
	// PATH/HOME/locale are preserved so `claude -p` still authenticates.
	// Append after sanitizing: a value inherited from the parent is stripped with
	// the other MESH_* variables, then replaced with the one trusted fixed value.
	cmd.Env = SubprocessEnv()
	out := boundedCLIOutput{limit: maxCLIOutputBytes, cancel: cancel}
	errb := boundedCLIOutput{limit: maxCLIErrorBytes, cancel: cancel}
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if out.overflow || errb.overflow {
		return "", ErrOutputLimit
	}
	if ctx.Err() != nil {
		return "", ctx.Err() // timeout / cancellation: transient, retry next pass
	}
	if err != nil {
		// ErrAuth means "the operator must fix something, every job is affected", and
		// the curator honours that by ending the whole pass WITHOUT charging the job an
		// attempt. So only failures that really are global earn it: the CLI could not
		// start at all (missing binary, not executable) or its stderr names a login /
		// credential problem. Any other non-zero exit is job-specific (an oversize
		// prompt from two 1 MB note versions, a refusal, a parse error on this input),
		// and classifying those as ErrAuth let one poison job at the head of the
		// created_at-ordered pending list starve the whole queue forever.
		if startupFailure(err) || authFailureStderr(errb.String()) {
			return "", fmt.Errorf("curator cli %q: %w: %s", c.argv[0], ErrAuth, cliErrDetail(errb.String(), err))
		}
		return "", fmt.Errorf("curator cli %q failed: %s", c.argv[0], cliErrDetail(errb.String(), err))
	}
	s := strings.TrimSpace(out.String())
	if s == "" {
		// The process ran and exited 0 with nothing on stdout: this input produced no
		// completion (a refusal, a silent truncation). That is deterministic for THIS
		// job, so it must be charged an attempt and retired at the cap, not treated as
		// an operator outage that halts every other job.
		return "", fmt.Errorf("curator cli %q returned no output", c.argv[0])
	}
	if c.claude {
		return claudeCompletionResult(s)
	}
	return s, nil
}

// startupFailure reports whether the subprocess never ran: the binary is missing, is
// not on PATH, or is not executable. That is an operator/environment problem affecting
// every job, so it is the one exec failure that maps to ErrAuth.
func startupFailure(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false // the process ran and exited non-zero: job-specific
	}
	return errors.Is(err, exec.ErrNotFound) ||
		errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, exec.ErrDot)
}

// authFailureStderr reports whether the CLI's stderr names a login/credential problem,
// the other case where waiting for the operator (rather than charging the job) is
// right. The list is deliberately narrow: a phrase that could describe a bad INPUT
// (too long, invalid request, rate limit) must not land here, or one poison job would
// again be able to halt the pass indefinitely.
func authFailureStderr(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, pat := range []string{
		"not logged in", "please log in", "please login", "run /login", "login required",
		"unauthorized", "authentication failed", "authentication required", "not authenticated",
		"invalid api key", "missing api key", "no api key", "api key not", "invalid credentials",
		"expired token", "token expired", "forbidden",
		// A bare "401"/"403" would also match a token count or a line number, which is
		// exactly the oversize-prompt case this taxonomy must keep job-specific.
		"401 unauthorized", "403 forbidden", "status 401", "status 403", "status code 401", "status code 403",
	} {
		if strings.Contains(s, pat) {
			return true
		}
	}
	return false
}

// sanitizedEnv returns the parent environment with mesh's own secrets and other
// credential-shaped variables removed, so a BYOAI subprocess never receives
// MESH_UI_TOKEN, MESH_*_KEY, the ingest tokens, or the cookie/OIDC secrets. The child
// agent's own credentials pass through: ANTHROPIC_*/CLAUDE_* (how `claude -p`
// authenticates), plus PATH/HOME/locale so it still runs. An operator can force extra
// names through with MESH_CURATOR_ENV_PASSTHROUGH (comma-separated).
func sanitizedEnv() []string {
	var passthrough map[string]bool
	if v := strings.TrimSpace(os.Getenv("MESH_CURATOR_ENV_PASSTHROUGH")); v != "" {
		passthrough = map[string]bool{}
		for _, n := range strings.Split(v, ",") {
			passthrough[strings.ToUpper(strings.TrimSpace(n))] = true
		}
	}
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		up := strings.ToUpper(name)
		if passthrough[up] || strings.HasPrefix(up, "ANTHROPIC_") || strings.HasPrefix(up, "CLAUDE_") {
			out = append(out, kv) // the child's own auth / an explicit operator opt-in
			continue
		}
		if secretEnvName(up) {
			continue // drop mesh + credential-shaped vars
		}
		out = append(out, kv)
	}
	return out
}

// SubprocessEnv is the least-privilege environment for any command-line model
// Mesh starts. It removes Mesh and credential-shaped secrets, preserves the
// provider CLI's own authentication, and marks the process so Mesh hooks never
// harvest or nudge the strict-output child session.
func SubprocessEnv() []string {
	return append(sanitizedEnv(), ChildEnv+"=1")
}

// secretEnvName reports whether an env var name looks like a secret we must not hand to
// a third-party subprocess: anything mesh owns (MESH_*) or any credential-shaped name.
func secretEnvName(up string) bool {
	if strings.HasPrefix(up, "MESH_") {
		return true
	}
	for _, suf := range []string{"_TOKEN", "_SECRET", "_KEY", "_PASSWORD", "_PASSWD"} {
		if strings.HasSuffix(up, suf) {
			return true
		}
	}
	for _, sub := range []string{"SECRET", "PASSWORD", "PASSWD", "APIKEY", "API_KEY", "CREDENTIAL"} {
		if strings.Contains(up, sub) {
			return true
		}
	}
	return false
}

// cliErrDetail prefers the subprocess's stderr (truncated, no secrets assumed) and
// falls back to the exec error.
func cliErrDetail(stderr string, err error) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err.Error()
	}
	if len(stderr) > 300 {
		stderr = stderr[:300]
	}
	return stderr
}

const (
	defaultAnthropicBase  = "https://api.anthropic.com"
	defaultAnthropicModel = "claude-sonnet-4-6"
	anthropicVersion      = "2023-06-01"
	defaultMaxTokens      = 8192
	defaultCuratorCmd     = "claude -p" // built into a completion-only invocation
	defaultCLITimeout     = 300 * time.Second
)

// NewFromEnv builds the curator's LLM client from MESH_CURATOR_* env:
//
//	MESH_CURATOR_AGENT   cli (default) | anthropic | local
//	MESH_CURATOR_MODEL   model id (anthropic default claude-sonnet-4-6; required for local)
//	MESH_CURATOR_MAXTOK  max output tokens (default 8192; anthropic/local only)
//	cli:       MESH_CURATOR_CMD (default "claude -p"), MESH_CURATOR_CMD_TIMEOUT (seconds)
//	custom CLI: MESH_CURATOR_CLI_CONTRACT=mesh-completion-v1 (see docs/EXTRACTION.md)
//	anthropic: MESH_ANTHROPIC_KEY (fallback ANTHROPIC_API_KEY), MESH_ANTHROPIC_BASE
//	local:     MESH_CURATOR_ENDPOINT (e.g. http://localhost:11434/v1), MESH_CURATOR_KEY
func NewFromEnv() (Client, error) {
	return newFromEnvPrefix("MESH_CURATOR")
}

// NewJudgeFromEnv builds a SEPARATE judge client from MESH_JUDGE_* (same schema as
// MESH_CURATOR_*: MESH_JUDGE_AGENT / MESH_JUDGE_MODEL / MESH_JUDGE_CMD /
// MESH_JUDGE_CMD_TIMEOUT / MESH_JUDGE_ENDPOINT / MESH_JUDGE_KEY; the anthropic key
// stays the shared MESH_ANTHROPIC_KEY / ANTHROPIC_API_KEY). It exists so extraction
// precision can be graded by a model that did NOT write the candidate: a self-grade
// (the extractor judging itself) flatters its own output. When nothing under
// MESH_JUDGE_* is set it returns (fallback, false) so the judge is the extractor
// itself and the caller labels the measurement a self-grade, not independent.
func NewJudgeFromEnv(fallback Client) (Client, bool) {
	if !anyEnvWithPrefix("MESH_JUDGE_") {
		return fallback, false
	}
	c, err := newFromEnvPrefix("MESH_JUDGE")
	if err != nil || c == nil {
		return fallback, false
	}
	return c, true
}

// NewJudgePanelFromEnv builds up to 3 independent judge clients from MESH_JUDGE_*,
// MESH_JUDGE2_*, MESH_JUDGE3_* (each the same schema as MESH_CURATOR_*), for a
// model-diverse panel. Prefixes with no env set are skipped. When none are set it returns
// ([]Client{fallback}, false): the panel runs on the extractor itself (a self-grade) and
// the caller labels it honestly. independent is true when any configured judge exists.
func NewJudgePanelFromEnv(fallback Client) (judges []Client, independent bool) {
	for _, p := range []string{"MESH_JUDGE", "MESH_JUDGE2", "MESH_JUDGE3"} {
		if !anyEnvWithPrefix(p + "_") {
			continue
		}
		if c, err := newFromEnvPrefix(p); err == nil && c != nil {
			judges = append(judges, c)
		}
	}
	if len(judges) == 0 {
		return []Client{fallback}, false
	}
	return judges, true
}

// anyEnvWithPrefix reports whether any environment variable starts with prefix.
func anyEnvWithPrefix(prefix string) bool {
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

// newFromEnvPrefix builds a client from <prefix>_AGENT/_MODEL/_MAXTOK/_CMD/_ENDPOINT/
// _KEY (the anthropic key/base stay the shared MESH_ANTHROPIC_* names). NewFromEnv and
// NewJudgeFromEnv are the two prefixes, so the curator and the judge configure the same
// way without duplicating this switch.
func newFromEnvPrefix(p string) (Client, error) {
	agent := strings.ToLower(strings.TrimSpace(os.Getenv(p + "_AGENT")))
	if agent == "" {
		agent = "cli"
	}
	maxTok := defaultMaxTokens
	if v, err := strconv.Atoi(os.Getenv(p + "_MAXTOK")); err == nil && v > 0 {
		maxTok = v
	}
	model := strings.TrimSpace(os.Getenv(p + "_MODEL"))
	// Optional sampling temperature (api backends only; the cli passes none). Set
	// <prefix>_TEMP=0 for reproducible runs, e.g. a deterministic benchmark or a judge whose
	// verdicts should not flip run to run. Unset leaves the provider default (~1.0).
	var temp *float64
	if v := strings.TrimSpace(os.Getenv(p + "_TEMP")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			temp = &f
		}
	}
	// Every endpoint this constructor can reach comes from the PROCESS ENVIRONMENT
	// (<prefix>_ENDPOINT, MESH_ANTHROPIC_BASE), which no HTTP surface can write: the
	// config API writes .mesh/config.toml only. That is operator authority, the same
	// authority MESH_ALLOW_PRIVATE_LLM_ENDPOINT expresses, so a sovereign localhost
	// model server is reachable without a second opt-in. Guarding it instead just made
	// the documented `MESH_CURATOR_AGENT=local` setup fail with an SSRF refusal.
	hc := safehttp.OperatorLLMClient(120 * time.Second)

	switch agent {
	case "cli":
		cmdStr := strings.TrimSpace(os.Getenv(p + "_CMD"))
		if cmdStr == "" {
			cmdStr = defaultCuratorCmd
		}
		argv := strings.Fields(cmdStr)
		if len(argv) == 0 {
			return nil, fmt.Errorf("cli agent needs %s_CMD (e.g. %q)", p, defaultCuratorCmd)
		}
		to := defaultCLITimeout
		if v, err := strconv.Atoi(os.Getenv(p + "_CMD_TIMEOUT")); err == nil && v > 0 {
			to = time.Duration(v) * time.Second
		}
		return newCompletionCLI(argv, to, strings.TrimSpace(os.Getenv(p+"_CLI_CONTRACT")), model)
	case "anthropic":
		key := firstNonEmpty(os.Getenv("MESH_ANTHROPIC_KEY"), os.Getenv("ANTHROPIC_API_KEY"))
		if key == "" {
			return nil, fmt.Errorf("anthropic agent needs MESH_ANTHROPIC_KEY (or ANTHROPIC_API_KEY)")
		}
		if model == "" {
			model = defaultAnthropicModel
		}
		base := firstNonEmpty(os.Getenv("MESH_ANTHROPIC_BASE"), defaultAnthropicBase)
		return &anthropic{base: strings.TrimRight(base, "/"), key: key, model: model, maxTok: maxTok, temp: temp, hc: hc}, nil
	case "local":
		ep := strings.TrimRight(strings.TrimSpace(os.Getenv(p+"_ENDPOINT")), "/")
		if ep == "" {
			return nil, fmt.Errorf("local agent needs %s_ENDPOINT (e.g. http://localhost:11434/v1)", p)
		}
		if model == "" {
			return nil, fmt.Errorf("local agent needs %s_MODEL", p)
		}
		return &openaiCompat{endpoint: ep, key: os.Getenv(p + "_KEY"), model: model, maxTok: maxTok, temp: temp, hc: hc}, nil
	default:
		return nil, fmt.Errorf("unknown %s_AGENT %q (want cli|anthropic|local)", p, agent)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---- Anthropic Messages backend ----

type anthropic struct {
	base, key, model string
	maxTok           int
	temp             *float64 // nil = provider default
	hc               *http.Client
	baseOverride     string // tests
}

func (a *anthropic) Describe() string { return "anthropic/" + a.model }

func (a *anthropic) Complete(ctx context.Context, system, user string) (string, error) {
	payload := map[string]any{
		"model":      a.model,
		"max_tokens": a.maxTok,
		"system":     system,
		"messages":   []map[string]any{{"role": "user", "content": user}},
	}
	if a.temp != nil {
		payload["temperature"] = *a.temp
	}
	body, _ := json.Marshal(payload)
	base := a.base
	if a.baseOverride != "" {
		base = a.baseOverride
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("anthropic-version", anthropicVersion)
	resp, err := a.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusError("anthropic", resp.StatusCode)
	}
	var out struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		// Token spend was previously dropped on the floor, which made an agent-driven
		// curation/ask pass completely unobservable: no way to see what a run cost, and
		// no way to notice a prompt-size regression until the bill arrived.
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxLLMResponseBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("anthropic decode: %w", err)
	}
	// Debug, not Info: a curator pass makes one of these per job and this is
	// per-request accounting, not an operational event.
	slog.Debug("llm: anthropic usage",
		"model", a.model,
		"input_tokens", out.Usage.InputTokens,
		"output_tokens", out.Usage.OutputTokens,
		"cache_creation_input_tokens", out.Usage.CacheCreationInputTokens,
		"cache_read_input_tokens", out.Usage.CacheReadInputTokens,
		"stop_reason", out.StopReason)
	if out.StopReason == "max_tokens" {
		return "", fmt.Errorf("anthropic: %w", ErrTruncated)
	}
	var sb strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("anthropic: empty completion")
	}
	return sb.String(), nil
}

// ---- OpenAI-compatible (local/sovereign) backend ----

type openaiCompat struct {
	endpoint, key, model string
	maxTok               int
	temp                 *float64 // nil = provider default
	hc                   *http.Client
}

func (o *openaiCompat) Describe() string { return "local/" + o.model }

func (o *openaiCompat) Complete(ctx context.Context, system, user string) (string, error) {
	payload := map[string]any{
		"model":      o.model,
		"max_tokens": o.maxTok,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	if o.temp != nil {
		payload["temperature"] = *o.temp
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.key != "" {
		req.Header.Set("Authorization", "Bearer "+o.key)
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("local llm: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusError("local llm", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxLLMResponseBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("local llm decode: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("local llm: empty completion")
	}
	if out.Choices[0].FinishReason == "length" {
		return "", fmt.Errorf("local llm: %w", ErrTruncated)
	}
	return out.Choices[0].Message.Content, nil
}

func statusError(who string, code int) error {
	switch code {
	case http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w", who, ErrRateLimited)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s: %w", who, ErrAuth)
	}
	return fmt.Errorf("%s: unexpected status %d", who, code)
}
