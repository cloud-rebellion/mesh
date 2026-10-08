// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/vault"
)

func linkedGraph() *graph.Graph {
	g := graph.New()
	add := func(id, label string) {
		g.AddNode(&graph.Node{ID: notePrefix + id, Kind: "note", Label: label, NoteID: id, NotePath: id + ".md", Attrs: map[string]any{"type": "note"}})
	}
	add("hub", "Hub")
	add("alpha", "Alpha")
	add("beta", "Beta")
	add("gamma", "Gamma")
	g.AddNode(&graph.Node{ID: "tag:core", Kind: "tag", Label: "core"})
	edge := func(s, t, rel string) { g.AddEdge(graph.Edge{Source: s, Target: t, Relation: rel}) }
	edge("note:hub", "note:alpha", "references")
	edge("note:hub", "note:beta", "references")
	edge("note:alpha", "note:beta", "references")
	edge("note:hub", "note:gamma", "references")
	edge("note:hub", "tag:core", "tagged")
	// Production always recomputes degrees once the graph is assembled (BuildGraph and
	// LoadGraph both do), and connectedness now means knowledge degree, which only that
	// pass fills in. A fixture that skips it is not the graph the export sees.
	g.RecomputeDegrees()
	g.DetectCommunities(0)
	return g
}

func collectionGraph(t *testing.T) *graph.Graph {
	t.Helper()
	var notes []*index.ParsedNote
	for _, spec := range []struct{ id, typ, template, collections string }{
		{"personal-index", "map", "index", ""},
		{"product", "entity", "entity", "[personal-index]"},
		{"incident", "post-mortem", "post-mortem", "[product]"},
		{"method", "concept", "method", "[product]"},
		{"marketing", "note", "plan", "[product]"},
	} {
		body := "---\nid: " + spec.id + "\ntitle: " + spec.id + "\ntype: " + spec.typ + "\ntemplate: " + spec.template + "\ntemplate_version: 1\ncollections: " + spec.collections + "\n---\n# " + spec.id + "\n\n## Summary\nUseful recorded context.\n"
		pn, err := index.Parse(spec.id+".md", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		notes = append(notes, pn)
	}
	g, _ := index.BuildGraph(notes)
	return g
}

func TestBuildExportCollectionsNavigateWithoutSemanticEdges(t *testing.T) {
	g := collectionGraph(t)
	exp := BuildExport(g, "/vault", nil, nil)
	if exp.Meta.IndexID != "personal-index" || exp.Meta.EdgeCount != 0 || exp.Meta.CollectionEdgeCount != 4 {
		t.Fatalf("authored collection navigation lost or counted as knowledge: %+v", exp.Meta)
	}
	for _, n := range exp.Nodes {
		wantOrbit := 2
		if n.ID == "personal-index" {
			wantOrbit = 0
		} else if n.ID == "product" {
			wantOrbit = 1
		}
		if n.Orbit != wantOrbit || n.Degree != 0 || n.Size != 1 {
			t.Fatalf("bad navigation/semantic separation: %+v", n)
		}
		original, _ := g.Node(notePrefix + n.ID)
		if original.KnowledgeDegree != 0 {
			t.Fatal("viewer mutated semantic degree")
		}
		for _, e := range g.Neighbors(original.ID) {
			if e.Relation == "collection-membership" {
				t.Fatal("viewer wrote navigation into graph")
			}
		}
	}
	// Persistence converts []string/int attrs into []any/float64; both produce
	// the exact same projection without changing retained metadata.
	for _, n := range g.Nodes() {
		encoded, _ := json.Marshal(n.Attrs)
		var persisted map[string]any
		if err := json.Unmarshal(encoded, &persisted); err != nil {
			t.Fatal(err)
		}
		for key, value := range persisted {
			g.SetNodeAttr(n.ID, key, value)
		}
	}
	if got := BuildExport(g, "/vault", nil, nil); !reflect.DeepEqual(got.CollectionEdges, exp.CollectionEdges) || got.Meta != exp.Meta {
		t.Fatalf("persisted attrs changed navigation: %+v", got)
	}
}

func TestBuildExportCollectionAdmissionAndVisibleCounts(t *testing.T) {
	g := graph.New()
	add := func(id, path, scope, status string) {
		g.AddNode(&graph.Node{ID: notePrefix + id, Kind: "note", NoteID: id, Label: "Title-" + id, NotePath: path,
			Attrs: map[string]any{"type": "map", "scope": scope, "status": status, "template": "index", "template_version": 1}})
	}
	add("root", "public/root.md", "sales", "")
	add("public", "public/note.md", "sales", "")
	add("scope-hidden", "public/secret.md", "dev", "")
	add("folder-hidden", "private/secret.md", "sales", "")
	add("draft-secret", "public/draft.md", "sales", " Draft ")
	add("bare-secret", "", "sales", "")
	add("Ambiguous", "private/ambiguous.md", "dev", "")
	add("ambiguous", "public/ambiguous.md", "sales", "")
	g.SetNodeAttr("note:public", "collections", []string{"root", "root", "public", "scope-hidden", "folder-hidden", "draft-secret", "bare-secret", "missing-secret", "ambiguous", "ROOT", "public/root.md", "Title-root"})
	for _, id := range []string{"root", "scope-hidden", "folder-hidden", "draft-secret"} {
		g.AddEdge(graph.Edge{Source: "note:public", Target: notePrefix + id, Relation: graph.RelReferences})
	}
	g.RecomputeDegrees()
	before, _ := g.Node("note:public")
	if before.KnowledgeDegree != 4 {
		t.Fatal("fixture must have hidden knowledge neighbors")
	}
	exp := BuildExport(g, "/vault", map[string]bool{"sales": true}, func(p string) bool { return strings.HasPrefix(p, "public/") })
	if exp.Meta.NodeCount != 3 || exp.Meta.EdgeCount != 1 || exp.Meta.CollectionEdgeCount != 1 || len(exp.CollectionEdges) != 1 {
		t.Fatalf("bad visible counts: %+v", exp.Meta)
	}
	if exp.CollectionEdges[0] != (ExportEdge{Source: "public", Target: "root", Rel: "collection-membership"}) {
		t.Fatal(exp.CollectionEdges)
	}
	for _, n := range exp.Nodes {
		if n.ID == "public" && (n.Degree != 1 || !reflect.DeepEqual(n.Collections, []string{"root"})) {
			t.Fatalf("hidden counts/IDs in card: %+v", n)
		}
	}
	encoded, _ := json.Marshal(exp)
	for _, secret := range []string{"scope-hidden", "folder-hidden", "draft-secret", "bare-secret", "missing-secret", "Title-Ambiguous", "private/"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("projection leaked %s", secret)
		}
	}
	if before.KnowledgeDegree != 4 {
		t.Fatal("projection changed retrieval degree")
	}
	// Unrestricted control still sees real authorized members, while a draft and
	// missing-path placeholder never become a published navigation endpoint.
	admin := BuildExport(g, "/vault", nil, nil)
	if admin.Meta.NodeCount != 6 || admin.Meta.CollectionEdgeCount != 3 {
		t.Fatalf("bad unrestricted control: %+v", admin.Meta)
	}
}

func TestBuildExportCollectionBoundsOrderAndIndexSelection(t *testing.T) {
	g := collectionGraph(t)
	g.SetNodeAttr("note:method", "collections", []string{"product", "personal-index", "product", "method"})
	first := BuildExport(g, "/vault", nil, nil)
	g.SetNodeAttr("note:method", "collections", []string{"method", "personal-index", "product"})
	second := BuildExport(g, "/vault", nil, nil)
	if !reflect.DeepEqual(first.CollectionEdges, second.CollectionEdges) {
		t.Fatal("membership order changed projection")
	}
	for _, invalid := range []any{[]any{"product", 4}, strings.Repeat("product", 100), make([]string, vault.MaxAuthoringList+1)} {
		g.SetNodeAttr("note:method", "collections", invalid)
		for _, e := range BuildExport(g, "/vault", nil, nil).CollectionEdges {
			if e.Source == "method" {
				t.Fatalf("unbounded or malformed memberships admitted: %v", invalid)
			}
		}
	}
	// Unsupported or omitted template versions do not claim explicit index status.
	for _, invalidVersion := range []any{0, 2, 1.5, "1"} {
		g.SetNodeAttr("note:personal-index", "template_version", invalidVersion)
		if got := BuildExport(g, "/vault", nil, nil).Meta.IndexID; got != "incident" {
			t.Fatalf("invalid version %v selected explicit index %s", invalidVersion, got)
		}
	}
	g.SetNodeAttr("note:personal-index", "template_version", 1)
	g.SetNodeAttr("note:personal-index", "type", "note")
	if got := BuildExport(g, "/vault", nil, nil).Meta.IndexID; got != "incident" {
		t.Fatal("template/type mismatch chose index")
	}
	// Multiple supported explicit indexes: visible semantic degree, then ID; a
	// collection list never chooses one arbitrarily as the main home.
	g.SetNodeAttr("note:personal-index", "type", "map")
	g.SetNodeAttr("note:product", "type", "map")
	g.SetNodeAttr("note:product", "template", "index")
	if got := BuildExport(g, "/vault", nil, nil).Meta.IndexID; got != "personal-index" {
		t.Fatal("index tie not deterministic")
	}
	g.AddEdge(graph.Edge{Source: "note:product", Target: "note:incident", Relation: graph.RelReferences})
	g.RecomputeDegrees()
	if got := BuildExport(g, "/vault", nil, nil).Meta.IndexID; got != "product" {
		t.Fatal("visible explicit index degree ignored")
	}
}

func TestCollectionGraphHTTPUsesCurrentScopeAndFolderFence(t *testing.T) {
	dir := t.TempDir()
	for path, body := range map[string]string{
		"public/index.md":      "---\nid: home\ntitle: Public home\ntype: map\ntemplate: index\ntemplate_version: 1\nscope: sales\n---\n# Public home\n",
		"public/product.md":    "---\nid: product\ntitle: Public product\ntype: entity\nscope: sales\ncollections: [home, restricted-product, private-root, draft-root]\n---\n# Product\n[[restricted-product]] [[private-root]]\n",
		"public/restricted.md": "---\nid: restricted-product\ntitle: RestrictedProduct\ntype: entity\nscope: dev\ncollections: [home]\n---\n# Restricted\n",
		"private/index.md":     "---\nid: private-root\ntitle: PrivateRoot\ntype: map\ntemplate: index\ntemplate_version: 1\nscope: sales\ncollections: [home]\n---\n# Private\n",
		"public/draft.md":      "---\nid: draft-root\ntitle: DraftRoot\ntype: map\nstatus: draft\nscope: sales\ncollections: [home]\n---\n# Draft\n",
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seedIndex(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	allowed := true
	s.SetMemberAuth(
		func(token string) (int64, string, bool) { return 2, "sales", token == "fixture-member" },
		func(int64) map[string]bool { return map[string]bool{"sales": true} },
		func(int64) func(string) bool {
			return func(p string) bool { return allowed && strings.HasPrefix(p, "public/") }
		},
		func(int64) (string, int64, bool) { return "member", 2000, true },
	)
	request := func() Export {
		req := httptest.NewRequest(http.MethodGet, "/graph.json", nil)
		req.Header.Set("Authorization", "Bearer fixture-member")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("graph HTTP %d: %s", w.Code, w.Body.String())
		}
		for _, secret := range []string{"RestrictedProduct", "PrivateRoot", "DraftRoot", "restricted-product", "private-root", "draft-root"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("HTTP graph leaked %s", secret)
			}
		}
		var exp Export
		if err := json.Unmarshal(w.Body.Bytes(), &exp); err != nil {
			t.Fatal(err)
		}
		return exp
	}
	before := request()
	if before.Meta.IndexID != "home" || before.Meta.NodeCount != 2 || before.Meta.EdgeCount != 0 || before.Meta.CollectionEdgeCount != 1 {
		t.Fatalf("HTTP projection: %+v", before.Meta)
	}
	for _, n := range before.Nodes {
		if n.Degree != 0 {
			t.Fatal("HTTP leaked hidden semantic counts")
		}
	}
	allowed = false
	after := request()
	if after.Meta.NodeCount != 0 || after.Meta.CollectionEdgeCount != 0 || after.Meta.EdgeCount != 0 || after.Meta.IndexID != "" {
		t.Fatal("projection retained revoked folder visibility")
	}
}

func TestBuildExport(t *testing.T) {
	exp := BuildExport(linkedGraph(), "/vault", nil, nil)

	// Notes only (the tag node is excluded), index = the most-connected note. hub links
	// three notes, alpha and beta two each, gamma one: the center is a real hub, not
	// whichever note happens to own the most headings.
	if exp.Meta.NodeCount != 4 || len(exp.Nodes) != 4 {
		t.Fatalf("want 4 note nodes, got %d", len(exp.Nodes))
	}
	if exp.Meta.IndexID != "hub" {
		t.Fatalf("index should be the most-connected note (hub), got %q", exp.Meta.IndexID)
	}
	if exp.Nodes[0].ID != "hub" {
		t.Fatalf("nodes should be importance-sorted (hub first), got %q", exp.Nodes[0].ID)
	}

	// Edges are note-to-note only (no tag edges leak in).
	hex := regexp.MustCompile(`^#[0-9a-f]{6}$`)
	for _, e := range exp.Edges {
		if e.Source == "" || e.Target == "" {
			t.Fatalf("empty edge endpoint: %+v", e)
		}
	}
	if exp.Meta.EdgeCount != 4 {
		t.Fatalf("want 4 note-note edges (the tag edge excluded), got %d", exp.Meta.EdgeCount)
	}

	byID := map[string]ExportNode{}
	for _, n := range exp.Nodes {
		byID[n.ID] = n
	}
	// Galaxy orbit: index at 0, its neighbors at 1.
	if byID["hub"].Orbit != 0 {
		t.Fatalf("index note orbit must be 0, got %d", byID["hub"].Orbit)
	}
	if byID["alpha"].Orbit != 1 || byID["beta"].Orbit != 1 || byID["gamma"].Orbit != 1 {
		t.Fatalf("hub's neighbors should be orbit 1: alpha=%d beta=%d gamma=%d",
			byID["alpha"].Orbit, byID["beta"].Orbit, byID["gamma"].Orbit)
	}
	// Tags surfaced from tagged edges.
	if len(byID["hub"].Tags) != 1 || byID["hub"].Tags[0] != "core" {
		t.Fatalf("hub should carry the 'core' tag, got %v", byID["hub"].Tags)
	}
	// Size grows with connectedness, and connectedness is links: gamma is a one-link
	// leaf, so it must be the smallest dot no matter how long its page is.
	if byID["hub"].Size <= byID["alpha"].Size {
		t.Fatalf("more-connected note should have a larger size: hub=%v alpha=%v", byID["hub"].Size, byID["alpha"].Size)
	}
	if byID["gamma"].Size >= byID["alpha"].Size {
		t.Fatalf("a one-link leaf should be smaller than a two-link note: gamma=%v alpha=%v", byID["gamma"].Size, byID["alpha"].Size)
	}
	if byID["hub"].Degree != 3 || byID["gamma"].Degree != 1 {
		t.Fatalf("exported degree must be distinct linked notes: hub=%d gamma=%d", byID["hub"].Degree, byID["gamma"].Degree)
	}
	// Communities all carry a valid color.
	if len(exp.Communities) == 0 {
		t.Fatal("expected at least one community")
	}
	for _, c := range exp.Communities {
		if !hex.MatchString(c.Color) {
			t.Fatalf("community %d has a bad color %q", c.ID, c.Color)
		}
	}
	if exp.Meta.Vault != "/vault" {
		t.Fatalf("vault root should be carried for the editor bridge, got %q", exp.Meta.Vault)
	}
}

func TestBuildExportEmpty(t *testing.T) {
	exp := BuildExport(graph.New(), "/vault", nil, nil)
	if exp.Meta.NodeCount != 0 || len(exp.Nodes) != 0 || exp.Meta.IndexID != "" {
		t.Fatalf("empty graph should export nothing, got %+v", exp.Meta)
	}
}

// Exercise the shipped controller functions with a synthetic DOM/3D transport.
// This is client behavior coverage, not rendered browser or GPU acceptance.
func TestCollectionProjectionClient(t *testing.T) {
	runtime, err := exec.LookPath("node")
	if err != nil {
		runtime, err = exec.LookPath("bun")
		if err != nil {
			t.Skip("client behavior fixture requires Node or Bun; not rendered acceptance")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtime, "-", "assets/app.js")
	cmd.Stdin = strings.NewReader(collectionClientFixture)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shipped collection client fixture: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "PASS shipped collection controller") {
		t.Fatal("missing completion receipt")
	}
	t.Log(strings.TrimSpace(string(output)))
}

const collectionClientFixture = `
const assert = require('node:assert/strict'), fs = require('node:fs'), vm = require('node:vm');
const source = fs.readFileSync(process.argv[2], 'utf8');
const fragment = (from, to) => {
  const start = source.indexOf(from), end = source.indexOf(to, start);
  assert.ok(start >= 0 && end > start, 'ship function boundaries');
  return source.slice(start, end);
};
const home = {id:'home',label:'Home <script>',path:'home.md',type:'map',degree:0,collections:[],community:1};
const product = {id:'product',label:'Product',path:'product.md',type:'entity',degree:0,collections:['home','missing-secret'],community:2};
const method = {id:'method',label:'Method',path:'method.md',type:'concept',degree:0,collections:['product','home'],community:3};
const G = {meta:{vault:'/fixture',node_count:3,edge_count:0,collection_edge_count:3,index_id:'home'},nodes:[home,product,method],communities:[],edges:[],collection_edges:[
 {source:'product',target:'home',rel:'collection-membership'},
 {source:'method',target:'product',rel:'collection-membership'},
 {source:'method',target:'home',rel:'collection-membership'}]};
const elements = new Map(), clicked = [], buttons = [];
const element = id => elements.get(id) || elements.set(id, {innerHTML:'',textContent:'',classList:{toggle(){},remove(){},add(){}},setAttribute(){},querySelectorAll(selector){return selector === '.collection-link' ? buttons : []}}).get(id);
buttons.push({dataset:{id:'home'}});
const context = vm.createContext({G,console,Map,Set,Int32Array,Uint8Array,Float64Array,
nodeIndex:new Map(G.nodes.map((n,i)=>[n.id,i])),byId:new Map(G.nodes.map(n=>[n.id,n])),
deg:null,chg:null,CHARGE:0.15,grouping:'community',selected:null,gl3d:null,gl3dCam:null,
canvas3d:{},domainColor:new Map(),commColor:new Map(),captureStill:true,inset:0,spotlight:null,
activeGroups:()=>[],groupKeyOf:n=>n.community,groupColorOf:()=> '#7c766e',calm:()=>true,wheelIntent:()=>{},
focusNodeById:id=>clicked.push('focus:'+id),Mesh:{openNote:id=>clicked.push('open:'+id)},
navigator:{},$:element,window:{Mesh:{}},joinPath:(a,b)=>a+'/'+b,
esc:s=>String(s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))});
vm.runInContext(fragment('  const adj = new Map();','  // ---- float:'),context);
vm.runInContext('buildAdjacency()',context);
assert.deepEqual(JSON.parse(vm.runInContext('JSON.stringify([...adj.get("home")])',context)),['product','method']);
assert.equal(vm.runInContext('edgeIdx.length',context),6);
assert.equal(vm.runInContext('[...collectionPair].every(Boolean)',context),true);
assert.equal(G.edges.length,0,'semantic edges are not overwritten');
assert.ok(G.nodes.every(n=>n.degree===0),'semantic card counts are unchanged');
G.edges.push({source:'home',target:'product',rel:'references'});
G.collection_edges.push({source:'home',target:'product',rel:'collection-membership'},{source:'home',target:'home',rel:'collection-membership'},{source:'home',target:'missing-secret',rel:'collection-membership'});
vm.runInContext('buildAdjacency()',context);
assert.equal(vm.runInContext('edgeIdx.length',context),6,'overlaps/self/missing do not duplicate springs');
assert.equal(vm.runInContext('collectionPair[0]',context),0,'semantic style wins overlapping membership');
vm.runInContext(fragment('  function showCard(n)','  function hideCard()'),context);
vm.runInContext('showCard(G.nodes[1])',context);
assert.match(element('card').innerHTML,/links <b>0<\/b>/);
assert.match(element('card').innerHTML,/Collections/);
assert.match(element('card').innerHTML,/Home &lt;script&gt;/);
assert.doesNotMatch(element('card').innerHTML,/missing-secret|<script>/);
buttons[0].onclick();
assert.deepEqual(clicked,['focus:home','open:home']);
vm.runInContext(fragment('  function setStats()','  function buildLegend()'),context);
vm.runInContext('setStats()',context);
assert.match(element('stats').textContent,/0 links \/ 3 collection memberships/);
vm.runInContext(fragment('  function buildExplorer()','  function resize()'),context);
vm.runInContext('buildExplorer()',context);
assert.match(element('exp-list').innerHTML,/Collection · Home &lt;script&gt;/);
assert.match(element('exp-list').innerHTML,/Collection · Product/);
assert.doesNotMatch(element('exp-list').innerHTML,/missing-secret|<script>/);
let rendered = null;
context.window.Mesh3D={init:(canvas,data,options)=>{rendered={data,options};return {setSpotlight(){}};}};
vm.runInContext(fragment('  function initGl3d()','  function setView(v)'),context);
vm.runInContext('initGl3d()',context);
assert.equal(rendered.options.indexId,'home');
assert.equal(rendered.data.edges.length,G.edges.length+G.collection_edges.length);
assert.equal(G.edges.length,1,'3D receives navigation copy, semantic graph retained');
assert.equal(rendered.data.nodes,G.nodes);
console.log('PASS shipped collection controller: adjacency, dedup, labels, escaping, click routing, explorer, 3D copy; synthetic DOM/GPU transport only');
`
