// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package watch keeps a Mesh index live with the vault on disk. It is the
// local-first, Obsidian-like immediacy of Milestone S0: edit a note in your
// editor and it is searchable at once, no commit, no manual `mesh index`.
//
// The design mirrors Milestone S's "reconcile-first" principle at local scale.
// The periodic reconcile is the primary consistency mechanism, so it always
// converges and a missed or dropped file event never leaves lasting drift. The
// fsnotify event stream is a best-effort speedup layered on top, debounced so a
// burst of editor saves collapses into one reindex. The expensive reindex runs
// single-flight on one goroutine and only rebuilds when retrieval-relevant
// content actually changed, so an idle vault costs nothing but a cheap scan.
//
// That tick runs at two depths. Every fire takes the mtime fast path, which sees
// added, removed and normally-edited notes for one stat each; only every
// DefaultFullReconcile does it escalate to the authoritative content-hash check
// `mesh doctor` uses. Both halves used to run on the short interval, which meant
// re-parsing the entire vault seven times a minute forever on an idle machine.
package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/fsnotify/fsnotify"
)

// Result is what one reconcile did, for the progress log.
type Result struct {
	Added     int
	Changed   int
	Removed   int
	Reindexed bool
	Dur       time.Duration
}

// Pass reasons. A callback that needs to know WHY it is running must read Pass.Reason
// rather than infer it from Pass.Authoritative: those two used to move together, and
// `mesh sync` inferred "this is the periodic tick" from authoritative=true to rate-limit
// its hub round. The moment the tick stopped being authoritative on every fire, that
// inference flipped and the daemon started a full hub sync every 8 seconds instead of
// every 60.
const (
	ReasonStartup = "startup" // first pass, before anything is known about the vault
	ReasonChange  = "change"  // a debounced local edit, or an external Trigger nudge
	ReasonTick    = "tick"    // the periodic safety net, with nothing observed to prompt it
	ReasonRefresh = "refresh" // completed external work; discover files without requesting that work again
)

// Pass describes one reconcile the watcher is asking the callback to run.
type Pass struct {
	Reason string // ReasonStartup / ReasonChange / ReasonTick / ReasonRefresh
	// Authoritative selects the content-hash drift check (parses every note) over the
	// mtime fast path (one stat per note). True on startup and on the periodic FULL pass.
	Authoritative bool
	// Paths is the exact set of markdown paths reported by fsnotify for a local
	// change burst. It is empty for startup, periodic, directory, and external
	// trigger passes, where the callback must discover drift across the vault.
	// Paths are absolute and de-duplicated. Carrying them lets the owning writer
	// publish a one-note write-back without walking every note first.
	Paths []string
}

// Options configure a watch run.
type Options struct {
	Root          string                     // vault root to watch
	Debounce      time.Duration              // quiet window to coalesce a save burst; <=0 uses the default
	Reconcile     time.Duration              // periodic safety-net interval; <=0 disables the tick
	FullReconcile time.Duration              // how often the periodic tick escalates to the authoritative full-hash pass; <=0 uses the default
	OnReindex     func(Pass) (Result, error) // drift-check + reindex, single-flight
	Logf          func(string, ...any)       // progress sink; nil is silent
	Trigger       <-chan struct{}            // optional external nudge (e.g. an SSE event); fires OnReindex like a local change
	Refresh       <-chan struct{}            // optional completed-work nudge; emits ReasonRefresh unless merged with a real change
}

const defaultDebounce = 300 * time.Millisecond

// changeBatch records observed events, not when a filesystem write happened.
// A refresh may precede a delayed fsnotify event in a separate pass; it must
// never erase an outbound sync request already observed in this batch.
type changeBatch struct {
	paths           map[string]struct{}
	full, needsSync bool
}

func (b *changeBatch) change(path string) {
	b.needsSync = true
	if path == "" {
		b.refresh()
	} else if !b.full {
		if b.paths == nil {
			b.paths = make(map[string]struct{})
		}
		b.paths[filepath.Clean(path)] = struct{}{}
	}
}

func (b *changeBatch) refresh() {
	b.full = true
	clear(b.paths)
}

func (b *changeBatch) take() Pass {
	p := Pass{Reason: ReasonRefresh}
	if b.needsSync {
		p.Reason = ReasonChange
	}
	if !b.full {
		for path := range b.paths {
			p.Paths = append(p.Paths, path)
		}
		sort.Strings(p.Paths)
	}
	b.full, b.needsSync = false, false
	clear(b.paths)
	return p
}

// DefaultFullReconcile is how often the safety tick runs the AUTHORITATIVE pass, which
// parses and content-hashes every note in the vault. The tick itself has to stay frequent
// (a note that missed its file event must be indexed before a reader gives up at
// mcp.OwnerIndexBound), but the expensive half does not: the cheap mtime pass already
// catches every added, removed and normally-edited file, and it costs one stat per note
// instead of a parse.
//
// Running BOTH halves on the same short interval is what made an idle laptop hot. Measured
// 2026-08-10 on a 1216-note vault: `mesh sync --watch` burned 5.6% of a core doing nothing
// but re-parsing an unchanged vault every 8s, and each `mesh mcp --watch` (one per open
// agent session) another ~2.8%, so three idle daemons held ~12% of a core between them
// around the clock.
//
// What the longer interval actually costs: the ONLY drift the mtime pass cannot see is a
// content change that leaves mtime untouched (a mtime-preserving write, or a second edit
// inside the same one-second mtime granularity). That case now converges within
// defaultFullReconcile instead of within Reconcile. Everything else is unchanged.
const DefaultFullReconcile = 5 * time.Minute

// Run watches opt.Root until ctx is cancelled, calling opt.OnReindex whenever
// the vault changes (debounced) and on a periodic safety tick. It reconciles
// once at startup so the index reflects disk from the first moment. Run blocks;
// callers typically run it in a goroutine alongside their main loop.
func Run(ctx context.Context, opt Options) error {
	return runWithDirectoryDiscovery(ctx, opt, vault.DirsContext)
}

// The read-only discovery seam tests cancellation before startup publication
// without mutable global hooks or an actual unhealthy network filesystem.
func runWithDirectoryDiscovery(ctx context.Context, opt Options, discover func(context.Context, string) ([]string, error)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if opt.OnReindex == nil {
		return errors.New("watch: OnReindex is required")
	}
	if ctx.Err() != nil {
		return nil
	}
	if opt.Debounce <= 0 {
		opt.Debounce = defaultDebounce
	}
	if opt.FullReconcile <= 0 {
		opt.FullReconcile = DefaultFullReconcile
	}
	logf := opt.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if err := addWatchesContext(ctx, w, opt.Root, logf, discover); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if ctx.Err() != nil {
		return nil
	}

	// Reflect disk from the moment we start (bootstraps an empty index too). Startup is
	// always authoritative: nothing is known about what changed while we were not running.
	var lastFull time.Time
	if reconcile(opt, logf, Pass{Reason: ReasonStartup, Authoritative: true}) {
		lastFull = time.Now()
	}

	// Debounce timer, created stopped: armed only once an event arrives.
	debounce := time.NewTimer(opt.Debounce)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()
	var pending changeBatch

	var tick <-chan time.Time
	if opt.Reconcile > 0 {
		t := time.NewTicker(opt.Reconcile)
		defer t.Stop()
		tick = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			switch {
			case ev.Op&fsnotify.Create != 0 && isDir(ev.Name):
				// A new subdirectory: kqueue/inotify watch one dir at a time, so
				// add it (and any children) to the watch set, then reconcile in
				// case files landed inside before the watch was in place.
				if err := addWatchesContext(ctx, w, opt.Root, logf, discover); err != nil {
					if ctx.Err() != nil {
						return nil
					}
					logf("watch directory discovery: %v", err)
				}
				pending.change("")
				resetTimer(debounce, opt.Debounce)
			case ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 && watched(w, ev.Name):
				// A watched directory went away: drop its now-dangling watch so we
				// do not leak a descriptor, and reconcile so its notes leave the
				// index promptly rather than waiting for the periodic tick.
				_ = w.Remove(ev.Name)
				pending.change("")
				resetTimer(debounce, opt.Debounce)
			case relevant(ev):
				pending.change(ev.Name)
				resetTimer(debounce, opt.Debounce)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			logf("watch error: %v", err)
		case <-opt.Trigger:
			// An external nudge (a hub SSE "head changed" event). Treat it like a
			// local change: arm the debounce so a burst of nudges coalesces into one
			// reconcile. A nil Trigger channel blocks forever, so this case is inert
			// unless a caller wired one in.
			pending.change("")
			resetTimer(debounce, opt.Debounce)
		case <-opt.Refresh:
			// Completion must discover incoming/partially-applied files even if
			// fsnotify missed them, but must not create a sync -> refresh -> sync
			// feedback loop. A coalesced real edit/SSE still wins via needsSync.
			pending.refresh()
			resetTimer(debounce, opt.Debounce)
		case <-debounce.C:
			reconcile(opt, logf, pending.take())
		case <-tick:
			// Safety net: catches anything the event stream missed (a dropped event,
			// a same-second rename, a note written before our watches were in place).
			// The cheap mtime pass sees all of those, so it runs on every tick; the
			// authoritative content-hash pass, which parses the whole vault, runs only
			// once per FullReconcile. See DefaultFullReconcile for why the two cadences
			// are separate.
			full := lastFull.IsZero() || time.Since(lastFull) >= opt.FullReconcile
			if reconcile(opt, logf, Pass{Reason: ReasonTick, Authoritative: full}) && full {
				lastFull = time.Now()
			}
		}
	}
}

// reconcile runs one drift-check + reindex. The caller decides both halves of the Pass:
// which reason this is, and whether it gets the content-hash check or the mtime fast path.
func reconcile(opt Options, logf func(string, ...any), p Pass) bool {
	res, err := opt.OnReindex(p)
	if err != nil {
		logf("reindex failed (%s): %v", p.Reason, err)
		return false
	}
	if res.Reindexed {
		logf("reindexed +%d ~%d -%d in %s (%s)",
			res.Added, res.Changed, res.Removed, res.Dur.Round(time.Millisecond), p.Reason)
	}
	return true
}

// addWatchesContext (re)adds a watch on every indexed directory under root. fsnotify
// dedupes repeat adds, so calling it again after a new directory appears simply
// picks up the newcomer. It honors the same skip rules the indexer walks with.
func addWatchesContext(ctx context.Context, w *fsnotify.Watcher, root string, logf func(string, ...any), discover func(context.Context, string) ([]string, error)) error {
	dirs, err := discover(ctx, root)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.Add(d); err != nil {
			logf("watch add %s: %v", d, err)
		}
	}
	return ctx.Err()
}

// watched reports whether path is currently in the fsnotify watch set (i.e. a
// directory we added), so a Remove/Rename event can be told apart from churn on
// files and temp artifacts we never watched.
func watched(w *fsnotify.Watcher, path string) bool {
	clean := filepath.Clean(path)
	for _, p := range w.WatchList() {
		if filepath.Clean(p) == clean {
			return true
		}
	}
	return false
}

// relevant reports whether an event is a content change to a markdown note.
// Pure Chmod events (and non-.md files) are ignored.
func relevant(ev fsnotify.Event) bool {
	if !strings.EqualFold(filepath.Ext(ev.Name), ".md") {
		return false
	}
	if vault.IsConflictSibling(filepath.Base(ev.Name)) {
		return false // conflict artifacts are not indexed; ignore their churn
	}
	return ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// resetTimer safely restarts a timer for debouncing: stop, drain any pending
// fire, then reset. Called only from Run's single goroutine.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
