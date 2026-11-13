// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package relate

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/bright-interaction/mesh/internal/graph"
)

func TestContextCandidatesFindsTaggedPeersOutsideRetrievalAndFiltersBeforeCap(t *testing.T) {
	g := graph.New()
	current := map[string]ContextCandidate{}
	// A semantic top-16/top-32 pool can consist entirely of unreadable or
	// source-audience-incompatible hits. None may consume this lane's limit.
	for i := range 48 {
		id := fmt.Sprintf("incompatible-%02d", i)
		addContextTestNote(g, id, "Private decision", false, 100, "mesh")
		current[id] = ContextCandidate{ID: id, Title: "Private decision", Tags: []string{"mesh"}}
	}
	addContextTestNote(g, "eligible-peer", "Graph persistence", false, 1, "mesh")
	current["eligible-peer"] = ContextCandidate{ID: "eligible-peer", Title: "Graph persistence", Tags: []string{"mesh"}}
	var reads int
	read := func(n *graph.Node) (ContextCandidate, bool) {
		reads++
		candidate, ok := current[n.NoteID]
		// The caller rejects both unreadable targets and targets that cannot
		// preserve every audience of the current source before the output cap.
		return candidate, ok && n.NoteID == "eligible-peer"
	}
	got := ContextCandidates(context.Background(), g, "source", []string{"mesh"}, 1, read)
	if want := []string{"eligible-peer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates=%v want %v", got, want)
	}
	if reads != len(current) {
		t.Fatalf("read %d potential candidates, want all %d before applying cap", reads, len(current))
	}
}

func TestContextCandidatesCanonicalAnchorsRequireDeclaredUniqueCurrentMetadata(t *testing.T) {
	g := graph.New()
	current := map[string]ContextCandidate{}
	for _, note := range []struct{ id, title string }{
		{"mesh", "Mesh"},
		{"hash", "Hash"},
		{"workspace", "Google Workspace"},
		{"ci", "CI"},
	} {
		addContextTestNote(g, note.id, note.title, true, 0)
		current[note.id] = ContextCandidate{ID: note.id, Title: note.title, Entity: true}
	}
	read := func(n *graph.Node) (ContextCandidate, bool) { candidate, ok := current[n.NoteID]; return candidate, ok }
	for _, tc := range []struct {
		name string
		tags []string
		want []string
	}{
		{"short canonical ID", []string{"mesh"}, []string{"mesh"}},
		{"short canonical title", []string{" HASH "}, []string{"hash"}},
		{"canonical multiword title", []string{"google-workspace"}, []string{"workspace"}},
		{"canonical whitespace normalization", []string{"google   workspace"}, []string{"workspace"}},
		{"no inferred historical alias", []string{"stage"}, nil},
		{"ordinary prose is not metadata", nil, nil},
		{"similar metadata is not an alias", []string{"meshes"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContextCandidates(context.Background(), g, "source", tc.tags, 3, read); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("candidates=%v want %v", got, tc.want)
			}
		})
	}
	// The entity does not need to repeat its own ID as a tag. A fresh final
	// predicate must enforce the same metadata policy as candidate generation.
	node, _ := g.Node("note:mesh")
	evidence := ContextEvidence(g, []string{"mesh"})
	if !evidence(node, current["mesh"]) {
		t.Fatal("canonical untagged entity anchor not corroborated")
	}
	for name, changed := range map[string]ContextCandidate{
		"renamed title": {ID: "mesh", Title: "Other Product", Entity: true},
		"changed type":  {ID: "mesh", Title: "Mesh"},
		"changed ID":    {ID: "other", Title: "Mesh", Entity: true},
		"missing title": {ID: "mesh", Entity: true},
	} {
		t.Run(name, func(t *testing.T) {
			if evidence(node, changed) {
				t.Fatal("stale or missing current entity metadata became an anchor")
			}
		})
	}

	// Even an inaccessible duplicate's title makes the canonical key ambiguous.
	addContextTestNote(g, "private-duplicate", "MESH", true, 0)
	if got := ContextCandidates(context.Background(), g, "source", []string{"mesh"}, 3, read); len(got) != 0 {
		t.Fatalf("ambiguous inaccessible title selected a canonical entity: %v", got)
	}
	// An entity ID colliding with a different canonical title is ambiguous too.
	addContextTestNote(g, "google-workspace", "Other Entity", true, 0)
	if got := ContextCandidates(context.Background(), g, "source", []string{"google-workspace"}, 3, read); len(got) != 0 {
		t.Fatalf("ID/title collision selected a canonical entity: %v", got)
	}
}

func TestContextCandidatesUseCurrentTagsAndIdentity(t *testing.T) {
	g := graph.New()
	for _, id := range []string{"stale-tags", "wrong-id", "current", "source"} {
		addContextTestNote(g, id, id, false, 0, "shared")
	}
	current := map[string]ContextCandidate{
		"stale-tags": {ID: "stale-tags", Tags: []string{"different"}},
		"wrong-id":   {ID: "other", Tags: []string{"shared"}},
		"current":    {ID: "current", Tags: []string{" SHARED ", "shared", ""}},
		"source":     {ID: "source", Tags: []string{"shared"}},
	}
	read := func(n *graph.Node) (ContextCandidate, bool) { candidate, ok := current[n.NoteID]; return candidate, ok }
	got := ContextCandidates(context.Background(), g, "source", []string{"shared", "SHARED"}, 10, read)
	if want := []string{"current"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates=%v want %v", got, want)
	}
	evidence := ContextEvidence(g, []string{"shared"})
	node, _ := g.Node("note:current")
	if !evidence(node, current["current"]) {
		t.Fatal("current shared tag missing from final predicate")
	}
	if evidence(node, ContextCandidate{ID: "current", Tags: []string{"different"}}) {
		t.Fatal("final predicate trusted stale candidate tags")
	}
	// Structural graph edges and heading nodes do not become note relations.
	g.AddNode(&graph.Node{ID: "note:heading#section", Kind: "heading", NoteID: "heading", Label: "Section"})
	g.AddEdge(graph.Edge{Source: "note:heading#section", Target: "tag:shared", Relation: "tagged"})
	addContextTestNote(g, "structural", "Structural", false, 0)
	g.AddEdge(graph.Edge{Source: "note:structural", Target: "tag:shared", Relation: "contains"})
	if got := ContextCandidates(context.Background(), g, "source", []string{"shared"}, 10, func(n *graph.Node) (ContextCandidate, bool) {
		if n.NoteID == "heading" || n.NoteID == "structural" {
			t.Fatalf("non-note or non-tagged edge read as candidate: %q", n.ID)
		}
		return read(n)
	}); !reflect.DeepEqual(got, []string{"current"}) {
		t.Fatalf("structural graph nodes affected candidates: %v", got)
	}
}

func TestContextCandidatesDeterministicEvidenceOrder(t *testing.T) {
	notes := []struct {
		id     string
		entity bool
		degree int
		tags   []string
	}{
		{"one-overlap-b", false, 2, []string{"shared"}},
		{"one-overlap-a", false, 2, []string{"shared"}},
		{"one-overlap-popular", false, 20, []string{"shared"}},
		{"one-overlap-entity", true, 0, []string{"shared"}},
		{"two-overlaps", false, 0, []string{"shared", "topic"}},
		{"canonical", true, 0, nil},
	}
	want := []string{"canonical", "two-overlaps", "one-overlap-entity", "one-overlap-popular", "one-overlap-a", "one-overlap-b"}
	for _, reverse := range []bool{false, true} {
		g := graph.New()
		current := make(map[string]ContextCandidate)
		for i := range notes {
			if reverse {
				i = len(notes) - 1 - i
			}
			note := notes[i]
			addContextTestNote(g, note.id, note.id, note.entity, note.degree, note.tags...)
			current[note.id] = ContextCandidate{ID: note.id, Title: note.id, Tags: note.tags, Entity: note.entity}
		}
		read := func(n *graph.Node) (ContextCandidate, bool) { candidate, ok := current[n.NoteID]; return candidate, ok }
		got := ContextCandidates(context.Background(), g, "source", []string{"shared", "topic", "canonical"}, 10, read)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("reverse=%v candidates=%v want %v", reverse, got, want)
		}
		if got := ContextCandidates(context.Background(), g, "source", []string{"shared", "topic", "canonical"}, 3, read); !reflect.DeepEqual(got, want[:3]) {
			t.Errorf("reverse=%v limited candidates=%v want %v", reverse, got, want[:3])
		}
	}
}

func TestContextCandidatesCancellationAndEmptyInputs(t *testing.T) {
	g := graph.New()
	addContextTestNote(g, "first", "First", false, 0, "shared")
	addContextTestNote(g, "second", "Second", false, 0, "shared")
	ctx, cancel := context.WithCancel(context.Background())
	reads := 0
	read := func(n *graph.Node) (ContextCandidate, bool) {
		reads++
		cancel()
		return ContextCandidate{ID: n.NoteID, Tags: []string{"shared"}}, true
	}
	if got := ContextCandidates(ctx, g, "source", []string{"shared"}, 3, read); got != nil || reads != 1 {
		t.Fatalf("cancelled planning candidates=%v reads=%d, want nil and one read", got, reads)
	}
	if got := ContextCandidates(ctx, g, "source", []string{"shared"}, 3, read); got != nil || reads != 1 {
		t.Fatalf("already cancelled planning candidates=%v reads=%d, want no new read", got, reads)
	}
	for name, run := range map[string]func() []string{
		"nil graph": func() []string {
			return ContextCandidates(context.Background(), nil, "source", []string{"shared"}, 3, read)
		},
		"nil reader": func() []string {
			return ContextCandidates(context.Background(), g, "source", []string{"shared"}, 3, nil)
		},
		"zero limit": func() []string {
			return ContextCandidates(context.Background(), g, "source", []string{"shared"}, 0, read)
		},
		"no tags": func() []string { return ContextCandidates(context.Background(), g, "source", nil, 3, read) },
	} {
		t.Run(name, func(t *testing.T) {
			if got := run(); got != nil {
				t.Fatalf("candidates=%v want nil", got)
			}
		})
	}
	if reads != 1 {
		t.Fatalf("invalid inputs caused %d reads, want one total", reads)
	}
	if ContextEvidence(nil, []string{"shared"})(nil, ContextCandidate{ID: "first", Tags: []string{"shared"}}) {
		t.Fatal("nil graph/node became evidence")
	}
	node, _ := g.Node("note:first")
	if ContextEvidence(nil, []string{"shared"})(node, ContextCandidate{ID: "first", Tags: []string{"shared"}}) {
		t.Fatal("missing graph became evidence")
	}
}

func addContextTestNote(g *graph.Graph, id, title string, entity bool, degree int, tags ...string) {
	typ := "decision"
	if entity {
		typ = "entity"
	}
	g.AddNode(&graph.Node{ID: "note:" + id, Kind: "note", NoteID: id, Label: title, KnowledgeDegree: degree, Attrs: map[string]any{"type": typ}})
	for _, tag := range tags {
		g.AddNode(&graph.Node{ID: "tag:" + tag, Kind: "tag", Label: tag})
		g.AddEdge(graph.Edge{Source: "note:" + id, Target: "tag:" + tag, Relation: "tagged"})
	}
}
