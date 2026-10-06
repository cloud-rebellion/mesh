// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
)

func TestHTTPWritePreparationTimesOutWithoutLatePublication(t *testing.T) {
	s := newTestServer(t)
	s.writePrepareTimeout = 25 * time.Millisecond
	returned := make(chan struct{})
	s.SetNotePublisher(func(ctx context.Context, spec vault.NewNoteSpec) (*vault.CreateResult, error) {
		defer close(returned)
		if _, ok := ctx.Deadline(); !ok {
			t.Error("HTTP request without a caller deadline must still bound preparation")
			return nil, errors.New("missing preparation deadline")
		}
		<-ctx.Done() // stalled preparation; no durable publication has begun
		// Resuming the actual creator with that cancelled context must not create
		// even an empty type directory or a late note after the response.
		return vault.CreateNoteContext(ctx, s.vaultRoot, spec)
	})
	requestBody := string(mustJSON(map[string]any{"jsonrpc":"2.0", "id":1, "method":"tools/call", "params":map[string]any{"name":"mesh_append_note", "arguments": fixtureNote("Never publish after deadline")}}))
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(requestBody))
	w := httptest.NewRecorder()
	start := time.Now()
	s.HandleHTTP(w, r)
	if time.Since(start) > 2*time.Second {
		t.Fatal("preparation did not stop within its bounded fixture window")
	}
	select {
	case <-returned:
	default:
		t.Fatal("publisher was detached instead of joined")
	}
	var reply response
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || reply.Error == nil {
		t.Fatalf("expected a failed pre-publication receipt: %s (%v)", w.Body.String(), err)
	}
	if _, err := os.Stat(filepath.Join(s.vaultRoot, "notes", "never-publish-after-deadline.md")); !os.IsNotExist(err) {
		t.Fatalf("deadline left a published note: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.vaultRoot, "notes")); !os.IsNotExist(err) {
		t.Fatalf("cancelled preparation created a type directory: %v", err)
	}
}

func TestWritePreparationUsesProductionDeadline(t *testing.T) {
	s := newTestServer(t) // leave the per-server override unset
	var preparation context.Context
	started := time.Now()
	s.SetNotePublisher(func(ctx context.Context, _ vault.NewNoteSpec) (*vault.CreateResult, error) {
		preparation = ctx
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Before(started.Add(15*time.Second)) || deadline.After(time.Now().Add(15*time.Second)) {
			t.Errorf("production preparation deadline = %v; want 15 seconds", deadline)
		}
		return nil, errors.New("fixture preparation failure")
	})
	_, rerr := s.toolWrite(context.Background(), mustJSON(map[string]any{
		"title": "Production deadline", "summary": "Publication deadline fixture.", "sections": fixtureSections("note"),
	}), "")
	if rerr == nil || preparation == nil || preparation.Err() != context.Canceled {
		t.Fatalf("default preparation context was not cancelled after return: %v", rerr)
	}
}

func TestExpiredPreparationCannotUndoDurableSuccess(t *testing.T) {
	s := newTestServer(t)
	s.writePrepareTimeout = time.Second
	if err := s.store.Close(); err != nil {
		t.Fatal(err) // deterministic stale-index receipt, without another writer
	}
	s.SetNotePublisher(func(ctx context.Context, spec vault.NewNoteSpec) (*vault.CreateResult, error) {
		res, err := vault.CreateNoteContext(ctx, s.vaultRoot, spec)
		if err != nil {
			return nil, err
		}
		<-ctx.Done() // publication completed before its preparation budget expired
		return res, nil
	})
	res, rerr := s.toolWrite(context.Background(), mustJSON(map[string]any{
		"title": "Durable despite expired preparation", "type": "note", "summary": "Publication deadline fixture.", "sections": fixtureSections("note"),
	}), "")
	if rerr != nil {
		t.Fatalf("durable publication was misreported as a failed write: %v", rerr)
	}
	receipt := toolText(t, res.(map[string]any))
	if receipt["index_stale"] != true {
		t.Fatalf("expected saved-but-stale receipt: %#v", receipt)
	}
	if _, err := os.Stat(filepath.Join(s.vaultRoot, receipt["path"].(string))); err != nil {
		t.Fatal(err)
	}
}

func TestWritePreparationKeepsEarlierCallerDeadlineAndCancelsTimer(t *testing.T) {
	s := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	callerDeadline, _ := ctx.Deadline()
	var preparation context.Context
	s.SetNotePublisher(func(ctx context.Context, _ vault.NewNoteSpec) (*vault.CreateResult, error) {
		preparation = ctx
		if got, ok := ctx.Deadline(); !ok || !got.Equal(callerDeadline) {
			t.Errorf("preparation widened the caller deadline: %v", got)
		}
		return nil, errors.New("fixture preparation failure")
	})
	_, rerr := s.toolWrite(ctx, mustJSON(map[string]any{"title": "Caller deadline", "summary": "Publication deadline fixture.", "sections": fixtureSections("note")}), "")
	if rerr == nil || preparation == nil || preparation.Err() != context.Canceled {
		t.Fatalf("preparation timer was not cancelled after return: %v", rerr)
	}
	if ctx.Err() != nil {
		t.Fatal("preparation cancellation cancelled the caller context")
	}
}

func TestExpiredPreparationStillAcknowledgesHealthyOwner(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)
	s.writePrepareTimeout = time.Second
	s.SetNotePublisher(func(ctx context.Context, spec vault.NewNoteSpec) (*vault.CreateResult, error) {
		res, err := vault.CreateNoteContext(ctx, s.vaultRoot, spec)
		if err != nil {
			return nil, err
		}
		<-ctx.Done()
		return res, nil
	})
	res, rerr := s.toolWrite(context.Background(), mustJSON(map[string]any{
		"title": "Independent acknowledgement deadline", "summary": "Publication deadline fixture.", "sections": fixtureSections("note"),
	}), "")
	if rerr != nil {
		t.Fatalf("durable publication failed: %v", rerr)
	}
	receipt := toolText(t, res.(map[string]any))
	if _, present := receipt["index_stale"]; present {
		t.Fatalf("expired preparation was reused for the independent owner acknowledgement: %#v", receipt)
	}
	if _, err := s.store.NotePath(receipt["id"].(string)); err != nil {
		t.Fatalf("acknowledged note is not queryable: %v", err)
	}
}
