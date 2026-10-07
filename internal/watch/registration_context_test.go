// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/fsnotify/fsnotify"
)

func TestRunCancelledDuringDirectoryDiscoveryDoesNotPublish(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	called := false
	go func() {
		done <- runWithDirectoryDiscovery(ctx, Options{
			Root: t.TempDir(),
			OnReindex: func(Pass) (Result, error) {
				called = true
				return Result{}, nil
			},
		}, func(ctx context.Context, root string) ([]string, error) {
			close(entered)
			<-ctx.Done()
			return []string{root}, ctx.Err()
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("watch directory discovery did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil || called {
			t.Fatalf("cancelled registration published or failed shutdown: %v %v", called, err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch controller did not join after discovery cancellation")
	}
}

func TestRegistrationCancellationDoesNotInstallPartialWatchSet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	root := t.TempDir()
	err = addWatchesContext(ctx, w, root, func(string, ...any) {}, func(context.Context, string) ([]string, error) {
		cancel() // cancellation after discovery must still fence registration
		return []string{root}, nil
	})
	if !errors.Is(err, context.Canceled) || len(w.WatchList()) != 0 {
		t.Fatalf("cancelled registration installed watches: %v %v", w.WatchList(), err)
	}
}

func TestRunAlreadyCancelledDoesNotDiscoverDirectories(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := runWithDirectoryDiscovery(ctx, Options{
		OnReindex: func(Pass) (Result, error) { t.Fatal("cancelled startup published"); return Result{}, nil },
	}, func(context.Context, string) ([]string, error) { called = true; return nil, nil })
	if err != nil || called {
		t.Fatalf("already cancelled Run started registration: %v %v", called, err)
	}
}

func TestRunCancellationDuringNewDirectoryDiscoveryDoesNotReconcile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	startup, discovering := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	passes, discoveries := 0, 0 // read only after the run's result synchronizes
	go func() {
		done <- runWithDirectoryDiscovery(ctx, Options{
			Root: root, Reconcile: 0,
			OnReindex: func(Pass) (Result, error) {
				passes++
				if passes == 1 {
					close(startup)
				}
				return Result{}, nil
			},
		}, func(ctx context.Context, root string) ([]string, error) {
			discoveries++
			if discoveries == 1 {
				return vault.DirsContext(ctx, root)
			}
			close(discovering)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	select {
	case <-startup:
	case <-time.After(time.Second):
		t.Fatal("watch did not publish startup")
	}
	if err := os.Mkdir(filepath.Join(root, "new"), 0700); err != nil {
		t.Fatal(err)
	}
	select {
	case <-discovering:
	case <-time.After(time.Second):
		t.Fatal("directory creation did not initiate registration")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil || passes != 1 || discoveries != 2 {
			t.Fatalf("cancelled directory registration reconciled: err=%v passes=%d discoveries=%d", err, passes, discoveries)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not join after directory event cancellation")
	}
}
