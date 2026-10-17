// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bright-interaction/mesh/internal/safehttp"
)

const (
	openAIResponsesURL    = "https://api.openai.com/v1/responses"
	responsesInputBytes   = 1 << 20
	responsesRequestBytes = 7 << 20 // JSON escaping can expand bounded input sixfold.
	responsesStreamBytes  = 4 << 20
	responsesEventBytes   = 256 << 10
	responsesTextBytes    = 64 << 10
	responsesMaxEvents    = 4096
	responsesMaxTokens    = 32768
	responsesTimeout      = 120 * time.Second
	responsesIdleTimeout  = 30 * time.Second
)

// openAIResponses is an inference-only API boundary. It has no subprocess,
// tool registry, OAuth discovery, configurable origin or publication authority.
type openAIResponses struct {
	key, model  string
	maxTok      int
	hc          *http.Client
	idleTimeout time.Duration
	admission   *responsesAdmission
}

func newOpenAIResponses(prefix, model string) (Client, error) {
	key := strings.TrimSpace(os.Getenv(prefix + "_KEY"))
	if key == "" || len(key) > 8192 || !visibleASCII(key) {
		return nil, fmt.Errorf("openai needs a valid dedicated %s_KEY", prefix)
	}
	if model == "" || len(model) > 128 || !modelIdentifier(model) {
		return nil, fmt.Errorf("openai needs an explicit valid %s_MODEL", prefix)
	}
	if strings.TrimSpace(os.Getenv(prefix+"_ENDPOINT")) != "" || strings.TrimSpace(os.Getenv(prefix+"_TEMP")) != "" {
		return nil, fmt.Errorf("openai does not accept %s_ENDPOINT or %s_TEMP", prefix, prefix)
	}
	maxTok := 0
	if raw := os.Getenv(prefix + "_MAXTOK"); raw != "" {
		var err error
		maxTok, err = strconv.Atoi(raw)
		if err != nil || maxTok < 1 || maxTok > responsesMaxTokens {
			return nil, fmt.Errorf("openai %s_MAXTOK must be 1..%d", prefix, responsesMaxTokens)
		}
	}
	admission, err := newResponsesAdmission(prefix, model, maxTok)
	if err != nil {
		return nil, err
	}
	hc := safehttp.Client(responsesTimeout)
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &openAIResponses{key: key, model: model, maxTok: maxTok, hc: hc, idleTimeout: responsesIdleTimeout, admission: admission}, nil
}

func visibleASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 33 || s[i] > 126 {
			return false
		}
	}
	return true
}

func modelIdentifier(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._:/", c)) {
			return false
		}
	}
	return true
}

func (o *openAIResponses) Describe() string { return "openai/" + o.model }

func (o *openAIResponses) Complete(parent context.Context, system, user string) (string, error) {
	if len(system) > responsesInputBytes || len(user) > responsesInputBytes-len(system) || !utf8.ValidString(system) || !utf8.ValidString(user) {
		return "", fmt.Errorf("openai: invalid or oversized input") // job-specific, never ErrAuth.
	}
	deadline, finish := context.WithTimeout(parent, responsesTimeout)
	defer finish()
	ctx, cancel := context.WithCancelCause(deadline)
	defer cancel(nil)
	payload := struct {
		Model           string              `json:"model"`
		Instructions    string              `json:"instructions"`
		Input           []map[string]string `json:"input"`
		Tools           []string            `json:"tools"`
		ToolChoice      string              `json:"tool_choice"`
		Store           bool                `json:"store"`
		Stream          bool                `json:"stream"`
		MaxOutputTokens int                 `json:"max_output_tokens"`
		ServiceTier     string              `json:"service_tier"`
		Truncation      string              `json:"truncation"`
	}{o.model, system, []map[string]string{{"role": "user", "content": user}}, []string{}, "none", false, true, o.maxTok, "default", "disabled"}
	body, err := json.Marshal(payload)
	if err != nil || len(body) > responsesRequestBytes {
		return "", fmt.Errorf("openai: request exceeds input bound")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIResponsesURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("openai: cannot prepare request")
	}
	req.Header.Set("Authorization", "Bearer "+o.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	reservation, err := o.admission.reserve(ctx, len(system)+len(user))
	if err != nil {
		return "", err
	}
	ctx, spendCancel := context.WithDeadline(ctx, time.UnixMilli(reservation.ticket.DeadlineMS))
	defer spendCancel()
	req = req.WithContext(ctx)
	if ctx.Err() != nil {
		// This request never reached Do; only this call can prove that fact.
		if err := o.finishTicket(reservation); err != nil {
			return "", err
		}
		return "", responsesContextError(ctx)
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", responsesContextError(ctx)
		}
		return "", fmt.Errorf("openai: transport failed") // Never expose transport/body/key details.
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusError("openai", resp.StatusCode)
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "text/event-stream" {
		return "", fmt.Errorf("openai: expected event stream")
	}
	r := &responsesReader{body: resp.Body, ctx: ctx, cancel: cancel, idle: o.idleTimeout}
	outcome := readResponsesOutcome(r, &reservation)
	if errors.Is(outcome.err, errResponsesSpendMismatch) {
		// Persist the circuit break independently of a cancelled inference context.
		haltCtx, haltCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer haltCancel()
		return "", o.admission.halt(haltCtx)
	}
	if outcome.terminal {
		if err := o.finishTicket(reservation); err != nil {
			return "", err
		}
	}
	if ctx.Err() != nil {
		return "", responsesContextError(ctx)
	}
	return outcome.text, outcome.err
}

func (o *openAIResponses) finishTicket(reservation responsesReservation) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return o.admission.releaseTicket(ctx, reservation)
}

func responsesContextError(ctx context.Context) error {
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ctx.Err()
}

// Cancel the HTTP request on an idle read. No goroutine is left consuming a body
// after a deadline; the standard HTTP transport honours the request context.
type responsesReader struct {
	body   io.Reader
	ctx    context.Context
	cancel context.CancelCauseFunc
	idle   time.Duration
	n      int
}

func (r *responsesReader) Read(p []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, responsesContextError(r.ctx)
	}
	if r.n > responsesStreamBytes {
		return 0, ErrOutputLimit
	}
	if len(p) > responsesStreamBytes+1-r.n {
		p = p[:responsesStreamBytes+1-r.n]
	}
	timer := time.AfterFunc(r.idle, func() { r.cancel(context.DeadlineExceeded) })
	n, err := r.body.Read(p)
	timer.Stop()
	r.n += n
	if r.ctx.Err() != nil {
		return 0, responsesContextError(r.ctx)
	}
	if r.n > responsesStreamBytes {
		return 0, ErrOutputLimit
	}
	return n, err
}

type responsesEvent struct {
	Type         string             `json:"type"`
	Sequence     *int64             `json:"sequence_number"`
	Response     *responsesResponse `json:"response"`
	Item         json.RawMessage    `json:"item"`
	Part         json.RawMessage    `json:"part"`
	OutputIndex  *int               `json:"output_index"`
	ContentIndex *int               `json:"content_index"`
	ItemID       string             `json:"item_id"`
	Delta        string             `json:"delta"`
	Text         string             `json:"text"`
	Code         string             `json:"code"`
}

var errResponsesSpendMismatch = errors.New("openai: admission accounting mismatch")

type responsesResponse struct {
	Model       string `json:"model"`
	ServiceTier string `json:"service_tier"`
	Usage       *struct {
		Input  *int64 `json:"input_tokens"`
		Output *int64 `json:"output_tokens"`
		Total  *int64 `json:"total_tokens"`
	} `json:"usage"`
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Code string `json:"code"`
	} `json:"error"`
	Incomplete *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output     []json.RawMessage `json:"output"`
	Tools      []json.RawMessage `json:"tools"`
	ToolChoice string            `json:"tool_choice"`
	Store      *bool             `json:"store"`
}

type responsesItem struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Status  string          `json:"status"`
	Content []responsesPart `json:"content"`
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
}

type responsesPart struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Annotations []json.RawMessage `json:"annotations"`
}

type responsesItemState struct {
	item     responsesItem
	text     strings.Builder
	textDone bool
	done     json.RawMessage
}

type responsesState struct {
	reservation *responsesReservation
	id          string
	sequence    int64
	items       []*responsesItemState
	message     *responsesItemState
	completed   bool
	text        string
	terminal    bool
	terminalErr error
}

// Terminal is independent of usable output. Only the parser can supply this
// proof after a coherent supported server terminal and complete stream checks.
type responsesOutcome struct {
	text     string
	err      error
	terminal bool
}

// Deliberately closed SSE vocabulary. New provider capabilities must be reviewed
// before admission, rather than silently treated as text or metadata.
func readResponses(r io.Reader) (string, error) {
	return readReservedResponses(r, nil)
}

func readReservedResponses(r io.Reader, reservation *responsesReservation) (string, error) {
	outcome := readResponsesOutcome(r, reservation)
	return outcome.text, outcome.err
}

func readResponsesOutcome(r io.Reader, reservation *responsesReservation) responsesOutcome {
	s := responsesState{sequence: -1, reservation: reservation}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), responsesEventBytes+1)
	var data strings.Builder
	eventName := ""
	eventBytes, events := 0, 0
	ended := false
	for scan.Scan() {
		line := strings.TrimSuffix(scan.Text(), "\r")
		eventBytes += len(line) + 1
		if eventBytes > responsesEventBytes {
			return responsesOutcome{err: ErrOutputLimit}
		}
		if line == "" {
			if data.Len() == 0 && eventName == "" {
				eventBytes = 0
				continue
			}
			events++
			if events > responsesMaxEvents {
				return responsesOutcome{err: ErrOutputLimit}
			}
			if strings.TrimSpace(data.String()) == "[DONE]" {
				if !s.terminal || ended || eventName != "" {
					return responsesOutcome{err: fmt.Errorf("openai: invalid stream termination")}
				}
				ended = true
			} else {
				if s.terminal || ended {
					return responsesOutcome{err: fmt.Errorf("openai: event after completion")}
				}
				var e responsesEvent
				if err := strictResponsesJSON([]byte(data.String()), &e); err != nil {
					return responsesOutcome{err: err}
				}
				if eventName != "" && eventName != e.Type {
					return responsesOutcome{err: fmt.Errorf("openai: mismatched event name")}
				}
				if err := s.accept(e); err != nil {
					return responsesOutcome{err: err}
				}
			}
			data.Reset()
			eventName = ""
			eventBytes = 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			return responsesOutcome{err: fmt.Errorf("openai: malformed event field")}
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			if eventName != "" {
				return responsesOutcome{err: fmt.Errorf("openai: duplicate event name")}
			}
			eventName = value
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		default:
			return responsesOutcome{err: fmt.Errorf("openai: unsupported event field")}
		}
	}
	if err := scan.Err(); err != nil {
		if errors.Is(err, ErrOutputLimit) || errors.Is(err, bufio.ErrTooLong) {
			return responsesOutcome{err: ErrOutputLimit}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return responsesOutcome{err: err}
		}
		return responsesOutcome{err: fmt.Errorf("openai: stream read failed")}
	}
	if data.Len() != 0 || eventName != "" || !s.terminal {
		return responsesOutcome{err: fmt.Errorf("openai: missing complete terminal")}
	}
	return responsesOutcome{text: s.text, err: s.terminalErr, terminal: true}
}

func (s *responsesState) accept(e responsesEvent) error {
	bad := func() error { return fmt.Errorf("openai: inconsistent or unsupported response event") }
	if s.reservation != nil && e.Response != nil && (e.Response.Model != "" && e.Response.Model != s.reservation.model || e.Response.ServiceTier != "" && e.Response.ServiceTier != "default") {
		return errResponsesSpendMismatch
	}
	// Primary examples use both zero and one as the first sequence. Once the
	// stream starts, require consecutive ordering; never infer a missing event.
	first := s.id == "" && e.Type == "response.created"
	if e.Sequence == nil || first && *e.Sequence != 0 && *e.Sequence != 1 || !first && *e.Sequence != s.sequence+1 {
		return bad()
	}
	s.sequence = *e.Sequence
	if e.Type == "response.created" {
		if s.id != "" || e.Response == nil || e.Response.ID == "" || len(e.Response.ID) > 256 || e.Response.Status != "in_progress" || e.Response.Error != nil || len(e.Response.Output) != 0 {
			return bad()
		}
		s.id = e.Response.ID
		return responseConfiguration(e.Response, false)
	}
	if s.id == "" {
		return bad()
	}
	switch e.Type {
	case "response.in_progress":
		if e.Response == nil || e.Response.ID != s.id || e.Response.Status != "in_progress" || e.Response.Error != nil {
			return bad()
		}
		if err := responseConfiguration(e.Response, false); err != nil {
			return err
		}
		for _, raw := range e.Response.Output {
			if _, err := decodeResponsesItem(raw, false); err != nil {
				return err
			}
		}
		return nil
	case "response.output_item.added":
		if e.OutputIndex == nil || *e.OutputIndex != len(s.items) || len(s.items) >= 2 {
			return bad()
		}
		item, err := decodeResponsesItem(e.Item, false)
		if err != nil {
			return err
		}
		if item.ID == "" || len(item.ID) > 256 {
			return bad()
		}
		for _, old := range s.items {
			if old.item.ID == item.ID {
				return bad()
			}
		}
		state := &responsesItemState{item: item}
		if item.Type == "message" {
			if s.message != nil || len(item.Content) != 0 || item.Status != "in_progress" {
				return bad()
			}
			s.message = state
		}
		s.items = append(s.items, state)
		return nil
	case "response.output_item.done":
		state, err := s.target(e, false)
		if err != nil {
			return err
		}
		item, err := decodeResponsesItem(e.Item, true)
		if err != nil {
			return err
		}
		if len(state.done) != 0 || item.ID != state.item.ID || item.Type != state.item.Type {
			return bad()
		}
		if item.Type == "message" && (!state.textDone || item.Content[0].Text != state.text.String()) {
			return bad()
		}
		state.done = append(json.RawMessage(nil), e.Item...)
		return nil
	case "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done":
		state, err := s.target(e, true)
		if err != nil {
			return err
		}
		if len(state.done) != 0 {
			return bad()
		}
		switch e.Type {
		case "response.output_text.delta":
			if state.textDone || len(e.Delta) > responsesTextBytes-state.text.Len() {
				return ErrOutputLimit
			}
			state.text.WriteString(e.Delta)
		case "response.output_text.done":
			if state.textDone || e.Text != state.text.String() {
				return bad()
			}
			state.textDone = true
		default:
			var part responsesPart
			if err := strictResponsesJSON(e.Part, &part); err != nil {
				return err
			}
			if part.Type != "output_text" || len(part.Annotations) != 0 || e.Type == "response.content_part.added" && part.Text != "" || e.Type == "response.content_part.done" && (!state.textDone || part.Text != state.text.String()) {
				return bad()
			}
		}
		return nil
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		state, err := s.target(e, false)
		if err != nil {
			return err
		}
		if state.item.Type != "reasoning" || e.ItemID != state.item.ID || len(state.done) != 0 {
			return bad()
		}
		return nil // Metadata only; never returned or logged.
	case "response.completed":
		r := e.Response
		if s.reservation != nil && !responseSpendMatches(r, *s.reservation) {
			return errResponsesSpendMismatch
		}
		if r == nil || r.ID != s.id || r.Status != "completed" || r.Error != nil || r.Incomplete != nil || len(r.Output) != len(s.items) || s.message == nil {
			return bad()
		}
		if err := responseConfiguration(r, true); err != nil {
			return err
		}
		for i, raw := range r.Output {
			item, err := decodeResponsesItem(raw, true)
			if err != nil {
				return err
			}
			state := s.items[i]
			if item.ID != state.item.ID || len(state.done) == 0 || !sameResponsesJSON(raw, state.done) {
				return bad()
			}
		}
		text := s.message.text.String()
		if strings.TrimSpace(text) == "" || len(text) > responsesTextBytes {
			return bad()
		}
		s.text = text
		s.completed = true
		s.terminal = true
		return nil
	case "response.incomplete", "response.failed":
		r := e.Response
		if r == nil || r.ID != s.id || r.Status != strings.TrimPrefix(e.Type, "response.") {
			return bad()
		}
		if e.Type == "response.incomplete" {
			if r.Incomplete == nil || r.Incomplete.Reason != "max_output_tokens" || r.Error != nil {
				return bad()
			}
		} else if r.Error == nil || r.Incomplete != nil || !supportedTerminalError(r.Error.Code) {
			return bad()
		}
		if r.Model == "" || !modelIdentifier(r.Model) || r.ServiceTier != "default" {
			return bad()
		}
		if s.reservation != nil && (r.Model != s.reservation.model || r.Usage != nil && !responseSpendMatches(r, *s.reservation)) {
			return errResponsesSpendMismatch
		}
		if err := responseConfiguration(r, true); err != nil {
			return err
		}
		// Error output is never returned. Reject tool/foreign/inconsistent items
		// rather than treating an unsupported envelope as termination authority.
		if len(r.Output) != len(s.items) {
			return bad()
		}
		for i, raw := range r.Output {
			state := s.items[i]
			item, err := decodeResponsesItem(raw, len(state.done) != 0)
			if err != nil {
				return err
			}
			if item.Type == "message" && item.Status != "in_progress" && item.Status != "completed" && item.Status != "incomplete" {
				return bad()
			}
			if item.ID != state.item.ID || item.Type != state.item.Type || len(state.done) != 0 && !sameResponsesJSON(raw, state.done) {
				return bad()
			}
		}
		s.terminal = true
		if e.Type == "response.incomplete" {
			s.terminalErr = ErrTruncated
		} else {
			s.terminalErr = responsesErrorCode(r.Error.Code)
		}
		return nil
	case "error":
		return responsesErrorCode(e.Code)
	default:
		return fmt.Errorf("openai: unsupported response event") // Includes every tool/refusal event.
	}
}

func (s *responsesState) target(e responsesEvent, text bool) (*responsesItemState, error) {
	bad := fmt.Errorf("openai: inconsistent output identity")
	if e.OutputIndex == nil || *e.OutputIndex < 0 || *e.OutputIndex >= len(s.items) {
		return nil, bad
	}
	state := s.items[*e.OutputIndex]
	if e.ItemID != "" && e.ItemID != state.item.ID {
		return nil, bad
	}
	if text && (e.ItemID != state.item.ID || state.item.Type != "message" || e.ContentIndex == nil || *e.ContentIndex != 0) {
		return nil, bad
	}
	return state, nil
}

func responseConfiguration(r *responsesResponse, final bool) error {
	if len(r.Tools) != 0 || r.ToolChoice != "" && r.ToolChoice != "none" || r.Store != nil && *r.Store || final && (r.ToolChoice != "none" || r.Store == nil) {
		return fmt.Errorf("openai: unsafe response configuration")
	}
	return nil
}

func decodeResponsesItem(raw json.RawMessage, done bool) (responsesItem, error) {
	var item responsesItem
	if err := strictResponsesJSON(raw, &item); err != nil {
		return item, err
	}
	bad := fmt.Errorf("openai: unsupported output item")
	switch item.Type {
	case "message":
		if item.Role != "assistant" || done && (item.Status != "completed" || len(item.Content) != 1) {
			return item, bad
		}
		for _, part := range item.Content {
			if part.Type != "output_text" || len(part.Annotations) != 0 || len(part.Text) > responsesTextBytes {
				return item, bad
			}
		}
	case "reasoning":
		if len(item.Content) != 0 || done && item.Status != "" && item.Status != "completed" || !done && item.Status != "" && item.Status != "in_progress" {
			return item, bad
		}
		for _, part := range item.Summary {
			if part.Type != "summary_text" {
				return item, bad
			}
		}
	default:
		return item, bad
	}
	return item, nil
}

func supportedTerminalError(code string) bool {
	switch code {
	case "invalid_api_key", "authentication_error", "permission_denied", "rate_limit_exceeded", "insufficient_quota", "content_policy", "server_error":
		return true
	default:
		return false
	}
}

func responsesErrorCode(code string) error {
	switch code {
	case "invalid_api_key", "authentication_error", "permission_denied":
		return statusError("openai", http.StatusUnauthorized)
	case "rate_limit_exceeded", "insufficient_quota":
		return statusError("openai", http.StatusTooManyRequests)
	default:
		return fmt.Errorf("openai: provider error")
	}
}

func sameResponsesJSON(a, b []byte) bool {
	var x, y any
	dx, dy := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	dx.UseNumber()
	dy.UseNumber()
	if dx.Decode(&x) != nil || dy.Decode(&y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

// Bound nesting and refuse duplicate object keys before typed decoding. Do not
// include decoder messages (which can echo input bytes) in returned errors.
func strictResponsesJSON(raw []byte, target any) error {
	bad := fmt.Errorf("openai: malformed event JSON")
	if len(raw) == 0 || len(raw) > responsesEventBytes || !utf8.Valid(raw) {
		return bad
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return bad
		}
		token, err := d.Token()
		if err != nil {
			return bad
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return bad
				}
				name, ok := key.(string)
				// Go's struct decoder accepts case-insensitive field aliases. The API
				// vocabulary is canonical lowercase ASCII: reject aliases before they
				// can shadow type, tools, store or any other capability-bearing field.
				if !ok || !responsesPropertyName(name) || seen[name] {
					return bad
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return bad
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return bad
			}
		default:
			return bad
		}
		return nil
	}
	if value(0) != nil {
		return bad
	}
	if _, err := d.Token(); err != io.EOF {
		return bad
	}
	if json.Unmarshal(raw, target) != nil {
		return bad
	}
	return nil
}

func responsesPropertyName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func responseSpendMatches(r *responsesResponse, admitted responsesReservation) bool {
	if r == nil || r.Model != admitted.model || r.ServiceTier != "default" || r.Usage == nil || r.Usage.Input == nil || r.Usage.Output == nil || r.Usage.Total == nil {
		return false
	}
	i, o, total := *r.Usage.Input, *r.Usage.Output, *r.Usage.Total
	sum, ok := spendAdd(i, o)
	return ok && total == sum && i <= admitted.inputTokens && o <= admitted.outputTokens
}
