// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// readOnlyCommands are the CLI commands that only READ the index. Each one is a
// function in this package that builds a *cobra.Command, and none of them may open a
// writable store.
//
// This is the CLI half of the single-writer split. The MCP server, the TUI and the web
// viewer were each moved to a read-only store one at a time, and every time the reason
// was the same: a process that only reads has no business taking the SQLite write lock,
// because opening a writable store applies the schema, and applying the schema against a
// live owning writer fails with SQLITE_BUSY on an index that is perfectly fresh.
//
// `mesh doctor` was the one left behind, and it failed exactly that way on the live vault
// on 2026-08-10 while the owner was reconciling: "apply schema: another mesh process
// holds the write lock". The command reads two counts and a drift report. It never wrote
// anything.
//
// A command that genuinely writes (index, embed, health, sync, watch, extract) belongs in
// writerCommands instead. Every command that touches the index at all must appear in
// exactly one of the maps in this file; see TestEveryIndexTouchingCommandIsClassified.
var readOnlyCommands = map[string]string{
	"draftsCmd":        "bounded current-file draft browsing",
	"doctorCmd":        "counts rows and reports drift",
	"statusCmd":        "counts rows, loads the graph, reads vector stats",
	"guardsListCmd":    "reads stored gotchas",
	"guardsSuggestCmd": "reads stored gotchas",
	"searchCmd":        "runs one query",
	"orientCmd":        "loads the graph and reads ChangedSince; it is the SessionStart hook, so it runs concurrently with every other mesh process the user has open",
	"askCmd":           "retrieves and answers; the retriever and ask.Answer only read",
	"structureCmd":     "analyses structure and, with --wire-orphans, writes links into the MARKDOWN, never into the index",
	"codeSearchCmd":    "only calls store.SearchCode",
	"codeContextCmd":   "only calls store.SearchCode + store.NotesForSymbolName",
	"flywheelCmd":      "only calls store.FlywheelStats + store.TopReused",
	"economicsCmd":     "only reads content-free retrieval counters",
	"evalCmd":          "loads the graph and runs the retrieval benchmark",
	"tuneCmd":          "loads the graph and sweeps retrieval parameters",
}

// writerCommands are the commands that genuinely write the index, with what they write.
// Being on this list is not a licence to open writable, it is a record that somebody
// checked: the reason has to name a real write, so that "it has always opened writable"
// can never again pass for a justification.
var writerCommands = map[string]string{
	"indexCmd":            "rebuilds the whole index from the markdown; it IS the rebuild, hence OpenRebuild",
	"installCmd":          "builds the first index for a freshly wired agent (OpenRebuild + index.Reindex)",
	"initCmd":             "creates the vault and builds its first index (index.Reindex)",
	"joinCmd":             "joins a team vault and reconciles it into the index (index.Reconcile)",
	"embedCmd":            "stores BYOAI embeddings",
	"healthCmd":           "persists the note-health pass (store.ComputeHealth, store.ComputeContradictions)",
	"watchCmd":            "the owning writer: reindexes on every change",
	"syncCmd":             "pulls the team vault and reindexes it",
	"extractCmd":          "queues extracted candidates (store.AddPending) and reconciles the vault for dedup",
	"codeReindexCmd":      "rebuilds the source-code index (index.ReindexCode)",
	"conflictsResolveCmd": "reindexes the vault after applying a resolution (reindexBestEffort)",
	"ingestAllCmd":        "ingests every configured source into the vault and reindexes it (reindexVault)",
	"ingestGitHubCmd":     "ingests GitHub into the vault and reindexes it (reindexVault)",
	"ingestJiraCmd":       "ingests Jira into the vault and reindexes it (reindexVault)",
	"ingestLinearCmd":     "ingests Linear into the vault and reindexes it (reindexVault)",
	"ingestNotionCmd":     "ingests Notion into the vault and reindexes it (reindexVault)",
	"ingestSlackCmd":      "ingests Slack into the vault and reindexes it (reindexVault)",
}

// writableOpeners are the constructors that take the SQLite write lock: they
// apply the schema, start a writer goroutine and TRUNCATE-checkpoint on Close, and they
// CREATE the database when it is absent. index.OpenReadOnly does neither, which is why it
// is the only one a read-only command may call.
var writableOpeners = map[string]bool{
	"Open":             true,
	"OpenOwned":        true,
	"OpenAt":           true,
	"OpenCurrent":      true,
	"OpenCurrentAt":    true,
	"OpenRebuild":      true,
	"OpenRebuildOwned": true,
}

// readOnlyOpener is the one constructor a read-only command may use.
const readOnlyOpener = "OpenReadOnly"

// commandGraph is the AST view this file's guards reason over: every package-level
// function, which of them build a *cobra.Command, and which index constructors each one
// reaches through package-local helpers.
type commandGraph struct {
	fset  *token.FileSet
	funcs map[string][]*ast.FuncDecl // name -> decls (pro/stub twins share a name)
	ctors map[string]bool            // functions returning *cobra.Command
}

// parseCommandGraph reads the package source. Test files are excluded: the guard is about
// what ships, and a test may legitimately open a writable store to build a fixture.
func parseCommandGraph(t *testing.T) *commandGraph {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	cg := &commandGraph{fset: fset, funcs: map[string][]*ast.FuncDecl{}, ctors: map[string]bool{}}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Body == nil {
					continue
				}
				cg.funcs[fn.Name.Name] = append(cg.funcs[fn.Name.Name], fn)
				if returnsCobraCommand(fn) {
					cg.ctors[fn.Name.Name] = true
				}
			}
		}
	}
	if len(cg.ctors) == 0 {
		t.Fatal("parsed no *cobra.Command constructors: the AST walk is broken, not the package")
	}
	return cg
}

// returnsCobraCommand reports whether fn's signature returns *cobra.Command, which is how
// every CLI command in this package is built. This is the DERIVATION: the set of commands
// comes from the source, so a new one cannot be missed by a hand-written list.
func returnsCobraCommand(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, r := range fn.Type.Results.List {
		star, ok := r.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "cobra" && sel.Sel.Name == "Command" {
			return true
		}
	}
	return false
}

// opensReached walks a command constructor and every package-local helper it calls, and
// returns each index constructor call it can reach.
//
// The transitive walk is the second half of the fix. The guard used to look only inside
// the command function itself, so moving the open one line down into a helper (which is
// exactly what a tidy-up does) silently emptied the check. It stops at other command
// constructors: a parent command that merely AddCommands its children must not inherit
// their index usage, because each child is judged on its own.
func (cg *commandGraph) opensReached(root string) []openSite {
	var sites []openSite
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, fn := range cg.funcs[name] {
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					pkgIdent, ok := fun.X.(*ast.Ident)
					if !ok || pkgIdent.Name != "index" {
						return true
					}
					if writableOpeners[fun.Sel.Name] || fun.Sel.Name == readOnlyOpener {
						sites = append(sites, openSite{
							opener: fun.Sel.Name,
							via:    name,
							pos:    cg.fset.Position(call.Pos()).String(),
						})
					}
				case *ast.Ident:
					// A package-local helper. Command constructors are their own subjects.
					if cg.ctors[fun.Name] || seen[fun.Name] || len(cg.funcs[fun.Name]) == 0 {
						return true
					}
					seen[fun.Name] = true
					queue = append(queue, fun.Name)
				}
				return true
			})
		}
	}
	return sites
}

type openSite struct {
	opener string // one of writableOpeners, or OpenReadOnly
	via    string // the function the call is written in
	pos    string
}

// TestReadOnlyCommandsDoNotTakeTheWriteLock fails if a command on readOnlyCommands can
// reach any constructor in writableOpeners.
//
// AST rather than a runtime check, because the failure it guards against is not
// observable without a second live writer: on an idle laptop a writable open succeeds
// and everything looks fine. It only bites when the owner is busy, which is exactly when
// someone is running doctor to find out what is wrong.
func TestReadOnlyCommandsDoNotTakeTheWriteLock(t *testing.T) {
	cg := parseCommandGraph(t)
	for _, name := range sortedKeys(readOnlyCommands) {
		if !cg.ctors[name] {
			t.Errorf("%s is on readOnlyCommands but no function of that name builds a *cobra.Command in this package; "+
				"rename it here or remove the entry, do not leave the guard pointing at nothing", name)
			continue
		}
		for _, site := range cg.opensReached(name) {
			if !writableOpeners[site.opener] {
				continue
			}
			t.Errorf("%s (%s) reaches index.%s in %s at %s: it only reads, so it must use "+
				"index.OpenReadOnly. A writable open applies the schema, which fails with "+
				"SQLITE_BUSY against a live owning writer on an index that is perfectly fresh, "+
				"and it CREATES the database when absent, which turns a deleted index into a "+
				"silently empty one.",
				name, readOnlyCommands[name], site.opener, site.via, site.pos)
		}
	}
}

// TestEveryIndexTouchingCommandIsClassified is the guard on the guard.
//
// The read-only list used to be the only list, hand-written, and it drifted: orientCmd
// was added as the SessionStart hook, took index.Open for two pure reads, and this file
// stayed green over it for as long as the command existed, because a name that was never
// added is a name that is never checked. So the SUBJECTS are now derived from the AST
// (every function returning *cobra.Command) and each one that reaches the index at all
// must be classified. A new command is unclassified by construction, so it fails here
// until somebody states which half of the single-writer split it belongs to, and saying
// "read-only" immediately submits it to the test above.
func TestEveryIndexTouchingCommandIsClassified(t *testing.T) {
	cg := parseCommandGraph(t)
	for _, name := range sortedKeys(cg.ctors) {
		sites := cg.opensReached(name)
		if len(sites) == 0 {
			continue // never opens the index: nothing to classify
		}
		buckets := 0
		for _, m := range []map[string]string{readOnlyCommands, writerCommands} {
			if _, ok := m[name]; ok {
				buckets++
			}
		}
		switch buckets {
		case 1:
			// classified
		case 0:
			t.Errorf("%s opens the index (%s) but is classified nowhere.\n"+
				"    Classify it in cmd/mesh/readonly_commands_test.go:\n"+
				"      - readOnlyCommands, if it only READS (then it must call index.OpenReadOnly)\n"+
				"      - writerCommands, naming the write it performs\n"+
				"    An unclassified command is how `mesh orient` shipped as the SessionStart hook "+
				"holding the SQLite write lock for two pure reads.", name, describeSites(sites))
		default:
			t.Errorf("%s is classified in both readOnlyCommands and writerCommands: pick one", name)
		}
	}
	// A renamed or deleted command must not silently drop off any list.
	for _, m := range []map[string]string{readOnlyCommands, writerCommands} {
		for _, name := range sortedKeys(m) {
			if !cg.ctors[name] {
				t.Errorf("%s is classified in this file but no function of that name builds a "+
					"*cobra.Command in this package; rename it here or remove the entry", name)
			}
		}
	}
}

func describeSites(sites []openSite) string {
	var parts []string
	seen := map[string]bool{}
	for _, s := range sites {
		k := "index." + s.opener + " in " + s.via
		if seen[k] {
			continue
		}
		seen[k] = true
		parts = append(parts, k)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
