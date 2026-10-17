//go:build linux || darwin

// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package llm

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAIAdmissionReleaseDirectorySyncPhases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase int
	}{
		{"after intent write", 1},
		{"after state rename before directory durability", 2},
		{"after durable state and intent removal", 3},
		{"fully synced control", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := spendFixturePolicy()
			p.MaxInFlight = 1
			var hits atomic.Int32
			c, _ := responsesFixtureClientWithPolicy(t, p, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				writeResponsesFixture(w, responseFixtureStream(responseFixtureEvents("verified terminal")))
			})
			fault := errors.New("unit directory sync fault")
			var releaseErr error
			var syncCalls int
			c.admission.persist = func(r *os.Root, s responsesSpendState) error {
				if len(s.Tickets) != 0 {
					return admissionCommit(r, s)
				}
				releaseErr = admissionCommitWithDirectorySync(r, s, func(root *os.Root) error {
					syncCalls++
					if syncCalls == tc.phase {
						return fault
					}
					return admissionSyncDirectory(root)
				})
				return releaseErr
			}
			text, err := c.Complete(context.Background(), "s", "u")
			s := spendFixtureState(t, c.admission.dir)
			if hits.Load() != 1 || s.Reservations != 1 || s.Reserved <= 0 || s.Halted {
				t.Fatal("terminal/persistence changed spending or remote dispatch", err)
			}
			var committed *admissionCommittedCleanupError
			classified := errors.As(releaseErr, &committed)
			if classified != (tc.phase == 3) || tc.phase != 0 && !errors.Is(releaseErr, fault) {
				t.Fatal("state durability phase misclassified", releaseErr)
			}
			wantCalls := tc.phase
			if wantCalls == 0 {
				wantCalls = 3
			}
			if syncCalls != wantCalls {
				t.Fatal("did not reach actual requested commit phase", syncCalls)
			}
			if tc.phase == 0 {
				if text != "verified terminal" || err != nil || c.admission.failed.Load() {
					t.Fatal("normal durable terminal control failed", err)
				}
			} else if text != "" || !errors.Is(err, ErrRateLimited) || !c.admission.failed.Load() {
				t.Fatal("uncertain cleanup/write acknowledged current operation", err)
			}
			intent, intentErr := os.Lstat(filepath.Join(c.admission.dir, "intent"))
			if tc.phase == 1 || tc.phase == 2 {
				if intentErr != nil || !intent.Mode().IsRegular() || intent.Mode().Perm() != 0600 {
					t.Fatal("early uncertainty lost intent boundary", intentErr)
				}
				if _, freshErr := newResponsesAdmission("MESH_JUDGE", c.model, c.maxTok); !errors.Is(freshErr, ErrRateLimited) {
					t.Fatal("fresh prefix adopted uncertain state before durable commit", freshErr)
				}
			} else {
				if !errors.Is(intentErr, os.ErrNotExist) || len(s.Tickets) != 0 {
					t.Fatal("committed state release or normal cleanup missing", intentErr)
				}
				fresh, freshErr := newResponsesAdmission("MESH_JUDGE", c.model, c.maxTok)
				if freshErr != nil {
					t.Fatal("fresh prefix refused already durable terminal release", freshErr)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if _, freshErr = fresh.reserve(ctx, 1); freshErr != nil {
					t.Fatal("durable terminal release failed to recover capacity", freshErr)
				}
				if tc.phase == 3 && !strings.Contains(err.Error(), "ticket release committed") {
					t.Fatal("late phase error lost durability qualification", err)
				}
			}
		})
	}
}

func TestOpenAIAdmissionReserveLateCleanupKeepsTicket(t *testing.T) {
	p := spendFixturePolicy()
	p.MaxInFlight = 1
	dir := spendFixtureEnv(t, p)
	a, err := newResponsesAdmission("MESH_CURATOR", "unit-explicit-model", 512)
	if err != nil {
		t.Fatal(err)
	}
	var syncCalls int
	a.persist = func(r *os.Root, s responsesSpendState) error {
		return admissionCommitWithDirectorySync(r, s, func(root *os.Root) error {
			syncCalls++
			if syncCalls == 3 {
				return errors.New("unit final cleanup fault")
			}
			return admissionSyncDirectory(root)
		})
	}
	reservation, err := a.reserve(context.Background(), 1)
	if !errors.Is(err, ErrRateLimited) || reservation.ticket.ID != "" || !a.failed.Load() || syncCalls != 3 {
		t.Fatal("uncertain admission dispatched/returned an owned permit", err)
	}
	s := spendFixtureState(t, dir)
	if s.Reservations != 1 || s.Reserved <= 0 || len(s.Tickets) != 1 {
		t.Fatal("uncertain admission erased committed spending/ticket")
	}
	fresh, err := newResponsesAdmission("MESH_JUDGE", "unit-explicit-model", 512)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err = fresh.reserve(ctx, 1); !errors.Is(err, ErrRateLimited) {
		t.Fatal("late admission cleanup invented terminal proof/reclaimed capacity", err)
	}
	if got := spendFixtureState(t, dir); got.Reservations != 1 || got.Reserved != s.Reserved || len(got.Tickets) != 1 {
		t.Fatal("fresh deferred attempt spent/reclaimed uncertain admission")
	}
}
