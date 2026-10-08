// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package mcp serves Mesh's retrieval + write-back surface to a coding agent
// over JSON-RPC 2.0 on stdio. A local agent (Claude Code / Codex) spawns
// `mesh mcp` and talks to it directly; no port or auth surface. The JSON-RPC
// envelope follows the standard MCP shape.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/latency"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/watch"
)

const (
	protocolVersion = "2024-11-05"
	serverName      = "mesh"
	serverVersion   = "0.1.0"
)

// Server holds the live index, graph, and retriever for one vault.
//
// Concurrency: tool calls run on the ServeStdio goroutine while the optional
// background watcher (Watch) rebuilds on file changes. mu guards the graph +
// retriever pointers so a reader always sees a consistent pair; reloadMu makes a
// rebuild single-flight so the dispatch goroutine (a write-back) and the watcher
// never reindex at the same time.
type Server struct {
	vaultRoot        string
	retrievalOptions retrieve.ConstructionOptions
	store            *index.Store

	mu        sync.RWMutex // guards graph + retriever
	graph     *graph.Graph
	retriever *retrieve.Retriever

	reloadMu sync.Mutex       // serializes rebuilds across dispatch + watcher
	electMu  sync.Mutex       // serializes opportunistic recovery elections
	cache    *index.NoteCache // parsed-note cache for incremental reconcile; guarded by reloadMu
	// viewHashes fingerprints the index the in-memory graph was last loaded from
	// (path -> retrieval hash), so a read-only server can report what a refresh changed
	// in its view. Guarded by reloadMu; nil until the first refresh.
	viewHashes map[string]string
	// Refresh reuse state is guarded by reloadMu. The monitor is connection-local;
	// it must never be replaced while retaining a reusable version stamp.
	changeMonitor          *index.ChangeMonitor
	viewVersion            index.ReaderVersion
	viewConfig             [32]byte
	viewReusable           bool
	viewBuildComplete      bool
	refreshClosed          bool
	beforeReaderInstall    func() // deterministic concurrent-commit test seam
	afterCachedNoteVersion func() // deterministic acknowledgement proof race seam

	// owner is the vault's owning-writer claim when THIS process elected itself (see
	// NewOwningServer). nil on a read-only server and on the hub, which owns an index
	// nobody else can reach. A declared owner (`mesh watch`, `mesh sync --watch`,
	// `mesh ui --own-index`) can take the claim back mid-session, so every write path
	// asks owns() rather than assuming the store's writability is the whole answer.
	owner *index.OwnerLock
	// ownerRole is non-empty only for NewOwningServer. It lets a server that lost a
	// temporary declared-owner interval recover indexing on the next watch/tool pass.
	ownerRole string

	reindexMu      sync.Mutex // guards the mesh_reindex throttle state below
	lastReindexAt  time.Time
	lastReindex    index.Reconciliation
	lastReindexErr error // ErrOwnerNotIndexing when that pass left the index stale

	ready    chan struct{} // closed when retrieval is servable (initial reload done)
	readyErr error         // written once before ready closes
	bg       chan struct{} // closed when ALL background startup work is done (enrichment included)
	opCancel context.CancelFunc
	opDone   chan struct{}
	opWake   chan struct{} // deterministic wake in tests; ticker is the cross-process path

	agent string // calling client's name from initialize (provenance default), guarded by mu

	// lastSearch connects one subsequent mesh_fetch to the ranked slate that
	// surfaced it. It stores only ids, ranks, route and time, never query or note
	// content, and is guarded separately because HTTP transports may be concurrent.
	searchMu   sync.Mutex
	lastSearch map[string]searchAttribution

	// ownerIndexTimeout bounds the wait for the owning writer to index a just-written
	// note. A field rather than a bare const so a test can shorten it without mutating
	// global state (which would race across parallel tests); production never sets it.
	ownerIndexTimeout time.Duration
	notePublisher     NotePublisher
	// writePrepareTimeout bounds reversible write preparation before publication.
	// Zero selects the production default; tests shorten it without global mutation.
	writePrepareTimeout time.Duration
	// Deterministic test seam immediately before the atomic expected-version graph
	// snapshot. Production leaves it nil.
	beforeOwnerVersionRefresh func()
}

type searchAttribution struct {
	at    time.Time
	ranks map[string]int
	route string
}

// Deterministic startup seams for owner turnover tests. Production leaves them nil.
var afterMCPReadOnlyOpen func()
var beforeMCPOwnedOpen func(*index.OwnerLock)
var beforeMCPInitialReload func()
var beforeMCPAwaitOwnerCaughtUp func()
var beforeMCPEnrichment func()

// NewServer opens the vault's index (at <vaultRoot>/.mesh) READ-ONLY and loads it into
// memory. This is the per-window server for a vault that already has an owning writer:
// there can be many of these at once against one vault, so none of them may hold the
// SQLite write lock. Write-back still works, because creating a note is a filesystem
// write the owner picks up; see toolWrite.
//
// Most callers want NewOwningServer, which uses this whenever the vault is already
// owned and otherwise elects itself. Use NewServer directly only where something else
// is guaranteed to be the writer.
//
// NewServerAt (the hub) deliberately does NOT go through here: the hub owns the index it
// serves, so it keeps a writable store.
func NewServer(vaultRoot string) (*Server, error) {
	store, err := index.OpenReadOnly(vaultRoot)
	if err != nil {
		return nil, err
	}
	return newServerWithStore(vaultRoot, store, nil, "")
}

// NewServerWithRetrievalOptions is the normal reader with an explicit immutable
// construction choice and caller-controlled startup cancellation. It gains no
// index ownership or hosted authorization capabilities.
func NewServerWithRetrievalOptions(ctx context.Context, vaultRoot string, options retrieve.ConstructionOptions) (*Server, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store, err := index.OpenReadOnly(vaultRoot)
	if err != nil {
		return nil, err
	}
	return newServerWithStoreOptions(ctx, vaultRoot, store, nil, "", ownerIndexTimeout, options)
}

// NewOwningServer elects this process the vault's OWNING WRITER when no other live
// writer holds it, and falls back to the read-only server (NewServer) when one does.
// role names this surface in the lock, for the error a losing peer prints.
//
// This exists because the shipped agent config is `mesh mcp --vault <path>` and nothing
// else. A read-only MCP server cannot index, so on a vault with no separate `mesh watch`
// running (which is every new user) mesh_append_note blocked the full owner bound,
// returned index_stale + owner_down, and left the note unqueryable, while a note edited
// in the user's editor never became searchable at all. Electing here makes the shipped
// configuration self-sufficient without reintroducing multi-writer contention: the lock
// is what keeps it to one.
//
// The claim is preemptible. A declared owner started later (`mesh watch`,
// `mesh sync --watch`, `mesh ui --own-index`) takes it, and this server drops back to
// reader behaviour on its own; see owns.
func NewOwningServer(vaultRoot, role string) (*Server, error) {
	meshDir := filepath.Join(vaultRoot, ".mesh")
	deadline := time.Now().Add(index.OwnerStartupBound)
	for {
		lock, err := index.AcquireOwnerLock(meshDir, role, true)
		if errors.Is(err, index.ErrOwnerHeld) {
			// A declared owner can publish its claim before its first full pass. Do not
			// open the read-only connection against that in-flight rebuild: wait for the
			// explicit readiness marker, then verify the claim is still live.
			if _, starting := index.OwnerStarting(meshDir); starting {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					return nil, index.ErrOwnerNotReady
				}
				if werr := index.AwaitOwnerReady(context.Background(), meshDir, remaining); werr != nil {
					if _, live := index.OwnerStatus(meshDir); !live {
						continue
					}
					if time.Now().After(deadline) {
						return nil, werr
					}
					continue
				}
			}
			store, oerr := index.OpenReadOnly(vaultRoot)
			if oerr == nil {
				if afterMCPReadOnlyOpen != nil {
					afterMCPReadOnlyOpen()
				}
				if _, live := index.OwnerStatus(meshDir); live {
					s, serr := newServerWithStore(vaultRoot, store, nil, role)
					if serr != nil {
						_ = store.Close()
						return nil, serr
					}
					return s, nil
				}
				_ = store.Close()
			} else if _, live := index.OwnerStatus(meshDir); !live {
				// The holder vanished before a read-only open completed. Re-elect
				// instead of returning a permanently ownerless reader.
				continue
			}
			if time.Now().After(deadline) {
				if oerr != nil {
					return nil, oerr
				}
				return nil, ErrOwnerNotIndexing
			}
			time.Sleep(index.OwnerIndexPollInterval)
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := lock.MarkStarting(); err != nil {
			_ = lock.Release()
			return nil, err
		}
		if beforeMCPOwnedOpen != nil {
			beforeMCPOwnedOpen(lock)
		}
		store, oerr := index.OpenOwned(vaultRoot, lock)
		if oerr != nil {
			lost := !lock.Held()
			_ = lock.Release()
			if lost && errors.Is(oerr, index.ErrReadOnly) && time.Now().Before(deadline) {
				continue
			}
			return nil, oerr
		}
		s, serr := newServerWithStore(vaultRoot, store, lock, role)
		if serr != nil {
			_ = store.Close()
			_ = lock.Release()
			return nil, serr
		}
		return s, nil
	}
}

// NewServerAt is like NewServer but keeps the index in an explicit dir instead of
// <vaultRoot>/.mesh. The hub uses it to serve hosted MCP over its vault while
// indexing OUTSIDE the git repo (so the index never syncs to clients).
func NewServerAt(vaultRoot, indexDir string) (*Server, error) {
	store, err := index.OpenAt(vaultRoot, indexDir)
	if err != nil {
		return nil, err
	}
	return newServerWithStore(vaultRoot, store, nil, "")
}

func newServerWithStore(vaultRoot string, store *index.Store, owner *index.OwnerLock, ownerRole string) (*Server, error) {
	return newServerWithStoreTimeout(vaultRoot, store, owner, ownerRole, ownerIndexTimeout)
}

func newServerWithStoreTimeout(vaultRoot string, store *index.Store, owner *index.OwnerLock, ownerRole string, wait time.Duration) (*Server, error) {
	return newServerWithStoreOptions(context.Background(), vaultRoot, store, owner, ownerRole, wait, retrieve.ConstructionOptions{})
}

func newServerWithStoreOptions(ctx context.Context, vaultRoot string, store *index.Store, owner *index.OwnerLock, ownerRole string, wait time.Duration, options retrieve.ConstructionOptions) (*Server, error) {
	opCtx, opCancel := context.WithCancel(ctx)
	s := &Server{retrievalOptions: options, vaultRoot: vaultRoot, store: store, owner: owner, ownerRole: ownerRole, cache: index.NewNoteCache(), ready: make(chan struct{}), bg: make(chan struct{}), ownerIndexTimeout: wait, opCancel: opCancel, opDone: make(chan struct{}), opWake: make(chan struct{}, 1)}
	// The initial load runs in the background so the MCP handshake answers
	// immediately: a full reload of a grown vault plus the note<->code bridge
	// exceeds a client's connect timeout (Claude Code kills the server at 30s
	// and never retries, orphaning the whole session), and a concurrent
	// `mesh code reindex` holding the db write lock makes it worse. ready
	// closes as soon as retrieval is servable (reload done) so a tool call
	// gating on awaitReady waits ~1s, not for the enrichment passes below it.
	go func() {
		opPollStarted := false
		defer close(s.bg)
		defer func() {
			if !opPollStarted {
				close(s.opDone)
			}
		}()
		// A read-only store cannot reindex: ReindexFull rewrites notes / search_index /
		// nodes / edges, so on this path the owning writer has already done that work and
		// all this server has to do is read the result into memory. LoadGraph is pure SQL
		// over readDB and was written for exactly this split.
		var loadErr error
		if !s.owns() {
			_, loadErr = s.refreshContext(opCtx)
		} else {
			loadErr = s.load()
		}
		if errors.Is(loadErr, index.ErrReadOnly) && !s.owns() {
			// A declared owner may take an elected MCP claim after construction but
			// before the asynchronous ReindexFull commits. That displacement is not a
			// permanent startup failure: wait for/refresh the winner (or recover if it
			// already exited), exactly as later reindex passes do.
			// AwaitOwnerCaughtUp has its own exact owner bound. Give the wrapping context
			// one poll of slack so the semantic ErrOwnerNotIndexing wins its deadline race
			// with context.DeadlineExceeded and startup can attempt owner-exit recovery.
			ctx, cancel := context.WithTimeout(context.Background(), s.ownerIndexTimeout+index.OwnerIndexPollInterval)
			_, loadErr = s.reindexPass(ctx)
			cancel()
			if errors.Is(loadErr, ErrOwnerNotIndexing) {
				// The declared winner can exit after reindexPass observes its claim but
				// before/during AwaitOwnerCaughtUp. The wait then correctly reports stale,
				// but sealing that into readyErr leaves an ownerless MCP permanently dead.
				// Take one final election after the terminal wait and refresh the stable
				// read connection when recovery committed the drift.
				recovered, rerr := s.reconcileAfterOwnerExit()
				switch {
				case rerr != nil:
					loadErr = rerr
				case recovered:
					_, loadErr = s.refresh()
				}
			}
		}
		if loadErr != nil {
			s.readyErr = fmt.Errorf("initial index load: %w", loadErr)
			fmt.Fprintf(os.Stderr, "mesh mcp: %v\n", s.readyErr)
			close(s.ready)
			return
		}
		if s.owns() && s.owner != nil {
			if err := s.owner.MarkReady(); err != nil {
				slog.Warn("mesh mcp: could not publish owner readiness", "err", err)
			}
		}
		if s.ownerRole != "" {
			// Start the queue wake path as soon as the index is servable. Backfill and
			// note/code enrichment can take longer than the owner's 10-second receipt
			// bound on a large vault; queued extraction must not wait behind them.
			opPollStarted = true
			go s.pollOwnerOps(opCtx)
		}
		close(s.ready)
		if !s.owns() {
			// Both enrichment passes below are writes. They belong to the owning writer
			// now, not to every window that connects; doing them here would just be N
			// servers taking the write lock at startup, which is the contention this
			// whole split exists to remove.
			return
		}
		if beforeMCPEnrichment != nil {
			beforeMCPEnrichment()
		}
		// Seed the flywheel measurement from the existing agent-authored corpus once, so
		// the reuse number reflects accumulated knowledge from day one (idempotent).
		_, _ = store.BackfillWritebacks()
		_, _ = store.LinkNotesToCode(vaultRoot) // build the note<->code bridge if a code index exists
	}()
	return s, nil
}

// WaitReady blocks until ALL background startup work (initial reload plus the
// writeback backfill and note<->code bridge) has finished and reports the load
// error, for callers that need fully deterministic startup (tests, hub boot).
func (s *Server) WaitReady() error {
	<-s.bg
	return s.readyErr
}

// awaitReady blocks until the initial background load finishes. Graph-dependent
// calls wait for the index rather than racing a nil graph; known-note fetches
// use the opened store and current file without this graph-only dependency.
func (s *Server) awaitReady(ctx context.Context) *rpcError {
	select {
	case <-s.ready:
		if s.readyErr != nil {
			return &rpcError{Code: codeInternalError, Message: s.readyErr.Error()}
		}
		return nil
	case <-ctx.Done():
		return &rpcError{Code: codeInternalError, Message: "index still loading: " + ctx.Err().Error()}
	}
}

// Reconcile re-reads the vault and rebuilds the in-memory index (authoritative).
// The hub calls this after a sync lands so hosted MCP serves fresh results.
func (s *Server) Reconcile() error {
	_, err := s.reconcileOnce(true)
	return err
}

// NotePath resolves a note id to its vault-relative path (for the hub's ACL gate).
func (s *Server) NotePath(id string) (string, error) { return s.store.NotePath(id) }

// FlywheelStats exposes the write-back reuse metrics (authored notes, reuse rate,
// median time-to-reuse, writes-per-100-reads) of this server's index. The hub reads
// it for the team-level /team metrics, so the flywheel shows up team-wide and not only
// on the per-vault web dashboard. Reuse events counted here are those served by THIS
// index (the hosted MCP), so a team on `mesh mcp --http` over synced vaults contributes
// authored counts but its reuse lands in each member's local index, not here.
func (s *Server) FlywheelStats() (index.FlywheelStats, error) { return s.store.FlywheelStats() }

func (s *Server) Close() error {
	<-s.bg // never close the store under the initial background load/enrichment
	s.opCancel()
	<-s.opDone // never close the store under an in-flight owner-op reconcile
	s.reloadMu.Lock()
	s.refreshClosed, s.viewReusable = true, false
	if s.changeMonitor != nil {
		_ = s.changeMonitor.Close()
		s.changeMonitor = nil
	}
	s.reloadMu.Unlock()
	err := s.store.Close()
	// Give the vault up AFTER the store is closed, so the next owner never starts
	// indexing while this process still has a writable connection open.
	if rerr := s.owner.Release(); err == nil {
		err = rerr
	}
	return err
}

const ownerOpPollInterval = 250 * time.Millisecond

// pollOwnerOps gives owner-routed bookkeeping a wake path even when an MCP server was
// started without --watch. The queue lives under .mesh, intentionally outside the note
// watcher, so relying only on fsnotify/periodic vault passes makes automatic extraction
// time out beside an otherwise healthy no-watch owner.
func (s *Server) pollOwnerOps(ctx context.Context) {
	defer close(s.opDone)
	ticker := time.NewTicker(ownerOpPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.opWake:
		}
		entries, err := os.ReadDir(index.OpsDir(s.store.MeshDir()))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			slog.Warn("mesh mcp: cannot inspect owner op queue", "err", err)
			continue
		}
		if len(entries) == 0 {
			continue
		}
		queued := false
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				queued = true
				break
			}
		}
		if !queued {
			continue
		}
		if !s.owns() {
			if _, live := index.OwnerStatus(s.store.MeshDir()); live {
				continue // the current owner has its own drain path
			}
		}
		if _, err := s.reconcileOnce(false); err != nil {
			slog.Warn("mesh mcp: owner op queue remains pending", "err", err)
		}
	}
}

// owns reports whether this process is the vault's owning writer RIGHT NOW: the store is
// writable and, when this server elected itself, it still holds the claim.
//
// Every write path branches on this rather than on store.ReadOnly(), because an elected
// claim is preemptible: a declared owner started later takes the lock, and from that
// moment this server must behave exactly like the read-only one (publish the file, wait
// for the owner, re-read) or two processes reindex the same vault.
func (s *Server) owns() bool {
	if s.store.ReadOnly() {
		return false
	}
	if s.owner != nil {
		return s.owner.Held()
	}
	return true
}

// OwnsIndex reports whether this server is the vault's owning writer, so the command
// that started it can tell the operator which of the two it got.
func (s *Server) OwnsIndex() bool { return s.owns() }

// reconcileAfterOwnerExit restores self-sufficiency for an electing MCP server that
// started beside, or was temporarily preempted by, a declared owner. Its original
// Store may be physically read-only (or permanently bound to the old nonce), so the
// recovery pass uses a separately owned Store and then closes it before this server
// refreshes through its stable read connection. This avoids swapping Store pointers
// under concurrent tool calls while still ensuring the next watch/tool pass indexes
// editor and write-back bytes after the temporary owner exits.
func (s *Server) reconcileAfterOwnerExit() (bool, error) {
	if s.ownerRole == "" || s.owns() {
		return false, nil
	}
	s.electMu.Lock()
	defer s.electMu.Unlock()
	if s.owns() {
		return false, nil
	}
	meshDir := filepath.Join(s.vaultRoot, ".mesh")
	if _, live := index.OwnerStatus(meshDir); live {
		return false, nil
	}
	lock, err := index.AcquireOwnerLock(meshDir, s.ownerRole, true)
	if errors.Is(err, index.ErrOwnerHeld) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	writer, err := index.OpenOwned(s.vaultRoot, lock)
	if err != nil {
		lost := !lock.Held()
		return false, errors.Join(func() error {
			if lost && errors.Is(err, index.ErrReadOnly) {
				return nil
			}
			return err
		}(), lock.Release())
	}
	if _, derr := writer.DrainOps(); derr != nil {
		slog.Warn("mesh mcp: could not drain owner op queue during recovery", "err", derr)
	}
	_, rerr := index.Reconcile(writer, s.vaultRoot)
	lost := !lock.Held()
	cerr := writer.Close()
	lerr := lock.Release()
	if lost && errors.Is(rerr, index.ErrReadOnly) {
		return false, errors.Join(cerr, lerr)
	}
	return rerr == nil, errors.Join(rerr, cerr, lerr)
}

// snapshot returns the current graph + retriever under a read lock, so a
// concurrent rebuild swapping them in never tears a reader's view.
func (s *Server) snapshot() (*graph.Graph, *retrieve.Retriever) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.graph, s.retriever
}

// swap atomically replaces the in-memory graph + retriever.
func (s *Server) swap(g *graph.Graph) {
	// All callers hold reloadMu. Owned rebuilds did not sample a monitor stamp.
	s.viewReusable = false
	r, _ := retrieve.NewFromEnvContextWithOptions(context.Background(), s.store, g, s.retrievalOptions)
	s.mu.Lock()
	s.graph = g
	s.retriever = r
	s.mu.Unlock()
}

// load puts the vault's graph in memory by whichever route this store allows: a writable
// store re-indexes (and seeds the parsed-note cache so later reconciles are incremental),
// a read-only one reads the owning writer's persisted result. Startup calls this.
func (s *Server) load() error {
	if !s.owns() {
		_, err := s.refresh() // the startup load has no previous view to report a delta against
		return err
	}
	return s.reload()
}

// refresh rebuilds the in-memory graph + retriever from the PERSISTED tables, without
// touching the vault or the write lock. It is how a read-only server picks up whatever
// the owning writer has indexed since. It takes reloadMu like reload does, so a refresh
// and a rebuild can never swap under each other.
//
// It reports what the swap changed in THIS server's view (notes it gained, lost, or whose
// content moved), which is the only honest set of counts available to a process that did
// not run the pass. That is a different question from the writable path's "what did the
// vault drift by", and it is the one a caller of mesh_reindex here is actually asking:
// the reindex may well have happened in the owner minutes ago, and what this call did was
// bring it into view.
//
// The parsed-note cache is deliberately left unseeded: it exists to make a REINDEX
// incremental, and this server never reindexes.
func (s *Server) refresh() (index.Reconciliation, error) {
	return s.refreshContext(context.Background())
}

func (s *Server) lockReloadContext(ctx context.Context) error {
	if ctx.Done() == nil {
		s.reloadMu.Lock()
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.reloadMu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := ctx.Err(); err != nil {
				return err
			}
			if s.reloadMu.TryLock() {
				return nil
			}
		}
	}
}

func (s *Server) refreshContext(ctx context.Context) (index.Reconciliation, error) {
	trace := latency.Start("mcp_refresh", "reload_lock")
	defer trace.End()
	if err := s.lockReloadContext(ctx); err != nil {
		return index.Reconciliation{}, err
	}
	defer s.reloadMu.Unlock()
	if s.refreshClosed {
		return index.Reconciliation{}, errors.New("mesh: reader closed")
	}
	trace.Phase("freshness_check")
	in, err := retrieve.LoadConfigInputsWithOptions(ctx, s.store.MeshDir(), s.retrievalOptions)
	if err != nil {
		return index.Reconciliation{}, err
	}
	version, valid := s.readerVersion(ctx)
	fingerprint, reusable := in.Fingerprint()
	if valid && reusable && s.viewReusable && version == s.viewVersion && fingerprint == s.viewConfig {
		return index.Reconciliation{}, ctx.Err()
	}
	s.viewReusable = false
	trace.Phase("load_graph")
	g, err := s.store.LoadGraphContext(ctx)
	if err != nil {
		return index.Reconciliation{}, err
	}
	trace.Phase("install")
	if s.beforeReaderInstall != nil {
		s.beforeReaderInstall()
	}
	rec, err := s.installRefreshedGraph(ctx, g, in)
	if err == nil {
		s.rememberReaderVersion(ctx, version, valid, in)
	}
	return rec, err
}

func (s *Server) refreshAtNoteVersion(ctx context.Context, noteID, notePath, noteHash string) (bool, error) {
	trace := latency.Start("mcp_version_refresh", "reload_lock")
	defer trace.End()
	if err := s.lockReloadContext(ctx); err != nil {
		return false, err
	}
	defer s.reloadMu.Unlock()
	if s.refreshClosed {
		return false, errors.New("mesh: reader closed")
	}
	// A previous refresh/acknowledgement may already have installed this exact
	// version. Reuse needs the same monitored database revision on BOTH sides of
	// the note-version check; a matching id or cached note hash alone is not proof.
	trace.Phase("cached_version_check")
	version, valid := s.readerVersion(ctx)
	matched, reused, err := s.reuseAcknowledgement(ctx, version, valid, noteID, notePath, noteHash)
	if err != nil || reused {
		return matched, err
	}
	// A failed monitor probe in the reuse path may have closed its connection.
	// Establish the fallback build's stamp on the monitor it will actually use.
	version, valid = s.readerVersion(ctx)
	trace.Phase("load_versioned_graph")
	g, matched, err := s.store.LoadGraphAtNoteVersionContext(ctx, noteID, notePath, noteHash)
	if err != nil || !matched {
		return matched, err
	}
	trace.Phase("install")
	in, err := retrieve.LoadConfigInputsWithOptions(ctx, s.store.MeshDir(), s.retrievalOptions)
	if err != nil {
		return false, err
	}
	s.viewReusable = false
	if s.beforeReaderInstall != nil {
		s.beforeReaderInstall()
	}
	_, err = s.installRefreshedGraph(ctx, g, in)
	if err == nil {
		s.rememberReaderVersion(ctx, version, valid, in)
	}
	return err == nil, err
}

func (s *Server) installRefreshedGraph(ctx context.Context, g *graph.Graph, in *retrieve.ConfigInputs) (index.Reconciliation, error) {
	trace := latency.Start("mcp_install_graph", "note_hashes")
	defer trace.End()
	// Fingerprint the index we just loaded. A read error here costs the NEXT refresh its
	// counts, never its correctness, so it must not fail the refresh: the graph is already
	// good and the caller's notes are already queryable.
	after, herr := s.store.NoteHashesContext(ctx)
	if err := ctx.Err(); err != nil {
		return index.Reconciliation{}, err
	}
	trace.Phase("retriever")
	_, previous := s.snapshot()
	r, err := retrieve.NewFromInputsReusingContext(ctx, s.store, g, in, previous)
	if err != nil {
		return index.Reconciliation{}, err
	}
	if err := ctx.Err(); err != nil {
		return index.Reconciliation{}, err
	}
	// Reindexed carries the same meaning it does on the writable path: something actually
	// changed. A refresh always swaps a graph in, but saying so every time would make the
	// watcher log a line on every idle tick (and mesh_reindex claim a pass it did not
	// need), which is noise that hides the ticks that did bring something in. Unknown
	// counts (the hash read failed) report true: we did swap, and under-reporting a real
	// change is the worse error.
	trace.Phase("reconcile_counts")
	rec := index.Reconciliation{Reindexed: herr != nil}
	if herr == nil {
		first := s.viewHashes == nil
		for p, h := range after {
			switch prev, had := s.viewHashes[p]; {
			case !had && !first:
				rec.Added++
			case had && prev != h:
				rec.Changed++
			}
		}
		for p := range s.viewHashes {
			if _, still := after[p]; !still {
				rec.Removed++
			}
		}
		rec.Reindexed = first || rec.Any()
	}
	if err := ctx.Err(); err != nil {
		return index.Reconciliation{}, err
	}
	if herr == nil {
		s.viewHashes = after
	}
	trace.Phase("publish_lock")
	s.mu.Lock()
	trace.Phase("publish")
	s.graph, s.retriever = g, r
	s.mu.Unlock()
	s.viewBuildComplete = herr == nil && r.RefreshReusable()
	return rec, nil
}

// ErrOwnerNotIndexing means the single owning writer did not index a just-written note
// inside ownerIndexTimeout, or this reader could not install the matching snapshot
// within that same bound. The note IS durably on disk; its queryability is unconfirmed,
// not its durability. This does not prove the owner stopped. Callers must say exactly
// that, because the one
// thing that must never happen here is reporting a failed write for a note that exists:
// the agent retries and Mesh mints a near-duplicate.
//
// It is the index package's sentinel, not a second one: the web viewer waits on the same
// owner for the same reason, and two sentinels would mean errors.Is answering differently
// on the two surfaces.
var ErrOwnerNotIndexing = index.ErrOwnerNotIndexing

// Keep the legacy stale-receipt classification without blaming the owner when
// a busy reader, rather than a missing committed row, exhausted the wait.
type acknowledgementTimeoutError struct{}

func (acknowledgementTimeoutError) Error() string {
	return "the note was saved, but this reader could not confirm its indexed version before the acknowledgement deadline; do not retry the write"
}

func (acknowledgementTimeoutError) Unwrap() error { return ErrOwnerNotIndexing }

// ownerIndexTimeout bounds the wait for the owning writer. The value, and why it is that
// value, live with the wait itself in internal/index.
const ownerIndexTimeout = index.OwnerIndexBound

// OwnerIndexBound is ownerIndexTimeout, re-exported so the owning writer's own periodic
// sweep can be pinned UNDER it (see cmd/mesh). A note that misses its fsnotify event is
// picked up by that sweep, so a sweep interval above this bound is precisely the case
// where a durable, perfectly fine note reads as owner_down.
const OwnerIndexBound = index.OwnerIndexBound

// awaitOwnerIndexed blocks until the single owning writer has indexed the CURRENT
// version of notePath, then refreshes this server's in-memory graph from what the owner
// persisted. Checking only noteID is insufficient: an editor can remove an indexed note
// and a writer can reuse its slug before the owner sees the removal. In that window the
// old row has the right id and path but the wrong retrieval hash, and accepting it would
// return a false success receipt while queries still serve the deleted note's content.
func (s *Server) awaitOwnerIndexed(ctx context.Context, noteID, notePath string) (result error) {
	trace := latency.Start("mcp_acknowledge", "setup")
	defer trace.End()
	timeout := s.ownerIndexTimeout
	if timeout <= 0 {
		timeout = ownerIndexTimeout
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if result != nil && ctx.Err() != nil {
			if parent.Err() != nil {
				result = parent.Err()
			} else {
				result = acknowledgementTimeoutError{}
			}
		}
	}()
	rel, err := filepath.Rel(s.vaultRoot, notePath)
	if err != nil {
		return err
	}
	rel = filepath.Clean(rel)
	for {
		trace.Phase("parse_target")
		pn, perr := index.ParseFileContext(ctx, notePath)
		switch {
		case perr == nil:
			pn.Path = rel
			trace.Phase("target_hash")
			targetHash := index.RetrievalHash(pn)
			if s.beforeOwnerVersionRefresh != nil {
				s.beforeOwnerVersionRefresh()
			}
			trace.Phase("version_refresh")
			matched, rerr := s.refreshAtNoteVersion(ctx, noteID, rel, targetHash)
			if rerr != nil {
				return rerr
			}
			if matched {
				// Installing one atomic database snapshot is not enough to prove the
				// CURRENT file is queryable. An editor can publish newer bytes after
				// ParseFile and before the snapshot is installed. Re-read the path
				// after publication and only acknowledge if it still names the exact
				// version represented by that snapshot.
				trace.Phase("verify_database")
				currentDB, derr := s.store.NoteVersionMatchesContext(ctx, noteID, rel, targetHash)
				if derr != nil {
					return derr
				}
				if !currentDB {
					break
				}
				trace.Phase("verify_file")
				current, cerr := index.ParseFileContext(ctx, notePath)
				switch {
				case cerr == nil:
					current.Path = rel
					if index.RetrievalHash(current) == targetHash {
						return ctx.Err()
					}
				case os.IsNotExist(cerr):
					// The current path is now ahead of (or absent from) the
					// installed snapshot. Keep waiting for its owner.
				default:
					return cerr
				}
			}
		case os.IsNotExist(perr):
			// An editor removed the new file before publication. Keep waiting so
			// the caller gets the same honest not-queryable receipt as any other
			// change the owner has not absorbed.
		default:
			return perr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		trace.Phase("poll_wait")
		timer := time.NewTimer(index.OwnerIndexPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// pendingDrift is the part of the vault the owning writer has not absorbed yet and still
// can. See index.PendingDrift for what is excluded and why.
func (s *Server) pendingDrift() (index.Drift, error) {
	return s.store.PendingDrift(s.vaultRoot)
}

// awaitOwnerCaughtUp is mesh_reindex's half of the contract awaitOwnerIndexed gives
// write-back: it waits for the single owning writer to absorb whatever the vault has
// drifted by, then refreshes this server's in-memory graph from what the owner
// persisted. The counts it returns are what became queryable BECAUSE of this call.
//
// Without the wait, mesh_reindex on a read-only server is a lie in the one situation the
// tool exists for. The contract an agent is handed says "if you edited note files
// directly, call mesh_reindex to make those edits queryable now"; a bare snapshot swap
// returns instantly with the owner still inside its debounce, so the tool reports success
// and the edit is not there. The bound and the loud failure are the same as write-back's,
// because it is the same failure: the owner is not running.
func (s *Server) awaitOwnerCaughtUp(ctx context.Context) (index.Reconciliation, error) {
	start := time.Now()
	werr := s.store.AwaitOwnerCaughtUp(ctx, s.vaultRoot, s.ownerIndexTimeout)
	stale := errors.Is(werr, index.ErrOwnerNotIndexing)
	if werr != nil && !stale {
		return index.Reconciliation{}, werr
	}
	// The counts come from the refresh, not from the drift we waited on: what a caller
	// gets out of this call is what entered THIS server's view, and on the stale path
	// that is whatever the owner did manage to index, never what is still missing.
	// Preserve caller cancellation through the reader lock and construction too;
	// refresh() substitutes Background and can outlive the request after the wait.
	rec, err := s.refreshContext(ctx)
	if err != nil {
		return index.Reconciliation{}, err
	}
	rec.Dur = time.Since(start)
	if stale {
		return rec, ErrOwnerNotIndexing
	}
	return rec, nil
}

// publishWriteBack makes a just-created note queryable, by whichever route this server
// is allowed to take: the owner of its index reindexes directly, a read-only server
// waits for the owning writer to do it.
func (s *Server) publishWriteBack(ctx context.Context, noteID, notePath string) error {
	if !s.owns() {
		if _, err := s.reconcileAfterOwnerExit(); err != nil {
			return err
		}
		return s.awaitOwnerIndexed(ctx, noteID, notePath)
	}
	if _, err := s.reconcilePaths([]string{notePath}); err != nil {
		return err
	}
	// The targeted pass can legitimately quarantine a malformed or duplicate
	// file while still publishing other paths in the burst. For this tool's
	// receipt, however, success means THESE exact bytes are queryable. Verify the
	// committed version just as the read-only wait does, without reloading the
	// graph the owning server already swapped in memory.
	pn, err := index.ParseFile(notePath)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(s.vaultRoot, notePath)
	if err != nil {
		return err
	}
	pn.Path = filepath.Clean(rel)
	matched, err := s.store.NoteVersionMatches(noteID, pn.Path, index.RetrievalHash(pn))
	if err != nil {
		return err
	}
	if !matched {
		return ErrOwnerNotIndexing
	}
	return nil
}

// reload fully re-indexes the vault and rebuilds the in-memory graph +
// retriever, seeding the parsed-note cache so later reconciles can be incremental.
// Run at startup and after a write-back so new notes are immediately retrievable.
func (s *Server) reload() error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if beforeMCPInitialReload != nil {
		beforeMCPInitialReload()
	}
	if _, err := s.store.DrainOps(); err != nil {
		slog.Warn("mesh mcp: could not drain owner op queue", "err", err)
	}
	g, notes, err := index.ReindexFull(s.store, s.vaultRoot)
	if err != nil {
		return err
	}
	s.cache.Seed(notes)
	s.swap(g)
	return nil
}

// reconcileOnce reindexes only when the vault has drifted, swapping in the fresh
// graph when it did. It is the watcher's reindex callback. Incremental: it parses
// only changed files and rebuilds the graph in memory from the cache.
func (s *Server) reconcileOnce(authoritative bool) (index.Reconciliation, error) {
	// A read-only server has no reindex to run: the owning writer does that. The
	// equivalent act here is picking up what the owner has already persisted, so the
	// watcher, mesh_reindex and write-back all still refresh, none of them write, and
	// none of them need to know which kind of store they are on. Branch BEFORE taking
	// reloadMu: refresh takes it too, and it is not reentrant.
	//
	// The counts describe this server's VIEW rather than the vault: a snapshot swap
	// classifies nothing on disk, but it can say exactly which notes it gained, lost or
	// saw change, which is what the watcher's log line and mesh_reindex both want.
	if !s.owns() {
		start := time.Now()
		if _, err := s.reconcileAfterOwnerExit(); err != nil {
			return index.Reconciliation{}, err
		}
		rec, err := s.refresh()
		if err != nil {
			return index.Reconciliation{}, err
		}
		rec.Dur = time.Since(start)
		return rec, nil
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if _, err := s.store.DrainOps(); err != nil {
		slog.Warn("mesh mcp: could not drain owner op queue", "err", err)
	}
	rec, err := index.ReconcileIncremental(s.store, s.vaultRoot, s.cache, !authoritative)
	if err != nil {
		return rec, err
	}
	if rec.Reindexed {
		s.swap(rec.Graph)
	}
	return rec, nil
}

// reconcilePaths publishes exact fsnotify/write-back paths through the owning
// writer without a vault-wide discovery scan. Read-only servers never call this:
// they wait for their owner and refresh the committed snapshot instead.
func (s *Server) reconcilePaths(paths []string) (index.Reconciliation, error) {
	if !s.owns() {
		return s.reconcileOnce(false)
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if _, err := s.store.DrainOps(); err != nil {
		slog.Warn("mesh mcp: could not drain owner op queue", "err", err)
	}
	rec, err := index.ReconcilePaths(s.store, s.vaultRoot, s.cache, paths)
	if err != nil {
		return rec, err
	}
	if rec.Reindexed {
		s.swap(rec.Graph)
	}
	return rec, nil
}

// reindexThrottle is how long a REMOTE mesh_reindex serves the previous pass's
// result instead of forcing another authoritative reconcile. Each pass content-hashes
// every note and then re-indexes the code roots, and it takes reloadMu, the same lock
// the hub's post-sync reconcile worker needs, so an unthrottled remote caller can burn
// a core on hashing and delay synced notes becoming visible for other members.
const reindexThrottle = 5 * time.Second

// reindexPass is one authoritative mesh_reindex pass by whichever route this server is
// allowed to take: the owner of its index reindexes, a read-only server waits for the
// owning writer to catch up and then re-reads the result. Note this is NOT what the
// watcher calls (that is reconcileOnce): a watcher tick must never sit waiting on the
// owner, it just picks up whatever has landed.
func (s *Server) reindexPass(ctx context.Context) (index.Reconciliation, error) {
	if !s.owns() {
		recovered, err := s.reconcileAfterOwnerExit()
		if err != nil {
			return index.Reconciliation{}, err
		}
		if recovered {
			return s.refresh()
		}
		if beforeMCPAwaitOwnerCaughtUp != nil {
			beforeMCPAwaitOwnerCaughtUp()
		}
		return s.awaitOwnerCaughtUp(ctx)
	}
	return s.reconcileOnce(true)
}

// reconcileThrottled is reindexPass with a per-server cooldown for remote callers.
// remote=false (the local stdio operator, who owns the box and expects "reindex now" to
// mean now) always runs the real pass. It returns the pass result, whether the cooldown
// replayed the previous one, and the pass's staleness verdict.
//
// ErrOwnerNotIndexing is a RESULT, not a failure: the pass ran, and what it found is that
// the owner has not caught up. It is cached and replayed with the rest of the pass, so a
// throttled caller is told the index is stale instead of being handed the last healthy
// pass as if nothing were wrong.
func (s *Server) reconcileThrottled(ctx context.Context, remote bool) (index.Reconciliation, bool, error) {
	s.reindexMu.Lock()
	defer s.reindexMu.Unlock()
	if remote && !s.lastReindexAt.IsZero() && time.Since(s.lastReindexAt) < reindexThrottle {
		return s.lastReindex, true, s.lastReindexErr
	}
	rec, err := s.reindexPass(ctx)
	if err != nil && !errors.Is(err, ErrOwnerNotIndexing) {
		return rec, false, err
	}
	s.lastReindexAt = time.Now()
	s.lastReindex = rec
	s.lastReindexErr = err
	return rec, false, err
}

// Watch live-reindexes the vault in the background until ctx is cancelled, so a
// long-running agent session sees notes a human edits in their editor without a
// restart. logf must write to stderr: stdout carries the JSON-RPC stream.
func (s *Server) Watch(ctx context.Context, debounce, reconcile, fullReconcile time.Duration, logf func(string, ...any)) error {
	return watch.Run(ctx, watch.Options{
		Root:          s.vaultRoot,
		Debounce:      debounce,
		Reconcile:     reconcile,
		FullReconcile: fullReconcile,
		Logf:          logf,
		OnReindex: func(p watch.Pass) (watch.Result, error) {
			var rec index.Reconciliation
			var err error
			if len(p.Paths) > 0 && s.owns() {
				rec, err = s.reconcilePaths(p.Paths)
			} else {
				rec, err = s.reconcileOnce(p.Authoritative)
			}
			if err != nil {
				return watch.Result{}, err
			}
			return watch.Result{
				Added:     rec.Added,
				Changed:   rec.Changed,
				Removed:   rec.Removed,
				Reindexed: rec.Reindexed,
				Dur:       rec.Dur,
			}, nil
		},
	})
}

// ServeStdio reads newline-delimited JSON-RPC requests from stdin and writes
// responses to stdout until EOF.
func (s *Server) ServeStdio() error {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	// Stdio is the trusted local transport: the operator spawned this process, so its
	// requests carry the local-operator marker that gates the local-only tools. No
	// other transport sets it, so they all fail closed (see WithLocalOperator).
	base := WithLocalOperator(context.Background())
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		// Notifications (no id) expect no response.
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}
		result, rerr := s.dispatch(base, req)
		resp := response{JSONRPC: "2.0", ID: req.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}

// HandleHTTP serves ONE JSON-RPC request over HTTP (MCP Streamable HTTP, the
// request/response shape; no SSE stream). Same dispatch as stdio, so a remote agent
// gets identical results. Stateless per call; auth + transport live in the caller
// (cmd/mesh `mesh mcp --http` or the hub's /mcp route). The body cap mirrors the
// largest sane tool call.
func (s *Server) HandleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	var req request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeRPC(w, response{JSONRPC: "2.0", Error: &rpcError{Code: codeInvalidParams, Message: "bad request"}})
		return
	}
	// Notifications (no id) get a bare 202 with no body.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := s.dispatch(r.Context(), req)
	resp := response{JSONRPC: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(ctx, req.Params), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.handleToolsList(), nil
	case "tools/call":
		if toolCallNeedsGraph(req.Params) {
			if rerr := s.awaitReady(ctx); rerr != nil {
				return nil, rerr
			}
		}
		return s.handleToolsCall(ctx, req.Params)
	case "resources/list":
		return s.handleResourcesList(), nil
	case "resources/read":
		if rerr := s.awaitReady(ctx); rerr != nil {
			return nil, rerr
		}
		return s.handleResourcesRead(ctx, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found", Data: req.Method}
	}
}

// Fetching a known note needs the opened store and current file, not the global
// graph/retriever. Both fetch handlers enforce indexed and current-file scope,
// confinement, cancellation and safety context themselves. A slow or failed
// graph load must not prevent those independent reads. Every other tool (and
// any unknown or malformed call) retains the existing startup gate; normal
// tool classification and argument validation still run in handleToolsCall.
func toolCallNeedsGraph(params json.RawMessage) bool {
	var call struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return true
	}
	return call.Name != "mesh_fetch" && call.Name != "mesh_fetch_many" && call.Name != "mesh_templates" && call.Name != "mesh_note_template" && call.Name != "mesh_block_template"
}

func (s *Server) handleInitialize(ctx context.Context, params json.RawMessage) any {
	// Capture the calling agent's name (provenance default for write-back).
	var p struct {
		ClientInfo struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if name := strings.TrimSpace(p.ClientInfo.Name); name != "" {
		s.mu.Lock()
		s.agent = name
		s.mu.Unlock()
	}
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools":     map[string]any{"listChanged": false},
			"resources": map[string]any{"listChanged": false, "subscribe": false},
		},
		"serverInfo":   map[string]any{"name": serverName, "version": serverVersion},
		"instructions": s.initializeInstructions(ctx),
	}
}

// ---- JSON-RPC envelope ----

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

const (
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeMethodNotFound = -32601
)

// internalErr wraps an internal failure (index reload, vault I/O, driver error)
// as a JSON-RPC error WITHOUT leaking the raw message: sqlite driver text and
// absolute filesystem paths would otherwise reach the agent verbatim. The real
// error is logged server-side; the agent gets a generic message. Validation
// errors use codeInvalidParams with explicit operator-authored messages instead.
func internalErr(err error) *rpcError {
	slog.Error("mesh mcp internal error", "err", err)
	return &rpcError{Code: codeInternalError, Message: "internal error"}
}

// retrievalUnavailableMsg is the one thing an agent whose search failed most needs to
// know, and the thing the opaque form never said: this is NOT an empty result set. A
// retrieval tool that fails silently is worse than one that is down, because "the index
// refused to answer" and "the vault holds nothing on this" are the same observation to
// the caller, and the caller's next move differs completely. Measured on this vault
// 2026-08-26: 296 timeouts over four days, every one of them reaching the agent as
// "internal error", while the same queries answered from a fresh CLI connection in
// ~1.5s. Agents read that as "no prior knowledge" and re-derived from source, which is
// how a 2300-note vault stops preventing repeat work.
const retrievalUnavailableMsg = "retrieval unavailable: the index did not answer in time. " +
	"This is NOT an empty result: prior knowledge may exist and was NOT searched. " +
	"Do not conclude the vault is silent on your topic. Retry once with fewer, more " +
	"specific words; if it repeats, the mesh MCP daemon is degraded and needs a restart."

// retrievalErr is internalErr for the READ path. It keeps internalErr's security
// property (sqlite driver text and absolute filesystem paths never reach the agent)
// while separating the one failure mode the agent can reason about from the ones it
// cannot. Only operator-authored, path-free text is surfaced; anything unrecognised
// still degrades to the opaque form.
//
// Data carries machine-readable fields so a client can branch without parsing prose,
// and retryable distinguishes "ask again" from "this call will never work".
func retrievalErr(err error) *rpcError {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		slog.Error("mesh mcp retrieval timed out", "err", err)
		return &rpcError{
			Code:    codeInternalError,
			Message: retrievalUnavailableMsg,
			Data: map[string]any{
				"reason":    "search_timeout",
				"retryable": true,
				"empty":     false,
			},
		}
	case errors.Is(err, context.Canceled):
		slog.Error("mesh mcp retrieval cancelled", "err", err)
		return &rpcError{
			Code:    codeInternalError,
			Message: "retrieval cancelled before it completed. This is NOT an empty result: prior knowledge may exist and was NOT searched.",
			Data: map[string]any{
				"reason":    "search_cancelled",
				"retryable": true,
				"empty":     false,
			},
		}
	}
	return internalErr(err)
}

// textResult wraps a value as an MCP text content block, JSON-encoding it so the
// agent gets terse structured data, not chatty prose.
func textResult(v any) any {
	b, _ := json.Marshal(v)
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}
}

func rawText(s string) any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": s}}}
}
