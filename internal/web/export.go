// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package web serves `mesh ui`: a sovereign, single-binary localhost graph viewer
// (no CDN, no third-party JS, self-hosted fonts). It exposes the same in-memory
// graph the agent reads over MCP, as one /graph.json the SPA renders in two views
// (an Obsidian-style force graph and a galaxy orbiting the index note).
package web

import (
	"math"
	"sort"
	"strings"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/vault"
	"golang.org/x/text/unicode/norm"
)

const (
	notePrefix = "note:"
	topColored = 24 // the top-N communities by size get a distinct hue; the tail is gray
)

// Export is the graph payload the SPA consumes: visible published note nodes,
// note-to-note semantic links and separately authored collection memberships.
// Tags/headings are intra-structure. Membership affects navigation only; visible
// semantic degree, community, size and navigation orbit are precomputed here.
type Export struct {
	Meta            ExportMeta   `json:"meta"`
	Nodes           []ExportNode `json:"nodes"`
	Edges           []ExportEdge `json:"edges"`
	CollectionEdges []ExportEdge `json:"collection_edges,omitempty"` // navigation membership, never semantic references
	Communities     []ExportComm `json:"communities"`
}

type ExportMeta struct {
	Vault               string `json:"vault"`    // absolute vault root, for the editor:// bridge
	IndexID             string `json:"index_id"` // visible versioned index, otherwise most-connected visible note
	NodeCount           int    `json:"node_count"`
	EdgeCount           int    `json:"edge_count"`
	CollectionEdgeCount int    `json:"collection_edge_count"`
	MaxOrbit            int    `json:"max_orbit"` // largest navigation distance from the index note
}

type ExportNode struct {
	ID        string `json:"id"`    // frontmatter id
	Label     string `json:"label"` // title
	Path      string `json:"path"`  // vault-relative
	Line      int    `json:"line"`  // source line for the editor bridge (1 = top)
	Type      string `json:"type"`  // frontmatter type (decision/gotcha/entity/...)
	Community int    `json:"community"`
	// Degree counts distinct VISIBLE other notes linked by semantic references.
	// It never exposes hidden-neighbor counts or includes collection memberships.
	// The client labels it "links" in the hover card, and raw fan-out would
	// count the note's own headings and tags, so a long unlinked note would show a
	// link count it does not have and be drawn as a giant.
	Degree      int      `json:"degree"`
	Size        float64  `json:"size"`  // sqrt(degree), the client scales to a radius
	Orbit       int      `json:"orbit"` // semantic/membership navigation distance (0 = center)
	Tags        []string `json:"tags,omitempty"`
	Collections []string `json:"collections,omitempty"` // readable exact IDs; no implied primary home
}

type ExportEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Rel    string `json:"rel"`
}

type ExportComm struct {
	ID    int    `json:"id"`
	Size  int    `json:"size"`
	Color string `json:"color"` // hex; perceptually-spaced for the top N, gray for the tail
	Label string `json:"label"` // exemplar (most-connected member) title
}

// scopeVisible reports whether a note node is visible under an allowed-scope set.
// allowed==nil means unrestricted (standalone viewer). Absent scope = dev (fail-safe).
func scopeVisible(n *graph.Node, allowed map[string]bool) bool {
	if allowed == nil {
		return true
	}
	sc, _ := n.Attrs["scope"].(string)
	return vault.ScopeAllowsCSV(sc, allowed)
}

// pathVisible reports whether a note node is visible under the caller's folder ACLs.
// allowPath==nil means unrestricted. A node with no recorded path is refused under an
// active fence: an unnameable path cannot be evaluated, and the payload carries the
// note's title either way.
func pathVisible(n *graph.Node, allowPath func(string) bool) bool {
	if allowPath == nil {
		return true
	}
	return n.NotePath != "" && allowPath(n.NotePath)
}

// BuildExport projects the in-memory graph into the SPA payload. allowed (nil =
// unrestricted) filters notes to the caller's readable scopes and allowPath (nil =
// unrestricted) to their readable folders; an excluded note drops out of nodes, so its
// edges and orbit fall away with it.
func BuildExport(g *graph.Graph, vaultRoot string, allowed map[string]bool, allowPath func(string) bool) Export {
	all := g.Nodes()

	// Note nodes only, and a quick membership set for filtering edges/adjacency.
	notes := make([]*graph.Node, 0, len(all))
	isNote := make(map[string]bool)
	for _, n := range all {
		if n.Kind == "note" && n.NoteID != "" && n.ID == notePrefix+n.NoteID && n.NotePath != "" && !draftNode(n) && scopeVisible(n, allowed) && pathVisible(n, allowPath) {
			notes = append(notes, n)
			isNote[n.ID] = true
		}
	}

	// Undirected note-only adjacency, for the galaxy orbit BFS.
	adj := make(map[string][]string, len(notes))
	link := func(a, b string) {
		if isNote[a] && isNote[b] {
			adj[a] = append(adj[a], b)
		}
	}
	noteIDs := make([]string, 0, len(notes))
	for _, n := range notes {
		noteIDs = append(noteIDs, n.ID)
	}
	var edges []ExportEdge
	// Visible semantic degree avoids exposing the count of inaccessible neighbors.
	// This projection never changes the graph's degree, ranking or communities.
	semanticNeighbors := make(map[string]map[string]bool, len(notes))
	semanticLink := func(a, b string) {
		if semanticNeighbors[a] == nil {
			semanticNeighbors[a] = map[string]bool{}
		}
		semanticNeighbors[a][b] = true
	}
	for _, n := range notes {
		for _, e := range g.Neighbors(n.ID) {
			if isNote[e.Target] && e.Target != n.ID { // skip self-links (degenerate edge + degree inflation)
				edges = append(edges, ExportEdge{Source: strip(n.ID), Target: strip(e.Target), Rel: e.Relation})
				link(n.ID, e.Target)
				link(e.Target, n.ID)
				if e.Relation == graph.RelReferences {
					semanticLink(n.ID, e.Target)
					semanticLink(e.Target, n.ID)
				}
			}
		}
	}
	collectionEdges, collections, unambiguous := projectCollections(all, notes, isNote)
	for _, e := range collectionEdges {
		link(notePrefix+e.Source, notePrefix+e.Target)
		link(notePrefix+e.Target, notePrefix+e.Source)
	}
	// Prefer a supported explicit index; neither hidden degree nor array order
	// establishes ownership or picks the entry point. Ties use visible degree/ID.
	indexCandidate := map[string]bool{}
	for _, n := range notes {
		indexCandidate[n.ID] = unambiguous[n.NoteID] && versionedIndex(n)
	}
	var index *graph.Node
	for _, n := range notes {
		if index == nil || (indexCandidate[n.ID] && !indexCandidate[index.ID]) ||
			(indexCandidate[n.ID] == indexCandidate[index.ID] && (len(semanticNeighbors[n.ID]) > len(semanticNeighbors[index.ID]) ||
				(len(semanticNeighbors[n.ID]) == len(semanticNeighbors[index.ID]) && n.ID < index.ID))) {
			index = n
		}
	}

	orbit, maxOrbit := orbits(adj, noteIDs, indexID(index))

	// Community sizing + colors: rank communities by note count, hue the top N.
	commSize := map[int]int{}
	commExemplar := map[int]*graph.Node{}
	for _, n := range notes {
		commSize[n.Community]++
		if ex := commExemplar[n.Community]; ex == nil || len(semanticNeighbors[n.ID]) > len(semanticNeighbors[ex.ID]) || (len(semanticNeighbors[n.ID]) == len(semanticNeighbors[ex.ID]) && n.ID < ex.ID) {
			commExemplar[n.Community] = n
		}
	}
	ranked := make([]int, 0, len(commSize))
	for c := range commSize {
		ranked = append(ranked, c)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if commSize[ranked[i]] != commSize[ranked[j]] {
			return commSize[ranked[i]] > commSize[ranked[j]]
		}
		return ranked[i] < ranked[j]
	})
	color := make(map[int]string, len(ranked))
	communities := make([]ExportComm, 0, len(ranked))
	for rank, c := range ranked {
		col := tailGray
		if rank < topColored {
			col = communityHue(rank)
		}
		color[c] = col
		label := ""
		if ex := commExemplar[c]; ex != nil {
			label = ex.Label
		}
		communities = append(communities, ExportComm{ID: c, Size: commSize[c], Color: col, Label: label})
	}

	nodes := make([]ExportNode, 0, len(notes))
	for _, n := range notes {
		nodes = append(nodes, ExportNode{
			ID: n.NoteID, Label: n.Label, Path: n.NotePath, Line: lineOf(n),
			Type: typeOf(n), Community: n.Community, Degree: len(semanticNeighbors[n.ID]),
			Size: math.Sqrt(float64(len(semanticNeighbors[n.ID])) + 1), Orbit: orbit[n.ID], Tags: tagsOf(g, n.ID),
			Collections: collections[n.ID],
		})
	}
	sort.Slice(nodes, func(i, j int) bool { // stable, importance-first
		if nodes[i].Degree != nodes[j].Degree {
			return nodes[i].Degree > nodes[j].Degree
		}
		return nodes[i].ID < nodes[j].ID
	})

	return Export{
		Meta: ExportMeta{
			Vault: vaultRoot, IndexID: indexID(index),
			NodeCount: len(nodes), EdgeCount: len(edges), MaxOrbit: maxOrbit,
			CollectionEdgeCount: len(collectionEdges),
		},
		Nodes: nodes, Edges: edges, CollectionEdges: collectionEdges, Communities: communities,
	}
}

func draftNode(n *graph.Node) bool {
	status, _ := n.Attrs["status"].(string)
	return strings.EqualFold(strings.TrimSpace(status), "draft")
}

func versionedIndex(n *graph.Node) bool {
	if n.Attrs["template"] != "index" {
		return false
	}
	version := 0
	switch v := n.Attrs["template_version"].(type) {
	case int:
		version = v
	case float64: // persisted JSON attrs
		if v >= 1 && v < float64(math.MaxInt) && v == math.Trunc(v) {
			version = int(v)
		}
	}
	if version <= 0 {
		return false
	}
	template, err := vault.TemplateFor("index", version)
	return err == nil && typeOf(n) == string(template.Type)
}

// projectCollections resolves exact stable IDs only. No title/path aliases,
// case-folded matches or guessed ownership. Canonical identity collisions fail
// closed even when the other node is hidden. Invalid/overlarge metadata contributes
// no memberships; accepted output is unique, sorted, and bounded by authoring limits.
func projectCollections(all, notes []*graph.Node, visible map[string]bool) ([]ExportEdge, map[string][]string, map[string]bool) {
	identityCounts := map[string]int{}
	canonical := func(id string) string { return norm.NFC.String(strings.ToLower(strings.TrimSpace(id))) }
	for _, n := range all {
		if n.Kind == "note" {
			identityCounts[canonical(n.NoteID)]++
		}
	}
	var edges []ExportEdge
	collections := map[string][]string{}
	unambiguous := map[string]bool{}
	for _, n := range notes {
		if identityCounts[canonical(n.NoteID)] != 1 {
			continue
		}
		unambiguous[n.NoteID] = true
		var ids []string
		switch values := n.Attrs["collections"].(type) {
		case []string:
			ids = values
		case []any:
			if len(values) > vault.MaxAuthoringList {
				continue
			}
			for _, value := range values {
				id, ok := value.(string)
				if !ok {
					ids = nil
					break
				}
				ids = append(ids, id)
			}
		}
		if len(ids) > vault.MaxAuthoringList {
			continue
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if id == "" || id == n.NoteID || seen[id] || identityCounts[canonical(id)] != 1 || !visible[notePrefix+id] {
				continue
			}
			seen[id] = true
			collections[n.ID] = append(collections[n.ID], id)
		}
		sort.Strings(collections[n.ID])
		for _, id := range collections[n.ID] {
			edges = append(edges, ExportEdge{Source: n.NoteID, Target: id, Rel: "collection-membership"})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Source != edges[j].Source {
			return edges[i].Source < edges[j].Source
		}
		return edges[i].Target < edges[j].Target
	})
	return edges, collections, unambiguous
}

func indexID(n *graph.Node) string {
	if n == nil {
		return ""
	}
	return n.NoteID
}

// orbits BFS the visible undirected semantic/membership projection from the index,
// returning navigation distance (keyed by node id "note:<id>") and its maximum. Every note
// not reached (an orphan with no links, or a disconnected component) is parked one
// ring beyond the farthest reachable note, so nothing collides with the center.
func orbits(adj map[string][]string, noteNodeIDs []string, indexNoteID string) (map[string]int, int) {
	dist := map[string]int{}
	if indexNoteID == "" {
		return dist, 0
	}
	start := notePrefix + indexNoteID
	dist[start] = 0
	queue := []string{start}
	maxd := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nb := range adj[cur] {
			if _, seen := dist[nb]; seen {
				continue
			}
			dist[nb] = dist[cur] + 1
			if dist[nb] > maxd {
				maxd = dist[nb]
			}
			queue = append(queue, nb)
		}
	}
	// Park every unreached note (orphans included) one ring out; only widen the
	// max if something was actually parked, so a fully-connected graph reports its
	// true farthest distance instead of an empty outer ring.
	outer := maxd + 1
	parked := false
	for _, id := range noteNodeIDs {
		if _, ok := dist[id]; !ok {
			dist[id] = outer
			parked = true
		}
	}
	if parked {
		maxd = outer
	}
	return dist, maxd
}

func strip(nodeID string) string { return strings.TrimPrefix(nodeID, notePrefix) }

func typeOf(n *graph.Node) string {
	if n.Attrs != nil {
		if t, ok := n.Attrs["type"].(string); ok {
			return t
		}
	}
	return ""
}

func lineOf(n *graph.Node) int {
	if loc := strings.TrimPrefix(n.SourceLoc, "L"); loc != n.SourceLoc {
		if v := atoi(loc); v > 0 {
			return v
		}
	}
	return 1
}

func atoi(s string) int {
	v := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		v = v*10 + int(r-'0')
	}
	return v
}

func tagsOf(g *graph.Graph, noteNodeID string) []string {
	var tags []string
	for _, e := range g.Neighbors(noteNodeID) {
		if e.Relation == "tagged" {
			tags = append(tags, strings.TrimPrefix(e.Target, "tag:"))
		}
	}
	return tags
}
