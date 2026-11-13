// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/vault"
)

type candidateReaderTestStore struct {
	mu    sync.Mutex
	calls int
}

func (store *candidateReaderTestStore) NoteMetadataFor(ctx context.Context, ids []string) (map[string]index.NoteMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	store.calls++
	store.mu.Unlock()
	return map[string]index.NoteMetadata{ids[0]: {Path: "peer.md"}}, nil
}

func (store *candidateReaderTestStore) count() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.calls
}

func TestRepairCandidateReaderSharesSuccessfulConcurrentReadsOnly(t *testing.T) {
	store := &candidateReaderTestStore{}
	reader := newRepairCandidateReader("vault-root", store)
	started, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	reader.readCurrent = func(ctx context.Context, root, path string) (*vault.Frontmatter, error) {
		if root != "vault-root" || path != "peer.md" {
			t.Errorf("read escaped reader root/path: %q/%q", root, path)
		}
		if reads.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return &vault.Frontmatter{ID: "peer", Title: "Peer", Type: vault.TypeNote, Tags: vault.StringList{"mesh"}, Scope: vault.StringList{"dev"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	const workers = 8
	var wg sync.WaitGroup
	results := make(chan bool, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, fm, ok := reader.read(context.Background(), "peer")
			results <- ok && path == "peer.md" && fm != nil && fm.ID == "peer"
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(results)
	for ok := range results {
		if !ok {
			t.Fatal("concurrent reader lost the validated candidate")
		}
	}
	if reads.Load() != 1 || store.count() != 1 {
		t.Fatalf("file reads=%d metadata queries=%d, want one of each", reads.Load(), store.count())
	}
	_, first, _ := reader.read(context.Background(), "peer")
	first.Tags[0], first.Scope[0] = "changed", "sales"
	_, second, _ := reader.read(context.Background(), "peer")
	if second.Tags[0] != "mesh" || second.Scope[0] != "dev" {
		t.Fatal("caller mutation changed cached evidence or audiences")
	}
	if !relationAudienceAllows(&vault.Frontmatter{Scope: vault.StringList{"dev"}}, second) ||
		relationAudienceAllows(&vault.Frontmatter{Scope: vault.StringList{"sales"}}, second) {
		t.Fatal("planning reused a source-specific audience decision")
	}
}

func TestRepairCandidateReaderDoesNotCacheFailureOrCancellation(t *testing.T) {
	store := &candidateReaderTestStore{}
	reader := newRepairCandidateReader("vault-root", store)
	var reads int
	reader.readCurrent = func(context.Context, string, string) (*vault.Frontmatter, error) {
		reads++
		if reads == 1 {
			return nil, errors.New("transient read failure")
		}
		return &vault.Frontmatter{ID: "peer", Type: vault.TypeNote}, nil
	}
	if _, _, ok := reader.read(context.Background(), "peer"); ok {
		t.Fatal("failed read became a successful candidate")
	}
	if _, _, ok := reader.read(context.Background(), "peer"); !ok || reads != 2 || store.count() != 2 {
		t.Fatalf("failed load was cached: ok=%v reads=%d queries=%d", ok, reads, store.count())
	}

	// A caller cancelled while holding the in-flight slot must release waiters;
	// a later caller gets its own live read rather than a cached cancellation.
	reader = newRepairCandidateReader("vault-root", store)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	reader.readCurrent = func(ctx context.Context, _, _ string) (*vault.Frontmatter, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan bool, 1)
	go func() { _, _, ok := reader.read(ctx, "peer"); done <- ok }()
	<-started
	cancel()
	if <-done {
		t.Fatal("cancelled load became a successful candidate")
	}
	reader.readCurrent = func(context.Context, string, string) (*vault.Frontmatter, error) {
		return &vault.Frontmatter{ID: "peer", Type: vault.TypeNote}, nil
	}
	if _, _, ok := reader.read(context.Background(), "peer"); !ok {
		t.Fatal("cancelled in-flight load prevented a later live retry")
	}
	queries := store.count()
	if _, _, ok := reader.read(ctx, "peer"); ok || store.count() != queries {
		t.Fatal("already-cancelled reader used a cached result or queried storage")
	}
	// A fresh command receives a fresh cache even when it uses the same store.
	newCommand := newRepairCandidateReader("vault-root", store)
	newCommand.readCurrent = reader.readCurrent
	if _, _, ok := newCommand.read(context.Background(), "peer"); !ok || store.count() != queries+1 {
		t.Fatal("candidate cache leaked between command lifetimes")
	}
}

func TestRepairPlanningCacheNeverReplacesFinalCurrentChecks(t *testing.T) {
	root := t.TempDir()
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("source.md", "---\nid: source\ntype: note\ntitle: Source\nscope: [dev]\ntags: [mesh]\n---\nSource body.\n")
	write("peer.md", "---\nid: peer\ntype: note\ntitle: Peer\nscope: [dev]\ntags: [mesh]\n---\nPeer body.\n")
	write("entity-source.md", "---\nid: entity-source\ntype: note\ntitle: Capture\nscope: [dev]\n---\nMeridianCRM owns this implementation.\n")
	write("entity.md", "---\nid: meridiancrm\ntype: entity\ntitle: MeridianCRM\nscope: [dev]\n---\nCanonical entity.\n")
	cmd := rootCmd()
	cmd.SetArgs([]string{"index", root})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	store, err := index.OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g, err := store.LoadGraph()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	reader := newRepairCandidateReader(root, store)
	var candidateReads int
	reader.readCurrent = func(ctx context.Context, root, path string) (*vault.Frontmatter, error) {
		candidateReads++
		return currentRepairSource(ctx, root, path)
	}
	source, err := currentRepairSource(ctx, root, "source.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := orphanContextCandidates(ctx, root, "source.md", source, store, g, reader); !reflect.DeepEqual(got, []string{"peer"}) {
		t.Fatalf("initial planning=%v want peer", got)
	}
	for _, changed := range []string{
		"---\nid: peer\ntype: note\ntitle: Peer\nscope: [sales]\ntags: [mesh]\n---\nPeer body.\n",
		"---\nid: peer\ntype: note\ntitle: Peer\nscope: [dev]\ntags: [other]\n---\nPeer body.\n",
		"---\nid: peer\ntype: note\ntitle: Peer\nscope: [dev]\ntags: [mesh]\nstatus: draft\n---\nPeer body.\n",
	} {
		write("peer.md", changed)
		if got := orphanContextCandidates(ctx, root, "source.md", source, store, g, reader); !reflect.DeepEqual(got, []string{"peer"}) {
			t.Fatalf("test did not retain planning snapshot: %v", got)
		}
		if got, err := filterOrphanLinkAudiences(ctx, root, "source.md", store, []string{"peer"}, false, g); err != nil || len(got) != 0 {
			t.Fatalf("stale planning metadata passed final checks: links=%v err=%v", got, err)
		}
	}
	entitySource, err := currentRepairSource(ctx, root, "entity-source.md")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if !orphanEntityEvidence(ctx, root, "entity-source.md", entitySource, store, g, reader)("meridiancrm", "") {
			t.Fatal("current canonical entity not available to planning")
		}
	}
	if candidateReads != 2 {
		t.Fatalf("planning read peer/entity %d times, want once per target", candidateReads)
	}
	write("entity.md", "---\nid: meridiancrm\ntype: entity\ntitle: Changed Entity\nscope: [dev]\n---\nChanged declaration.\n")
	if got, err := filterOrphanLinkAudiences(ctx, root, "entity-source.md", store, []string{"meridiancrm"}, false, g); err != nil || len(got) != 0 {
		t.Fatalf("final named evidence reused the planning entity: links=%v err=%v", got, err)
	}
}

var _ repairCandidateMetadataStore = (*index.Store)(nil)
