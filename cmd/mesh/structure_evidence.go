// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/relate"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
)

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

func orphanEntityEvidence(ctx context.Context, root, path string, source *vault.Frontmatter, store *index.Store, g *graph.Graph) func(string, string) bool {
	text := currentOrphanText(ctx, root, path, source)
	return relate.EntityEvidence(text, g, func(node *graph.Node) (string, bool) {
		if ctx.Err() != nil || node.NoteID == source.ID {
			return "", false
		}
		metadata, err := store.NoteMetadataFor(ctx, []string{"note:" + node.NoteID})
		m, ok := metadata["note:"+node.NoteID]
		if err != nil || !ok || !filepath.IsLocal(m.Path) || m.Path == path {
			return "", false
		}
		target, err := currentRepairSource(ctx, root, m.Path)
		if err != nil || target.ID != node.NoteID || target.Type != vault.TypeEntity || !relationAudienceAllows(source, target) {
			return "", false
		}
		return target.Title, true
	})
}

// Retrieval orders ordinary candidates. Explicit canonical entity mentions also
// form candidates when an orphan has no tags or its generic capture title has no
// useful search terms. Every candidate still passes current-file corroboration
// and every-source-audience checks before becoming a durable edge.
func orphanEvidenceCandidates(ctx context.Context, root, path string, source *vault.Frontmatter, store *index.Store, g *graph.Graph, rt *retrieve.Retriever, query string) []string {
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
	for _, card := range cards {
		add(card.NoteID)
	}
	named := orphanEntityEvidence(ctx, root, path, source, store, g)
	nodes := g.Nodes()
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NoteID < nodes[j].NoteID })
	for _, node := range nodes {
		if node.Kind == "note" && node.Attrs["type"] == "entity" && named(node.NoteID, "") {
			add(node.NoteID)
		}
	}
	return candidates
}
