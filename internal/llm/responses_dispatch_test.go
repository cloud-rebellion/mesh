// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Every policy value/key/model below is synthetic fixture data, not a selected
// operational limit, account or provider request. Transport always stays TLS
// loopback with the original fixed production URL assertion.
func dispatchPeer(t *testing.T, c *openAIResponses) *openAIResponses {
	t.Helper()
	a, err := newResponsesAdmission("MESH_JUDGE", c.model, c.maxTok)
	if err != nil {
		t.Fatal(err)
	}
	copy := *c
	copy.admission = a
	copy.key = "unit-judge-key"
	return &copy
}

func dispatchResult(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic call did not drain")
	}
}

func TestOpenAIDispatchSharedCapacityAndPacing(t *testing.T) {
	t.Run("different prefixes wait before spending", func(t *testing.T) {
		p := spendFixturePolicy()
		p.MaxInFlight = 1
		arrived, release := make(chan struct{}, 2), make(chan struct{})
		var hits, active, peak atomic.Int32
		c, _ := responsesFixtureClientWithPolicy(t, p, func(w http.ResponseWriter, r *http.Request) {
			n := hits.Add(1)
			a := active.Add(1)
			for old := peak.Load(); a > old && !peak.CompareAndSwap(old, a); old = peak.Load() {
			}
			defer active.Add(-1)
			arrived <- struct{}{}
			if n == 1 {
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			writeResponsesFixture(w, responseFixtureStream(responseFixtureEvents("ok")))
		})
		peer := dispatchPeer(t, c)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		first, second := make(chan error, 1), make(chan error, 1)
		go func() { _, e := c.Complete(ctx, "s", "u"); first <- e }()
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal("first call never reached fixture")
		}
		go func() { _, e := peer.Complete(ctx, "s", "u"); second <- e }()
		time.Sleep(40 * time.Millisecond)
		s := spendFixtureState(t, c.admission.dir)
		if hits.Load() != 1 || s.Reservations != 1 || len(s.Tickets) != 1 {
			t.Fatal("waiting peer spent or dispatched")
		}
		close(release)
		dispatchResult(t, first)
		dispatchResult(t, second)
		s = spendFixtureState(t, c.admission.dir)
		if hits.Load() != 2 || peak.Load() != 1 || s.Reservations != 2 || len(s.Tickets) != 0 {
			t.Fatal("shared capacity or terminal release failed")
		}
		cost, _ := spendModelCost(p.Models[0], 512)
		if s.Reserved != cost*2 {
			t.Fatal("terminal release refunded spending")
		}
	})
	t.Run("restart retains admission spacing", func(t *testing.T) {
		p := spendFixturePolicy()
		p.MinIntervalMS = 60
		var hits atomic.Int32
		c, _ := responsesFixtureClientWithPolicy(t, p, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			writeResponsesFixture(w, responseFixtureStream(responseFixtureEvents("ok")))
		})
		if _, err := c.Complete(context.Background(), "s", "u"); err != nil {
			t.Fatal(err)
		}
		first := spendFixtureState(t, c.admission.dir)
		peer := dispatchPeer(t, c)
		if _, err := peer.Complete(context.Background(), "s", "u"); err != nil {
			t.Fatal(err)
		}
		second := spendFixtureState(t, c.admission.dir)
		if second.LastAdmissionMS-first.LastAdmissionMS < p.MinIntervalMS || second.NextAdmissionMS < second.LastAdmissionMS+p.MinIntervalMS || hits.Load() != 2 {
			t.Fatal("fresh client reset pacing")
		}
	})
}

func dispatchErrorEvents(kind string) []map[string]any {
	e := responseFixtureEvents("partial")
	r := e[7]["response"].(map[string]any)
	if kind == "incomplete" {
		e[7]["type"] = "response.incomplete"
		r["status"] = "incomplete"
		r["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	} else {
		e[7]["type"] = "response.failed"
		r["status"] = "failed"
		r["error"] = map[string]any{"code": "server_error"}
	}
	return e
}

func TestOpenAIDispatchTypedTerminalAndUncertainty(t *testing.T) {
	for _, tc := range []struct {
		name          string
		events        func() []map[string]any
		release, halt bool
		truncated     bool
	}{
		{"completed", func() []map[string]any { return responseFixtureEvents("ok") }, true, false, false},
		{"incomplete terminal", func() []map[string]any { return dispatchErrorEvents("incomplete") }, true, false, true},
		{"failed terminal", func() []map[string]any { return dispatchErrorEvents("failed") }, true, false, false},
		{"missing terminal", func() []map[string]any { return responseFixtureEvents("x")[:1] }, false, false, false},
		{"failed missing configuration", func() []map[string]any {
			e := dispatchErrorEvents("failed")
			delete(e[7]["response"].(map[string]any), "store")
			return e
		}, false, false, false},
		{"failed foreign identity", func() []map[string]any {
			e := dispatchErrorEvents("failed")
			e[7]["response"].(map[string]any)["id"] = "foreign"
			return e
		}, false, false, false},
		{"failed missing model", func() []map[string]any {
			e := dispatchErrorEvents("failed")
			delete(e[7]["response"].(map[string]any), "model")
			return e
		}, false, false, false},
		{"failed missing tier", func() []map[string]any {
			e := dispatchErrorEvents("failed")
			delete(e[7]["response"].(map[string]any), "service_tier")
			return e
		}, false, false, false},
		{"failed unknown code", func() []map[string]any {
			e := dispatchErrorEvents("failed")
			e[7]["response"].(map[string]any)["error"] = map[string]any{"code": "unreviewed"}
			return e
		}, false, false, false},
		{"failed tool configuration", func() []map[string]any {
			e := dispatchErrorEvents("failed")
			e[7]["response"].(map[string]any)["tools"] = []any{map[string]any{"type": "function"}}
			return e
		}, false, false, false},
		{"incomplete usage mismatch", func() []map[string]any {
			e := dispatchErrorEvents("incomplete")
			e[7]["response"].(map[string]any)["usage"] = map[string]any{"input_tokens": 999999, "output_tokens": 20, "total_tokens": 1000019}
			return e
		}, false, true, false},
		{"failed model drift", func() []map[string]any {
			e := dispatchErrorEvents("failed")
			e[7]["response"].(map[string]any)["model"] = "foreign"
			return e
		}, false, true, false},
		{"event after terminal", func() []map[string]any {
			e := dispatchErrorEvents("failed")
			return append(e, map[string]any{"type": "response.in_progress"})
		}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := spendFixturePolicy()
			p.MaxInFlight = 1
			var hits atomic.Int32
			c, _ := responsesFixtureClientWithPolicy(t, p, func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) == 1 {
					writeResponsesFixture(w, responseFixtureStream(tc.events()))
				} else {
					writeResponsesFixture(w, responseFixtureStream(responseFixtureEvents("next")))
				}
			})
			text, err := c.Complete(context.Background(), "s", "u")
			if tc.name == "completed" {
				if err != nil || text != "ok" {
					t.Fatal("completion refused", err)
				}
			} else if err == nil || text != "" {
				t.Fatal("error terminal returned usable content")
			}
			if tc.truncated && !errors.Is(err, ErrTruncated) {
				t.Fatal("supported truncation taxonomy lost")
			}
			s := spendFixtureState(t, c.admission.dir)
			want := 1
			if tc.release {
				want = 0
			}
			if len(s.Tickets) != want || s.Halted != tc.halt || s.Reservations != 1 || s.Reserved <= 0 {
				t.Fatal("wrong terminal capacity/spend outcome")
			}
			if tc.release {
				peer := dispatchPeer(t, c)
				if _, err := peer.Complete(context.Background(), "s", "u"); err != nil {
					t.Fatal(err)
				}
				if hits.Load() != 2 {
					t.Fatal("clean terminal did not restore capacity")
				}
			} else {
				if tc.halt {
					if _, err := newResponsesAdmission("MESH_JUDGE", c.model, c.maxTok); !errors.Is(err, ErrRateLimited) {
						t.Fatal("drift did not halt fresh prefix")
					}
				} else {
					peer := dispatchPeer(t, c)
					ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
					defer cancel()
					if _, err := peer.Complete(ctx, "s", "u"); !errors.Is(err, ErrRateLimited) {
						t.Fatal("unknown terminal was reclaimed", err)
					}
				}
				if hits.Load() != 1 {
					t.Fatal("unknown outcome replayed inference")
				}
			}
		})
	}
}

func TestOpenAIDispatchPreDoAndPersistence(t *testing.T) {
	t.Run("own proven preDo cancellation keeps spend", func(t *testing.T) {
		var hits atomic.Int32
		c, _ := responsesFixtureClient(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var commits int
		c.admission.persist = func(r *os.Root, s responsesSpendState) error {
			err := admissionCommit(r, s)
			commits++
			if commits == 1 {
				cancel()
			}
			return err
		}
		text, err := c.Complete(ctx, "s", "u")
		s := spendFixtureState(t, c.admission.dir)
		if text != "" || !errors.Is(err, context.Canceled) || hits.Load() != 0 || s.Reservations != 1 || s.Reserved <= 0 || len(s.Tickets) != 0 {
			t.Fatal("preDo proof lost or refunded spend", err)
		}
	})
	t.Run("release uncertainty refuses success and fresh client", func(t *testing.T) {
		var hits atomic.Int32
		c, _ := responsesFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			writeResponsesFixture(w, responseFixtureStream(responseFixtureEvents("ok")))
		})
		c.admission.persist = func(r *os.Root, s responsesSpendState) error {
			if len(s.Tickets) == 0 {
				if err := admissionWriteNew(r, "intent", []byte("unit-uncertain-release\n")); err != nil {
					return err
				}
				return errors.New("unit persistence refusal")
			}
			return admissionCommit(r, s)
		}
		text, err := c.Complete(context.Background(), "s", "u")
		if text != "" || !errors.Is(err, ErrRateLimited) || hits.Load() != 1 {
			t.Fatal("uncertain release claimed success")
		}
		if _, err := newResponsesAdmission("MESH_JUDGE", c.model, c.maxTok); !errors.Is(err, ErrRateLimited) {
			t.Fatal("fresh prefix adopted uncertain state")
		}
		s := spendFixtureState(t, c.admission.dir)
		if len(s.Tickets) != 1 || s.Reservations != 1 || s.Reserved <= 0 {
			t.Fatal("uncertain release erased evidence")
		}
	})
	t.Run("nonterminal HTTP and local truncation retain capacity", func(t *testing.T) {
		for _, kind := range []string{"http429", "local truncated", "postDo cancellation"} {
			t.Run(kind, func(t *testing.T) {
				p := spendFixturePolicy()
				p.MaxInFlight = 1
				// The fallback bounds the fixture, not the semantic transition.
				// A short pre-admission timer can expire during fsync and
				// correctly release an owned pre-Do ticket. Prove Do entry.
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				var dispatched atomic.Int32
				c, _ := responsesFixtureClientWithPolicy(t, p, func(w http.ResponseWriter, r *http.Request) {
					dispatched.Add(1)
					if kind == "http429" {
						w.WriteHeader(429)
						return
					}
					writeResponsesFixture(w, responseFixtureStream(responseFixtureEvents("x")[:1]))
					w.(http.Flusher).Flush()
					if kind == "postDo cancellation" {
						cancel() // observed HTTP entry, never a pre-admission timer
					}
					<-r.Context().Done()
				})
				if kind == "local truncated" {
					c.hc.Transport = responsesRoundTrip(func(*http.Request) (*http.Response, error) {
						dispatched.Add(1)
						return nil, ErrTruncated
					})
				}
				text, err := c.Complete(ctx, "s", "u")
				if dispatched.Load() != 1 {
					t.Fatalf("fixture did not prove exactly one post-Do attempt: dispatches=%d err=%v", dispatched.Load(), err)
				}
				if err == nil || text != "" {
					t.Fatal("unknown outcome succeeded")
				}
				if kind == "postDo cancellation" && !errors.Is(err, context.Canceled) {
					t.Fatal("observed handler did not cancel the in-flight request", err)
				}
				s := spendFixtureState(t, c.admission.dir)
				if len(s.Tickets) != 1 || s.Reservations != 1 {
					t.Fatalf("nonterminal error changed ticket/spend: tickets=%d reservations=%d dispatches=%d err=%v", len(s.Tickets), s.Reservations, dispatched.Load(), err)
				}
			})
		}
	})
}

func TestOpenAIDispatchSchema2Refusal(t *testing.T) {
	for _, kind := range []string{"v1", "zero capacity", "zero spacing", "excess capacity", "missing tickets", "null tickets", "backwards millisecond clock"} {
		t.Run(kind, func(t *testing.T) {
			p := spendFixturePolicy()
			switch kind {
			case "zero capacity":
				p.MaxInFlight = 0
			case "zero spacing":
				p.MinIntervalMS = 0
			case "excess capacity":
				p.MaxInFlight = responsesDispatchMaxTickets + 1
			}
			dir := spendFixtureEnv(t, p)
			policyPath, statePath := filepath.Join(dir, "policy.json"), filepath.Join(dir, "state.json")
			if kind == "v1" {
				for _, path := range []string{policyPath, statePath} {
					b, _ := os.ReadFile(path)
					var v map[string]any
					json.Unmarshal(b, &v)
					v["version"] = 1
					for _, key := range []string{"max_in_flight", "min_admission_interval_ms", "last_admission_unix_ms", "next_admission_unix_ms", "outstanding_tickets"} {
						delete(v, key)
					}
					b, _ = json.Marshal(v)
					os.WriteFile(path, b, 0600)
				}
			}
			if kind == "missing tickets" || kind == "null tickets" {
				b, _ := os.ReadFile(statePath)
				var v map[string]any
				json.Unmarshal(b, &v)
				if kind == "missing tickets" {
					delete(v, "outstanding_tickets")
				} else {
					v["outstanding_tickets"] = nil
				}
				b, _ = json.Marshal(v)
				os.WriteFile(statePath, b, 0600)
			}
			if kind == "backwards millisecond clock" {
				s := spendFixtureState(t, dir)
				s.Reservations = 1
				s.Reserved = 1
				s.LastAdmissionMS = time.Now().UnixMilli() + 1000
				s.LastAdmission = s.LastAdmissionMS / 1000
				s.NextAdmissionMS = s.LastAdmissionMS + p.MinIntervalMS
				b, _ := json.Marshal(s)
				os.WriteFile(statePath, b, 0600)
			}
			beforeP, _ := os.ReadFile(policyPath)
			beforeS, _ := os.ReadFile(statePath)
			if _, err := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512); !errors.Is(err, ErrRateLimited) {
				t.Fatal("invalid schema gained authority", err)
			}
			afterP, _ := os.ReadFile(policyPath)
			afterS, _ := os.ReadFile(statePath)
			if sha256.Sum256(beforeP) != sha256.Sum256(afterP) || sha256.Sum256(beforeS) != sha256.Sum256(afterS) {
				t.Fatal("refusal adopted/converted ledger")
			}
		})
	}
}

// Real separate test processes use a fresh synthetic key/ledger and a pinned
// public fixture CA. No HOME/account/config/real credentials are inherited.
func TestOpenAIDispatchChild(t *testing.T) {
	if os.Getenv("MESH_UNIT_DISPATCH_CHILD") != "1" {
		return
	}
	client, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	c := client.(*openAIResponses)
	local, err := url.Parse(os.Getenv("MESH_UNIT_TLS_URL"))
	if err != nil || local.Scheme != "https" || local.Hostname() != "127.0.0.1" {
		t.Fatal("unsafe fixture origin")
	}
	der, err := base64.StdEncoding.DecodeString(os.Getenv("MESH_UNIT_TLS_CERT"))
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, Proxy: nil}
	defer transport.CloseIdleConnections()
	c.hc.Transport = responsesRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != openAIResponsesURL {
			return nil, errors.New("nonfixed fixture request")
		}
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Host = local.Host
		copy.URL = &u
		return transport.RoundTrip(copy)
	})
	ready := os.NewFile(3, "unit-ready")
	if ready == nil {
		t.Fatal("missing fixture readiness pipe")
	}
	if _, err = ready.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}
	ready.Close()
	ms, err := strconv.Atoi(os.Getenv("MESH_UNIT_TIMEOUT_MS"))
	if err != nil || ms < 1 || ms > 5000 {
		t.Fatal("invalid fixture deadline")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(ms)*time.Millisecond)
	defer cancel()
	_, err = c.Complete(ctx, "s", "u")
	if os.Getenv("MESH_UNIT_EXPECT_DEFERRED") == "1" {
		if !errors.Is(err, ErrRateLimited) {
			t.Fatal("expected shared refusal", err)
		}
		fmt.Println("DEFERRED")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("COMPLETE")
}

func dispatchChild(t *testing.T, c *openAIResponses, server *httptest.Server, timeoutMS int, deferred bool) (*exec.Cmd, <-chan error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestOpenAIDispatchChild$", "-test.count=1")
	cmd.Env = []string{"GOMAXPROCS=2", "MESH_UNIT_DISPATCH_CHILD=1", "MESH_OPENAI_SPEND_DIR=" + c.admission.dir, "MESH_CURATOR_AGENT=openai", "MESH_CURATOR_KEY=unit-dedicated-key", "MESH_CURATOR_MODEL=unit-explicit-model", "MESH_CURATOR_MAXTOK=512", "MESH_UNIT_TLS_URL=" + server.URL, "MESH_UNIT_TLS_CERT=" + base64.StdEncoding.EncodeToString(server.Certificate().Raw), "MESH_UNIT_TIMEOUT_MS=" + strconv.Itoa(timeoutMS)}
	if deferred {
		cmd.Env = append(cmd.Env, "MESH_UNIT_EXPECT_DEFERRED=1")
	}
	cmd.ExtraFiles = []*os.File{w}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err = cmd.Start(); err != nil {
		r.Close()
		w.Close()
		t.Fatal(err)
	}
	w.Close()
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		err := cmd.Wait()
		if err != nil {
			err = fmt.Errorf("synthetic child: %w; %s", err, output.String())
		}
		done <- err
		close(finished)
	}()
	t.Cleanup(func() {
		select {
		case <-finished:
		default:
			cmd.Process.Kill()
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Error("synthetic child cleanup uncertain")
			}
		}
	})
	r.SetReadDeadline(time.Now().Add(3 * time.Second))
	ready := make([]byte, 5)
	_, err = io.ReadFull(r, ready)
	r.Close()
	if err != nil || string(ready) != "ready" {
		t.Fatal("synthetic child did not initialize", err)
	}
	return cmd, done
}

func TestOpenAIDispatchSeparateProcessAndCrash(t *testing.T) {
	t.Run("another process shares capacity and resumes", func(t *testing.T) {
		p := spendFixturePolicy()
		p.MaxInFlight = 1
		release, arrived := make(chan struct{}), make(chan struct{}, 2)
		var hits atomic.Int32
		c, server := responsesFixtureClientWithPolicy(t, p, func(w http.ResponseWriter, r *http.Request) {
			n := hits.Add(1)
			arrived <- struct{}{}
			if n == 1 {
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			writeResponsesFixture(w, responseFixtureStream(responseFixtureEvents("ok")))
		})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		first := make(chan error, 1)
		go func() { _, err := c.Complete(ctx, "s", "u"); first <- err }()
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal("fixture first arrival missing")
		}
		_, child := dispatchChild(t, c, server, 2000, false)
		time.Sleep(40 * time.Millisecond)
		if hits.Load() != 1 || spendFixtureState(t, c.admission.dir).Reservations != 1 {
			t.Fatal("second process bypassed capacity")
		}
		close(release)
		dispatchResult(t, first)
		dispatchResult(t, child)
		s := spendFixtureState(t, c.admission.dir)
		if hits.Load() != 2 || s.Reservations != 2 || len(s.Tickets) != 0 {
			t.Fatal("cross-process completion did not restore capacity")
		}
	})
	t.Run("killed admitted process is not reclaimed", func(t *testing.T) {
		p := spendFixturePolicy()
		p.MaxInFlight = 1
		arrived := make(chan struct{}, 1)
		var hits atomic.Int32
		c, server := responsesFixtureClientWithPolicy(t, p, func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			arrived <- struct{}{}
			writeResponsesFixture(w, responseFixtureStream(responseFixtureEvents("x")[:1]))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})
		cmd, done := dispatchChild(t, c, server, 5000, false)
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
			t.Fatal("child never reached fixture")
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("killed child reported success")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("killed child did not drain")
		}
		// The exact old permit remains; no PID/TTL inference or account discovery.
		_, next := dispatchChild(t, c, server, 60, true)
		dispatchResult(t, next)
		s := spendFixtureState(t, c.admission.dir)
		if hits.Load() != 1 || s.Reservations != 1 || len(s.Tickets) != 1 {
			t.Fatal("restart erased outstanding inference/spend")
		}
	})
}

func TestOpenAIDispatchWaitingCancellationAndExpiredTicket(t *testing.T) {
	p := spendFixturePolicy()
	p.MaxInFlight = 1
	dir := spendFixtureEnv(t, p)
	a, err := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = a.reserve(ctx, 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	peer, err := newResponsesAdmission("MESH_JUDGE", "unit-explicit-model", 512)
	if err != nil {
		t.Fatal(err)
	}
	blocked, stop := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer stop()
	if _, err = peer.reserve(blocked, 1); !errors.Is(err, ErrRateLimited) {
		t.Fatal("deadline recycled outstanding permit", err)
	}
	canceled, abort := context.WithCancel(context.Background())
	abort()
	if _, err = peer.reserve(canceled, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("waiting cancellation reclassified", err)
	}
	s := spendFixtureState(t, dir)
	if s.Reservations != 1 || len(s.Tickets) != 1 {
		t.Fatal("waiting/expiry spent or reset capacity")
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatal("private path leaked")
	}
}
