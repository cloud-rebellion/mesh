// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bright-interaction/mesh/internal/latency"
	"github.com/bright-interaction/mesh/internal/shellpath"

	_ "modernc.org/sqlite"
)

// Store is the SQLite index. Concurrency model (spec section 3.2): the database
// is opened in WAL mode with two pools. All writes funnel through a single
// writer goroutine over a channel, so there is never a second writer and no
// "database is locked" contention. Reads use a separate pool, which WAL serves
// concurrently with the writer. This is the foundation the fsnotify watcher
// (later) needs: it can stream upserts while mesh_search reads, with no deadlock.
type Store struct {
	dir      string
	dbPath   string
	writeDB  *sql.DB
	readDB   *sql.DB
	jobs     chan job
	done     chan struct{}
	wg       sync.WaitGroup // tracks the writer goroutine so Close can join it
	readOnly bool           // true for OpenReadOnly: no writer goroutine, Write always fails
	// opsDrainGate serializes queue consumers without making cancellation wait behind a
	// filesystem/SQLite drain already in progress. DrainOpsContext holds the sole token
	// from listing the queue through effect commit and file removal.
	opsDrainGate chan struct{}
	// Guarded by opsDrainGate. A failed optional source refresh must not repeat
	// its full scan on every note event while the operational fault persists.
	codeRefreshRetryAfter  time.Time
	codeRefreshRetryConfig string
	codeRefreshRetryErr    error

	closeOnce sync.Once // Close is idempotent: a second close(s.done) would panic
	closeErr  error     // the first Close's result, replayed to later callers

	healthMu          sync.Mutex
	healthClosed      bool
	healthCancel      context.CancelFunc
	healthDone        chan struct{}
	healthLastAttempt time.Time

	// writeGuard is an optional live ownership check. Opportunistic MCP ownership can
	// be preempted after this Store was opened; every transaction and checkpoint must
	// then stop writing even though the physical write connection still exists until
	// orderly shutdown.
	writeGuardMu sync.RWMutex
	writeGuard   func() bool
	// writeLease makes ownership linearizable with COMMIT. An OwnerLock lease holds
	// the same cross-process metadata lock takeover needs, from authorization through
	// schema/transaction commit.
	writeLease func() (release func(), ok bool)

	mu      sync.Mutex  // guards dropped + droppedSynced
	dropped []FileError // notes dropped as unparseable by the last full reindex
	// droppedSynced records whether this store has mirrored its dropped set into the
	// index yet. The first pass always writes (the table may hold a previous run's
	// rows), later ones only when the set changed; see persistDropped.
	droppedSynced bool

	// Telemetry is accumulated in memory and flushed in one batched transaction by the
	// writer goroutine (see flushTelemetry). It must never be written inline: the
	// read-only MCP/web tools call IncrMetric/RecordReuse purely for counters AFTER the
	// answer is already computed, and Write is a synchronous handoff to the single
	// writer, so a search or fetch used to inherit the latency of whatever transaction
	// was running (a full reindex) or, with a second mesh process holding the SQLite
	// write lock, up to the DSN's 30s busy_timeout.
	telMu    sync.Mutex
	telCount map[string]int64 // metrics key -> pending increment
	telReuse []reuseEvent     // pending flywheel reuse events, timestamped at call time
}

// reuseEvent is one deferred RecordReuse. The timestamp is captured when the fetch
// happened, not when the batch is flushed, so the gap check stays honest.
type reuseEvent struct {
	noteID string
	gapSec int64
	at     int64
}

// SchemaVersion bumps whenever schema.sql changes shape OR derived-index semantics
// change in a way ordinary drift detection cannot observe. The index is a derived,
// deletable artifact (the markdown vault is the source of truth), so a version mismatch
// drops and rebuilds rather than running a migration. This is why Mesh uses no
// goose/golang-migrate: there is no irreplaceable data to migrate.
// Note: the source-code tables (code_files/code_symbols/code_edges/code_search)
// were added additively via CREATE TABLE IF NOT EXISTS, so they appear on existing
// databases without a destructive rebuild and the version stays 2. Shape changes to
// existing tables and undetectable changes to derived rows require the drop+rebuild below.
// v3: notes gained review_by + source columns (provenance / lifecycle, Phase A).
// v4: notes gained a scope column (access-control partition; absent = dev).
// v5: no column changed, but retrievalHash gained scope/updated/when/review_by/source,
// so every stored retrieval_hash is computed from an older, narrower input. The bump
// forces one rebuild that re-hashes every row; without it a note whose scope was already
// tightened before the upgrade would keep comparing equal and never reindex, leaving the
// old (wider) scope live in notes.scope. Kept embeddings survive the rebuild
// (see schemaKeep and metaKeptWithVectors); their note_hash is stale against the new
// hash, so they are excluded from retrieval until the next `mesh embed`, which re-stamps
// them from the content-hash cache without paying for a single new embedding.
// v6: dropped_notes landed in schema.sql on 2026-08-09 with NO version bump, so every
// index written by a released binary before that date is stamped 5 and has no such table.
// Nothing rebuilt it, because only the writable Open compares versions and the shipped
// per-window setup is read-only. The bump is what makes those databases identifiable, and
// OpenReadOnlyAt now refuses them by version instead of querying a table that is not there.
// v7: BuildGraph began deriving supersedes edges plus a superseded_by node attribute.
// `supersedes` was already retrieval-hashed, so a v6 index over unchanged Markdown looks
// perfectly current to DriftReport even though it lacks that derived state. The semantic
// bump forces one graph rebuild; kept vectors and their model metadata survive it.
const SchemaVersion = 8

type job struct {
	ctx   context.Context
	fn    func(*sql.Tx) error
	reply chan error
}

const (
	// walSizeLimit is the size mesh.db-wal is held near on disk. SQLite's WAL
	// autocheckpoint is PASSIVE: it resets the write pointer but NEVER shrinks the file,
	// so without help the WAL only grows to its high-water mark and stays there (observed
	// 223MB, which starved writers into SQLITE_BUSY).
	//
	// journal_size_limit is set from this, but do NOT read that as a guarantee. It only
	// applies when a checkpoint RESETS the WAL, and a PASSIVE checkpoint cannot reset it
	// while any reader holds a snapshot, which is the normal state under a watch daemon.
	// This comment previously claimed "every checkpoint truncates the WAL back to at most
	// this size" and the live vault disproved it at 26.44MB. What actually bounds the file
	// is the writer loop escalating to TRUNCATE when it sees the file over this size; see
	// writer() and TestPassiveCheckpointDoesNotBoundTheWAL.
	//
	// 16MB holds the largest single reindex transaction's frames without thrashing.
	walSizeLimit = 16 * 1024 * 1024
	// walCheckpointInterval is how often the single writer goroutine checkpoints: PASSIVE
	// every tick (never blocks the writer on a reader), escalating to a best-effort
	// TRUNCATE only when the file is actually over walSizeLimit. Autocheckpoint alone
	// fires only on writes, so an idle stretch would otherwise leave the WAL as it was.
	walCheckpointInterval = 2 * time.Minute
	// telemetryFlushInterval is how often the writer goroutine drains the in-memory
	// usage counters into one batched transaction. Short enough that a dashboard is
	// never meaningfully behind, long enough that a burst of fetches costs one write
	// instead of three per request.
	telemetryFlushInterval = 2 * time.Second
	// Telemetry is explicitly best-effort. A periodic or final flush must not keep the
	// writer (and therefore Store.Close) behind a contended SQLite writer for the normal
	// 30-second transaction patience.
	telemetryFlushTimeout = time.Second
)

const (
	// busyTimeoutMS is how long any mesh connection waits for another mesh process's
	// write lock before giving up. It has to be generous: a sibling watcher's reconcile
	// is a whole-vault write transaction (nodes + edges + note_code_links are rewritten
	// wholesale even for one edited note), so seconds of waiting is normal and correct.
	busyTimeoutMS = 30000
	// writeBusyPollMS slices schema and write-transaction lock waits so a canceled owning
	// server does not inherit the full 30-second SQLite busy handler. modernc SQLite does
	// not interrupt an active busy handler when ExecContext/BeginTx's context is canceled;
	// one 100ms attempt followed by a ctx check is the bounded substitute. Background
	// callers retry for busyTimeoutMS in total, preserving the legacy patience.
	writeBusyPollMS = 100
	// checkpointBusyTimeoutMS is the patience of the best-effort TRUNCATE checkpoint,
	// and of nothing else. Short on purpose so a contended vault costs one skipped tick
	// instead of stalling the writer. It is applied on the checkpoint's OWN connection;
	// see checkpointTruncateBestEffort for why it must never touch a pool.
	checkpointBusyTimeoutMS = 2000
	// walAutocheckpointPages is SQLite's own default (1000 pages, ~4MB at the default
	// 4096-byte page size), set EXPLICITLY on the DSN rather than left implicit. This is
	// the outstanding rider from the multi-writer decision (journal_size_limit already
	// shipped; wal_autocheckpoint did not). It changes nothing about the bound: at 1000
	// pages it fires well under walSizeLimit's 16MB, so it is redundant with the writer's
	// own PASSIVE-then-escalate-to-TRUNCATE loop below, not a replacement for it. Being
	// explicit means a future SQLite default change can't silently move this threshold
	// out from under the tuning the comments here already documented.
	walAutocheckpointPages = 1000
)

func dsn(path string) string { return dsnBusy(path, busyTimeoutMS) }

// dsnWrite reserves SQLite's write lock when BeginTx starts, before a transaction
// callback is invoked. A busy result is therefore safe to retry: no callback state or
// SQL work has run yet. Its short busy handler is paired with runTxContext's retry loop.
func dsnWrite(path string) string { return dsnBusy(path, writeBusyPollMS) + "&_txlock=immediate" }

// dsnBusy is dsn with an explicit busy_timeout, so a connection that wants a different
// patience gets it in its DSN rather than by running a PRAGMA on a shared pool.
func dsnBusy(path string, busyMS int) string {
	return fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)&_pragma=foreign_keys(on)&_pragma=journal_size_limit(%d)&_pragma=wal_autocheckpoint(%d)",
		path, busyMS, walSizeLimit, walAutocheckpointPages,
	)
}

// dsnReadOnly is the DSN for OpenReadOnly: mode=ro refuses the OS-level open flags SQLite
// would need to write at all (SQLITE_READONLY on any write attempt, "attempt to write a
// readonly database"), and query_only(true) is defense in depth on top of that at the SQL
// layer. Deliberately does NOT set journal_mode(WAL): that pragma is a schema change, and
// asking a read-only connection to (re-)declare it risks the exact write attempt mode=ro
// exists to refuse, on a file already in WAL mode on disk from the owning writer. It also
// does not set journal_size_limit or wal_autocheckpoint: both are checkpoint tuning, and
// a connection that cannot write never checkpoints.
func dsnReadOnly(path string) string {
	return fmt.Sprintf(
		"file:%s?mode=ro&_pragma=busy_timeout(%d)&_pragma=foreign_keys(on)&_pragma=query_only(true)",
		path, busyTimeoutMS,
	)
}

// vaultMeshDirMode keeps <vault>/.mesh owner-only. It agrees with pkg/meshclient and
// internal/ingest, which create the same directory. Three packages create it and
// MkdirAll is a NO-OP on an existing directory, so the FIRST one to run silently
// decides the mode for every other one's files, and this package is almost always
// first: every mesh command opens the index. That is how a vault ends up with a
// correctly 0600 .mesh/credentials sitting in a world-traversable directory, next to
// a 0644 mesh.db holding the full text of every note.
const vaultMeshDirMode = 0o700

// Open creates (or opens) <vaultRoot>/.mesh/mesh.db, applies the schema, and
// starts the writer goroutine.
func Open(vaultRoot string) (*Store, error) {
	dir := filepath.Join(vaultRoot, ".mesh")
	return openUnownedAt(vaultRoot, dir, true)
}

// OpenOwned holds owner's cross-process lease across writable Open and installs the
// same lease on every later Store transaction before takeover can proceed.
func OpenOwned(vaultRoot string, owner *OwnerLock) (*Store, error) {
	return OpenOwnedContext(context.Background(), vaultRoot, owner)
}

// OpenOwnedContext is OpenOwned with a caller-owned schema-open lifetime. It is the
// startup path for declared long-lived owners: cancellation interrupts SQLite's
// BEGIN IMMEDIATE/busy wait instead of inheriting the DSN's 30-second timeout after the
// owner claim has already been published.
func OpenOwnedContext(ctx context.Context, vaultRoot string, owner *OwnerLock) (*Store, error) {
	return openOwnedWithHookContext(ctx, vaultRoot, owner, nil)
}

// openOwnedWithHook is OpenOwned with a deterministic test seam after the lease is
// acquired and before schema open begins.
func openOwnedWithHook(vaultRoot string, owner *OwnerLock, afterLease func()) (*Store, error) {
	return openOwnedWithHookContext(context.Background(), vaultRoot, owner, afterLease)
}

func openOwnedWithHookContext(ctx context.Context, vaultRoot string, owner *OwnerLock, afterLease func()) (*Store, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	release, ok := owner.acquireLease()
	if !ok {
		return nil, ErrReadOnly
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if afterLease != nil {
		afterLease()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store, err := openAtContext(ctx, vaultRoot, filepath.Join(vaultRoot, ".mesh"), true)
	if err != nil {
		return nil, err
	}
	attachOwnerLease(store, owner)
	return store, nil
}

func attachOwnerLease(store *Store, owner *OwnerLock) {
	store.writeGuardMu.Lock()
	store.writeGuard = owner.Held
	store.writeLease = owner.acquireLease
	store.writeGuardMu.Unlock()
}

func attachUnownedLease(store *Store) {
	store.writeGuardMu.Lock()
	store.writeLease = func() (func(), bool) {
		release, err := acquireUnownedLease(store.dir, false)
		return release, err == nil
	}
	store.writeGuardMu.Unlock()
}

// OpenCurrent opens a writable store without replacing an existing mismatched schema.
// Use it when a command writes auxiliary state but does not immediately rebuild notes.
func OpenCurrent(vaultRoot string) (*Store, error) {
	dir := filepath.Join(vaultRoot, ".mesh")
	return openUnownedAt(vaultRoot, dir, false)
}

// OpenCurrentOwned is OpenCurrent under owner's lifetime lease. Commands that mutate
// auxiliary index tables (embeddings, health, code, review queue) use it so they cannot
// open a second writer beside the vault's declared owner.
func OpenCurrentOwned(vaultRoot string, owner *OwnerLock) (*Store, error) {
	release, ok := owner.acquireLease()
	if !ok {
		return nil, ErrReadOnly
	}
	defer release()
	store, err := openAt(vaultRoot, filepath.Join(vaultRoot, ".mesh"), false)
	if err != nil {
		return nil, err
	}
	attachOwnerLease(store, owner)
	return store, nil
}

// ensureVaultMeshDir creates <vault>/.mesh at vaultMeshDirMode and, when it already
// exists wider than that, narrows it. The repair is the point: MkdirAll's mode applies
// only when it CREATES the directory, exactly like os.WriteFile's perm argument, so
// tightening the constant alone would leave every vault created by an older mesh at
// 0755 forever. Only the group and other bits are touched, and only downward, so an
// operator who deliberately narrowed further is not widened back.
func ensureVaultMeshDir(dir string) error {
	if err := os.MkdirAll(dir, vaultMeshDirMode); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		// fi.Mode(), not perm: Perm() masks off setgid/setuid/sticky, so chmod'ing the
		// masked value silently cleared them. Only group/other are meant to change.
		if err := os.Chmod(dir, fi.Mode()&^0o077); err != nil {
			// Best effort: a vault on a filesystem that refuses chmod (or one owned by
			// another uid) must still be indexable. Log it rather than fail the command,
			// but log it at WARN, because the credentials beside it are exposed.
			slog.Warn("index: cannot narrow world-readable vault .mesh",
				"dir", dir, "mode", perm.String(), "err", err)
		}
	}
	return nil
}

// indexFileMode is the mode the index files themselves carry. Narrowing the directory
// was only half the fix: SQLite creates mesh.db with 0666&^umask, which is 0644 under
// the default umask, so the file holding the full text of every note was world-readable
// on its own terms. That is invisible while it sits inside a 0700 directory and becomes
// real the moment the bytes are copied out of it, which is the normal case: a backup
// tool, a `cp -r`, a vault on a synced volume, or an older vault whose .mesh predates
// the 0700 constant.
const indexFileMode = 0o600

// narrowIndexFiles chmods mesh.db and its WAL/SHM siblings down to owner-only, both on
// create and on an existing index written by an older mesh.
//
// The siblings are not an afterthought: mesh.db-wal holds every recently committed note
// body until a checkpoint folds it back, so leaving it 0644 leaks the newest writes,
// which are the ones most worth reading. Best effort throughout, for the same reason
// ensureVaultMeshDir is: an index on a filesystem that refuses chmod must still work.
func narrowIndexFiles(dbPath string) { NarrowDBFiles(dbPath) }

// NarrowDBFiles is narrowIndexFiles for any SQLite database this project owns, exported
// so internal/hub narrows hub.db through the SAME code rather than growing a second copy
// of it. hub.db is the higher-value target of the two: it holds client and invite token
// hashes plus AES-GCM connector PATs whose key, when MESH_HUB_SECRET_KEY is unset, is
// derived from hub.db itself, so one read yields key and ciphertext together.
func NarrowDBFiles(dbPath string) {
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue // -wal/-shm need not exist yet
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			if err := os.Chmod(p, fi.Mode()&^0o077); err != nil {
				slog.Warn("index: cannot narrow world-readable index file",
					"path", p, "mode", perm.String(), "err", err)
			}
		}
	}
}

// EnsureOwnerOnlyDir is ensureVaultMeshDir for any directory this project owns, exported
// so internal/hub REPAIRS its data directory rather than only setting a mode on create.
// MkdirAll's perm applies solely when it creates, and the hub's directory is a bind mount
// that always already exists, so the 0700 constant alone was a no-op on every real
// deployment while a fresh install got 0700 and looked correct.
func EnsureOwnerOnlyDir(dir string) error { return ensureVaultMeshDir(dir) }

// OpenAt is like Open but stores the index in an explicit directory instead of
// <vaultRoot>/.mesh. The hub uses this to index its served vault into a dir OUTSIDE
// the git repo, so the index is never synced to clients.
func OpenAt(vaultRoot, meshDir string) (*Store, error) {
	return openUnownedAt(vaultRoot, meshDir, true)
}

// OpenCurrentAt is OpenCurrent with an explicit index directory.
func OpenCurrentAt(vaultRoot, meshDir string) (*Store, error) {
	return openUnownedAt(vaultRoot, meshDir, false)
}

func openUnownedAt(vaultRoot, meshDir string, allowSchemaRebuild bool) (*Store, error) {
	if err := ensureVaultMeshDir(meshDir); err != nil {
		return nil, err
	}
	release, err := acquireUnownedLease(meshDir, true)
	if err != nil {
		return nil, err
	}
	defer release()
	store, err := openAt(vaultRoot, meshDir, allowSchemaRebuild)
	if err != nil {
		return nil, err
	}
	attachUnownedLease(store)
	return store, nil
}

func openAt(vaultRoot, meshDir string, allowSchemaRebuild bool) (*Store, error) {
	return openAtContext(context.Background(), vaultRoot, meshDir, allowSchemaRebuild)
}

func openAtContext(ctx context.Context, vaultRoot, meshDir string, allowSchemaRebuild bool) (*Store, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The same 0700 treatment Open gets, because this is the OTHER entry point that
	// creates the directory. It used to MkdirAll at 0755 and never narrow: harmless
	// when reached via Open (which has already created the dir, making this call a
	// no-op) and wide open on every path that calls OpenAt directly, which is the
	// hub indexing the served vault. That index holds the full text of every note in
	// the team vault, so on a shared box it was world-traversable.
	if err := ensureVaultMeshDir(meshDir); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(meshDir, "mesh.db")
	_, statErr := os.Stat(dbPath)
	dbExisted := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, statErr
	}
	// Deferred, not straight-line: a corrupt or unreadable database returns before any
	// later statement, and that is precisely the case where the file sits at 0644 while
	// the operator retries. Matches OpenReadOnlyAt.
	defer narrowIndexFiles(dbPath)
	// The directory half of the same claim. Repair only, never create: a read-only open
	// must still fail closed on a vault that has no index at all.
	if fi, serr := os.Stat(meshDir); serr == nil && fi.IsDir() {
		_ = ensureVaultMeshDir(meshDir)
	}

	writeDB, err := sql.Open("sqlite", dsnWrite(dbPath))
	if err != nil {
		return nil, err
	}
	writeDB.SetMaxOpenConns(1) // the single write connection

	readDB, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		writeDB.Close()
		return nil, err
	}

	if err := ensureSchemaCheckedContext(ctx, writeDB, allowSchemaRebuild, dbExisted); err != nil {
		writeDB.Close()
		readDB.Close()
		if isUnreadableDB(err) {
			return nil, corruptIndexError(vaultRoot, dbPath, err)
		}
		if errors.Is(err, ErrSchemaMismatch) && !errors.Is(err, ErrSchemaTooNew) {
			err = fmt.Errorf("%w\n  fix: mesh index %s", err, shellpath.Quote(vaultRoot))
		}
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	s := &Store{
		dir:          meshDir,
		dbPath:       dbPath,
		writeDB:      writeDB,
		readDB:       readDB,
		jobs:         make(chan job),
		done:         make(chan struct{}),
		opsDrainGate: newOpsDrainGate(),
	}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

// ErrReadOnly is returned by every write path on a Store opened with OpenReadOnly (Write
// itself, and anything built on it: RecordWriteback, IncrMetric, ComputeHealth, ...). It
// is a sentinel, not a message match, so a caller can branch on it (errors.Is) instead of
// getting the generic "database is locked" a real contended write would produce; the two
// must never look alike; see TestReadOnlyWriteFailsLoudly.
var ErrReadOnly = errors.New("mesh: this index store is read-only; write-back routes through the single owning writer (mesh watch / mesh sync --watch)")

// ErrNoIndexYet is returned by OpenReadOnly when the vault has no index at all. A
// sentinel because it is the one startup failure a caller can give better advice about
// than this package can: what to run instead depends on which surface you are.
var ErrNoIndexYet = errors.New("no index yet")

// OpenReadOnly opens <vaultRoot>/.mesh/mesh.db for READS ONLY: no writer goroutine, no
// write connection, Write always fails with ErrReadOnly. This is what every per-window
// `mesh mcp` server must use (see the mesh multi-writer decision note): only the single
// owning writer may hold a writable Store against a given mesh.db, so N concurrent MCP
// servers never again fight each other (or the owner) for the SQLite write lock.
//
// Unlike Open, this does NOT create the database: a read-only store has nothing to apply
// a schema with, so a vault with no index yet (or one whose owning writer has never run)
// fails closed with a clear message instead of silently producing an empty graph.
func OpenReadOnly(vaultRoot string) (*Store, error) {
	return OpenReadOnlyAt(vaultRoot, filepath.Join(vaultRoot, ".mesh"))
}

// OpenReadOnlyAt is OpenReadOnly with an explicit index directory, mirroring OpenAt.
func OpenReadOnlyAt(vaultRoot, meshDir string) (*Store, error) {
	dbPath := filepath.Join(meshDir, "mesh.db")
	// A read-only open does not CREATE the index, but SQLite still creates -wal and -shm
	// beside it at 0666&^umask, and those persist after close because a mode=ro
	// connection cannot checkpoint them away. Without this, a user whose only surfaces
	// are read-only (mesh ui without --own-index, mesh doctor, the TUI) keeps a 0755
	// .mesh and a 0644 mesh.db forever and mints two fresh 0644 files on every open.
	defer narrowIndexFiles(dbPath)
	// The DIRECTORY half of the same claim, which the first version of this fix left out
	// while its comment promised it. Repair only, never create: a read-only open must
	// still fail closed on a vault that has no index at all, so this runs on an existing
	// directory and is a no-op otherwise. Every surface that only ever opens read-only
	// (mesh ui without --own-index, the TUI, mesh doctor, an MCP window that lost the
	// owner-lock race) reaches this and nothing else, so without it .mesh stays 0755
	// forever and the credentials, ingest config and extraction log inside it are
	// protected by their own modes alone.
	if fi, serr := os.Stat(meshDir); serr == nil && fi.IsDir() {
		_ = ensureVaultMeshDir(meshDir)
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w at %s: start the owning writer first (`mesh watch` or `mesh sync --watch`), or run `mesh index` once", ErrNoIndexYet, dbPath)
		}
		return nil, err
	}
	readDB, err := sql.Open("sqlite", dsnReadOnly(dbPath))
	if err != nil {
		return nil, err
	}
	// Probe now, not on the first caller query: a corrupt or half-written file should
	// fail at Open like the writable path does, not surface as a confusing query error
	// deep inside the first mesh_search.
	if err := readDB.Ping(); err != nil {
		readDB.Close()
		if isUnreadableDB(err) {
			return nil, corruptIndexError(vaultRoot, dbPath, err)
		}
		return nil, fmt.Errorf("open read-only index: %w", err)
	}
	// Then check the SHAPE, not just that the file opens. Only the writable Open compares
	// schema_version (in ensureSchema) and rebuilds on a mismatch, and the shipped setup
	// had no writable process at all: `mesh mcp`, `mesh doctor`, `mesh ui` and the TUI
	// were all read-only. (`mesh mcp` now elects itself the owning writer and opens
	// writable, so it is the one surface that DOES rebuild a mismatched index; the three
	// below still land here.) So an upgraded user's index was never migrated and every
	// read-only surface queried a schema its binary does not match. That did not fail
	// loudly, it
	// failed quietly: a v5 index has no dropped_notes table, and mesh_health swallowed the
	// "no such table" and reported a CLEAN vault while quarantined notes stayed invisible.
	// An index one version older still has no note_health, and mesh_health answered a bare
	// "internal error" with the cause on stderr, which the agent client hides.
	if err := checkReadOnlySchemaVersion(readDB, vaultRoot, dbPath); err != nil {
		readDB.Close()
		return nil, err
	}
	s := &Store{
		dir:          meshDir,
		dbPath:       dbPath,
		readDB:       readDB,
		readOnly:     true,
		done:         make(chan struct{}),
		opsDrainGate: newOpsDrainGate(),
	}
	// Usage counters still have to reach the index. They are the flywheel measurement
	// (queries, fetches, note reuse), and after the single-writer split the only
	// processes that generate them are read-only: every `mesh mcp` window and the web
	// viewer. Dropping them here would freeze the dashboard's reuse rate at whatever the
	// last writable process left behind, while the app kept reporting it as current.
	s.wg.Add(1)
	go s.forwardTelemetry()
	return s, nil
}

// dropOnVersionChange lists the tables wiped and rebuilt on a schema-version change.
// It must be every table in schema.sql EXCEPT those in schemaKeep. A test asserts it
// stays in sync with schema.sql so a newly-added table cannot silently leak stale
// rows (or an orphaned renamed table) on a version bump.
var dropOnVersionChange = []string{"notes", "nodes", "edges", "search_index", "corpus_stats", "meta", "code_files", "code_symbols", "code_edges", "code_search", "note_health", "note_code_links", "dropped_notes"}

// schemaKeep are tables deliberately preserved across a schema-version rebuild:
//   - metrics: accumulated usage counters, NOT re-derivable from the vault.
//   - vectors: BYOAI embeddings + the content-hash embed cache. These ARE derivable
//     but only by RE-EMBEDDING every chunk (a paid API call), and reindex does not
//     re-embed; they stay keyed by the same note ids and the note_hash staleness
//     check excludes any whose content changed. So a notes-shape bump must not wipe
//     them. (If the vectors table's OWN shape ever changes, drop it for that release.)
//   - note_reuse: the flywheel measurement (authoring time + observed reuse events),
//     accumulated at runtime and NOT re-derivable from the vault.
//   - pending_notes: auto-extracted write-back candidates awaiting review, not yet in
//     the vault, so they would be lost on a rebuild if dropped.
var schemaKeep = map[string]bool{"metrics": true, "vectors": true, "note_reuse": true, "pending_notes": true}

// keepShapeVersion tracks the COLUMN SHAPE of the schemaKeep tables. ensureSchema runs
// schema.sql with CREATE TABLE IF NOT EXISTS, which is a no-op on an existing table, so
// adding a column to a kept table in schema.sql would NOT apply on a live hub DB and the
// change would silently do nothing. TestKeptTableShapeGuard fingerprints each kept
// table's DDL against a baked-in value; if you change a kept table's columns you must
// bump this version (which makes that release drop+rebuild the kept tables, accepting
// the one-time data loss) or otherwise migrate the live rows, and update the guard.
// ensureSchema stores it under meta.keep_shape_version and compares on every open, so
// the escape hatch is real: bumping it DOES drop and rebuild the kept tables. It used to
// be declared and never read, which made the documented remedy a no-op and would have
// left a newly-added column failing at INSERT time on every deployed hub.
const keepShapeVersion = 2

// metaKeptWithVectors are meta keys that DESCRIBE a schemaKeep table and therefore have
// to survive the schema-version rebuild alongside it. `vectors` is kept so a version bump
// never forces a paid re-embed, but its canonical model/dim live in `meta`, which IS
// dropped. Losing them left every kept embedding unreadable: LoadVectors, VectorStats and
// CachedVectors all filter on meta.vector_model, so the rows survived but read back as
// model="" (semantic search silently degraded to lexical+graph) and the content-hash embed
// cache missed on every chunk, forcing exactly the full paid re-embed schemaKeep exists to
// prevent. schema_version itself is deliberately NOT carried: it is rewritten below.
var metaKeptWithVectors = []string{"vector_model", "vector_dim"}

// ensureSchema applies the schema, dropping and rebuilding if the stored version
// differs. No data is lost that matters: everything is re-derivable from the
// markdown vault, so this replaces a migration tool.
// isBusy reports whether err is SQLite's "database is locked" (SQLITE_BUSY). Matched on
// the message rather than a driver error type so this stays correct if the driver is
// swapped; the string is SQLite's own and has been stable for years.
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "database is locked") || strings.Contains(m, "SQLITE_BUSY")
}

// ErrIndexCorrupt marks a .mesh/mesh.db that SQLite refuses to read at all: a truncated,
// half-written or overwritten file (SQLITE_NOTADB, result code 26). It is the OTHER
// operator-facing SQLite failure next to SQLITE_BUSY, and unlike a busy lock it never
// clears by waiting. It is a sentinel so callers match with errors.Is instead of reading
// the message, because exactly one caller is allowed to act on it destructively
// (recoverCorruptIndex) and matching a string there would be far too loose.
var ErrIndexCorrupt = errors.New("index database is corrupt")

// isUnreadableDB reports whether err is SQLite refusing the file itself, in EITHER of the
// two ways it can. Matched on the message rather than a driver error type for the same
// reason isBusy is: so this survives swapping the driver. The strings are SQLite's own.
//
//   - SQLITE_NOTADB (26), "file is not a database": the 16-byte header does not match, so
//     the file is not SQLite at all. That is what junk written over the whole file gives.
//   - SQLITE_CORRUPT (11), "database disk image is malformed": the header is fine and a
//     page is not. This is the ORDINARY damage (a crash mid-write, a bad sector, a copy
//     taken while a write was in flight) and covering only code 26 left it with the exact
//     dead end this whole path exists to remove.
//
// Both mean the same thing to Mesh: the index cannot be opened and cannot be repaired in
// place, and since it is derived from the markdown the only answer is to rebuild it.
func isUnreadableDB(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "file is not a database") || strings.Contains(m, "SQLITE_NOTADB") ||
		strings.Contains(m, "database disk image is malformed") || strings.Contains(m, "SQLITE_CORRUPT")
}

// ErrSchemaMismatch marks an index whose schema_version is not this binary's
// SchemaVersion: written by an older (or newer) Mesh and never rebuilt, because only the
// writable Open migrates and a read-only surface cannot. A sentinel so callers can give
// their own surface-specific advice (`mesh doctor` prints a status line, the viewer says
// it does not build indexes) instead of pattern-matching the message.
var ErrSchemaMismatch = errors.New("index schema mismatch")

// ErrSchemaTooNew marks an index written by a newer Mesh binary. Older code must leave
// it untouched because it cannot know which non-derivable state the newer schema keeps.
var ErrSchemaTooNew = errors.New("index schema is newer than this Mesh binary")

// schemaVersionInIndex reads meta.schema_version from a read-only connection. A missing
// meta table or missing row answers 0: that is a database this Mesh never stamped, which
// is a mismatch, not an internal error.
func schemaVersionInIndex(db *sql.DB) (int, error) {
	return versionInIndex(db, "schema_version")
}

func keepShapeVersionInIndex(db *sql.DB) (int, error) {
	return versionInIndex(db, "keep_shape_version")
}

// Parse stamps strictly in Go: SQLite considers CAST('6garbage' AS INTEGER) equal to 6.
func versionInIndex(db *sql.DB, key string) (int, error) {
	var raw string
	err := db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&raw)
	switch {
	case err == nil:
		v, parseErr := strconv.Atoi(strings.TrimSpace(raw))
		if parseErr != nil || v < 0 {
			return -1, nil
		}
		return v, nil
	case errors.Is(err, sql.ErrNoRows), strings.Contains(err.Error(), "no such table"):
		return 0, nil
	case strings.Contains(err.Error(), "no such column"):
		return -1, nil
	default:
		return 0, err
	}
}

// checkReadOnlySchemaVersion is the read-only twin of the version comparison ensureSchema
// makes on the writable path. It cannot migrate (a read-only store has no write
// connection), so it fails closed with the command that CAN: `mesh index`. Serving reads
// off a mismatched schema is what produced a false clean bill of health, and answering a
// bare "internal error" from a missing table is what hid the cause.
func checkReadOnlySchemaVersion(db *sql.DB, vaultRoot, dbPath string) error {
	found, err := schemaVersionInIndex(db)
	if err != nil {
		return fmt.Errorf("read index schema version: %w", err)
	}
	foundKeep, err := keepShapeVersionInIndex(db)
	if err != nil {
		return fmt.Errorf("read index kept-table shape version: %w", err)
	}
	abs, aerr := filepath.Abs(dbPath)
	if aerr != nil {
		abs = dbPath
	}
	root, rerr := filepath.Abs(vaultRoot)
	if rerr != nil {
		root = vaultRoot
	}
	if found > SchemaVersion || foundKeep > keepShapeVersion {
		return fmt.Errorf("%w: %w: %s was written by a newer version of Mesh (index schema v%d / kept-table shape v%d; this binary supports v%d / v%d)\n"+
			"  this binary refuses to downgrade that index because it may contain non-derivable state it does not understand\n"+
			"  your notes are safe; leave the index untouched\n"+
			"  fix: upgrade Mesh to the version that wrote this index (or a newer one); do not run `mesh index` with this older binary",
			ErrSchemaMismatch, ErrSchemaTooNew, abs, found, foundKeep, SchemaVersion, keepShapeVersion)
	}
	if found == SchemaVersion && (foundKeep == 0 || foundKeep == keepShapeVersion) {
		if probeErr := validateSchemaProbes(context.Background(), db, allSchemaProbes()); probeErr == nil {
			return nil
		} else if isUnreadableDB(probeErr) {
			return corruptIndexError(vaultRoot, dbPath, probeErr)
		} else if !isSchemaShapeError(probeErr) {
			return fmt.Errorf("validate current index schema: %w", probeErr)
		} else {
			return fmt.Errorf("%w: %s is stamped with the current schema but its table shape does not match: %v\n"+
				"  refusing to serve partial or mis-shaped tables\n"+
				"  fix: mesh index %s   (rebuilds the derived index from your safe Markdown notes)",
				ErrSchemaMismatch, abs, probeErr, shellpath.Quote(root))
		}
	}
	stamped := fmt.Sprintf("v%d", found)
	if found == 0 {
		stamped = "unstamped"
	} else if found < 0 {
		stamped = "invalid"
	}
	return fmt.Errorf("%w: %s was written by a different version of Mesh (index schema %s, this binary expects v%d)\n"+
		"  read-only surfaces cannot migrate it, so this one refuses to answer from a schema it does not match\n"+
		"  your notes are safe: the index is derived from the markdown, so rebuilding it loses nothing\n"+
		"  fix: mesh index %s   (rebuilds the index for this version)\n"+
		"  stored embeddings survive the rebuild; re-run mesh embed only if you changed embedding model",
		ErrSchemaMismatch, abs, stamped, SchemaVersion, shellpath.Quote(root))
}

// indexFiles are the three files one SQLite index occupies: the database plus the WAL and
// shared-memory sidecars. A corrupt database has to take its sidecars with it, or the
// rebuilt database inherits frames written against the old, unreadable one.
func indexFiles(dbPath string) []string {
	return []string{dbPath, dbPath + "-wal", dbPath + "-shm"}
}

// corruptIndexError turns result code 26 into something an operator can act on. The bare
// driver message ("apply schema: file is not a database (26)") is emitted identically by
// search, doctor, health AND index, which is a dead end: `mesh index` is the command that
// would rebuild, so the operator is told to fix it by the one thing that cannot run. The
// message names the path and guarded repair without pretending that the entire
// database is derived: pending reviews and accumulated usage have no Markdown copy.
func corruptIndexError(vaultRoot, dbPath string, cause error) error {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		abs = dbPath
	}
	root, err := filepath.Abs(vaultRoot)
	if err != nil {
		root = vaultRoot
	}
	return fmt.Errorf("%w: %s is not a readable SQLite database (truncated, overwritten, damaged mid-write, or not SQLite at all)\n"+
		"  Markdown notes are unchanged; pending review notes, usage/reuse history and stored embeddings are database-only state\n"+
		"  before repair: stop vault services and preserve a consistent backup of the vault and database files\n"+
		"  prefer a verified database restore; rebuilding cannot recover database-only state from Markdown\n"+
		"  repair:  mesh index %s   (discards the corrupt file, then rebuilds)\n"+
		"  stored embeddings go with it, so re-run mesh embed if you use semantic search\n"+
		"  cause:   %w",
		ErrIndexCorrupt, abs, shellpath.Quote(root), cause)
}

// recoverCorruptIndex deletes an unreadable index so it can be rebuilt, and does so ONLY
// for ErrIndexCorrupt. Every other open failure returns untouched: a busy lock, a
// permission problem or a disk error all leave a database that is still perfectly good,
// and deleting on those would turn a wait-and-retry into permanent loss of the embed cache
// (a paid re-embed). The narrow condition is the whole point of the function, which is why
// it is separate from OpenRebuild and tested on its own.
func recoverCorruptIndex(dbPath string, openErr error) (bool, error) {
	if !errors.Is(openErr, ErrIndexCorrupt) {
		return false, openErr
	}
	for _, p := range indexFiles(dbPath) {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return false, errors.Join(openErr, err)
		}
	}
	return true, nil
}

// OpenRebuild is Open for the one command whose job is to REBUILD the index. `mesh index`
// reads the markdown and writes the database from scratch, so a database it cannot even
// open is not an obstacle, it is the thing being replaced. Anything else (the hub, the MCP
// server, search, doctor) must keep using Open and report the error instead.
//
// The bool reports whether a corrupt index was discarded, so the caller can say so out
// loud: silently deleting a file on the operator's disk is not acceptable even when the
// file is garbage.
func OpenRebuild(vaultRoot string) (*Store, bool, error) {
	meshDir := filepath.Join(vaultRoot, ".mesh")
	if err := ensureVaultMeshDir(meshDir); err != nil {
		return nil, false, err
	}
	release, err := acquireUnownedLease(meshDir, true)
	if err != nil {
		return nil, false, err
	}
	defer release()
	s, recovered, err := openRebuild(vaultRoot)
	if err != nil {
		return nil, false, err
	}
	attachUnownedLease(s)
	return s, recovered, nil
}

func openRebuild(vaultRoot string) (*Store, bool, error) {
	s, err := openAt(vaultRoot, filepath.Join(vaultRoot, ".mesh"), true)
	if err == nil {
		if integrityErr := s.CheckIntegrity(vaultRoot); integrityErr == nil {
			return s, false, nil
		} else {
			err = integrityErr
			if closeErr := s.Close(); closeErr != nil {
				return nil, false, errors.Join(err, closeErr)
			}
		}
	}
	dbPath := filepath.Join(vaultRoot, ".mesh", "mesh.db")
	recovered, rerr := recoverCorruptIndex(dbPath, err)
	if !recovered {
		return nil, false, rerr
	}
	s, err = openAt(vaultRoot, filepath.Join(vaultRoot, ".mesh"), true)
	if err != nil {
		return nil, false, err
	}
	return s, true, nil
}

// OpenRebuildOwned is OpenRebuild under the same lifetime ownership lease as
// OpenOwned. Corruption recovery can close, remove and recreate mesh.db, so the lease
// deliberately spans that entire sequence, not merely the final Store transaction.
func OpenRebuildOwned(vaultRoot string, owner *OwnerLock) (*Store, bool, error) {
	release, ok := owner.acquireLease()
	if !ok {
		return nil, false, ErrReadOnly
	}
	defer release()
	store, recovered, err := openRebuild(vaultRoot)
	if err != nil {
		return nil, false, err
	}
	attachOwnerLease(store, owner)
	return store, recovered, nil
}

// CheckIntegrity scans the whole index before a diagnostic or recovery surface claims
// it is healthy. Only genuine SQLite corruption is wrapped with ErrIndexCorrupt; busy
// and I/O failures never authorize deletion.
func (s *Store) CheckIntegrity(vaultRoot string) error {
	// Do not borrow a connection from readDB here. With the bundled SQLite driver,
	// repeated FTS5 integrity checks on a reused connection can report a checksum
	// mismatch after another connection commits perfectly valid FTS updates. A fresh
	// read-only connection verifies the actual database without recycling readers,
	// taking ownership, changing the schema, or rebuilding any data.
	db, err := sql.Open("sqlite", dsnReadOnly(s.dbPath))
	if err != nil {
		return fmt.Errorf("open index integrity reader: %w", err)
	}
	defer db.Close()
	var result string
	err = db.QueryRow(`PRAGMA quick_check(1)`).Scan(&result)
	if err != nil {
		if isUnreadableDB(err) {
			return corruptIndexError(vaultRoot, s.dbPath, err)
		}
		return fmt.Errorf("check index integrity: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(result), "ok") {
		return nil
	}
	return corruptIndexError(vaultRoot, s.dbPath, fmt.Errorf("PRAGMA quick_check: %s", result))
}

// ensureSchemaChecked is ensureSchema with an error a human can act on.
//
// WHAT IS MEASURED, and what is not. The SQLITE_BUSY that operators hit on `mesh index`
// got three confident explanations here before the real one, and all three were wrong, so
// this records evidence rather than a story.
//
// RESOLVED: leaked read snapshots pinned the WAL. A `for rows.Next()` loop only closes its
// result set when it runs to exhaustion, so every early return inside the loop leaked an
// open *sql.Rows, and in WAL mode an open Rows holds a READ SNAPSHOT that no checkpoint can
// reclaim past. Seven sites in this package did that, DriftReport among them, which both
// watch daemons call on every reconcile. So the leaks accumulated for as long as the daemon
// lived, the WAL grew past journal_size_limit, and writers starved. Fixed by `defer
// rows.Close()` at all seven; TestWalPinnedByLeakedRows pins the mechanism and
// TestEveryQueryClosesItsRows stops it recurring. Note the shape of this bug: it was in the
// READ paths, and it only hurt a THIRD process, hours later. Symptom and cause shared
// neither a code path nor a process.
//
// Measured on the live vault, and still worth keeping because it rules out the wrong fixes:
//   - a plain `mesh index` against a held lock fails after 31s, i.e. exactly the DSN's
//     busy_timeout(30000). The busy handler runs; it loses. (So it was NOT "the timeout
//     is never applied", explanation one.)
//   - a full reindex holds the write lock for about 3 seconds, not minutes: the note
//     transaction is ~377ms for 894 notes and the code transaction ~2.6s for 3056 files
//     / 235k edges. (So it was NOT "one long transaction blocks everyone", explanation
//     two, and chunking that transaction would not have helped.)
//   - `Exec(SchemaSQL)` succeeds against a held write lock, so the schema check is not the
//     thing that blocks. (Explanation three.)
//   - the symptom tracked daemon UPTIME, not load: a 32-hour `mesh sync --watch` won 0 of
//     276 lock attempts in 45s with a 24MB WAL and failed `mesh index` 8 of 8, while a
//     freshly restarted one showed 0% busy and a 0MB WAL. That is what pointed at an
//     unbounded accumulation rather than a race.
//
// If this ever comes back, measure before theorising. Do not add a retry loop (tried,
// measured, it turned a 31s failure into a 247s hang because every attempt inherits the
// same timeout) and do not chunk the reindex (more transactions make starvation worse).
//
// The message below stays regardless: the error said "database is locked" and left the
// operator guessing, so it names the processes that plausibly hold it and what to do.
func ensureSchemaChecked(db *sql.DB, allowRebuild, dbExisted bool) error {
	return ensureSchemaCheckedContext(context.Background(), db, allowRebuild, dbExisted)
}

func ensureSchemaCheckedContext(ctx context.Context, db *sql.DB, allowRebuild, dbExisted bool) error {
	err := ensureSchemaContext(ctx, db, allowRebuild, dbExisted)
	if !isBusy(err) {
		return err
	}
	return fmt.Errorf("another mesh process holds the write lock (most likely a `mesh sync --watch` "+
		"or `mesh mcp --watch` daemon mid-write; a reindex itself only holds it for a few seconds). "+
		"Wait a moment and retry, or stop the daemon if it persists: %w", err)
}

type schemaConnection interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type schemaProbe struct {
	table string
	query string
}

// A matching version stamp is only a claim. These zero-row queries prove that every
// table and column this binary reads is actually present, catching interrupted/manual
// schema edits without reading user rows.
var derivedSchemaProbes = []schemaProbe{
	{"notes", `SELECT id,path,type,title,retrieval_hash,frontmatter,summary_short,summary_card,mtime,updated,contributor,review_by,source,scope,local_version,hub_version FROM notes LIMIT 0`},
	{"nodes", `SELECT id,kind,label,note_id,note_path,anchor,source_loc,community,attrs FROM nodes LIMIT 0`},
	{"edges", `SELECT source,target,relation,confidence,confidence_score,weight,source_loc FROM edges LIMIT 0`},
	{"search_index", `SELECT node_id,kind,anchor,title,body FROM search_index LIMIT 0`},
	{"corpus_stats", `SELECT key,value FROM corpus_stats LIMIT 0`},
	{"meta", `SELECT key,value FROM meta LIMIT 0`},
	{"code_files", `SELECT path,lang,package,mtime,retrieval_hash FROM code_files LIMIT 0`},
	{"code_symbols", `SELECT id,path,lang,name,kind,start_line,end_line,signature,doc,calls FROM code_symbols LIMIT 0`},
	{"code_edges", `SELECT src_id,dst_id,relation FROM code_edges LIMIT 0`},
	{"code_search", `SELECT symbol_id,lang,kind,path,start_line,name,signature,doc FROM code_search LIMIT 0`},
	{"note_health", `SELECT id,note_id,path,issue,detail,detected_at FROM note_health LIMIT 0`},
	{"dropped_notes", `SELECT path,err,duplicate,detected_at FROM dropped_notes LIMIT 0`},
	{"note_code_links", `SELECT note_id,symbol_id,name FROM note_code_links LIMIT 0`},
}

var keptSchemaProbes = []schemaProbe{
	{"vectors", `SELECT node_id,chunk_ix,model,dim,embedding,content_hash,note_hash FROM vectors LIMIT 0`},
	{"metrics", `SELECT key,value FROM metrics LIMIT 0`},
	{"note_reuse", `SELECT note_id,authored_at,source,reuse_count,first_reuse,last_reuse FROM note_reuse LIMIT 0`},
	{"pending_notes", `SELECT id,type,title,do_text,dont_text,why,confidence,source,created_at,authoring_json FROM pending_notes LIMIT 0`},
}

func allSchemaProbes() []schemaProbe {
	probes := make([]schemaProbe, 0, len(derivedSchemaProbes)+len(keptSchemaProbes))
	probes = append(probes, derivedSchemaProbes...)
	return append(probes, keptSchemaProbes...)
}

func validateSchemaProbes(ctx context.Context, db schemaConnection, probes []schemaProbe) error {
	for _, probe := range probes {
		if err := validateSchemaProbe(ctx, db, probe); err != nil {
			return fmt.Errorf("table %s: %w", probe.table, err)
		}
	}
	return nil
}

var errSchemaObjectType = errors.New("schema object is not a table")

func validateSchemaProbe(ctx context.Context, db schemaConnection, probe schemaProbe) (err error) {
	// A zero-row SELECT alone is insufficient: a view can expose the same columns and
	// pass every read probe, then fail the first INSERT. Require the actual sqlite_schema
	// object to be a table (virtual tables also report type=table).
	var objectType string
	if err := db.QueryRowContext(ctx, `SELECT type FROM sqlite_schema WHERE name=?`, probe.table).Scan(&objectType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s is missing", errSchemaObjectType, probe.table)
		}
		return err
	}
	if objectType != "table" {
		return fmt.Errorf("%w: %s is a %s", errSchemaObjectType, probe.table, objectType)
	}
	rows, err := db.QueryContext(ctx, probe.query)
	if err != nil {
		return err
	}
	defer rows.Close()
	return rows.Err()
}

func isSchemaShapeError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return errors.Is(err, errSchemaObjectType) || strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column")
}

// invalidKeptTables returns only kept tables whose declared columns are missing. An
// unrelated SQLite failure is returned, never converted into permission to drop data.
func invalidKeptTables(ctx context.Context, db schemaConnection) ([]string, error) {
	var invalid []string
	for _, probe := range keptSchemaProbes {
		err := validateSchemaProbes(ctx, db, []schemaProbe{probe})
		if err == nil {
			continue
		}
		if !isSchemaShapeError(err) {
			return nil, err
		}
		invalid = append(invalid, probe.table)
	}
	return invalid, nil
}

// ensureSchema holds one IMMEDIATE transaction from the version read through the final
// stamp. DDL used to autocommit one statement at a time, so a late failure could leave
// half the index dropped. BEGIN IMMEDIATE also prevents two openers from both deciding
// to migrate from the same stale version.
func ensureSchema(db *sql.DB, allowRebuild, dbExisted bool) error {
	return ensureSchemaContext(context.Background(), db, allowRebuild, dbExisted)
}

func ensureSchemaContext(ctx context.Context, db *sql.DB, allowRebuild, dbExisted bool) error {
	return ensureSchemaSQLContext(ctx, db, allowRebuild, dbExisted, SchemaSQL)
}

// ensureSchemaSQL is split out so the atomicity test can append one deliberately
// failing final statement and prove every earlier DDL statement is rolled back.
func ensureSchemaSQL(db *sql.DB, allowRebuild, dbExisted bool, schemaSQL string) (err error) {
	return ensureSchemaSQLContext(context.Background(), db, allowRebuild, dbExisted, schemaSQL)
}

func ensureSchemaSQLContext(ctx context.Context, db *sql.DB, allowRebuild, dbExisted bool, schemaSQL string) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// A long SQLite busy handler ignores context cancellation in modernc SQLite. Override
	// it on this dedicated connection while schema work runs, then restore whatever the
	// pool configured before returning the connection. openAt's writer already uses the
	// short value; standalone schema tests and legacy callers can still use a 30s DSN.
	var originalBusyMS int
	if err = conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&originalBusyMS); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", writeBusyPollMS)); err != nil {
		return err
	}
	defer func() {
		_, resetErr := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout=%d", originalBusyMS))
		if resetErr != nil {
			err = errors.Join(err, fmt.Errorf("restore schema busy timeout: %w", resetErr))
		}
	}()

	busyDeadline := time.Now().Add(time.Duration(busyTimeoutMS) * time.Millisecond)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = ensureSchemaAttempt(ctx, conn, allowRebuild, dbExisted, schemaSQL)
		if err == nil || !isBusy(err) {
			return err
		}
		if time.Now().After(busyDeadline) {
			return err
		}
	}
}

// ensureSchemaAttempt performs one atomic schema transaction. A busy result is safe to
// retry because every unsuccessful attempt rolls back before returning.
func ensureSchemaAttempt(ctx context.Context, conn *sql.Conn, allowRebuild, dbExisted bool, schemaSQL string) (err error) {
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err = ensureSchemaOn(ctx, conn, allowRebuild, dbExisted, schemaSQL); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func ensureSchemaOn(ctx context.Context, db schemaConnection, allowRebuild, dbExisted bool, schemaSQL string) error {
	// Close the tiny stat/open race: if another process created the schema after our
	// os.Stat, sqlite_schema still makes this an existing database for safety decisions.
	if !dbExisted {
		var objects int
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%')`).Scan(&objects); err != nil {
			return err
		}
		dbExisted = objects != 0
	}
	current, err := readMetaVersion(ctx, db, "schema_version")
	if err != nil {
		return err
	}
	currentKeep, err := readMetaVersion(ctx, db, "keep_shape_version")
	if err != nil {
		return err
	}
	if current > SchemaVersion || currentKeep > keepShapeVersion {
		return fmt.Errorf("%w: %w: index schema v%d / kept-table shape v%d, but this binary supports v%d / v%d; upgrade Mesh and leave this index untouched",
			ErrSchemaMismatch, ErrSchemaTooNew, current, currentKeep, SchemaVersion, keepShapeVersion)
	}

	// Version 2 extends pending drafts in place. Never rebuild this table: its
	// unreviewed contents have no Markdown copy. Readers must wait for the owner.
	if dbExisted && currentKeep < 2 && allowRebuild {
		if err := migratePendingAuthoring(ctx, db); err != nil {
			return err
		}
	}

	var invalidKept []string
	if dbExisted {
		invalidKept, err = invalidKeptTables(ctx, db)
		if err != nil {
			return err
		}
	}
	schemaMismatch := dbExisted && current != SchemaVersion
	keptStampMismatch := currentKeep != 0 && currentKeep != keepShapeVersion

	if dbExisted && current == SchemaVersion {
		derivedErr := validateSchemaProbes(ctx, db, derivedSchemaProbes)
		if derivedErr != nil && !isSchemaShapeError(derivedErr) {
			return derivedErr
		}
		if !allowRebuild && (derivedErr != nil || len(invalidKept) != 0 || keptStampMismatch) {
			return fmt.Errorf("%w: current version stamp covers a mismatched table shape (derived: %v; kept tables: %v)",
				ErrSchemaMismatch, derivedErr, invalidKept)
		}
		schemaMismatch = derivedErr != nil
		if derivedErr == nil && len(invalidKept) == 0 && currentKeep == keepShapeVersion {
			// The base shape is current. Install the optional reader tracker once;
			// subsequent opens of a complete install perform no DDL or row writes.
			return ensureReaderRevision(ctx, db)
		}
	}
	if !allowRebuild && (schemaMismatch || len(invalidKept) != 0 || keptStampMismatch) {
		return fmt.Errorf("%w: index schema v%d / kept-table shape v%d does not match this binary's v%d / v%d",
			ErrSchemaMismatch, current, currentKeep, SchemaVersion, keepShapeVersion)
	}

	var carried map[string]string
	if allowRebuild && schemaMismatch {
		if carried, err = readMetaKeys(ctx, db, metaKeptWithVectors); err != nil {
			return err
		}
		for _, table := range dropOnVersionChange {
			if err := dropSchemaObject(ctx, db, table); err != nil {
				return err
			}
		}
	}
	for _, table := range invalidKept {
		if err := dropSchemaObject(ctx, db, table); err != nil {
			return err
		}
		if table == "vectors" {
			carried = nil
		}
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	if slicesContain(invalidKept, "vectors") {
		if _, err := db.ExecContext(ctx, `DELETE FROM meta WHERE key IN ('vector_model','vector_dim')`); err != nil {
			return err
		}
	}
	for key, value := range carried {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value,
		); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES('schema_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprint(SchemaVersion),
	); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES('keep_shape_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprint(keepShapeVersion),
	)
	if err != nil {
		return err
	}
	return ensureReaderRevision(ctx, db)
}

// dropSchemaObject removes an expected schema object by the type SQLite says it has.
// This is what lets a rebuild repair a view (or another object) installed under a table
// name instead of failing with "use DROP VIEW to delete view" after dropping other
// derived tables. Names come only from the static schema lists above; quoting remains
// defense in depth.
func dropSchemaObject(ctx context.Context, db schemaConnection, name string) error {
	var objectType string
	err := db.QueryRowContext(ctx, `SELECT type FROM sqlite_schema WHERE name=?`, name).Scan(&objectType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	verb := map[string]string{
		"table":   "TABLE",
		"view":    "VIEW",
		"index":   "INDEX",
		"trigger": "TRIGGER",
	}[objectType]
	if verb == "" {
		return fmt.Errorf("cannot replace schema object %q of unknown type %q", name, objectType)
	}
	quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	_, err = db.ExecContext(ctx, "DROP "+verb+" IF EXISTS "+quoted)
	return err
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func readMetaVersion(ctx context.Context, db schemaConnection, key string) (int, error) {
	var raw string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&raw)
	switch {
	case err == nil:
		v, parseErr := strconv.Atoi(strings.TrimSpace(raw))
		if parseErr != nil || v < 0 {
			return -1, nil
		}
		return v, nil
	case errors.Is(err, sql.ErrNoRows), strings.Contains(err.Error(), "no such table"):
		return 0, nil
	case strings.Contains(err.Error(), "no such column"):
		return -1, nil
	default:
		return 0, err
	}
}

// readMetaKeys reads the given meta keys, tolerating a missing meta table (a brand-new
// database) and absent keys. Returns only the keys that exist.
func readMetaKeys(ctx context.Context, db schemaConnection, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		var v string
		err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, k).Scan(&v)
		switch {
		case err == sql.ErrNoRows:
			continue
		case err != nil:
			// meta does not exist yet: nothing to carry.
			return out, nil
		}
		out[k] = v
	}
	return out, nil
}

// sortedKeys returns a map's keys in a deterministic order, so a DROP loop over a set
// is reproducible (and diffable in logs) rather than map-iteration random.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Store) Path() string { return s.dbPath }

// MeshDir returns the vault's .mesh directory (where mesh.db and the solo
// config.toml live).
func (s *Store) MeshDir() string { return s.dir }

// ReadOnly reports whether this store may write now. Besides stores physically opened
// by OpenReadOnly, it covers a writable MCP store whose preemptible ownership claim was
// taken by a declared owner. Callers must branch on this live state, not on which
// constructor they think ran.
func (s *Store) ReadOnly() bool { return !s.canWrite() }

// SetWriteGuard installs a live authorization check for every future write. It is used
// by an elected MCP owner whose claim is preemptible; ordinary stores leave it nil.
func (s *Store) SetWriteGuard(guard func() bool) {
	s.writeGuardMu.Lock()
	s.writeGuard = guard
	s.writeGuardMu.Unlock()
}

func (s *Store) canWrite() bool {
	release, ok, _ := s.acquireWriteAuthorization()
	if ok {
		release()
	}
	return ok
}

func (s *Store) acquireWriteAuthorization() (release func(), ok, linearized bool) {
	if s.readOnly {
		return func() {}, false, false
	}
	s.writeGuardMu.RLock()
	guard := s.writeGuard
	lease := s.writeLease
	s.writeGuardMu.RUnlock()
	if lease != nil {
		release, ok := lease()
		if !ok {
			return func() {}, false, true
		}
		return release, true, true
	}
	if guard != nil && !guard() {
		return func() {}, false, false
	}
	return func() {}, true, false
}

// NoteDate carries the lifecycle dates retrieval needs for freshness decay.
type NoteDate struct {
	Updated  string // frontmatter updated/when (YYYY-MM-DD)
	ReviewBy string // frontmatter review_by (YYYY-MM-DD or RFC3339), if any
}

// NoteDates returns id -> lifecycle dates for every note, for freshness decay.
func (s *Store) NoteDates() (map[string]NoteDate, error) {
	rows, err := s.readDB.Query(`SELECT id, COALESCE(updated,''), COALESCE(review_by,'') FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]NoteDate{}
	for rows.Next() {
		var id, upd, rev string
		if err := rows.Scan(&id, &upd, &rev); err != nil {
			return nil, err
		}
		out[id] = NoteDate{Updated: upd, ReviewBy: rev}
	}
	return out, rows.Err()
}

func (s *Store) writer() {
	defer s.wg.Done()
	// Periodically checkpoint the WAL from inside the single writer so mesh.db-wal
	// cannot grow without bound even across idle stretches (autocheckpoint only fires
	// on writes). PASSIVE does not wait for readers, but can copy pages and sync the
	// database: its filesystem work can still delay the next job. Running it here never
	// races a write: the select serves one job OR one checkpoint per iteration.
	ticker := time.NewTicker(walCheckpointInterval)
	defer ticker.Stop()
	// Telemetry counters accumulated by the read paths are flushed here, on the writer
	// itself, so they cost one batched transaction per interval instead of three
	// synchronous ones per request. runTx directly (not Write): this IS the writer, so
	// routing through the jobs channel would deadlock.
	telTicker := time.NewTicker(telemetryFlushInterval)
	defer telTicker.Stop()
	for {
		select {
		case <-s.done:
			return
		case j := <-s.jobs:
			j.reply <- s.runTxContext(j.ctx, j.fn)
		case <-telTicker.C:
			if !s.canWrite() {
				s.queueTelemetry()
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), telemetryFlushTimeout)
				fn, ok := s.drainTelemetryContext(ctx)
				if ok {
					if err := s.runTxContext(ctx, fn); err != nil {
						slog.Warn("mesh: telemetry flush failed; this batch of counters is lost", "err", err)
					}
				}
				cancel()
			}
		case <-ticker.C:
			s.checkpointWAL()
		}
	}
}

// checkpointWAL stays on the existing writer with the same authorization and
// escalation policy. A PASSIVE checkpoint avoids reader lock waits, not disk I/O.
func (s *Store) checkpointWAL() {
	trace := latency.Start("index_checkpoint", "authorize")
	defer trace.End()
	release, ok, _ := s.acquireWriteAuthorization()
	if !ok {
		return
	}
	defer func() {
		trace.Phase("release")
		release()
	}()
	trace.Phase("passive")
	_, _ = s.writeDB.Exec("PRAGMA wal_checkpoint(PASSIVE)")
	// PASSIVE alone does not bound the file: journal_size_limit applies only
	// after a reset. Preserve the existing bounded TRUNCATE escalation.
	trace.Phase("wal_size")
	if s.walBytes() > walSizeLimit {
		trace.Phase("truncate")
		s.checkpointTruncateBestEffortAuthorized()
	}
}

func (s *Store) runTx(fn func(*sql.Tx) error) error {
	return s.runTxContext(context.Background(), fn)
}

// runTxContext is runTx with a caller-owned lifetime. BeginTx ties the transaction
// to ctx, so cancellation rolls it back even after the writer goroutine accepted the
// job. The callback should still use the context-aware SQL methods for prompt
// interruption of a statement that is already running.
func (s *Store) runTxContext(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	trace := latency.Start("index_transaction", "authorize")
	defer trace.End()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	release, ok, linearized := s.acquireWriteAuthorization()
	if !ok {
		return ErrReadOnly
	}
	defer func() {
		trace.Phase("release")
		release()
	}()
	trace.Phase("begin")
	tx, err := s.beginWriteTxContext(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// A panic in fn (a nil deref on malformed parsed data, a panic inside Exec)
		// must never kill the single writer goroutine: a dead writer would leave every
		// future Write blocked forever on the jobs channel. Recover it into an error so
		// the writer keeps serving and the caller is told what happened. This mirrors
		// the hub Store, which already guards this.
		if r := recover(); r != nil {
			trace.Phase("rollback")
			_ = tx.Rollback()
			err = fmt.Errorf("index write panicked: %v", r)
			return
		}
		if err != nil {
			trace.Phase("rollback")
			_ = tx.Rollback()
		}
	}()
	trace.Phase("callback")
	if err = fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A preemptible claim may have been taken while fn was running (a full reindex can
	// take seconds). Roll the work back instead of committing beside the new owner.
	trace.Phase("revalidate")
	if !linearized && !s.canWrite() {
		return ErrReadOnly
	}
	trace.Phase("commit")
	return tx.Commit()
}

// beginWriteTxContext acquires SQLite's write reservation in short, cancelable slices.
// writeDB uses _txlock=immediate, so BeginTx either owns the write lock or returns busy
// before fn runs. Retrying is therefore side-effect free. Background wrappers retain
// the historical aggregate patience; request contexts can stop between slices.
func (s *Store) beginWriteTxContext(ctx context.Context) (*sql.Tx, error) {
	deadline := time.Now().Add(time.Duration(busyTimeoutMS) * time.Millisecond)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tx, err := s.writeDB.BeginTx(ctx, nil)
		if err == nil {
			return tx, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if !isBusy(err) || !time.Now().Before(deadline) {
			return nil, err
		}
	}
}

// Write runs fn inside a transaction on the single writer goroutine. On a Store opened
// with OpenReadOnly this fails loudly with ErrReadOnly instead of blocking: s.jobs is
// nil on a read-only store (there is no writer goroutine to receive from it), and
// sending on a nil channel blocks forever, so this check must run BEFORE the select,
// not be left to fall out of it.
func (s *Store) Write(fn func(*sql.Tx) error) error {
	return s.WriteContext(context.Background(), fn)
}

// WriteContext runs fn on the single writer with a caller-owned lifetime. It can be
// canceled while waiting to hand the job to the writer. Once the writer accepts a job,
// this method joins its callback and returns the transaction's actual result. The
// context is bound to BeginTx, so cancellation rolls the transaction back promptly;
// joining is load-bearing because callers commonly capture result counters in fn, and
// returning while fn can still mutate them would be a data race.
func (s *Store) WriteContext(ctx context.Context, fn func(*sql.Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A physically read-only store has no writer goroutine and a nil jobs channel, so it
	// must fail before the select. A writable-but-preempted store still has a writer;
	// let runTxContext perform the live ownership check there. Calling canWrite here
	// would wait on the ownership lease held by an in-flight transaction and make ctx
	// unable to cancel a queued write.
	if s.readOnly {
		return ErrReadOnly
	}
	trace := latency.Start("index_write", "queue")
	defer trace.End()
	reply := make(chan error, 1)
	select {
	case s.jobs <- job{ctx: ctx, fn: fn, reply: reply}:
		trace.Phase("transaction")
		return <-reply
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return fmt.Errorf("store is closed")
	}
}

// recordTelemetry accumulates a pending counter increment and/or a reuse event without
// touching the database. It is the non-blocking half of the telemetry path: callers on
// a read path (mesh_search, mesh_fetch, the web search API) must never wait on the
// single writer just to bump a counter.
func (s *Store) recordTelemetry(key string, n int64, reuse *reuseEvent) {
	s.telMu.Lock()
	defer s.telMu.Unlock()
	if key != "" {
		if s.telCount == nil {
			s.telCount = map[string]int64{}
		}
		s.telCount[key] += n
	}
	if reuse != nil {
		s.telReuse = append(s.telReuse, *reuse)
	}
}

// drainTelemetry takes everything pending and returns the transaction that applies it,
// or ok=false when there is nothing to write. The pending state is cleared under the
// lock before the transaction runs, so a concurrent request never blocks on the flush;
// the trade is that a failed flush loses that batch, which is acceptable for counters
// that are best-effort at every call site anyway.
func (s *Store) drainTelemetry() (func(*sql.Tx) error, bool) {
	return s.drainTelemetryContext(context.Background())
}

func (s *Store) drainTelemetryContext(ctx context.Context) (func(*sql.Tx) error, bool) {
	s.telMu.Lock()
	counts, reuses := s.telCount, s.telReuse
	s.telCount, s.telReuse = nil, nil
	s.telMu.Unlock()
	if len(counts) == 0 && len(reuses) == 0 {
		return nil, false
	}
	// Deterministic key order so the batched transaction touches rows in a stable
	// sequence (reproducible, and no lock-order surprises against another writer).
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return telemetryTxContext(ctx, keys, counts, reuses), true
}

// telemetryTx is the transaction that applies one batch of counters. Factored out
// because the batch now arrives by two routes: the writable store's own flush ticker,
// and a read-only store's batch forwarded through the owner's op queue. The two must
// apply identically, or the same fetch would count differently depending on which
// process observed it.
func telemetryTx(keys []string, counts map[string]int64, reuses []reuseEvent) func(*sql.Tx) error {
	return telemetryTxContext(context.Background(), keys, counts, reuses)
}

func telemetryTxContext(ctx context.Context, keys []string, counts map[string]int64, reuses []reuseEvent) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		for _, k := range keys {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO metrics(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=value+excluded.value`,
				k, counts[k]); err != nil {
				return err
			}
		}
		for _, r := range reuses {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE note_reuse
				    SET reuse_count = reuse_count + 1,
				        first_reuse = COALESCE(first_reuse, ?),
				        last_reuse  = ?
				  WHERE note_id = ? AND (? - authored_at) >= ?`,
				r.at, r.at, r.noteID, r.at, r.gapSec); err != nil {
				return err
			}
		}
		return nil
	}
}

// forwardTelemetry is the read-only store's equivalent of the writer goroutine's flush
// ticker: it drains the in-memory counters onto the owning writer's op queue on the same
// cadence. One small file per interval per process, and only when something happened, so
// an idle window writes nothing at all.
func (s *Store) forwardTelemetry() {
	defer s.wg.Done()
	t := time.NewTicker(telemetryFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			s.queueTelemetry() // land the last batch on the way out
			return
		case <-t.C:
			s.queueTelemetry()
		}
	}
}

// queueTelemetry hands one batch of counters to the owning writer. Best effort by
// design: counters are the one thing in this index that is genuinely lossy (see
// walSizeLimit's neighbours), and a queue that is full means the owner is long gone,
// which the surfaces report on their own paths with a great deal more context than a
// counter could.
func (s *Store) queueTelemetry() {
	s.telMu.Lock()
	counts, reuses := s.telCount, s.telReuse
	s.telCount, s.telReuse = nil, nil
	s.telMu.Unlock()
	if len(counts) == 0 && len(reuses) == 0 {
		return
	}
	op := Op{Kind: OpTelemetry, Counts: counts}
	for _, r := range reuses {
		op.Reuse = append(op.Reuse, ReuseEvent{NoteID: r.noteID, GapSec: r.gapSec, At: r.at})
	}
	if _, err := s.EnqueueOp(op); err != nil {
		slog.Debug("mesh: could not queue usage counters for the owning writer", "err", err)
	}
}

// flushTelemetry writes pending counters from the CALLER's goroutine (via the jobs
// channel), but only within telemetryFlushTimeout. Reporting surfaces prefer a current
// picture; best-effort counters never justify pinning an admitted request or shutdown
// behind SQLite's ordinary 30-second write patience.
func (s *Store) flushTelemetry() {
	ctx, cancel := context.WithTimeout(context.Background(), telemetryFlushTimeout)
	defer cancel()
	s.flushTelemetryContext(ctx)
}

func (s *Store) flushTelemetryContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	if !s.canWrite() {
		// Same intent, the only route available: hand the batch to the owning writer now
		// rather than at the next tick. Going through Write here would fail with
		// ErrReadOnly and log "this batch of counters is lost", which was true before the
		// queue existed and is not any more.
		s.queueTelemetry()
		return
	}
	fn, ok := s.drainTelemetryContext(ctx)
	if !ok {
		return
	}
	if err := s.WriteContext(ctx, fn); err != nil {
		slog.Warn("mesh: telemetry flush failed; this batch of counters is lost", "err", err)
	}
}

// Count returns the row count of a table (read pool). The table name is a fixed
// internal identifier, never user input.
func (s *Store) Count(table string) (int, error) {
	return s.CountContext(context.Background(), table)
}

// CountContext is Count with caller-controlled cancellation.
func (s *Store) CountContext(ctx context.Context, table string) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var n int
	err := s.readDB.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n)
	return n, err
}

// Close stops the writer goroutine and closes both pools. It waits for the writer to
// drain any in-flight transaction before closing the pools, so a write racing
// shutdown completes cleanly instead of hitting a closed DB.
// A second Close must not panic. `close(s.done)` on an already-closed channel takes
// the whole process down, and Close sits on shutdown paths where a double call is
// easy to arrange: Server.Close calls it, and so does any caller that closed the
// store itself first. A panic during shutdown loses whatever the rest of the
// shutdown was going to flush, which is the worst possible moment for it.
// Idempotent instead, replaying the first result to every later caller.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.shutdown() })
	return s.closeErr
}

func (s *Store) shutdown() error {
	s.stopBackgroundHealth()
	// A read-only store has no writer goroutine, no jobs channel and no writeDB. It must
	// also never open its own writable connection to run checkpointTruncateBestEffort,
	// which is exactly what the writable path below does: an OpenReadOnly caller (a
	// per-window `mesh mcp`) is not allowed to become a writer even for one PRAGMA. What
	// it does have is the telemetry forwarder, which lands its last batch on the op queue
	// when done closes; waiting for it is why a window that is closed right after a
	// search still contributes that search to the flywheel.
	if s.readOnly {
		close(s.done)
		s.wg.Wait()
		return s.readDB.Close()
	}
	// Land the last batch of in-memory telemetry while the writer is still serving:
	// after close(s.done) every Write returns "store is closed".
	telemetryCtx, cancelTelemetry := context.WithTimeout(context.Background(), telemetryFlushTimeout)
	s.flushTelemetryContext(telemetryCtx)
	cancelTelemetry()
	close(s.done)
	s.wg.Wait()
	// The writer has drained, so a clean shutdown is the safe moment to TRUNCATE the WAL
	// back to zero: journal_size_limit only caps mesh.db-wal to 16MB, so without this a
	// cleanly-restarted process inherits (and re-grows from) a 16MB high-water mark. This
	// is the in-process, restart-time complement to the hourly mesh-doctor's out-of-process
	// TRUNCATE (which reaps stale readers of processes that never Close). Bounded to a few
	// seconds so a reader in another `mesh mcp --watch` process can never stall shutdown.
	s.checkpointTruncateBestEffort()
	errW := s.writeDB.Close()
	errR := s.readDB.Close()
	if errW != nil {
		return errW
	}
	return errR
}

// checkpointTruncateBestEffort runs a single TRUNCATE checkpoint on shutdown to reclaim
// the WAL file. Best-effort and tightly bounded: a short busy_timeout plus a context
// deadline mean that if another process holds a read lock it gives up quickly rather than
// blocking Close for the DSN's 30s busy_timeout. Full durability across never-closing
// multi-writer processes remains the deferred read-only-MCP refactor (see the mesh WAL
// decision note); this only covers the clean-restart path.
// walBytes is the size of mesh.db-wal on disk, or 0 if it cannot be read. The on-disk
// size is the only honest signal here: PRAGMA wal_checkpoint reports frames, not the file
// length, and the file is what fills a disk and what an operator sees.
func (s *Store) walBytes() int64 {
	fi, err := os.Stat(s.dbPath + "-wal")
	if err != nil {
		return 0
	}
	return fi.Size()
}

func (s *Store) checkpointTruncateBestEffort() {
	release, ok, _ := s.acquireWriteAuthorization()
	if !ok {
		return
	}
	defer release()
	s.checkpointTruncateBestEffortAuthorized()
}

func (s *Store) checkpointTruncateBestEffortAuthorized() {
	// Its OWN connection, never one out of writeDB. writeDB is capped at a single
	// connection, so `PRAGMA busy_timeout=2000` executed on it did not end with this
	// function: the connection went back to the pool carrying 2000 and every later write
	// transaction in the process inherited it, silently replacing the DSN's 30s. The
	// trigger is a WAL over walSizeLimit, which is itself a symptom of write contention,
	// so the daemon lost 28 seconds of patience exactly when a sibling watcher was
	// holding the lock. Reconciles then returned SQLITE_BUSY, the drift stayed, and the
	// next tick failed the same way, which is how the index stops picking up new notes.
	// The short timeout belongs in this connection's DSN instead.
	db, err := sql.Open("sqlite", dsnBusy(s.dbPath, checkpointBusyTimeoutMS))
	if err != nil {
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
}

// migratePendingAuthoring is additive and runs inside the schema transaction.
// Verify the historical shape first so unrelated damage cannot authorize mutation.
func migratePendingAuthoring(ctx context.Context, db schemaConnection) error {
	if err := validateSchemaProbes(ctx, db, []schemaProbe{{"pending_notes", `SELECT id,type,title,do_text,dont_text,why,confidence,source,created_at FROM pending_notes LIMIT 0`}}); err != nil {
		if isSchemaShapeError(err) {
			return nil
		} // ordinary schema recovery handles absent/damaged tables
		return err
	}
	if err := validateSchemaProbes(ctx, db, []schemaProbe{{"pending_notes", `SELECT authoring_json FROM pending_notes LIMIT 0`}}); err == nil {
		return nil
	} else if !isSchemaShapeError(err) {
		return err
	}
	_, err := db.ExecContext(ctx, `ALTER TABLE pending_notes ADD COLUMN authoring_json TEXT NOT NULL DEFAULT ''`)
	return err
}
