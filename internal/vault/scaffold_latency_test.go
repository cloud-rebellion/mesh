// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCanceledIDScanReportsPhaseWithoutUserData(t *testing.T) {
	// No parallelism: this verifies the production default-logger wiring.
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	defer slog.SetDefault(previous)
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := createNoteContext(ctx, root, completeFixtureSpec(NewNoteSpec{Type: TypeDecision, Title: "sensitive-title", Do: "sensitive-body"}),
		func(context.Context, string) (map[string]string, error) {
			// Hold the reversible boundary long enough for the production slow
			// summary, then cancel. No note or empty type directory may remain.
			time.Sleep(1100 * time.Millisecond)
			cancel()
			return nil, ctx.Err()
		}, os.OpenFile)
	if res != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if _, err := os.Stat(filepath.Join(root, "decisions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled write created a directory: %v", err)
	}
	got := output.String()
	for _, want := range []string{"operation=note_plan", "last_phase=id_scan", "operation=note_create", "last_phase=plan", "phases.id_scan="} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, secret := range []string{root, "sensitive-title", "sensitive-body"} {
		if strings.Contains(got, secret) {
			t.Errorf("diagnostic leaked user data")
		}
	}
}
