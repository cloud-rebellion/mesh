// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/syncproto"
	"github.com/bright-interaction/mesh/internal/vault"
)

func nativeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil || os.Mkdir(filepath.Join(dir, ".mesh"), 0700) != nil {
		t.Fatal("private fixture unavailable")
	}
	return dir
}
func TestNativeJoinTLSAcceptedBeforeMetadataFailureAndNeverRedeemedAgain(t *testing.T) {
	dir := nativeFixture(t)
	var joins atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/join":
			joins.Add(1)
			var req syncproto.JoinRequest
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.Invite != "synthetic-invitation" || r.Header.Get("Authorization") != "" {
				t.Error("invalid synthetic join")
			}
			json.NewEncoder(w).Encode(syncproto.JoinResponse{ClientToken: "synthetic-private-native-bearer", User: "fixture-user", VaultID: "fixture-vault"})
		case "/v1/vault":
			if r.Header.Get("Authorization") != "Bearer synthetic-private-native-bearer" {
				t.Error("saved bearer missing")
			}
			w.WriteHeader(503)
		default:
			t.Error("unexpected route")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	factory := func(ctx context.Context, hub, token string) *Client {
		c := NewWithContext(ctx, hub, token)
		c.HTTP = server.Client()
		return c
	}
	receipt, err := joinNativeVaultContext(t.Context(), server.URL, "synthetic-invitation", dir, factory)
	if !errors.Is(err, ErrNativeMetadataPending) || receipt.State != "accepted" || receipt.User != "fixture-user" {
		t.Fatalf("accepted join lost: %+v %v", receipt, err)
	}
	restored, err := ReadNativeJoinReceipt(dir)
	if err != nil || restored.State != "accepted" {
		t.Fatal("completed receipt not durable")
	}
	if strings.Contains(read(t, dir, ".mesh/native-join.json"), "synthetic-private-native-bearer") || strings.Contains(read(t, dir, ".mesh/native-join.json"), "synthetic-invitation") {
		t.Fatal("receipt contains a live capability")
	}
	if info, err := os.Stat(credPath(dir)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential privacy lost")
	}
	second, err := joinNativeVaultContext(t.Context(), server.URL, "synthetic-invitation", dir, factory)
	if err != nil || second.AcceptedAt != receipt.AcceptedAt || joins.Load() != 1 {
		t.Fatal("completed join redeemed invitation twice")
	}
	if _, err = joinNativeVaultContext(t.Context(), server.URL, "different-invitation", dir, factory); err == nil || joins.Load() != 1 {
		t.Fatal("joined identity switched")
	}
	c, _ := readCredentials(dir)
	c.Token = "different-synthetic-device"
	if writeCredentials(dir, c) != nil {
		t.Fatal("fixture rotation failed")
	}
	if _, err = ReadNativeJoinReceipt(dir); !errors.Is(err, ErrNativeJoinUncertain) {
		t.Fatal("receipt mislabeled changed credential generation")
	}
}
func TestNativeJoinUncertainTransportAndRedirectDoNotRetryOrForwardInvite(t *testing.T) {
	for _, scenario := range []string{"lost response", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			dir := nativeFixture(t)
			var calls, foreign atomic.Int32
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1); w.WriteHeader(500) }))
			defer target.Close()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if scenario == "redirect" {
					http.Redirect(w, r, target.URL+"/v1/join", 307)
					return
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
			}))
			defer server.Close()
			factory := func(ctx context.Context, hub, token string) *Client {
				c := NewWithContext(ctx, hub, token)
				c.HTTP = server.Client()
				return c
			}
			if _, err := joinNativeVaultContext(t.Context(), server.URL, "synthetic-invitation", dir, factory); !errors.Is(err, ErrNativeJoinUncertain) {
				t.Fatal("unknown enrollment treated as safe failure")
			}
			if _, err := joinNativeVaultContext(t.Context(), server.URL, "synthetic-invitation", dir, factory); !errors.Is(err, ErrNativeJoinUncertain) || calls.Load() != 1 || foreign.Load() != 0 {
				t.Fatal("uncertain enrollment retried or escaped origin")
			}
		})
	}
}
func TestNativeJoinPrivateAndHTTPSAdmissionPrecedesRedemption(t *testing.T) {
	dir := nativeFixture(t)
	var calls int
	factory := func(context.Context, string, string) *Client { calls++; return nil }
	if _, err := joinNativeVaultContext(t.Context(), "http://plain.test", "invite", dir, factory); !errors.Is(err, ErrNativeInvalid) || calls != 0 {
		t.Fatal("plaintext join issued")
	}
	if os.Chmod(filepath.Join(dir, ".mesh"), 0755) != nil {
		t.Fatal("fixture mode change failed")
	}
	if _, err := joinNativeVaultContext(t.Context(), "https://mesh.test", "invite", dir, factory); !errors.Is(err, ErrNativePrivate) || calls != 0 {
		t.Fatal("public metadata enrollment issued")
	}
}
func TestNativeCanceledSyncReleasesBothLockLevelsAndPreservesOfflineBytes(t *testing.T) {
	dir := nativeFixture(t)
	release, err := acquireVaultSyncLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err = acquireVaultSyncLockContext(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("local wait not canceled")
	}
	release()
	if release, err = acquireVaultSyncLockContext(t.Context(), dir); err != nil {
		t.Fatal("canceled local waiter leaked lock")
	}
	release()
	// Direct OS holder bypasses the package mutex to exercise its other level.
	f, err := os.OpenFile(filepath.Join(dir, ".mesh", "sync.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if lockSyncFile(f) != nil {
		t.Fatal("OS fixture lock failed")
	}
	ctx2, cancel2 := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel2()
	if _, err = acquireVaultSyncLockContext(ctx2, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("OS lock wait not canceled")
	}
	unlockSyncFile(f)
	note, err := vault.CreateNote(dir, vault.NewNoteSpec{Title: "Offline sync fixture", Template: "finding", Summary: "Synthetic local bytes remain intact.", Status: "draft"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(note.Path)
	entered := make(chan struct{})
	releaseHandler := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-releaseHandler:
		}
	}))
	defer server.Close()
	defer close(releaseHandler)
	if writeCredentials(dir, credentials{HubURL: server.URL, Token: "synthetic-private-native-bearer", VaultID: "fixture-vault"}) != nil {
		t.Fatal("credential fixture failed")
	}
	// Only this synthetic fixture supplies its private CA; production uses normal
	// system TLS. No global live credential/host is consulted.
	original := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = original }()
	ctx3, cancel3 := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := SyncVaultContext(ctx3, dir); done <- err }()
	<-entered
	cancel3()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sync cancellation lost: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sync request not canceled")
	}
	after, _ := os.ReadFile(note.Path)
	if string(before) != string(after) {
		t.Fatal("canceled sync changed offline content")
	}
	if release, err = acquireVaultSyncLockContext(t.Context(), dir); err != nil {
		t.Fatal("canceled request retained sync lock")
	}
	release()
}
func TestNativeTLSNormalSyncKeepsRejectedScopeWriteDirty(t *testing.T) {
	dir := nativeFixture(t)
	note, err := vault.CreateNote(dir, vault.NewNoteSpec{Title: "Native offline edit", Template: "finding", Summary: "Synthetic edit outside allowed remote scope.", Status: "draft"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(note.Path)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-private-native-bearer" {
			t.Error("authentication missing")
		}
		if r.URL.Path == "/v1/vault" {
			json.NewEncoder(w).Encode(syncproto.VaultInfo{VaultID: "fixture-vault"})
			return
		}
		if r.URL.Path != "/v1/sync" {
			t.Error("unexpected endpoint")
			w.WriteHeader(404)
			return
		}
		var req syncproto.SyncRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("bad sync")
		}
		requests.Add(1)
		rejected := []string{}
		for _, o := range req.Outbox {
			rejected = append(rejected, o.Path)
		}
		json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: strings.Repeat("a", 40), Rejected: rejected})
	}))
	defer server.Close()
	if writeCredentials(dir, credentials{HubURL: server.URL, Token: "synthetic-private-native-bearer", VaultID: "fixture-vault"}) != nil || writeState(dir, syncState{HeadSHA: strings.Repeat("b", 40), Hashes: map[string]string{}, VaultID: "fixture-vault", HubURL: server.URL}) != nil {
		t.Fatal("join fixture unavailable")
	}
	// Use a complete local note so normal preflight sends the synthetic write.
	spec := vault.NewNoteSpec{Title: "Native offline edit", Template: "finding", Summary: "Synthetic edit outside allowed remote scope.", Sections: map[string]string{"question": "Can remote rejection lose local content?", "findings": "The fixture refuses its outgoing note.", "evidence": "A synthetic TLS response reports the rejected path.", "limitations": "This is transport/retention, not actual hub policy acceptance.", "next_steps": "Retain the edit for review."}}
	// Replace fixture bytes through the normal draft revision fence.
	snap, err := vault.DraftSnapshotContext(t.Context(), dir, note.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec.DraftID = note.ID
	spec.DraftRevision = snap.Revision
	spec.Status = "active"
	if _, err = vault.CreateNote(dir, spec); err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(note.Path)
	original := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = original }()
	sum, err := SyncNativeVaultContext(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Rejected) != 1 || requests.Load() != 1 {
		t.Fatalf("rejection not exercised: %+v", sum)
	}
	after, _ := os.ReadFile(note.Path)
	if string(before) != string(after) {
		t.Fatal("remote refusal discarded edit")
	}
	if pending, err := NativePendingCount(t.Context(), dir); err != nil || pending != 1 {
		t.Fatal("refused note marked synced")
	}
	// Prove fixture is content-bearing, not just an empty queue assertion.
	if len(before) == 0 {
		t.Fatal("empty fixture")
	}
}

func TestNativeJoinedInfoPrivateLegacyPairAndCanceledLock(t *testing.T) {
	dir := nativeFixture(t)
	if writeCredentials(dir, credentials{HubURL: "https://mesh.invalid", Token: "synthetic-no-network-bearer", VaultID: "legacy-empty"}) != nil || writeState(dir, syncState{HubURL: "https://mesh.invalid", VaultID: "legacy-empty", Hashes: map[string]string{}}) != nil {
		t.Fatal("fixture pair")
	}
	release, err := acquireVaultSyncLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := ReadNativeJoinedInfo(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
		release()
		t.Fatal("joined-info lock ignored cancellation")
	}
	release()
	info, err := ReadNativeJoinedInfo(t.Context(), dir)
	if err != nil || !info.Joined || info.VaultID != "legacy-empty" || info.User != "" || strings.Contains(string(mustNativeJSON(info)), "synthetic-no-network-bearer") {
		t.Fatal("joined metadata invented user or exposed bearer")
	}
}
func mustNativeJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }
