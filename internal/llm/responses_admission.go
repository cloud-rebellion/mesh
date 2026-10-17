// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"time"
)

// No market prices, model limits, budget amounts or rollover defaults live here.
// Operators provision one private ledger for ALL OpenAI curator/judge prefixes.
// A reservation is never refunded: vendor cancellation/error outcomes are not
// evidence that inference was free. This bounds local admission under the
// attested policy; it is not a guarantee about the vendor's account invoice.
type responsesSpendPolicy struct {
	Version         int                   `json:"version"`
	BudgetID        string                `json:"budget_id"`
	ScopeID         string                `json:"scope_id"`
	Currency        string                `json:"currency"`
	Budget          int64                 `json:"budget_nano_usd"`
	PeriodStart     int64                 `json:"period_start"`
	PeriodEnd       int64                 `json:"period_end"`
	ReviewedAt      int64                 `json:"reviewed_at"`
	ValidUntil      int64                 `json:"valid_until"`
	MaxAgeSeconds   int64                 `json:"max_age_seconds"`
	PricingURL      string                `json:"pricing_url"`
	ModelsURL       string                `json:"models_url"`
	AllInputCharges bool                  `json:"all_input_charges_included"`
	Prefixes        []string              `json:"prefixes"`
	Models          []responsesSpendModel `json:"models"`
	MaxInFlight     int                   `json:"max_in_flight"`
	MinIntervalMS   int64                 `json:"min_admission_interval_ms"`
}

type responsesSpendModel struct {
	Model              string `json:"model"`
	Tier               string `json:"service_tier"`
	ContextTokens      int64  `json:"context_tokens"`
	MaxOutputTokens    int64  `json:"max_output_tokens"`
	MaxInputBytes      int64  `json:"max_input_bytes"`
	InputTokensPerByte int64  `json:"input_tokens_per_utf8_byte_ceiling"`
	FramingTokens      int64  `json:"framing_token_ceiling"`
	InputRate          int64  `json:"input_nano_usd_per_token_ceiling"`
	OutputRate         int64  `json:"output_nano_usd_per_token_ceiling"`
	FixedRate          *int64 `json:"fixed_nano_usd_per_request_ceiling"`
}

type responsesSpendState struct {
	Version         int                       `json:"version"`
	BudgetID        string                    `json:"budget_id"`
	ScopeID         string                    `json:"scope_id"`
	PolicySHA       string                    `json:"policy_sha256"`
	Reserved        int64                     `json:"reserved_nano_usd"`
	Reservations    int64                     `json:"reservations"`
	LastAdmission   int64                     `json:"last_admission_unix"`
	Halted          bool                      `json:"halted"`
	LastAdmissionMS int64                     `json:"last_admission_unix_ms"`
	NextAdmissionMS int64                     `json:"next_admission_unix_ms"`
	Tickets         []responsesDispatchTicket `json:"outstanding_tickets"`
}

// Durable permits survive process death. Deadlines never reclaim capacity: a
// local timeout/PID disappearance is not proof that vendor generation ended.
type responsesDispatchTicket struct {
	ID         string `json:"id"`
	Prefix     string `json:"prefix"`
	Model      string `json:"model"`
	AdmittedMS int64  `json:"admitted_unix_ms"`
	DeadlineMS int64  `json:"deadline_unix_ms"`
}

const responsesDispatchMaxTickets = 128 // representation bound, not a selected live limit

type responsesAdmission struct {
	dir, prefix, model, policySHA string
	maxTok                        int
	failed                        atomic.Bool
	// Per-instance deterministic persistence seam; production always uses admissionCommit.
	persist func(*os.Root, responsesSpendState) error
}

type responsesReservation struct {
	model                     string
	inputTokens, outputTokens int64
	validUntil                int64
	ticket                    responsesDispatchTicket
}

// The replacement state and its parent directory have already been synced.
// Removing the intent succeeded, but syncing that cleanup failed. The current
// operation still refuses success; a fresh process may see the committed state
// or a conservatively resurrected intent. This is not an uncertain state write.
type admissionCommittedCleanupError struct{ cause error }

func (e *admissionCommittedCleanupError) Error() string {
	return "openai: durable state committed; marker cleanup uncertain"
}

func (e *admissionCommittedCleanupError) Unwrap() error { return e.cause }

// Admission failures stop a pass using existing backoff semantics. They are
// never ErrAuth (a malformed prompt must not masquerade as an API key failure).
func spendRefused() error {
	return fmt.Errorf("openai: spending admission unavailable: %w", ErrRateLimited)
}

func newResponsesAdmission(prefix, model string, maxTok int) (*responsesAdmission, error) {
	dir := os.Getenv("MESH_OPENAI_SPEND_DIR") // global: no per-prefix budget bypass
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, spendRefused()
	}
	a := &responsesAdmission{dir: dir, prefix: prefix, model: model, maxTok: maxTok, persist: admissionCommit}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, release, err := admissionOpenLocked(ctx, dir)
	if err != nil {
		return nil, spendRefused()
	}
	defer release()
	p, hash, state, err := a.load(r, time.Now().Unix())
	if err != nil {
		return nil, spendRefused()
	}
	if _, _, err := a.cost(p, state, 0); err != nil {
		return nil, spendRefused()
	}
	a.policySHA = hash
	return a, nil
}

func strictAdmissionJSON(b []byte, dst any) error {
	if err := strictResponsesJSON(b, dst); err != nil {
		return err
	}
	if !admissionFieldsPresent(b, reflect.TypeOf(dst).Elem()) {
		return errors.New("incomplete admission JSON")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("invalid admission JSON")
	}
	return nil
}

// State must never acquire zero-balance authority from omitted/null fields.
func admissionFieldsPresent(raw json.RawMessage, t reflect.Type) bool {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	if t.Kind() == reflect.Pointer {
		return admissionFieldsPresent(raw, t.Elem())
	}
	if t.Kind() == reflect.Struct {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || len(fields) != t.NumField() {
			return false
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			v, ok := fields[f.Tag.Get("json")]
			if !ok || !admissionFieldsPresent(v, f.Type) {
				return false
			}
		}
	}
	if t.Kind() == reflect.Slice {
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return false
		}
		for _, v := range values {
			if !admissionFieldsPresent(v, t.Elem()) {
				return false
			}
		}
	}
	return true
}

func (a *responsesAdmission) load(r *os.Root, now int64) (responsesSpendPolicy, string, responsesSpendState, error) {
	var p responsesSpendPolicy
	var s responsesSpendState
	bad := errors.New("invalid admission state")
	if err := admissionInventory(r); err != nil {
		return p, "", s, bad
	}
	b, err := admissionRead(r, "policy.json", 16<<10)
	if err != nil || strictAdmissionJSON(b, &p) != nil {
		return p, "", s, bad
	}
	h := sha256.Sum256(b)
	hash := hex.EncodeToString(h[:])
	if a.policySHA != "" && hash != a.policySHA {
		return p, "", s, bad
	}
	if !validSpendPolicy(p, now) {
		return p, "", s, bad
	}
	b, err = admissionRead(r, "state.json", 64<<10)
	if err != nil || strictAdmissionJSON(b, &s) != nil {
		return p, "", s, bad
	}
	if s.Version != 2 || s.BudgetID != p.BudgetID || s.ScopeID != p.ScopeID || s.PolicySHA != hash || s.Reserved < 0 || s.Reserved > p.Budget || s.Reservations < 0 || s.Reservations > s.Reserved || s.Halted || s.LastAdmission > now || s.Reservations == 0 && (s.Reserved != 0 || s.LastAdmission != 0) || s.Reservations > 0 && (s.LastAdmission < p.PeriodStart || s.LastAdmission >= p.PeriodEnd) || !validDispatchState(p, s, time.Now().UnixMilli()) {
		return p, "", s, bad
	}
	return p, hash, s, nil
}

func validSpendPolicy(p responsesSpendPolicy, now int64) bool {
	if p.Version != 2 || !spendIdentifier(p.BudgetID) || !spendIdentifier(p.ScopeID) || p.Currency != "USD" || p.Budget <= 0 || p.PeriodStart <= 0 || p.PeriodStart > now || p.PeriodEnd <= now || p.PeriodEnd > math.MaxInt64/1000 || p.ReviewedAt < p.PeriodStart || p.ReviewedAt > now || p.ValidUntil <= now || p.ValidUntil > p.PeriodEnd || p.MaxAgeSeconds <= 0 || p.ValidUntil-p.ReviewedAt > p.MaxAgeSeconds || now-p.ReviewedAt > p.MaxAgeSeconds || p.PricingURL != "https://developers.openai.com/api/docs/pricing" || p.ModelsURL != "https://developers.openai.com/api/docs/models" || !p.AllInputCharges || len(p.Prefixes) == 0 || len(p.Prefixes) > 4 || len(p.Models) == 0 || len(p.Models) > 8 || p.MaxInFlight < 1 || p.MaxInFlight > responsesDispatchMaxTickets || p.MinIntervalMS < 1 || p.MinIntervalMS > math.MaxInt64/int64(time.Millisecond) {
		return false
	}
	seen := map[string]bool{}
	for _, prefix := range p.Prefixes {
		if seen[prefix] || prefix != "MESH_CURATOR" && prefix != "MESH_JUDGE" && prefix != "MESH_JUDGE2" && prefix != "MESH_JUDGE3" {
			return false
		}
		seen[prefix] = true
	}
	seen = map[string]bool{}
	for _, m := range p.Models {
		if !modelIdentifier(m.Model) || len(m.Model) == 0 || len(m.Model) > 128 || seen[m.Model] || m.Tier != "default" || m.ContextTokens <= 0 || m.MaxOutputTokens <= 0 || m.MaxOutputTokens > responsesMaxTokens || m.MaxOutputTokens >= m.ContextTokens || m.MaxInputBytes <= 0 || m.MaxInputBytes > responsesInputBytes || m.InputTokensPerByte <= 0 || m.FramingTokens <= 0 || m.InputRate <= 0 || m.OutputRate <= 0 || m.FixedRate == nil || *m.FixedRate < 0 {
			return false
		}
		seen[m.Model] = true
		input, ok := spendMultiply(m.MaxInputBytes, m.InputTokensPerByte)
		if !ok {
			return false
		}
		input, ok = spendAdd(input, m.FramingTokens)
		if !ok || input > m.ContextTokens-m.MaxOutputTokens {
			return false
		}
		if _, ok = spendModelCost(m, m.MaxOutputTokens); !ok {
			return false
		}
	}
	return true
}

func validDispatchState(p responsesSpendPolicy, s responsesSpendState, nowMS int64) bool {
	if len(s.Tickets) > p.MaxInFlight || int64(len(s.Tickets)) > s.Reservations {
		return false
	}
	if s.Reservations == 0 {
		return s.LastAdmissionMS == 0 && s.NextAdmissionMS == 0 && len(s.Tickets) == 0
	}
	next, ok := spendAdd(s.LastAdmissionMS, p.MinIntervalMS)
	if !ok || s.LastAdmissionMS/1000 != s.LastAdmission || s.LastAdmissionMS > nowMS || s.NextAdmissionMS < next {
		return false
	}
	seen := map[string]bool{}
	for _, ticket := range s.Tickets {
		id, err := hex.DecodeString(ticket.ID)
		if err != nil || len(id) != 16 || hex.EncodeToString(id) != ticket.ID || seen[ticket.ID] || ticket.AdmittedMS < p.PeriodStart*1000 || ticket.AdmittedMS > s.LastAdmissionMS || ticket.DeadlineMS <= ticket.AdmittedMS || ticket.DeadlineMS > p.ValidUntil*1000 {
			return false
		}
		seen[ticket.ID] = true
		prefix, model := false, false
		for _, v := range p.Prefixes {
			prefix = prefix || v == ticket.Prefix
		}
		for _, v := range p.Models {
			model = model || v.Model == ticket.Model
		}
		if !prefix || !model {
			return false
		}
	}
	return true
}

func spendIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.", c)) {
			return false
		}
	}
	return true
}
func spendMultiply(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a != 0 && b > math.MaxInt64/a {
		return 0, false
	}
	return a * b, true
}
func spendAdd(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}
func spendModelCost(m responsesSpendModel, out int64) (int64, bool) {
	i, ok := spendMultiply(m.ContextTokens, m.InputRate)
	if !ok {
		return 0, false
	}
	o, ok := spendMultiply(out, m.OutputRate)
	if !ok {
		return 0, false
	}
	c, ok := spendAdd(i, o)
	if !ok {
		return 0, false
	}
	return spendAdd(c, *m.FixedRate)
}

func (a *responsesAdmission) cost(p responsesSpendPolicy, s responsesSpendState, inputBytes int) (int64, responsesSpendModel, error) {
	var selected responsesSpendModel
	allowed := false
	for _, v := range p.Prefixes {
		if v == a.prefix {
			allowed = true
		}
	}
	if !allowed {
		return 0, selected, spendRefused()
	}
	for _, m := range p.Models {
		if m.Model == a.model {
			selected = m
		}
	}
	if int64(inputBytes) > selected.MaxInputBytes && selected.Model != "" {
		return 0, selected, fmt.Errorf("openai: input exceeds admitted context envelope")
	}
	if selected.Model == "" || a.maxTok <= 0 || int64(a.maxTok) > selected.MaxOutputTokens {
		return 0, selected, spendRefused()
	}
	c, ok := spendModelCost(selected, int64(a.maxTok))
	if !ok || c <= 0 || c > p.Budget-s.Reserved || s.Reservations == math.MaxInt64 {
		return 0, selected, spendRefused()
	}
	return c, selected, nil
}

func (a *responsesAdmission) reserve(ctx context.Context, inputBytes int) (responsesReservation, error) {
	if a == nil || a.failed.Load() {
		return responsesReservation{}, spendRefused()
	}
	ctx, cancel := context.WithTimeout(ctx, responsesTimeout)
	defer cancel()
	waited := false
	for {
		reservation, wait, err := a.tryReserve(ctx, inputBytes)
		if err != nil || wait == 0 {
			if waited && errors.Is(err, context.DeadlineExceeded) {
				return responsesReservation{}, fmt.Errorf("openai: dispatch admission deferred: %w: %w", ErrRateLimited, err)
			}
			return reservation, err
		}
		waited = true
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.Canceled) {
				return responsesReservation{}, ctx.Err()
			}
			return responsesReservation{}, fmt.Errorf("openai: dispatch admission deferred: %w: %w", ErrRateLimited, ctx.Err())
		case <-timer.C:
		}
	}
}

func (a *responsesAdmission) tryReserve(ctx context.Context, inputBytes int) (responsesReservation, time.Duration, error) {
	if a.failed.Load() {
		return responsesReservation{}, 0, spendRefused()
	}
	if ctx.Err() != nil {
		return responsesReservation{}, 0, ctx.Err()
	}
	r, release, err := admissionOpenLocked(ctx, a.dir)
	if err != nil {
		if ctx.Err() != nil {
			return responsesReservation{}, 0, ctx.Err()
		}
		return responsesReservation{}, 0, spendRefused()
	}
	defer release()
	p, _, s, err := a.load(r, time.Now().Unix())
	if err != nil {
		return responsesReservation{}, 0, spendRefused()
	}
	c, m, err := a.cost(p, s, inputBytes)
	if err != nil {
		return responsesReservation{}, 0, err
	}
	if ctx.Err() != nil {
		return responsesReservation{}, 0, ctx.Err()
	}
	nowMS := time.Now().UnixMilli()
	if len(s.Tickets) >= p.MaxInFlight || nowMS < s.NextAdmissionMS {
		return responsesReservation{}, 10 * time.Millisecond, nil // no spend/ticket while waiting
	}
	next, ok := spendAdd(nowMS, p.MinIntervalMS)
	if !ok {
		return responsesReservation{}, 0, spendRefused()
	}
	deadline := time.Unix(p.ValidUntil, 0)
	if d := time.Now().Add(responsesTimeout); d.Before(deadline) {
		deadline = d
	}
	if d, exists := ctx.Deadline(); exists && d.Before(deadline) {
		deadline = d
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return responsesReservation{}, 0, spendRefused()
	}
	ticket := responsesDispatchTicket{ID: hex.EncodeToString(nonce[:]), Prefix: a.prefix, Model: a.model, AdmittedMS: nowMS, DeadlineMS: deadline.UnixMilli()}
	if ticket.DeadlineMS <= nowMS {
		return responsesReservation{}, 0, spendRefused()
	}
	s.Reserved += c
	s.Reservations++
	s.LastAdmissionMS = nowMS
	s.LastAdmission = nowMS / 1000
	s.NextAdmissionMS = next
	s.Tickets = append(s.Tickets, ticket)
	// Recheck time after lock acquisition and just before persistence.
	if !validSpendPolicy(p, s.LastAdmission) || !validDispatchState(p, s, time.Now().UnixMilli()) {
		return responsesReservation{}, 0, spendRefused()
	}
	if a.persist == nil {
		return responsesReservation{}, 0, spendRefused()
	}
	if err := a.persist(r, s); err != nil {
		a.failed.Store(true)
		return responsesReservation{}, 0, spendRefused()
	}
	if !validSpendPolicy(p, time.Now().Unix()) {
		return responsesReservation{}, 0, spendRefused()
	}
	return responsesReservation{model: a.model, inputTokens: m.ContextTokens, outputTokens: int64(a.maxTok), validUntil: p.ValidUntil, ticket: ticket}, 0, nil
}

// Only Complete owns this opaque permit. Call solely on locally proven pre-Do
// cancellation or a typed, strictly validated terminal outcome. Never on a
// timeout, transport failure, bare ErrTruncated, PID death or ticket deadline.
func (a *responsesAdmission) releaseTicket(ctx context.Context, reservation responsesReservation) error {
	if a == nil || reservation.ticket.ID == "" {
		return spendRefused()
	}
	r, release, err := admissionOpenLocked(ctx, a.dir)
	if err != nil {
		a.failed.Store(true)
		return spendRefused()
	}
	defer release()
	_, _, s, err := a.load(r, time.Now().Unix())
	if err != nil {
		a.failed.Store(true)
		return spendRefused()
	}
	for i, ticket := range s.Tickets {
		if ticket != reservation.ticket {
			continue
		}
		if ticket.Prefix != a.prefix || ticket.Model != a.model || a.persist == nil || ctx.Err() != nil {
			break
		}
		s.Tickets = append(s.Tickets[:i], s.Tickets[i+1:]...)
		if err := a.persist(r, s); err != nil {
			a.failed.Store(true)
			var committed *admissionCommittedCleanupError
			if errors.As(err, &committed) {
				// Terminal/pre-Do proof was already earned and the removal is
				// durable. Suppress this operation's success without claiming
				// that every fresh process must reject the committed release.
				return fmt.Errorf("openai: ticket release committed; cleanup uncertain: %w", ErrRateLimited)
			}
			return spendRefused()
		}
		return nil // spending/pacing debits unchanged
	}
	a.failed.Store(true)
	return spendRefused()
}

func (a *responsesAdmission) halt(ctx context.Context) error {
	a.failed.Store(true)
	r, release, err := admissionOpenLocked(ctx, a.dir)
	if err != nil {
		return spendRefused()
	}
	defer release()
	_, _, s, err := a.load(r, time.Now().Unix())
	if err != nil {
		return spendRefused()
	}
	s.Halted = true
	if admissionCommit(r, s) != nil {
		return spendRefused()
	}
	return spendRefused()
}

// An explicitly selected OpenAI judge must never downgrade to self-grading or
// another backend merely because its key, model or admission policy is invalid.
type openAIDeniedClient struct{}

func (openAIDeniedClient) Describe() string { return "openai/unavailable" }
func (openAIDeniedClient) Complete(context.Context, string, string) (string, error) {
	return "", spendRefused()
}
