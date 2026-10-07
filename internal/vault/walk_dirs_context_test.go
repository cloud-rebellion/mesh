// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

func TestDirsContextAlreadyCancelledDoesNotStartTraversal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	paths, err := dirsContext(ctx, t.TempDir(), func(string, fs.WalkDirFunc) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || paths != nil || called {
		t.Fatalf("cancelled discovery started or returned partial paths: %v %v %v", paths, err, called)
	}
}

func TestDirsContextCancellationDropsPartialDirectories(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	workerDone := make(chan struct{})
	attempted, accepted := 0, 0
	paths, err := dirsContext(ctx, root, func(_ string, visit fs.WalkDirFunc) error {
		defer close(workerDone)
		for _, name := range []string{"", "first", "second", "third"} {
			attempted++
			if err := visit(filepath.Join(root, name), scriptedDirEntry{name: name, dir: true}, nil); err != nil {
				return err
			}
			accepted++
			if accepted == 2 {
				cancel()
			}
		}
		return nil
	})
	<-workerDone // join the isolated read-only worker before reading its counters
	if !errors.Is(err, context.Canceled) || paths != nil || attempted != 3 || accepted != 2 {
		t.Fatalf("partial discovery escaped cancellation: %v %v attempted=%d accepted=%d", paths, err, attempted, accepted)
	}
}

func TestDirsContextDoesNotWaitForStalledReadOnlyTraversal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	done := make(chan error, 1)
	go func() {
		paths, err := dirsContext(ctx, "/vault", func(_ string, visit fs.WalkDirFunc) error {
			defer close(exited)
			close(entered)
			<-release // an OS read between callbacks is not forcibly interrupted
			return visit("/vault", scriptedDirEntry{name: "vault", dir: true}, nil)
		})
		if paths != nil {
			done <- errors.New("cancelled discovery returned partial paths")
			return
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("read-only traversal did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled discovery returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller waited for stalled traversal")
	}
	// Release and join the read-only fixture after proving the caller returned.
	// This does not claim interruption of the underlying OS read.
	close(release)
	released = true
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("late read-only traversal did not exit")
	}
}
