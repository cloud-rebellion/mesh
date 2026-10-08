// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bright-interaction/mesh/internal/buildinfo"
	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/shellpath"
	"github.com/bright-interaction/mesh/internal/updatecheck"
)

//go:embed assets
var assetsFS embed.FS

// Server serves the localhost graph viewer + app shell for one vault.
type Server struct {
	retrievalOptions retrieve.ConstructionOptions
	vaultRoot        string
	store            *index.Store
	owner            *index.OwnerLock // non-nil only for NewOwningServer; released by Close
	ownerOpCancel    context.CancelFunc
	ownerOpDone      chan struct{}
	ownerOpWake      chan struct{} // deterministic test wake; ticker is the cross-process path
	ownerWatchCancel context.CancelFunc
	ownerWatchDone   chan struct{}
	ownerNotes       *index.NoteCache // protected by graphUpdateGate; nil after a manual full rebuild
	lifetimeCtx      context.Context
	auth             authConfig
	basePath         string // "" for root, or "/app" when served under a path

	// scopeResolver, when set, maps a request to the caller's allowed-scope set so the
	// graph/search/note surfaces are filtered per member. nil (standalone `mesh ui`) =
	// unrestricted, so the loopback single-user viewer is unchanged.
	scopeResolver func(*http.Request) map[string]bool
	// pathResolver is the folder-ACL twin of scopeResolver: it maps a request to the
	// caller's per-path read predicate (nil = unrestricted). The two are independent
	// partitions and BOTH gate every read surface, because a team can fence folders with
	// ACLs and never define a scope, and then scopeResolver returns nil and filters
	// nothing whatsoever.
	pathResolver func(*http.Request) func(string) bool
	// member, when set (mesh ui --hub-db), puts the app in per-member auth mode: each
	// request authenticates as a hub client instead of the single shared token.
	member      *memberAuth
	connections *connectionService // independent credential store; never an index writer

	mu    sync.RWMutex
	graph *graph.Graph
	// cachedRetriever is built lazily over graph; nil = rebuild needed. It is an atomic
	// pointer rather than a field of the graph lock so the (slow) build never has to
	// take s.mu exclusively, see retriever().
	cachedRetriever     atomic.Pointer[retrieve.Retriever]
	buildGate           chan struct{} // one token; unlike a mutex, waiting is request-cancellable
	buildRetriever      retrieverBuildFunc
	retrieverGeneration uint64        // guarded by mu; config invalidation can keep the same graph pointer
	graphUpdateGate     chan struct{} // one token; spans rebuild/load through graph publication
	reindexStore        owningReindexFunc
	// Read-only revision tracking, guarded by graphUpdateGate. A monitor never
	// writes schema or acquires index ownership.
	changeMonitor  *index.ChangeMonitor
	indexIdentity  os.FileInfo // pinned before opening the reader, never adopted from a later file
	viewVersion    index.ReaderVersion
	viewReusable   bool
	viewClosed     bool
	loadFreshGraph func(context.Context) (*graph.Graph, error) // optional deterministic refresh test seam

	// afterPendingFilePublished is a deterministic test seam at the durable boundary of
	// a pending promotion. Production leaves it nil.
	afterPendingFilePublished func()

	configGate chan struct{} // one token; serializes config.toml PUTs with cancellable waits

	// ownerWait bounds how long a read-only viewer waits for the owning writer to apply
	// what it queued. A field rather than a bare const so a test can shorten it without
	// mutating global state (which would race across parallel tests); production never
	// sets it and gets index.OwnerIndexBound.
	ownerWait time.Duration

	logins   *rateLimiter  // per-peer token bucket in front of POST /api/login
	asks     *rateLimiter  // per-caller token bucket in front of POST /api/ask
	askSlots chan struct{} // bounded in-flight LLM subprocesses/calls for POST /api/ask

	// updateCheck is a testable seam around the fixed, cached public release check.
	// It never affects serving: errors are omitted from status and the UI stays quiet.
	updateCheck func(context.Context, string) (updatecheck.Notice, error)
}

type retrieverBuildFunc func(context.Context, *index.Store, *graph.Graph) (*retrieve.Retriever, error)

// retriever returns a fused retriever over the current graph, building it lazily and
// caching it. Previously every /api/search rebuilt the retriever (LoadVectors from
// disk + an ANN rebuild in pro) per request: a latency cliff and a DoS amplifier.
// It is invalidated on a graph swap (reindex) and a config change, so a Settings edit
// still takes effect on the next search.
//
// The build runs OUTSIDE s.mu. retrieve.NewFromEnv reads every stored vector, builds
// the ANN index (pro) and probes the embedding endpoint over the network, so holding
// the exclusive graph lock across it froze /graph.json, every other search and the
// pending API for as long as that probe took (up to the HTTP client timeout) whenever
// the endpoint was unreachable. buildGate serialises the build itself so N concurrent
// searches cause one build, not N, and the result is only published if the graph it
// was built over is still the current one, so a reindex that lands mid-build wins.
// Same shape as mcp.Server.swap.
func (s *Server) retriever() *retrieve.Retriever {
	rt, _ := s.retrieverContext(context.Background())
	return rt
}

// retrieverContext returns the cached retriever or builds one while honoring the
// caller's cancellation both during construction and while waiting behind another
// build. A canceled or failed build is never published.
func (s *Server) retrieverContext(ctx context.Context) (*retrieve.Retriever, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := webContextErr(ctx); err != nil {
		return nil, err
	}
	if rt := s.cachedRetriever.Load(); rt != nil {
		return rt, nil
	}
	select {
	case <-ctx.Done():
		return nil, webContextErr(ctx)
	case <-s.buildGate:
	}
	defer func() { s.buildGate <- struct{}{} }()
	if err := webContextErr(ctx); err != nil {
		return nil, err
	}
	if rt := s.cachedRetriever.Load(); rt != nil {
		return rt, nil // another caller built it while we waited
	}
	for {
		s.mu.RLock()
		g := s.graph
		generation := s.retrieverGeneration
		s.mu.RUnlock()
		builder := s.buildRetriever
		if builder == nil {
			builder = retrieve.NewFromEnvContext
		}
		built, err := builder(ctx, s.store, g)
		if err != nil {
			return nil, err
		}
		if built == nil {
			return nil, errors.New("retriever build returned nil")
		}
		if err := webContextErr(ctx); err != nil {
			return nil, err
		}
		// Publish under the read lock: a graph swap (reindex, pending promote) takes
		// the write lock to set s.graph and clear the cache in one critical section.
		// If either the graph or config changed during the slow build, discard it and
		// rebuild while retaining the gate. Returning the discarded retriever would let
		// this request search a deleted note or use the pre-save embedding config even
		// though the newer state had already published.
		s.mu.RLock()
		current := s.graph == g && s.retrieverGeneration == generation
		if current && webContextErr(ctx) == nil {
			s.cachedRetriever.Store(built)
		}
		s.mu.RUnlock()
		if err := webContextErr(ctx); err != nil {
			return nil, err
		}
		if current {
			return built, nil
		}
	}
}

func webContextErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		if err := context.Cause(ctx); err != nil {
			return err
		}
		return ctx.Err()
	default:
		return nil
	}
}

// invalidateRetriever drops the cached retriever so the next search rebuilds it
// (after a config change).
func (s *Server) invalidateRetriever() {
	s.mu.Lock()
	s.retrieverGeneration++
	s.cachedRetriever.Store(nil)
	s.mu.Unlock()
}

// allowedScopes returns the caller's readable-scope set (nil = unrestricted).
func (s *Server) allowedScopes(r *http.Request) map[string]bool {
	if s.scopeResolver == nil {
		return nil
	}
	return s.scopeResolver(r)
}

// allowedPath returns the caller's per-path read predicate (nil = unrestricted).
func (s *Server) allowedPath(r *http.Request) func(string) bool {
	if s.pathResolver == nil {
		return nil
	}
	return s.pathResolver(r)
}

// canReadPath reports whether the caller may read a vault-relative note path.
func (s *Server) canReadPath(r *http.Request, path string) bool {
	allow := s.allowedPath(r)
	return allow == nil || allow(path)
}

// SetScopeResolver installs the per-request scope resolver (used by the hub to serve
// the app under per-member identity).
func (s *Server) SetScopeResolver(f func(*http.Request) map[string]bool) { s.scopeResolver = f }

// Store exposes the index store (for a host that needs NoteScope etc.).
func (s *Server) Store() *index.Store { return s.store }

// baseHref is the value injected into the SPA's <base> tag, so every relative
// asset and fetch resolves under the configured path. Always ends in "/".
func (s *Server) baseHref() string {
	if s.basePath == "" {
		return "/"
	}
	return s.basePath + "/"
}

// NewServer opens the vault's index READ-ONLY and loads the graph the owning writer has
// already persisted. This is the viewer you run beside an owner (`mesh watch` /
// `mesh sync --watch`), which is every laptop: the app stays open for hours, so a
// writable store here is a second long-lived writer against one mesh.db, and the whole
// point of the single-writer split is that there is exactly one.
//
// It does NOT reindex at startup. That used to be the first thing this constructor did,
// which meant opening the dashboard ran a full reindex of the vault before it drew a
// frame, against the same write lock the owner needs. Reading what the owner persisted
// is the same graph, without the fight.
//
// The write features still work; they route through the owner. See handleReindex and the
// pending queue in pending_api.go.
func NewServer(vaultRoot string) (*Server, error) {
	return NewServerContext(context.Background(), vaultRoot)
}

// NewServerContext is NewServer with caller-controlled cancellation for the graph
// load. The monitor and read-only store are closed before a cancellation is returned.
func NewServerContext(ctx context.Context, vaultRoot string) (*Server, error) {
	return newReadOnlyServerContext(ctx, vaultRoot, nil)
}

// NewServerWithRetrievalOptions retains the same read-only index and access
// boundaries, with an explicit immutable retrieval choice for native callers.
func NewServerWithRetrievalOptions(ctx context.Context, vaultRoot string, options retrieve.ConstructionOptions) (*Server, error) {
	return newReadOnlyServerContextWithOptions(ctx, vaultRoot, nil, options)
}

// The loader seam lets startup tests observe the same revision-bracketed load as
// request refreshes, without mutable package globals or a second startup read.
func newReadOnlyServerContext(ctx context.Context, vaultRoot string, load func(context.Context, *Server) (*graph.Graph, error)) (*Server, error) {
	return newReadOnlyServerContextWithOptions(ctx, vaultRoot, load, retrieve.ConstructionOptions{})
}

func newReadOnlyServerContextWithOptions(ctx context.Context, vaultRoot string, load func(context.Context, *Server) (*graph.Graph, error), options retrieve.ConstructionOptions) (*Server, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity, identityErr := os.Stat(filepath.Join(vaultRoot, ".mesh", "mesh.db"))
	if _, starting := index.OwnerStarting(filepath.Join(vaultRoot, ".mesh")); starting {
		if err := index.AwaitOwnerReady(ctx, filepath.Join(vaultRoot, ".mesh"), index.OwnerStartupBound); err != nil {
			if _, live := index.OwnerStatus(filepath.Join(vaultRoot, ".mesh")); live {
				return nil, err
			}
		}
	}
	store, err := index.OpenReadOnly(vaultRoot)
	if err != nil {
		return nil, err
	}
	if identityErr != nil {
		return nil, startupFailure(ctx, identityErr, store.Close())
	}
	s := newServerContextWithOptions(ctx, vaultRoot, store, nil, options)
	s.indexIdentity = identity
	if load != nil {
		s.loadFreshGraph = func(ctx context.Context) (*graph.Graph, error) { return load(ctx, s) }
	}
	// Pin the monitor to the original reader identity and bracket the initial
	// graph load with revision samples. The first unchanged request can now
	// reuse this graph instead of loading the entire index a second time.
	if err := s.ensureFresh(ctx); err != nil {
		return nil, startupFailure(ctx, err, s.Close())
	}
	return s, nil
}

func startupFailure(ctx context.Context, startupErr, cleanupErr error) error {
	// Context cancellation is an expected shutdown result. Preserve cleanup failures
	// instead of hiding them inside the expected context error; ServeContext suppresses
	// only a clean cancellation.
	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(startupErr, ctxErr) {
		if cleanupErr != nil {
			return cleanupErr
		}
		return ctxErr
	}
	return errors.Join(startupErr, cleanupErr)
}

// NewOwningServer opens the index WRITABLE and owns it: it reindexes at startup and
// applies its own writes. Use it only where this process is the sole writer of that
// index, which today is the mesh-ui container (`mesh ui --own-index`). There, no
// `mesh watch` runs against the served vault, so a read-only viewer would serve an index
// nobody ever updates. Same split as internal/mcp: NewServer is the per-window reader,
// NewServerAt is the hub that owns what it serves.
func NewOwningServer(vaultRoot string) (*Server, error) {
	return NewOwningServerContext(context.Background(), vaultRoot)
}

type owningReindexFunc func(context.Context, *index.Store, string) (*graph.Graph, error)

// NewOwningServerContext is NewOwningServer with cancellation spanning every expensive
// startup phase. On cancellation it closes SQLite and releases owner.lock before it
// returns, so an orchestrator can start the replacement immediately rather than waiting
// for the stale-owner window. The small reindex seam keeps that lifecycle deterministic
// under test without a mutable package-global hook.
func NewOwningServerContext(ctx context.Context, vaultRoot string) (*Server, error) {
	return newOwningServerWithRetrievalOptions(ctx, vaultRoot, retrieve.ConstructionOptions{})
}

func newOwningServerWithRetrievalOptions(ctx context.Context, vaultRoot string, options retrieve.ConstructionOptions) (*Server, error) {
	cache := index.NewNoteCache()
	return newOwningServerContextWithOptions(ctx, vaultRoot, func(ctx context.Context, store *index.Store, root string) (*graph.Graph, error) {
		g, notes, err := index.ReindexFullContext(ctx, store, root)
		if err == nil {
			cache.Seed(notes)
		}
		return g, err
	}, options, cache)
}

// NewOwningServerWithRetrievalOptions keeps the existing owner lock, watcher,
// authoring and cancellation lifecycle; only its retrieval constructor differs.
func NewOwningServerWithRetrievalOptions(ctx context.Context, vaultRoot string, options retrieve.ConstructionOptions) (*Server, error) {
	return newOwningServerWithRetrievalOptions(ctx, vaultRoot, options)
}

func newOwningServerContext(ctx context.Context, vaultRoot string, reindex owningReindexFunc, watchCache ...*index.NoteCache) (*Server, error) {
	return newOwningServerContextWithOptions(ctx, vaultRoot, reindex, retrieve.ConstructionOptions{}, watchCache...)
}

func newOwningServerContextWithOptions(ctx context.Context, vaultRoot string, reindex owningReindexFunc, options retrieve.ConstructionOptions, watchCache ...*index.NoteCache) (*Server, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	owner, err := index.AcquireOwnerLock(filepath.Join(vaultRoot, ".mesh"), "mesh ui --own-index", false)
	if err != nil {
		return nil, err
	}
	if err := owner.MarkStarting(); err != nil {
		return nil, startupFailure(ctx, err, owner.Release())
	}
	if err := ctx.Err(); err != nil {
		return nil, startupFailure(ctx, err, owner.Release())
	}
	store, err := index.OpenOwnedContext(ctx, vaultRoot, owner)
	if err != nil {
		return nil, startupFailure(ctx, err, owner.Release())
	}
	cleanup := func(startupErr error) error {
		return startupFailure(ctx, startupErr, errors.Join(store.Close(), owner.Release()))
	}
	identity, err := os.Stat(filepath.Join(vaultRoot, ".mesh", "mesh.db"))
	if err != nil {
		return nil, cleanup(err)
	}
	g, err := reindex(ctx, store, vaultRoot)
	if err != nil {
		return nil, cleanup(err)
	}
	if err := owner.MarkReady(); err != nil {
		return nil, cleanup(err)
	}
	// Apply anything a reader queued for an owner before this process started. Normally
	// empty: it matters when a vault is served read-only for a while and then owned.
	if _, err := store.DrainOpsContext(ctx); err != nil {
		if ctx.Err() != nil {
			return nil, cleanup(err)
		}
		slog.Warn("mesh ui: could not drain the owner op queue at startup", "error", err)
	}
	// Seed the flywheel measurement from the existing agent-authored corpus once, so the
	// Dashboard shows a real reuse number immediately even if no mesh mcp ran (idempotent).
	if _, err := store.BackfillWritebacksContext(ctx); err != nil && ctx.Err() != nil {
		return nil, cleanup(err)
	}
	// Build the note<->code bridge if a code index exists.
	if _, err := store.LinkNotesToCodeContext(ctx, vaultRoot); err != nil && ctx.Err() != nil {
		return nil, cleanup(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, cleanup(err)
	}
	s := newServerContextWithOptions(ctx, vaultRoot, store, g, options)
	s.indexIdentity = identity
	s.owner = owner
	opCtx, opCancel := context.WithCancel(s.lifetimeContext())
	s.ownerOpCancel = opCancel
	s.ownerOpDone = make(chan struct{})
	s.ownerOpWake = make(chan struct{}, 1)
	go s.pollOwnerOps(opCtx)
	if len(watchCache) > 0 {
		s.ownerNotes = watchCache[0]
		s.startOwnerVaultWatch(s.lifetimeContext())
	}
	return s, nil
}

func newServer(vaultRoot string, store *index.Store, g *graph.Graph) *Server {
	return newServerContext(context.Background(), vaultRoot, store, g)
}

func newServerContext(ctx context.Context, vaultRoot string, store *index.Store, g *graph.Graph) *Server {
	return newServerContextWithOptions(ctx, vaultRoot, store, g, retrieve.ConstructionOptions{})
}

func newServerContextWithOptions(ctx context.Context, vaultRoot string, store *index.Store, g *graph.Graph, options retrieve.ConstructionOptions) *Server {
	if ctx == nil {
		ctx = context.Background()
	}
	buildGate := make(chan struct{}, 1)
	buildGate <- struct{}{}
	graphUpdateGate := make(chan struct{}, 1)
	graphUpdateGate <- struct{}{}
	configGate := make(chan struct{}, 1)
	configGate <- struct{}{}
	return &Server{
		retrievalOptions: options,
		vaultRoot:        vaultRoot,
		store:            store,
		graph:            g,
		lifetimeCtx:      ctx,
		buildGate:        buildGate,
		buildRetriever: func(ctx context.Context, store *index.Store, g *graph.Graph) (*retrieve.Retriever, error) {
			return retrieve.NewFromEnvContextWithOptions(ctx, store, g, options)
		},
		graphUpdateGate: graphUpdateGate,
		reindexStore:    index.ReindexContext,
		configGate:      configGate,
		// POST /api/login is the one unauthenticated path that validates a secret, so it
		// is a guessing oracle for the shared admin token and every member client token.
		// 5 attempts per minute per peer, burst 5.
		logins: newRateLimiter(5.0/60.0, 5),
		// POST /api/ask forks an LLM subprocess (or bills a BYOAI key) per call, so it is
		// both rate limited (12/min, burst 4) and capped in flight.
		asks:        newRateLimiter(12.0/60.0, 4),
		askSlots:    make(chan struct{}, askMaxInFlight),
		ownerWait:   index.OwnerIndexBound,
		updateCheck: updatecheck.Default.Check,
	}
}

func (s *Server) lifetimeContext() context.Context {
	if s.lifetimeCtx == nil {
		return context.Background()
	}
	return s.lifetimeCtx
}

func (s *Server) Close() error {
	if s.ownerWatchCancel != nil {
		s.ownerWatchCancel()
	}
	if s.ownerOpCancel != nil {
		s.ownerOpCancel()
	}
	if s.ownerWatchDone != nil {
		<-s.ownerWatchDone
	}
	if s.ownerOpDone != nil {
		<-s.ownerOpDone // never close the store under an in-flight queue transaction
	}
	release, err := s.acquireGraphUpdate(context.Background())
	if err != nil {
		return err
	}
	s.viewClosed = true
	var monitorErr error
	if s.changeMonitor != nil {
		monitorErr = s.changeMonitor.Close()
	}
	release()
	var connectionsErr error
	if s.connections != nil {
		connectionsErr = s.connections.store.Close()
	}
	storeErr := errors.Join(connectionsErr, monitorErr, s.store.Close())
	if s.owner == nil {
		return storeErr
	}
	return errors.Join(storeErr, s.owner.Release())
}

const ownerOpPollInterval = 250 * time.Millisecond

// pollOwnerOps keeps a long-lived `mesh ui --own-index` honest as the vault's declared
// owner after startup. Reader processes publish bookkeeping and automatic extraction
// into .mesh/ops; draining only in the constructor leaves anything queued later waiting
// forever because a UI process can own the vault for days without an HTTP reindex call.
func (s *Server) pollOwnerOps(ctx context.Context) {
	defer close(s.ownerOpDone)
	ticker := time.NewTicker(ownerOpPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.ownerOpWake:
		}
		if n, err := s.store.DrainOpsContext(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("mesh ui: owner op queue remains pending", "error", err)
		} else if n > 0 {
			slog.Debug("mesh ui: applied queued owner ops", "count", n)
		}
	}
}

// refresh rebuilds the in-memory graph from the PERSISTED tables, without touching the
// vault or the write lock. It is how a read-only viewer picks up whatever the owning
// writer has indexed since, and it is the read-only counterpart of the graph swap the
// writable paths do after their own reindex.
func (s *Server) refresh() error {
	return s.refreshContext(context.Background())
}

func (s *Server) refreshContext(ctx context.Context) error {
	release, err := s.acquireGraphUpdate(ctx)
	if err != nil {
		return err
	}
	defer release()
	g, err := s.store.LoadGraphContext(ctx)
	if err != nil {
		return err
	}
	s.publishGraph(g)
	return nil
}

// reindexAndPublish serializes a complete owning rebuild through publication. SQLite
// serializes commits, but it cannot stop an older request that paused after commit from
// publishing its stale in-memory graph after a newer request. The token covers both.
func (s *Server) reindexAndPublish(ctx context.Context, drainOps bool) error {
	release, err := s.acquireGraphUpdate(ctx)
	if err != nil {
		return err
	}
	defer release()
	if drainOps {
		if _, drainErr := s.store.DrainOpsContext(ctx); drainErr != nil {
			if ctxErr := webContextErr(ctx); ctxErr != nil {
				return ctxErr
			}
			slog.Warn("mesh ui: could not drain the owner op queue", "error", drainErr)
		}
	}
	reindex := s.reindexStore
	if reindex == nil {
		reindex = index.ReindexContext
	}
	g, err := reindex(ctx, s.store, s.vaultRoot)
	if err != nil {
		return err
	}
	s.ownerNotes = nil // a full/manual writer bypassed the incremental cache
	s.publishGraph(g)
	return nil
}

func (s *Server) acquireGraphUpdate(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := webContextErr(ctx); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, webContextErr(ctx)
	case <-s.graphUpdateGate:
		return func() { s.graphUpdateGate <- struct{}{} }, nil
	}
}

func (s *Server) publishGraph(g *graph.Graph) {
	// One exclusive critical section for the swap + invalidation, so an in-flight
	// retriever build cannot publish over the old graph (see Server.retriever).
	s.mu.Lock()
	s.graph = g
	s.retrieverGeneration++
	s.cachedRetriever.Store(nil)
	s.mu.Unlock()
}

// ownerDownNote is what every surface here says when the owning writer did not apply a
// change in time. The action itself succeeded and is durable; what is missing is the
// indexing, and the remedy is always the same, so the wording is too.
const ownerDownNote = "This took effect on disk but the index has NOT caught up: the single owning writer " +
	"did not apply it inside the wait, and this viewer only reads what that writer persists. " +
	"The usual cause is that `mesh watch` / `mesh sync --watch` is not running, so check that " +
	"first; the change is picked up as soon as one runs."

// resolveIndexWrites performs index mutations by whichever route this server is allowed
// to take. When it owns the index it applies them directly. When it is a reader it
// queues them for the owning writer and waits for the LAST one to land: ops are applied
// oldest first, so the last one disappearing means all of them did, and one wait means
// one bound rather than one per op.
//
// ownerDown=true means the change is queued and durable but not yet applied. It is never
// an error: reporting a failure for something that WILL take effect is how a caller ends
// up retrying and doing it twice.
func (s *Server) resolveIndexWrites(ctx context.Context, direct func() error, ops ...index.Op) (ownerDown bool, err error) {
	if !s.store.ReadOnly() {
		if directErr := direct(); directErr == nil {
			return false, nil
		} else {
			// The durable artifact may already exist (pending promotion is the important
			// case). If the owning transaction is canceled during shutdown, persist the
			// idempotent follow-through in the same filesystem queue readers use. The
			// replacement owner drains it at startup, so the review row cannot survive and
			// be promoted into a duplicate later.
			for _, op := range ops {
				if _, queueErr := s.store.EnqueueOp(op); queueErr != nil {
					return false, errors.Join(directErr, fmt.Errorf("queue owner follow-through: %w", queueErr))
				}
			}
			if s.ownerOpWake != nil {
				select {
				case s.ownerOpWake <- struct{}{}:
				default:
				}
			}
			return true, nil
		}
	}
	var last string
	for _, op := range ops {
		name, err := s.store.EnqueueOp(op)
		if err != nil {
			return false, err
		}
		last = name
	}
	if err := s.store.AwaitOpApplied(ctx, last, s.ownerWait); err != nil {
		if errors.Is(err, index.ErrOwnerNotIndexing) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// route is one served pattern. The routes live in a table rather than in a run of
// mux.HandleFunc calls so a test can enumerate them: every one of them has to keep
// working on a read-only viewer, and the only way to be sure a NEW one was considered is
// to make the guard fail when it has no entry. See TestEveryRouteSurvivesReadOnly.
type route struct {
	pattern string
	h       http.HandlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{"GET /", s.handleIndex},
		{"GET /graph.json", s.handleGraph},
		{"GET /assets/", s.handleAsset},
		{"POST /api/login", s.handleLogin},
		{"POST /api/logout", s.handleLogout},
		{"GET /connect", s.handleConnectPage},
		{"POST /api/connect/device", s.handleConnectDevice},
		{"POST /api/connect/token", s.handleConnectToken},
		{"POST /api/connect/cancel", s.handleConnectCancel},
		{"POST /api/connect/revoke", s.handleConnectRevoke},
		{"GET /api/connect/request", s.handleConnectRequest},
		{"POST /api/connect/decision", s.handleConnectDecision},
		{"GET /api/connect/connections", s.handleConnectList},
		{"POST /api/connect/disconnect", s.handleConnectDisconnect},
		{"GET /api/status", s.handleStatus},
		{"GET /api/config", s.handleGetConfig},
		{"PUT /api/config", s.handlePutConfig},
		{"POST /api/reindex", s.handleReindex},
		{"GET /api/search", s.handleSearch},
		{"GET /api/note/{id}", s.handleNote},
		{"GET /api/docs", s.handleDocsList},
		{"GET /api/docs/{slug}", s.handleDoc},
		{"GET /api/mcp-tools", s.handleMCPTools},
		{"GET /api/dashboard", s.handleDashboard},
		{"POST /api/ask", s.handleAsk},
		{"GET /api/pending", s.handlePendingList},
		{"POST /api/pending/promote", s.handlePendingPromote},
		{"POST /api/pending/discard", s.handlePendingDiscard},
		{"GET /openapi.json", s.handleOpenAPI},
	}
}

// Handler wires the routes: the SPA shell, the graph payload, embedded assets, and
// the /api surface, all behind the auth guard (a no-op on a loopback bind).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		handler := rt.h
		// These routes consume the cached graph/retriever. Authenticate first
		// (the outer guard below), then observe external owner commits before
		// reading. Assets/docs and mutation-only routes need no freshness probe.
		switch rt.pattern {
		case "GET /graph.json", "GET /api/status", "GET /api/search", "GET /api/note/{id}", "GET /api/dashboard", "POST /api/ask":
			handler = s.freshRead(handler)
		}
		mux.HandleFunc(rt.pattern, handler)
	}
	var h http.Handler
	if s.member != nil {
		h = s.memberGuard(mux) // per-member auth (mesh ui --hub-db)
	} else {
		h = s.auth.guard(mux) // single shared token (standalone)
	}
	h = s.connectionGuard(h)
	if s.basePath != "" {
		// Serve the whole app under the path: strip it before the inner mux (so its
		// root-relative routes match) and let the subtree pattern redirect /app -> /app/.
		outer := http.NewServeMux()
		outer.Handle(s.basePath+"/", http.StripPrefix(s.basePath, h))
		h = outer
	}
	return securityHeaders(h)
}

// contentSecurityPolicy is the second line of defence behind renderMDSafe: the SPA
// assigns server-rendered note HTML to innerHTML, so if a sanitiser gap ever let a
// tag through, script-src 'self' still refuses to run an inline handler or a
// javascript: URL. Everything the app needs is same-origin (self-hosted fonts, no
// CDN, no third-party JS), so 'self' costs nothing. style-src keeps 'unsafe-inline'
// because the shell and a few panels use style="" attributes; that is a cosmetic
// surface, not a script one. base-uri stays 'self' rather than 'none' because the
// shell ships a <base href> the server rewrites for the configured base path.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// securityHeaders sets the standard hardening headers on every response. In
// particular Referrer-Policy: no-referrer keeps any token that ends up in a URL out
// of the Referer header on outbound navigations; responses carry private vault data
// so they are also marked no-store and non-framable.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusInternalServerError)
		return
	}
	html := strings.ReplaceAll(string(body), "__MESH_BASE__", s.baseHref())
	html = strings.ReplaceAll(html, "__MESH_SOURCE__", buildinfo.FooterInline())
	html = strings.ReplaceAll(html, "__MESH_SIGN_IN__", s.browserSignInAttribute(r))
	body = []byte(html)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	g := s.graph
	s.mu.RUnlock()
	exp := BuildExport(g, s.exposedVaultRoot(), s.allowedScopes(r), s.allowedPath(r))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(exp)
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	body, err := assetsFS.ReadFile("assets/" + name)
	// Dev-serve: when MESH_WEB_DEV points at an assets dir, read from disk so a
	// front-end edit is live on refresh with no binary rebuild. Off in prod (env
	// unset). name is already rejected if it contains "..", so this cannot escape.
	if dir := os.Getenv("MESH_WEB_DEV"); dir != "" {
		if b, e := os.ReadFile(filepath.Join(dir, name)); e == nil {
			body, err = b, nil
		}
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache") // assets change on every binary rebuild; revalidate
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
	case strings.HasSuffix(name, ".woff2"):
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // fonts never change
	default:
		if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(body)
}

// exposedVaultRoot is the vault path we are willing to hand a browser. In standalone
// mode the browser IS the operator on the same machine and the absolute path drives the
// editor:// bridge, so it stays. In member mode (mesh ui --hub-db) the caller is a
// remote teammate, possibly a read-only viewer, and the server's filesystem layout is
// pure reconnaissance for them, so only the vault's basename leaves the process.
func (s *Server) exposedVaultRoot() string {
	if s.member == nil {
		return s.vaultRoot
	}
	return filepath.Base(s.vaultRoot)
}

// handleStatus reports index counts and which retrieval signals are active, the
// browser equivalent of `mesh status`.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Count what THIS caller may read, not the whole corpus. The stored totals span
	// every scope, so a scope-confined member could read them off /api/status and learn
	// how many notes exist outside their partition, then poll to watch another scope
	// grow. The same aggregate is deliberately scoped in internal/mcp/tools.go ("A
	// scope-confined caller must not learn out-of-scope volume") and admin-gated on
	// /api/dashboard; this route was the one surface that reported it raw.
	// A folder ACL confines volume exactly as a scope does, and it is the only boundary
	// a team that fenced folders without defining a scope has, so it gates this branch too.
	var notes, nodes, edges, vectors int
	allowed, allowPath := s.allowedScopes(r), s.allowedPath(r)
	if allowed != nil || allowPath != nil {
		notes, nodes, edges = s.visibleCounts(func(n *graph.Node) bool {
			return scopeVisible(n, allowed) && pathVisible(n, allowPath)
		})
		vectors = 0 // per-partition vector counts are not tracked; do not leak the global
	} else {
		// An unrestricted caller (owner/admin, or the standalone single-token viewer)
		// still needs the note count: the web app reads counts.notes to decide whether
		// to show the "this vault is empty" overlay, so leaving it at zero here put a
		// full vault behind an empty-state card for exactly the people who can see all
		// of it.
		notes, _ = s.store.CountContext(r.Context(), "notes")
		nodes, _ = s.store.CountContext(r.Context(), "nodes")
		edges, _ = s.store.CountContext(r.Context(), "edges")
		vectors, _ = s.store.CountContext(r.Context(), "vectors")
	}
	var update updatecheck.Notice
	if s.updateCheck != nil {
		// The checker is capped at three seconds and cached for a day. A failure is
		// deliberately invisible: update awareness must never make the local UI fail.
		update, _ = s.updateCheck(r.Context(), buildinfo.ReleaseVer())
	}
	writeJSON(w, map[string]any{
		// Release identity belongs to the running process, not the optional public
		// update check. Keep it available when checks are disabled or unavailable.
		"release": buildinfo.ReleaseVer(),
		"vault":   s.exposedVaultRoot(),
		"viewer":  s.viewerReadiness(r),
		"counts":  map[string]int{"notes": notes, "nodes": nodes, "edges": edges, "vectors": vectors},
		"signals": map[string]bool{
			"fts":    true,
			"graph":  true,
			"vector": vectors > 0 && !s.retrievalOptions.LocalOnly,
			"rerank": !s.retrievalOptions.LocalOnly && os.Getenv("MESH_RERANK_ENDPOINT") != "",
			"ann":    !s.retrievalOptions.LocalOnly && os.Getenv("MESH_HNSW_THRESHOLD") != "" && os.Getenv("MESH_HNSW_THRESHOLD") != "0",
		},
		"authRequired": s.auth.authRequired() || s.member != nil,
		"update":       update,
	})
}

// visibleCounts walks the graph under the read lock and counts what visible admits.
// The lock is the same one handleGraph takes for s.graph and the same one refresh, the
// reindex route and the pending promote take to swap it; this walk is the only reader
// that used to skip it, which made an ordinary /api/status poll race a promote.
func (s *Server) visibleCounts(visible func(*graph.Node) bool) (notes, nodes, edges int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := s.graph
	if g == nil {
		return 0, 0, 0
	}
	seen := map[string]bool{}
	for _, n := range g.Nodes() {
		if !visible(n) {
			continue
		}
		nodes++
		if n.Kind == "note" && !seen[n.NoteID] {
			seen[n.NoteID] = true
			notes++
		}
		for _, e := range g.Neighbors(n.ID) {
			if tn, ok := g.Node(e.Target); ok && visible(tn) {
				edges++
			}
		}
	}
	return notes, nodes, edges
}

// normalizeBasePath returns "" for root, or a clean "/seg" with a leading slash and
// no trailing slash, so it composes with the route prefixes and the <base> href.
func normalizeBasePath(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	return "/" + p
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// requestDrain closes the gap left by http.Server.Close: Close tears down sockets but
// explicitly does not wait for handler goroutines. An owning UI must not release its
// Store/owner.lock while an old handler can still write through that Store, so shutdown
// first refuses new handler entries and then waits for every admitted handler to leave.
type requestDrain struct {
	mu         sync.Mutex
	active     int
	stopping   bool
	idle       chan struct{}
	idleClosed bool
}

func newRequestDrain() *requestDrain { return &requestDrain{idle: make(chan struct{})} }

func (d *requestDrain) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		if d.stopping {
			d.mu.Unlock()
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		d.active++
		d.mu.Unlock()

		defer func() {
			d.mu.Lock()
			d.active--
			if d.stopping && d.active == 0 && !d.idleClosed {
				close(d.idle)
				d.idleClosed = true
			}
			d.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// stop refuses future entries and returns a channel closed after every handler already
// admitted by wrap has returned. Waiting is intentionally unbounded after the HTTP
// shutdown timeout: Docker may SIGKILL at its outer grace limit (leaving owner.lock for
// stale recovery), but this process will never knowingly admit a second writer early.
func (d *requestDrain) stop() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopping = true
	if d.active == 0 && !d.idleClosed {
		close(d.idle)
		d.idleClosed = true
	}
	return d.idle
}

// Serve builds the server and listens on addr (e.g. 127.0.0.1:7474). A loopback bind
// needs no auth; binding beyond loopback is fail-closed and requires a token (see
// newAuthConfig). SIGINT/SIGTERM become a graceful shutdown so deferred cleanup runs --
// in particular, an owning UI removes owner.lock before its replacement starts. Go's
// default SIGTERM exits without running defers, which made every container recreate
// wait out the stale-owner window.
func Serve(vaultRoot, addr, token, basePath string, ownIndex bool, verify func(string) (int64, string, bool), scopesFor func(int64) map[string]bool, pathsFor func(int64) func(string) bool, roleFor func(int64) (string, int64, bool)) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return ServeContext(ctx, vaultRoot, addr, token, basePath, ownIndex, verify, scopesFor, pathsFor, roleFor)
}

// ServeContext is Serve with caller-controlled shutdown. When verify != nil the app
// runs in per-member mode (each request authenticates as a hub client and is scoped to
// them); member auth is then the fail-closed gate, so the single-token requirement is
// skipped. Otherwise it is the standalone single-token viewer (loopback needs no token).
// ownIndex=true makes this process the OWNING WRITER of the vault's index instead of a
// reader of it. Only correct where nothing else writes that index (the mesh-ui
// container); beside a `mesh watch` / `mesh sync --watch` it reintroduces the second
// long-lived writer this whole split removed.
func ServeContext(ctx context.Context, vaultRoot, addr, token, basePath string, ownIndex bool, verify func(string) (int64, string, bool), scopesFor func(int64) map[string]bool, pathsFor func(int64) func(string) bool, roleFor func(int64) (string, int64, bool), browser ...*BrowserSignIn) (retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	// The server lifetime follows the caller, but is also canceled when Serve itself
	// fails. Accepted pending promotions deliberately detach from a browser disconnect;
	// without this child context an unexpected listener failure could leave one running
	// while requestDrain waits for it, preventing Close and owner-lock release forever.
	lifetimeCtx, cancelLifetime := context.WithCancel(ctx)
	defer cancelLifetime()
	memberMode := verify != nil
	var auth authConfig
	if !memberMode {
		a, err := newAuthConfig(addr, token)
		if err != nil {
			return err
		}
		auth = a
	}
	var s *Server
	var err error
	if ownIndex {
		// NewOwningServer acquires and retains the declared owner lease for the whole
		// server lifetime. Keeping acquisition in the constructor also protects direct
		// embedders and tests, rather than only this HTTP wrapper.
		s, err = NewOwningServerContext(lifetimeCtx, vaultRoot)
	} else {
		s, err = NewServerContext(lifetimeCtx, vaultRoot)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return nil
		}
		// The read-only open refuses a vault with no index, which is right (an empty graph
		// served as if it were the vault is worse), but its advice is written for the MCP.
		// Say the part that is specific to being the viewer.
		if !ownIndex && errors.Is(err, index.ErrNoIndexYet) {
			return fmt.Errorf("%w\n\nmesh ui reads the index, it does not build it. Either start the owning "+
				"writer (`mesh watch %s`), index once (`mesh index %s`), or pass --own-index if nothing "+
				"else writes this vault", err, shellpath.Quote(vaultRoot), shellpath.Quote(vaultRoot))
		}
		return err
	}
	// Close stops and joins the owner op poller, closes SQLite, then releases the owner
	// lock last. Surface any failure: a silent lock-release error recreates the exact
	// restart loop graceful shutdown exists to prevent.
	defer func() { retErr = errors.Join(retErr, s.Close()) }()
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	s.auth = auth
	s.basePath = normalizeBasePath(basePath)
	if memberMode {
		s.auth.token = strings.TrimSpace(token) // bind delegated break-glass grants to token rotation
		s.SetMemberAuth(verify, scopesFor, pathsFor, roleFor)
	}
	if publicURL := strings.TrimSpace(os.Getenv("MESH_UI_PUBLIC_URL")); publicURL != "" {
		statePath := strings.TrimSpace(os.Getenv("MESH_UI_CONNECTIONS_DB"))
		if statePath == "" {
			statePath = filepath.Join(vaultRoot, ".mesh", "auth", "connections.db")
		}
		statePath, err = filepath.Abs(statePath)
		if err != nil {
			return err
		}
		if err = s.EnableConnections(publicURL, statePath); err != nil {
			return err
		}
	}
	for _, b := range browser {
		if b != nil {
			if err := s.SetBrowserSignIn(*b); err != nil {
				return err
			}
		}
	}
	exp := BuildExport(s.graph, vaultRoot, nil, nil)
	fmt.Printf("mesh ui: %d notes, %d links across %d communities\n", exp.Meta.NodeCount, exp.Meta.EdgeCount, len(exp.Communities))
	fmt.Print(indexOwnershipLine(vaultRoot, ownIndex))
	if memberMode {
		fmt.Printf("auth: per-member (hub client token; views scoped per member)\n")
	} else if auth.authRequired() {
		fmt.Printf("auth: token required (Authorization: Bearer ...)\n")
	}
	// Explicit timeouts, matching the hub's. The zero-value http.Server has none, so a
	// client that opens a connection and never finishes its headers (or never reads its
	// response) pins a goroutine, an fd and a connection forever. In prod this port sits
	// behind a buffering proxy, but anything else on the docker network can reach it
	// directly, and `mesh ui --token` on a public bind has no proxy at all.
	requestCtx, cancelRequests := context.WithCancel(lifetimeCtx)
	defer cancelRequests()
	requests := newRequestDrain()
	srv := &http.Server{
		Addr:              addr,
		Handler:           requests.wrap(s.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      askMaxDuration + 30*time.Second, // /api/ask is the long pole
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
		BaseContext:       func(net.Listener) context.Context { return requestCtx },
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Printf("serving at http://%s%s  (Ctrl-C to stop)\n", listener.Addr(), s.baseHref())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(listener) }()

	select {
	case err := <-errCh:
		idle := requests.stop()
		cancelLifetime()
		cancelRequests()
		closeErr := srv.Close()
		<-idle
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		return errors.Join(err, closeErr)
	case <-lifetimeCtx.Done():
		// Shutdown does not cancel handler contexts itself. Cancel first so long-running
		// ask/export requests can drain, then leave enough margin inside Compose's explicit
		// stop_grace_period for deferred ownership and SQLite cleanup to finish.
		idle := requests.stop()
		cancelRequests()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := srv.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			// The UI has no hijacked connections. Force-close the remaining HTTP connections,
			// then still wait for their handlers below: Close alone does not join them.
			closeErr := srv.Close()
			serveErr := <-errCh
			<-idle
			if errors.Is(serveErr, http.ErrServerClosed) {
				serveErr = nil
			}
			return errors.Join(fmt.Errorf("mesh ui graceful shutdown: %w", shutdownErr), closeErr, serveErr)
		}
		serveErr := <-errCh
		<-idle
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	}
}

// indexOwnershipLine is the `mesh ui` startup line about who indexes this vault.
//
// It consults the lock on disk instead of inferring from --own-index. Without that flag
// this line used to print "the owning writer (mesh watch / mesh sync --watch) indexes"
// unconditionally, so a vault with no owner.lock at all was told an owner was indexing
// it, on the same machine where `mesh doctor` printed "owner: NONE". A startup banner
// that asserts a healthy setup is worse than no banner: it is the line an operator reads
// INSTEAD of checking, and it sent them looking for a broken watcher that did not exist.
//
// Extracted from Serve so the claim is testable without binding a port.
func indexOwnershipLine(vaultRoot string, ownIndex bool) string {
	if ownIndex {
		return "index: OWNED by this process (it reindexes and writes)\n"
	}
	if info, live := index.OwnerStatus(filepath.Join(vaultRoot, ".mesh")); live {
		return fmt.Sprintf("index: read-only; %s owns this vault and indexes it\n", info.Describe())
	}
	return fmt.Sprintf("index: read-only, and NOTHING is indexing this vault\n  %s\n",
		index.NoOwnerRemedy(vaultRoot))
}
