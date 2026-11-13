// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/relate"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
)

type repairCandidateMetadataStore interface {
	NoteMetadataFor(context.Context, []string) (map[string]index.NoteMetadata, error)
}

type repairCandidateSnapshot struct {
	path string
	fm   vault.Frontmatter
}

// repairCandidateReader lives for one bulk command, not the daemon or a later
// repair. Only successful candidate-planning reads are shared across sources.
// Source-specific audience decisions and final write checks are never cached.
// Waiters share an in-flight read without spawning work detached from a caller.
type repairCandidateReader struct {
	root        string
	store       repairCandidateMetadataStore
	readCurrent func(context.Context, string, string) (*vault.Frontmatter, error)
	mu          sync.Mutex
	cached      map[string]repairCandidateSnapshot
	pending     map[string]chan struct{}
}

func newRepairCandidateReader(root string, store repairCandidateMetadataStore) *repairCandidateReader {
	return &repairCandidateReader{
		root: root, store: store, readCurrent: currentRepairSource,
		cached: make(map[string]repairCandidateSnapshot), pending: make(map[string]chan struct{}),
	}
}

func (reader *repairCandidateReader) read(ctx context.Context, id string) (string, *vault.Frontmatter, bool) {
	if reader == nil || reader.store == nil || id == "" {
		return "", nil, false
	}
	for ctx.Err() == nil {
		reader.mu.Lock()
		if snapshot, ok := reader.cached[id]; ok {
			reader.mu.Unlock()
			return cloneRepairCandidate(snapshot)
		}
		if pending, ok := reader.pending[id]; ok {
			reader.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", nil, false
			case <-pending:
				continue
			}
		}
		pending := make(chan struct{})
		reader.pending[id] = pending
		reader.mu.Unlock()

		snapshot, ok := loadRepairCandidate(ctx, reader.root, reader.store, reader.readCurrent, id)
		reader.mu.Lock()
		if ok && ctx.Err() == nil {
			reader.cached[id] = snapshot
		} else {
			ok = false
		}
		delete(reader.pending, id)
		close(pending)
		reader.mu.Unlock()
		if ok {
			return cloneRepairCandidate(snapshot)
		}
		return "", nil, false
	}
	return "", nil, false
}

func loadRepairCandidate(ctx context.Context, root string, store repairCandidateMetadataStore, readCurrent func(context.Context, string, string) (*vault.Frontmatter, error), id string) (repairCandidateSnapshot, bool) {
	if ctx.Err() != nil || store == nil || readCurrent == nil {
		return repairCandidateSnapshot{}, false
	}
	metadata, err := store.NoteMetadataFor(ctx, []string{"note:" + id})
	m, ok := metadata["note:"+id]
	if err != nil || !ok || !filepath.IsLocal(m.Path) || ctx.Err() != nil {
		return repairCandidateSnapshot{}, false
	}
	target, err := readCurrent(ctx, root, m.Path)
	if err != nil || target == nil || target.ID != id || vault.IsDraft(target) || ctx.Err() != nil {
		return repairCandidateSnapshot{}, false
	}
	// Keep just the immutable planning metadata, never a cached note body or
	// mutable aliases to caller-owned frontmatter slices.
	fm := vault.Frontmatter{ID: target.ID, Type: target.Type, Title: target.Title, Status: target.Status,
		Tags: append(vault.StringList(nil), target.Tags...), Scope: append(vault.StringList(nil), target.Scope...),
		RepoPath: target.RepoPath, SourceURL: target.SourceURL}
	return repairCandidateSnapshot{path: m.Path, fm: fm}, true
}

func cloneRepairCandidate(snapshot repairCandidateSnapshot) (string, *vault.Frontmatter, bool) {
	fm := snapshot.fm
	fm.Tags = append(vault.StringList(nil), fm.Tags...)
	fm.Scope = append(vault.StringList(nil), fm.Scope...)
	return snapshot.path, &fm, true
}

func readRepairCandidate(ctx context.Context, root string, store *index.Store, id string, readers []*repairCandidateReader) (string, *vault.Frontmatter, bool) {
	if len(readers) > 0 && readers[0] != nil {
		return readers[0].read(ctx, id)
	}
	snapshot, ok := loadRepairCandidate(ctx, root, store, currentRepairSource, id)
	if !ok {
		return "", nil, false
	}
	return cloneRepairCandidate(snapshot)
}

func currentOrphanText(ctx context.Context, root, path string, fm *vault.Frontmatter) string {
	data, err := vault.ReadConfinedFileContext(ctx, root, path, 1<<20)
	if err != nil || fm == nil || vault.UnterminatedFrontmatter(string(data)) {
		return ""
	}
	_, body, had := vault.SplitFrontmatter(string(data))
	if !had {
		return ""
	}
	clean, _ := vault.StripComments(fm.Title + "\n" + body)
	return strings.TrimSpace(clean)
}

func orphanEntityEvidence(ctx context.Context, root, path string, source *vault.Frontmatter, store *index.Store, g *graph.Graph, readers ...*repairCandidateReader) func(string, string) bool {
	text := currentOrphanText(ctx, root, path, source)
	return relate.EntityEvidence(text, g, func(node *graph.Node) (string, bool) {
		if ctx.Err() != nil || node.NoteID == source.ID {
			return "", false
		}
		targetPath, target, ok := readRepairCandidate(ctx, root, store, node.NoteID, readers)
		if !ok || targetPath == path || target.Type != vault.TypeEntity || !relationAudienceAllows(source, target) {
			return "", false
		}
		return target.Title, true
	})
}

func repairContextDescriptor(fm *vault.Frontmatter) relate.ContextCandidate {
	return relate.ContextCandidate{ID: fm.ID, Title: fm.Title, Tags: fm.Tags, Entity: fm.Type == vault.TypeEntity}
}

func orphanContextCandidates(ctx context.Context, root, path string, source *vault.Frontmatter, store *index.Store, g *graph.Graph, readers ...*repairCandidateReader) []string {
	return relate.ContextCandidates(ctx, g, source.ID, source.Tags, 3, func(node *graph.Node) (relate.ContextCandidate, bool) {
		targetPath, target, ok := readRepairCandidate(ctx, root, store, node.NoteID, readers)
		if !ok || targetPath == path || !relationAudienceAllows(source, target) {
			return relate.ContextCandidate{}, false
		}
		return repairContextDescriptor(target), true
	})
}

// Retrieval orders ordinary candidates. Explicit canonical entity mentions also
// form candidates when an orphan has no tags or its generic capture title has no
// useful search terms. Every candidate still passes current-file corroboration
// and every-source-audience checks before becoming a durable edge.
func orphanEvidenceCandidates(ctx context.Context, root, path string, source *vault.Frontmatter, store *index.Store, g *graph.Graph, rt *retrieve.Retriever, query string, readers ...*repairCandidateReader) []string {
	direct := orphanContextCandidates(ctx, root, path, source, store, g, readers...)
	if ctx.Err() != nil {
		return nil
	}
	if len(direct) == 3 {
		return direct
	}
	text := currentOrphanText(ctx, root, path, source)
	runes := []rune(text)
	if len(runes) > 2048 {
		runes = runes[:2048]
	}
	cards, _ := rt.Retrieve(ctx, query+"\n"+string(runes), retrieve.Options{Limit: 32})
	if ctx.Err() != nil {
		return nil
	}
	seen := map[string]bool{source.ID: true}
	var candidates []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			candidates = append(candidates, id)
		}
	}
	for _, id := range direct {
		add(id)
	}
	for _, card := range cards {
		add(card.NoteID)
	}
	named := orphanEntityEvidence(ctx, root, path, source, store, g, readers...)
	nodes := g.Nodes()
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NoteID < nodes[j].NoteID })
	for _, node := range nodes {
		if node.Kind == "note" && node.Attrs["type"] == "entity" && named(node.NoteID, "") {
			add(node.NoteID)
		}
	}
	return candidates
}
