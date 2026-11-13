// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package relate

import (
	"context"
	"sort"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
	"golang.org/x/text/unicode/norm"
)

// ContextCandidate is the current declared metadata of a possible relation.
// Callers must read it from the current file and verify identity, readability,
// draft policy and every audience of the source before returning it to
// ContextCandidates. An indexed graph node alone is not current evidence.
type ContextCandidate struct {
	ID, Title string
	Tags      []string
	Entity    bool
}

// ContextEvidence corroborates a candidate using declared metadata, independently
// of retrieval rank or prose. It accepts a shared current tag, or a source tag
// naming one unambiguous canonical entity by its ID or title. Title keys only
// fold case and join whitespace with hyphens; no aliases or synonyms are inferred.
// Ordinary words in the source body are not inputs to this policy.
//
// All indexed entities participate in ambiguity checks, including inaccessible
// ones. The returned predicate verifies the target's current identity and, for
// an entity anchor, its current entity type and unchanged canonical title.
// It does not authorize access: the caller must enforce current source/target
// audiences and repeat its normal final validation before writing a relation.
func ContextEvidence(g *graph.Graph, sourceTags []string) func(*graph.Node, ContextCandidate) bool {
	if g == nil {
		return func(*graph.Node, ContextCandidate) bool { return false }
	}
	policy := newContextPolicy(g, sourceTags)
	return func(node *graph.Node, current ContextCandidate) bool {
		_, _, ok := policy.match(node, current)
		return ok
	}
}

// ContextCandidates returns current, corroborated note IDs without depending on
// a bounded semantic retrieval result. It finds shared-tag candidates through
// inbound tagged edges and canonical entity anchors through indexed metadata,
// then checks every possible candidate with readCurrent before applying limit.
// Unreadable or source-audience-incompatible notes cannot crowd out eligible
// candidates if readCurrent rejects them as required by its contract.
//
// Entity anchors sort first, then shared-tag overlap, current entity priority,
// knowledge degree and ID. readCurrent may cache planning reads, but callers must
// retain their final current-file identity, evidence and audience validation.
// Cancellation returns no partial candidate list.
func ContextCandidates(ctx context.Context, g *graph.Graph, selfID string, sourceTags []string, limit int, readCurrent func(*graph.Node) (ContextCandidate, bool)) []string {
	if g == nil || limit <= 0 || readCurrent == nil || contextCancelled(ctx) {
		return nil
	}
	policy := newContextPolicy(g, sourceTags)
	if len(policy.tags) == 0 || contextCancelled(ctx) {
		return nil
	}

	possible := make(map[string]*graph.Node)
	for tag := range policy.tags {
		if contextCancelled(ctx) {
			return nil
		}
		// The index uses NFC and strips a leading '#'. These are only lookup
		// hints: corroboration below still compares the current declared tags.
		key := norm.NFC.String(strings.TrimPrefix(tag, "#"))
		for _, edge := range g.RefsTo("tag:" + key) {
			if contextCancelled(ctx) {
				return nil
			}
			if edge.Relation != "tagged" {
				continue
			}
			node, ok := g.Node(edge.Source)
			if ok && contextNote(node) && node.NoteID != selfID {
				possible[node.NoteID] = node
			}
		}
	}
	for id, node := range policy.anchors {
		if id != selfID {
			possible[id] = node
		}
	}

	// Stable read order also makes cancellation and caller-side diagnostics
	// independent of graph/map insertion order.
	ids := make([]string, 0, len(possible))
	for id := range possible {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	type rankedCandidate struct {
		id      string
		anchor  bool
		overlap int
		entity  bool
		degree  int
	}
	ranked := make([]rankedCandidate, 0, min(limit, len(ids)))
	for _, id := range ids {
		if contextCancelled(ctx) {
			return nil
		}
		node := possible[id]
		current, ok := readCurrent(node)
		if contextCancelled(ctx) {
			return nil
		}
		if !ok {
			continue
		}
		anchor, overlap, ok := policy.match(node, current)
		if !ok {
			continue
		}
		ranked = append(ranked, rankedCandidate{id: id, anchor: anchor, overlap: overlap, entity: current.Entity, degree: node.KnowledgeDegree})
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.anchor != b.anchor {
			return a.anchor
		}
		if a.overlap != b.overlap {
			return a.overlap > b.overlap
		}
		if a.entity != b.entity {
			return a.entity
		}
		if a.degree != b.degree {
			return a.degree > b.degree
		}
		return a.id < b.id
	})
	if contextCancelled(ctx) {
		return nil
	}
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	var out []string
	for _, candidate := range ranked {
		out = append(out, candidate.id)
	}
	return out
}

type contextPolicy struct {
	tags    map[string]bool
	anchors map[string]*graph.Node
}

func newContextPolicy(g *graph.Graph, sourceTags []string) contextPolicy {
	policy := contextPolicy{tags: tagSet(sourceTags), anchors: make(map[string]*graph.Node)}
	if g == nil || len(policy.tags) == 0 {
		return policy
	}
	// Count an entity only once per key when its canonical ID and title agree.
	// Conflicting IDs/titles on any other indexed entity make that key ambiguous.
	owners := make(map[string]map[string]*graph.Node)
	for _, node := range g.Nodes() {
		if !contextNote(node) || node.Attrs["type"] != "entity" {
			continue
		}
		for _, value := range []string{node.NoteID, node.Label} {
			key := contextEntityKey(value)
			if key == "" {
				continue
			}
			if owners[key] == nil {
				owners[key] = make(map[string]*graph.Node)
			}
			owners[key][node.NoteID] = node
		}
	}
	for tag := range policy.tags {
		entities := owners[contextEntityKey(tag)]
		if len(entities) != 1 {
			continue
		}
		for id, node := range entities {
			policy.anchors[id] = node
		}
	}
	return policy
}

func (policy contextPolicy) match(node *graph.Node, current ContextCandidate) (anchor bool, overlap int, ok bool) {
	if !contextNote(node) || current.ID != node.NoteID {
		return false, 0, false
	}
	for tag := range tagSet(current.Tags) {
		if policy.tags[tag] {
			overlap++
		}
	}
	if indexed := policy.anchors[current.ID]; indexed != nil && current.Entity && node.Attrs["type"] == "entity" &&
		entityText(current.Title) != "" && entityText(current.Title) == entityText(indexed.Label) {
		anchor = true
	}
	return anchor, overlap, anchor || overlap > 0
}

func contextEntityKey(value string) string {
	return norm.NFC.String(strings.ToLower(strings.Join(strings.Fields(value), "-")))
}

func contextNote(node *graph.Node) bool {
	return node != nil && node.Kind == "note" && node.NoteID != "" && node.ID == graph.NotePrefix+node.NoteID
}

func contextCancelled(ctx context.Context) bool { return ctx != nil && ctx.Err() != nil }
