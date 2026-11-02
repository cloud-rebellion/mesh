// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/syncproto"
)

// The transport fixture deliberately has no ordinary sync endpoint. The tested
// client must use the same operation after ACK loss and cannot hide broad writes.
type selectedFixture struct {
	snapshot                                   syncproto.CurationSnapshot
	receipt                                    *syncproto.CurationOperationReceipt
	snapshots, posts, lookups, syncs, provider int
	loseACK                                    bool
	deny                                       bool
	unsupported                                bool
	candidate                                  string
}

func newSelectedFixture(t *testing.T) (string, *selectedFixture) {
	t.Helper()
	root := t.TempDir()
	f := &selectedFixture{candidate: "verified merged bytes"}
	winner := []byte("approved hub winner")
	job := syncproto.CurationJob{ID: 7, Path: "notes/target.md", Status: "pending", IncomingB64: base64.StdEncoding.EncodeToString([]byte("loser evidence")), HeadSHA: "old-head", User: "writer", CreatedAt: 123}
	f.snapshot = syncproto.CurationSnapshot{Protocol: 1, VaultID: "vault-selected", ClientID: 23, Job: job, JobFingerprint: syncproto.CurationJobFingerprint(job), HeadSHA: strings.Repeat("b", 40), NoteExists: true, NoteSHA256: contentHash(winner), NoteB64: base64.StdEncoding.EncodeToString(winner)}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sync" {
			f.syncs++
			http.Error(w, "whole sync forbidden", 500)
			return
		}
		if f.unsupported {
			http.NotFound(w, r)
			return
		}
		if f.deny {
			http.Error(w, "forbidden", 403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/snapshot") {
			f.snapshots++
			_ = json.NewEncoder(w).Encode(f.snapshot)
			return
		}
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/operations/") {
			f.lookups++
			if f.receipt != nil {
				_ = json.NewEncoder(w).Encode(f.receipt)
				return
			}
			op := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_ = json.NewEncoder(w).Encode(syncproto.CurationOperationReceipt{Protocol: 1, VaultID: f.snapshot.VaultID, ClientID: f.snapshot.ClientID, JobID: 7, Path: f.snapshot.Job.Path, OperationID: op, Status: "not_found"})
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/operations") {
			f.posts++
			var q syncproto.CurationOperationRequest
			if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
				t.Error(err)
				http.Error(w, "bad", 400)
				return
			}
			if f.receipt == nil {
				status, jobStatus, head, content, attempts := "resolved", "resolved", strings.Repeat("a", 40), q.CandidateB64, f.snapshot.Job.Attempts
				if q.Action == syncproto.CurationActionNoop {
					content = f.snapshot.NoteB64
					head = f.snapshot.HeadSHA
				}
				if q.Action == syncproto.CurationActionReject {
					status, jobStatus, head, content = "rejected", "pending", "", ""
					attempts++
				}
				f.receipt = &syncproto.CurationOperationReceipt{Protocol: 1, VaultID: f.snapshot.VaultID, ClientID: f.snapshot.ClientID, JobID: 7, Path: f.snapshot.Job.Path, OperationID: q.OperationID, RequestSHA256: syncproto.CurationRequestSHA256(q), JobFingerprint: q.JobFingerprint, ExpectedExists: q.ExpectedExists, ExpectedSHA256: q.ExpectedSHA256, Action: q.Action, CandidateSHA256: q.CandidateSHA256, Status: status, LandedHead: head, ContentB64: content, JobStatus: jobStatus, Attempts: attempts}
			}
			if f.loseACK {
				f.loseACK = false
				http.Error(w, "lost acknowledgement", 503)
				return
			}
			_ = json.NewEncoder(w).Encode(f.receipt)
			return
		}
		t.Errorf("unexpected selected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	if err := writeCredentials(root, credentials{HubURL: ts.URL, Token: "fixture-only", VaultID: f.snapshot.VaultID}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(root, syncState{HeadSHA: "global-unrelated-cursor", TombSeq: 55, VaultID: f.snapshot.VaultID, HubURL: ts.URL, Hashes: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	return root, f
}
func (f *selectedFixture) prepare(_ syncproto.CurationSnapshot, start func() error) (syncproto.CurationOperationRequest, error) {
	if err := start(); err != nil {
		return syncproto.CurationOperationRequest{}, err
	}
	f.provider++
	b := []byte(f.candidate)
	return syncproto.CurationOperationRequest{Action: syncproto.CurationActionPublish, ModelB64: base64.StdEncoding.EncodeToString([]byte("model bytes")), CandidateB64: base64.StdEncoding.EncodeToString(b), CandidateSHA256: contentHash(b)}, nil
}
func selectedWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedOperationPreservesUnsentTargetUnrelatedDirtyAndDelete(t *testing.T) {
	for _, kind := range []string{"clean target", "unsent target", "deleted target", "new vault"} {
		t.Run(kind, func(t *testing.T) {
			root, f := newSelectedFixture(t)
			state := readState(root)
			state.Hashes["unrelated/deleted.md"] = contentHash([]byte("tracked deletion"))
			state.Hashes["unrelated/edit.md"] = contentHash([]byte("old unrelated"))
			selectedWrite(t, root, "unrelated/edit.md", "unsent unrelated edit")
			switch kind {
			case "clean target":
				selectedWrite(t, root, f.snapshot.Job.Path, "approved hub winner")
				state.Hashes[f.snapshot.Job.Path] = f.snapshot.NoteSHA256
			case "unsent target":
				selectedWrite(t, root, f.snapshot.Job.Path, "unsent target editor bytes")
				state.Hashes[f.snapshot.Job.Path] = f.snapshot.NoteSHA256
			case "deleted target":
				state.Hashes[f.snapshot.Job.Path] = f.snapshot.NoteSHA256
			}
			if err := writeState(root, state); err != nil {
				t.Fatal(err)
			}
			beforeState := readState(root)
			result, err := RunSelectedCuration(context.Background(), root, 7, f.prepare)
			if err != nil || result.Receipt.Status != "resolved" || f.provider != 1 || f.posts != 1 || f.syncs != 0 {
				t.Fatalf("selected result %+v fixture %+v err %v", result, f, err)
			}
			b, err := os.ReadFile(filepath.Join(root, "unrelated/edit.md"))
			if err != nil || string(b) != "unsent unrelated edit" {
				t.Fatal("unrelated dirty bytes changed")
			}
			if _, err := os.Stat(filepath.Join(root, "unrelated/deleted.md")); !os.IsNotExist(err) {
				t.Fatal("unrelated deletion restored")
			}
			after := readState(root)
			if after.HeadSHA != beforeState.HeadSHA || after.TombSeq != beforeState.TombSeq || after.Hashes["unrelated/edit.md"] != beforeState.Hashes["unrelated/edit.md"] || after.Hashes["unrelated/deleted.md"] != beforeState.Hashes["unrelated/deleted.md"] {
				t.Fatalf("unrelated sync state advanced: %+v", after)
			}
			got, err := os.ReadFile(filepath.Join(root, f.snapshot.Job.Path))
			if kind == "unsent target" {
				if err != nil || string(got) != "unsent target editor bytes" || !result.Parked || after.Hashes[f.snapshot.Job.Path] != beforeState.Hashes[f.snapshot.Job.Path] {
					t.Fatal("unsent target overwritten/cleared")
				}
			}
			if kind == "deleted target" {
				if !os.IsNotExist(err) || !result.Parked || after.Hashes[f.snapshot.Job.Path] != beforeState.Hashes[f.snapshot.Job.Path] {
					t.Fatal("selected pending deletion overwritten/cleared")
				}
			}
			if kind == "clean target" || kind == "new vault" {
				if err != nil || string(got) != f.candidate || !result.Refreshed {
					t.Fatalf("selected refresh %+v %s %v", result, got, err)
				}
			}
		})
	}
}
func TestSelectedOperationDoesNotScanOrFlushMoreThan4000DirtyNotes(t *testing.T) {
	root, f := newSelectedFixture(t)
	for i := 0; i < 4001; i++ {
		selectedWrite(t, root, fmt.Sprintf("dirty/%04d.md", i), "local-only unsent bytes")
	}
	before := readState(root)
	result, err := RunSelectedCuration(context.Background(), root, 7, f.prepare)
	if err != nil || result.Receipt.Status != "resolved" || f.syncs != 0 {
		t.Fatalf("result %+v err %v", result, err)
	}
	after := readState(root)
	for k, v := range before.Hashes {
		if after.Hashes[k] != v {
			t.Fatal("unrelated hash changed")
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "dirty"))
	if err != nil || len(entries) != 4001 {
		t.Fatal("dirty namespace changed")
	}
}
func TestSelectedOperationReconcilesACKLossAcrossRestartWithoutProviderRepeat(t *testing.T) {
	root, f := newSelectedFixture(t)
	f.loseACK = true
	if _, err := RunSelectedCuration(context.Background(), root, 7, f.prepare); err == nil {
		t.Fatal("lost ACK falsely reported success")
	}
	first := *f.receipt
	result, err := RunSelectedCuration(context.Background(), root, 7, func(syncproto.CurationSnapshot, func() error) (syncproto.CurationOperationRequest, error) {
		t.Fatal("restarted operation repeated provider")
		return syncproto.CurationOperationRequest{}, nil
	})
	if err != nil || !reflect.DeepEqual(result.Receipt, first) || f.provider != 1 || f.posts != 1 || f.lookups != 1 {
		t.Fatalf("restart %+v fixture %+v err %v", result, f, err)
	}
}
func TestSelectedOperationOldServerAndDemotionFailClosed(t *testing.T) {
	t.Run("unsupported 404", func(t *testing.T) {
		root, f := newSelectedFixture(t)
		f.unsupported = true
		if _, err := RunSelectedCuration(context.Background(), root, 7, f.prepare); err == nil || f.provider != 0 || f.syncs != 0 {
			t.Fatal("404 fallback/provider")
		}
	})
	t.Run("demotion after preparation", func(t *testing.T) {
		root, f := newSelectedFixture(t)
		prep := func(s syncproto.CurationSnapshot, start func() error) (syncproto.CurationOperationRequest, error) {
			q, err := f.prepare(s, start)
			f.deny = true
			return q, err
		}
		if _, err := RunSelectedCuration(context.Background(), root, 7, prep); err == nil {
			t.Fatal("demotion accepted")
		}
		if _, err := RunSelectedCuration(context.Background(), root, 7, f.prepare); err == nil || f.provider != 1 || f.posts != 0 || f.syncs != 0 {
			t.Fatal("demotion replayed provider or charged")
		}
	})
}
func TestSelectedOperationLocalCASChangeHoldsWithoutCharging(t *testing.T) {
	root, f := newSelectedFixture(t)
	selectedWrite(t, root, f.snapshot.Job.Path, "approved hub winner")
	prep := func(s syncproto.CurationSnapshot, start func() error) (syncproto.CurationOperationRequest, error) {
		q, err := f.prepare(s, start)
		selectedWrite(t, root, s.Job.Path, "concurrent editor")
		return q, err
	}
	if _, err := RunSelectedCuration(context.Background(), root, 7, prep); !errors.Is(err, ErrSelectedReview) || f.posts != 0 || f.provider != 1 {
		t.Fatalf("local CAS err %v fixture %+v", err, f)
	}
	if _, err := RunSelectedCuration(context.Background(), root, 7, f.prepare); !errors.Is(err, ErrSelectedReview) || f.posts != 0 || f.provider != 1 || f.lookups != 1 {
		t.Fatal("CAS hold replayed provider/publication")
	}
}
func TestSelectedProviderUncertaintySurvivesRestartWithoutCharge(t *testing.T) {
	root, f := newSelectedFixture(t)
	prep := func(_ syncproto.CurationSnapshot, start func() error) (syncproto.CurationOperationRequest, error) {
		if err := start(); err != nil {
			return syncproto.CurationOperationRequest{}, err
		}
		f.provider++
		return syncproto.CurationOperationRequest{}, errors.New("provider transport lost")
	}
	if _, err := RunSelectedCuration(context.Background(), root, 7, prep); err == nil {
		t.Fatal("provider failure accepted")
	}
	if _, err := RunSelectedCuration(context.Background(), root, 7, f.prepare); !errors.Is(err, ErrSelectedReview) || f.provider != 1 || f.posts != 0 || f.lookups != 1 {
		t.Fatalf("uncertain provider replayed: %+v %v", f, err)
	}
}

func TestSelectedOperationDeterministicRejectIsIdempotent(t *testing.T) {
	root, f := newSelectedFixture(t)
	prep := func(_ syncproto.CurationSnapshot, start func() error) (syncproto.CurationOperationRequest, error) {
		if err := start(); err != nil {
			return syncproto.CurationOperationRequest{}, err
		}
		f.provider++
		return syncproto.CurationOperationRequest{Action: syncproto.CurationActionReject, ModelB64: base64.StdEncoding.EncodeToString([]byte("invalid model output")), RejectionReason: "invalid authored output"}, nil
	}
	first, err := RunSelectedCuration(context.Background(), root, 7, prep)
	if err != nil || first.Receipt.Status != "rejected" || first.Receipt.Attempts != 1 {
		t.Fatalf("rejection proof %+v %v", first, err)
	}
	again, err := RunSelectedCuration(context.Background(), root, 7, prep)
	if err != nil || !reflect.DeepEqual(first.Receipt, again.Receipt) || f.posts != 1 || f.provider != 1 || f.lookups != 1 {
		t.Fatalf("rejection charged/recalled twice %+v %+v %v", again, f, err)
	}
}

func TestSelectedReceiptRejectsMismatchedIdentityContentHeadAndAttempt(t *testing.T) {
	changes := map[string]func(*syncproto.CurationOperationReceipt){
		"operation": func(r *syncproto.CurationOperationReceipt) { r.OperationID = strings.Repeat("f", 32) },
		"job":       func(r *syncproto.CurationOperationReceipt) { r.JobID++ },
		"client":    func(r *syncproto.CurationOperationReceipt) { r.ClientID++ },
		"vault":     func(r *syncproto.CurationOperationReceipt) { r.VaultID = "other-vault" },
		"path":      func(r *syncproto.CurationOperationReceipt) { r.Path = "unrelated.md" },
		"request":   func(r *syncproto.CurationOperationReceipt) { r.RequestSHA256 = strings.Repeat("0", 64) },
		"content": func(r *syncproto.CurationOperationReceipt) {
			r.ContentB64 = base64.StdEncoding.EncodeToString([]byte("other bytes"))
		},
		"head":       func(r *syncproto.CurationOperationReceipt) { r.LandedHead = "" },
		"job status": func(r *syncproto.CurationOperationReceipt) { r.JobStatus = "pending" },
		"attempt":    func(r *syncproto.CurationOperationReceipt) { r.Attempts++ },
		"unresolved": func(r *syncproto.CurationOperationReceipt) { r.Status = "prepared" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			root, f := newSelectedFixture(t)
			f.loseACK = true
			selectedWrite(t, root, f.snapshot.Job.Path, "approved hub winner")
			if _, err := RunSelectedCuration(context.Background(), root, 7, f.prepare); err == nil {
				t.Fatal("ACK-loss setup did not hold")
			}
			change(f.receipt)
			result, err := RunSelectedCuration(context.Background(), root, 7, f.prepare)
			if err == nil || result.Refreshed || f.provider != 1 || f.posts != 1 {
				t.Fatalf("bad receipt accepted/replayed %+v %+v %v", result, f, err)
			}
			b, err := os.ReadFile(filepath.Join(root, f.snapshot.Job.Path))
			if err != nil || string(b) != "approved hub winner" {
				t.Fatal("bad receipt changed local bytes")
			}
		})
	}
}

func TestSelectedRefreshPreservesUnknownSyncMetadata(t *testing.T) {
	root, f := newSelectedFixture(t)
	data, err := os.ReadFile(statePath(root))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["future_cursor"] = json.RawMessage(`{"unchanged":"retain this evidence"}`)
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(root), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RunSelectedCuration(context.Background(), root, 7, f.prepare); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(statePath(root))
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	var a, b any
	_ = json.Unmarshal(raw["future_cursor"], &a)
	_ = json.Unmarshal(after["future_cursor"], &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("selected refresh dropped unrelated future metadata")
	}
}

func TestSelectedLocalReadHonorsCancellation(t *testing.T) {
	root, f := newSelectedFixture(t)
	selectedWrite(t, root, f.snapshot.Job.Path, "approved hub winner")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := selectedLocal(ctx, root, f.snapshot.Job.Path); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled local read: %v", err)
	}
}

func TestSelectedStateDocumentValidatesTheBytesItRewrites(t *testing.T) {
	root, f := newSelectedFixture(t)
	creds, err := readCredentials(root)
	if err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{"head_sha": "retain-head", "tomb_seq": 41, "vault_id": "other-vault", "hub_url": creds.HubURL, "hashes": map[string]string{"unrelated.md": "unchanged"}, "future_metadata": map[string]string{"retain": "exact"}}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(root), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeSelectedHash(root, creds, f.snapshot.Job.Path, "candidate"); !errors.Is(err, ErrSelectedReview) {
		t.Fatalf("foreign state identity accepted: %v", err)
	}
	after, err := os.ReadFile(statePath(root))
	if err != nil || string(after) != string(data) {
		t.Fatal("rejected state bytes rewritten")
	}
}
