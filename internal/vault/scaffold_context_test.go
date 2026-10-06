// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreateNoteContextCancellationStopsIDScanBeforePublication(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	var opens atomic.Int32

	go func() {
		_, err := createNoteContext(
			ctx,
			root,
			completeFixtureSpec(NewNoteSpec{Type: TypeGotcha, Title: "cancelled scan"}),
			func(ctx context.Context, _ string) (map[string]string, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			},
			func(path string, flag int, perm os.FileMode) (*os.File, error) {
				opens.Add(1)
				return os.OpenFile(path, flag, perm)
			},
		)
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("CreateNoteContext did not enter the id scan")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CreateNoteContext returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CreateNoteContext did not stop when its id scan was cancelled")
	}
	if got := opens.Load(); got != 0 {
		t.Fatalf("O_EXCL open called %d times after cancellation, want 0", got)
	}
	assertNoMarkdownFiles(t, root)
	if _, err := os.Stat(filepath.Join(root, DirForType(TypeGotcha))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled pre-publication scan created an empty type directory: %v", err)
	}
}

func TestCreateNoteContextCancellationAfterExclusiveOpenCompletesThenCleans(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	opened := false
	var claimedPath string
	observer := filepath.Join(root, ".completed-note-observer")

	res, err := createNoteContext(
		ctx,
		root,
		completeFixtureSpec(NewNoteSpec{Type: TypeGotcha, Title: "finish the claimed note", Do: "do", Dont: "don't", Why: "why"}),
		func(context.Context, string) (map[string]string, error) {
			return map[string]string{}, nil
		},
		func(path string, flag int, perm os.FileMode) (*os.File, error) {
			f, err := os.OpenFile(path, flag, perm)
			if err == nil {
				opened = true
				claimedPath = path
				// A second link lets the test inspect the inode after CreateNoteContext
				// withdraws the public claim. If cancellation interrupted the write, this
				// observer would hold the same empty or partial bytes.
				if linkErr := os.Link(path, observer); linkErr != nil {
					t.Fatalf("link claimed inode: %v", linkErr)
				}
				cancel() // publication already happened; finish, then clean up
			}
			return f, err
		},
	)
	if !opened {
		t.Fatal("test never crossed the O_EXCL publication boundary")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("context state = %v, want context.Canceled", ctx.Err())
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateNoteContext returned %v, want context.Canceled", err)
	}
	if res != nil {
		t.Fatalf("cancelled publication returned a success receipt: %+v", res)
	}
	if _, statErr := os.Lstat(claimedPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancelled publication left its claimed path behind: %v", statErr)
	}
	content, err := os.ReadFile(observer)
	if err != nil {
		t.Fatalf("inspect completed inode: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("cancellation interrupted the claimed note write")
	}
	if err := validateRoundTrip(string(content), "finish-the-claimed-note"); err != nil {
		t.Fatalf("claimed note was torn before cleanup: %v", err)
	}
}

func TestCreateNoteContextFailedWriteRemovesExclusiveClaim(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	var claimedPath string

	res, err := createNoteContext(
		ctx,
		root,
		completeFixtureSpec(NewNoteSpec{Type: TypeGotcha, Title: "clean failed claim"}),
		func(context.Context, string) (map[string]string, error) {
			return map[string]string{}, nil
		},
		func(path string, flag int, perm os.FileMode) (*os.File, error) {
			f, err := os.OpenFile(path, flag, perm)
			if err != nil {
				return nil, err
			}
			claimedPath = path
			cancel()
			if err := f.Close(); err != nil {
				t.Fatalf("close injected file: %v", err)
			}
			return f, nil // the next write fails deterministically on the closed file
		},
	)
	if err == nil {
		t.Fatal("write through a closed claimed file unexpectedly succeeded")
	}
	if res != nil {
		t.Fatalf("failed write returned a success receipt: %+v", res)
	}
	if claimedPath == "" {
		t.Fatal("test never crossed the O_EXCL publication boundary")
	}
	if _, statErr := os.Lstat(claimedPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed write left its claimed path behind: %v", statErr)
	}
}

func TestClaimedIDScanCancellationReturnsNoPartialClaims(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entry := staticDirEntry{name: "first.md"}
	readStarted := make(chan struct{})
	done := make(chan struct {
		holders map[string]idHolder
		err     error
	}, 1)

	go func() {
		holders, err := scanClaimedIDHolders(
			ctx,
			"/vault",
			func(_ string, visit fs.WalkDirFunc) error {
				return visit("/vault/first.md", entry, nil)
			},
			func(ctx context.Context, _ string) (string, bool, error) {
				close(readStarted)
				<-ctx.Done()
				return "", false, ctx.Err()
			},
		)
		done <- struct {
			holders map[string]idHolder
			err     error
		}{holders: holders, err: err}
	}()

	select {
	case <-readStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("claim scan did not pass its context into the note-head reader")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("claim scan returned %v, want context.Canceled", got.err)
		}
		if got.holders != nil {
			t.Fatalf("cancelled claim scan returned partial claims: %v", got.holders)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("claim scan did not stop after its note-head read was cancelled")
	}
}

func TestClaimedIDScanCancellationDoesNotWaitForStalledTraversal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	workerExited := make(chan struct{})
	done := make(chan error, 1)
	var readCalled atomic.Bool

	go func() {
		_, err := claimedIDHoldersWith(
			ctx,
			"/vault",
			func(string, fs.WalkDirFunc) error {
				close(started)
				<-release // simulate WalkDir stalled between directory callbacks
				close(workerExited)
				return ctx.Err()
			},
			func(context.Context, string) (string, bool, error) {
				readCalled.Store(true)
				return "", false, errors.New("unexpected note read")
			},
		)
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("claim traversal did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("claim traversal returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled claim scan waited for the stalled WalkDir call")
	}

	// The detached traversal is read-only. Once the simulated filesystem wakes, its
	// private buffered publication must let it exit without a caller still receiving.
	close(release)
	select {
	case <-workerExited:
	case <-time.After(2 * time.Second):
		t.Fatal("late claim traversal did not exit")
	}
	if readCalled.Load() {
		t.Fatal("stalled walker unexpectedly reached a note read")
	}
}

func TestClaimedIDScanSkipsNonRegularMarkdownEntries(t *testing.T) {
	entries := []staticDirEntry{
		{name: "regular.md"},
		{name: "link.md", mode: os.ModeSymlink},
		{name: "pipe.md", mode: os.ModeNamedPipe},
		{name: "socket.md", mode: os.ModeSocket},
	}
	var reads []string
	holders, err := scanClaimedIDHolders(
		context.Background(),
		"/vault",
		func(_ string, visit fs.WalkDirFunc) error {
			for _, entry := range entries {
				if err := visit(filepath.Join("/vault", entry.name), entry, nil); err != nil {
					return err
				}
			}
			return nil
		},
		func(_ context.Context, path string) (string, bool, error) {
			reads = append(reads, path)
			return "regular", true, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 1 || filepath.Base(reads[0]) != "regular.md" {
		t.Fatalf("note-head reads = %v, want only regular.md", reads)
	}
	if len(holders) != 1 || holders["regular"].path != "regular.md" {
		t.Fatalf("claims = %v, want only the regular file", holders)
	}
}

func TestCreateNotePostcheckCoversEveryTypeDestination(t *testing.T) {
	want := make(map[string]bool, len(validTypes))
	for noteType := range validTypes {
		want[DirForType(noteType)] = true
	}
	want["inbox"] = true
	got := make(map[string]bool, len(createNoteDirs))
	for _, dir := range createNoteDirs {
		if got[dir] {
			t.Fatalf("duplicate CreateNote postcheck directory %q", dir)
		}
		got[dir] = true
	}
	if len(got) != len(want) {
		t.Fatalf("CreateNote postcheck destinations = %v, want %v", got, want)
	}
	for dir := range want {
		if !got[dir] {
			t.Fatalf("CreateNote postcheck omits type destination %q", dir)
		}
	}
}

type staticDirEntry struct {
	name string
	mode fs.FileMode
}

func (e staticDirEntry) Name() string               { return e.name }
func (e staticDirEntry) IsDir() bool                { return e.mode.IsDir() }
func (e staticDirEntry) Type() fs.FileMode          { return e.mode.Type() }
func (e staticDirEntry) Info() (fs.FileInfo, error) { return staticFileInfo(e), nil }

type staticFileInfo staticDirEntry

func (i staticFileInfo) Name() string       { return i.name }
func (i staticFileInfo) Size() int64        { return 0 }
func (i staticFileInfo) Mode() fs.FileMode  { return i.mode }
func (i staticFileInfo) ModTime() time.Time { return time.Time{} }
func (i staticFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i staticFileInfo) Sys() any           { return nil }

func assertNoMarkdownFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ".md" {
			t.Errorf("cancelled CreateNoteContext published %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
