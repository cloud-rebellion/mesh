// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// CreateNote used to pick a free path with os.Stat and then write it with os.WriteFile.
// Under any concurrent caller (mesh mcp --http, the hub's /mcp, the web pending API) two
// write-backs with the same title both saw the base path free, both rendered the same id,
// and the second truncated the first. Every caller got a success receipt; only one note
// survived. For a knowledge store whose whole flywheel is agents writing back what they
// learned, losing the write-back silently is the worst available outcome.
func TestCreateNoteConcurrentSameTitleLosesNothing(t *testing.T) {
	const n = 8
	root := t.TempDir()

	var wg sync.WaitGroup
	results := make([]*CreateResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together to make the collision likely
			results[i], errs[i] = CreateNote(root, completeFixtureSpec(NewNoteSpec{
				Type:  TypeGotcha,
				Title: "race condition note",
				Do:    fmt.Sprintf("do-%d", i),
				Dont:  "dont",
				Why:   "why",
			}))
		}(i)
	}
	close(start)
	wg.Wait()

	ids := map[string]bool{}
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if ids[results[i].ID] {
			t.Errorf("call %d got id %q, already handed to another caller", i, results[i].ID)
		}
		ids[results[i].ID] = true
	}

	// Every success receipt must name a file that actually exists.
	for i, r := range results {
		if _, err := os.Stat(r.Path); err != nil {
			t.Errorf("call %d was told it wrote %s, but: %v", i, r.Path, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, DirForType(TypeGotcha)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("%d notes on disk, want %d: %d write-backs were destroyed", len(entries), n, n-len(entries))
	}
}

func TestCreateNoteConcurrentStatusAndInboxPreserveUniqueIDs(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var wg sync.WaitGroup
	defer wg.Wait()
	defer releaseOnce.Do(func() { close(release) })
	results := make([]*CreateResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		spec := completeSpec(t, TypeStatus, "Cross destination")
		if i == 1 {
			spec.Status = "draft"
		}
		wg.Add(1)
		go func(i int, spec NewNoteSpec) {
			defer wg.Done()
			results[i], errs[i] = createNoteContext(ctx, root, spec,
				func(context.Context, string) (map[string]string, error) { return map[string]string{}, nil },
				func(path string, flags int, mode os.FileMode) (*os.File, error) {
					file, err := os.OpenFile(path, flags, mode)
					if err == nil && filepath.Base(path) == "cross-destination.md" {
						opened <- struct{}{}
						<-release // both destinations own their first claim before postcheck
					}
					return file, err
				})
		}(i, spec)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-opened:
		case <-time.After(2 * time.Second):
			t.Fatal("both destinations did not claim the initial id")
		}
	}
	releaseOnce.Do(func() { close(release) })
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if results[0].ID == results[1].ID {
		t.Fatal("status and inbox returned the same note id")
	}
	for _, result := range results {
		if NoteIDForFile(result.Path) != result.ID {
			t.Fatal("success receipt lost its actual note")
		}
	}
}
