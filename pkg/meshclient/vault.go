// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/merge"
	"github.com/bright-interaction/mesh/internal/syncproto"
	"github.com/bright-interaction/mesh/internal/teamtelemetry"
	"github.com/bright-interaction/mesh/internal/vault"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// credentials and sync state live under <vault>/.mesh, which is git-ignored and
// never itself synced. credentials is mode 0600 (it holds the bearer token).
type credentials struct {
	HubURL string `json:"hub_url"`
	Token  string `json:"token"`
	// VaultID lets SyncVault detect and fail safe if a crash ever leaves
	// credentials and sync.json describing different vaults. Older credential
	// files omit it, so HubURL remains the compatibility fallback.
	VaultID string `json:"vault_id,omitempty"`
}

type syncState struct {
	HeadSHA string            `json:"head_sha"`
	Hashes  map[string]string `json:"hashes"`             // vault-relative path -> content sha
	TombSeq int64             `json:"tomb_seq,omitempty"` // last delete high-water mark seen from the hub
	// VaultID and HubURL record WHICH hub this base describes. Everything else in this
	// file is meaningless without them: HeadSHA is a commit in one hub's history, Hashes
	// is "what that hub already has", and TombSeq is a position in that hub's delete
	// ledger. A join into an already-joined directory rewrites .mesh/credentials and
	// used to leave all three pointing at the previous hub, so hub A's hash map became
	// the push baseline for hub B and every note not edited since was skipped by
	// computeOutbox forever (see JoinVault).
	VaultID string `json:"vault_id,omitempty"`
	HubURL  string `json:"hub_url,omitempty"`
	// RequestZstd records a hub's standard Accept-Encoding advertisement. It is
	// transport capability, not vault data; false preserves old-hub compatibility.
	RequestZstd bool `json:"request_zstd,omitempty"`
}

func credPath(vaultDir string) string  { return filepath.Join(vaultDir, ".mesh", "credentials") }
func statePath(vaultDir string) string { return filepath.Join(vaultDir, ".mesh", "sync.json") }

func writeCredentials(vaultDir string, c credentials) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, ".mesh"), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeFileAtomicPrivate(credPath(vaultDir), b, 0o600)
}

func readCredentials(vaultDir string) (credentials, error) {
	var c credentials
	b, err := os.ReadFile(credPath(vaultDir))
	if err != nil {
		return c, fmt.Errorf("not joined to a hub (no .mesh/credentials); run: mesh join <hub-url> <invite>")
	}
	return c, json.Unmarshal(b, &c)
}

func writeState(vaultDir string, s syncState) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, ".mesh"), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	// Atomic, like every other write in this package. This was the one truncate-then-write
	// left, 30 lines above the writeFileAtomic that exists precisely because a half-written
	// file is a hazard, and mirrored on the hub side. A crash mid-write left a truncated
	// state file whose unmarshal error readState then swallowed, silently resetting the
	// base to empty: the drop gates become no-ops and the next sync is a noisy full
	// re-push with mass conflict siblings. Recoverable, but avoidable for one line.
	//
	// 0600, the mode this call site used when it was an os.WriteFile, and not the note
	// default: sync.json is a full listing of every note path in the vault plus a content
	// hash for each, and it sits next to the 0600 credentials file. Do not weaken this on
	// the assumption that the directory protects it. Three packages create .mesh and the
	// first one to run wins; internal/index is almost always first (every mesh command
	// opens the index) and it created the directory at 0755 until 2026-08-11. This file's
	// own mode is the only part of its privacy this package controls.
	return writeFileAtomic(statePath(vaultDir), b, 0o600)
}

func readState(vaultDir string) syncState {
	s := syncState{Hashes: map[string]string{}}
	if b, err := os.ReadFile(statePath(vaultDir)); err == nil {
		// Log rather than discard. A corrupt state file silently resets the sync base,
		// and the resulting full re-push looks like a mystery rather than a consequence.
		if uerr := json.Unmarshal(b, &s); uerr != nil {
			slog.Warn("sync: sync.json is unreadable; treating the vault as never synced",
				"path", statePath(vaultDir), "err", uerr)
			s = syncState{Hashes: map[string]string{}}
		}
	}
	if s.Hashes == nil {
		s.Hashes = map[string]string{}
	}
	return s
}

// Summary reports what a sync round did, for the CLI.
type Summary struct {
	Pushed           int
	Pulled           int
	Conflicts        int
	Head             string
	ConflictSiblings []string      // merge conflicts: our pushed version parked here
	Protected        []string      // external-editor race: incoming hub version parked here
	Dropped          []string      // full-reconcile: locals removed because deleted upstream
	Rejected         []string      // hub refused these (viewer/ACL/scope/oversize/unsupported path); kept dirty to retry
	Blocked          []BlockedNote // invalid local content; not uploaded, stays dirty until corrected
	Remaining        int           // dirty notes deferred to the next round because the batch was bounded
	Deferred         []string      // exact vault-relative paths counted by Remaining; none reached the hub this round
}

func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// computeOutbox diffs the vault's markdown files on disk against the last-synced
// hashes, returning the changes to push plus the current on-disk hash map.
func computeOutbox(vaultDir string, prev map[string]string) ([]syncproto.OutboxItem, map[string]string, error) {
	return computeOutboxContext(context.Background(), vaultDir, prev)
}
func computeOutboxContext(ctx context.Context, vaultDir string, prev map[string]string) ([]syncproto.OutboxItem, map[string]string, error) {
	files, err := vault.WalkContext(ctx, vaultDir)
	if err != nil {
		return nil, nil, err
	}
	current := map[string]string{}
	var outbox []syncproto.OutboxItem
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		rel, err := filepath.Rel(vaultDir, f)
		if err != nil {
			rel = f
		}
		rel = filepath.ToSlash(rel)
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		h := contentHash(b)
		current[rel] = h
		if prev[rel] != h {
			outbox = append(outbox, syncproto.OutboxItem{Path: rel, Op: "upsert", ContentB64: base64.StdEncoding.EncodeToString(b)})
		}
	}
	for rel := range prev {
		// The response stream also carries the hub-owned root mesh.toml and
		// .gitattributes, but the client never pushes them. Older/buggy clients
		// may have admitted those paths into sync.json; silently shed them rather
		// than manufacturing a delete because vault.Walk intentionally omits them.
		if _, ok := vault.SafeSyncNotePath(rel); !ok {
			continue
		}
		if _, ok := current[rel]; !ok {
			outbox = append(outbox, syncproto.OutboxItem{Path: rel, Op: "delete"})
		}
	}
	return outbox, current, nil
}

// park records a path whose incoming hub version was set aside (sibling) because
// the local file changed during the sync window.
type park struct {
	note    string // the original path whose local change we kept
	sibling string // where the incoming hub version was parked
}

// pathProtection describes which local state must win over an inbound delta.
// Presence and absence are separate: an unwriteable conflict sibling protects
// bytes only when the live note exists, while a rejected/deferred delete protects
// the deliberate absence too.
type pathProtection struct {
	present bool
	missing bool
}

var beforeDurableUpsert func(string)

// readDeltaFile is a test seam for deterministic local I/O failures between
// outbox capture and response application.
var readDeltaFile = os.ReadFile

// Test seam for an editor save after unknown-base tombstone quarantine but
// before the survivor phase begins.
var afterUntrustedTombstoneQuarantine func()

// applyDeltas writes or removes files per the hub's response, guarding against
// the external-editor race (SPEC 6.6): sentHashes is the on-disk state captured
// when the outbox was computed, so if a path changed on disk SINCE then a local
// edit OR delete landed during the sync window. In that case the incoming hub
// version is parked in a sibling and the local change is kept; SyncVault then
// keeps the path "dirty" so the local change re-pushes next sync (it is not
// silently dropped). Each write is atomic (temp + rename); a partial-batch
// failure self-heals because the base is not advanced. Returns the parked paths.
//
// Every delta path goes through safeNotePath first: the path is hub-controlled,
// so an unvalidated Join is arbitrary file write outside the vault. An unsafe
// path is skipped and logged, never fatal, so one bad entry cannot stop the rest
// of a legitimate sync.
func applyDeltas(vaultDir string, deltas []syncproto.Delta, sentHashes map[string]string, protected map[string]pathProtection) ([]park, error) {
	if err := validateDeltas(vaultDir, deltas); err != nil {
		return nil, err
	}
	var parked []park
	for _, d := range deltas {
		if d.Op != "upsert" && d.Op != "delete" {
			return parked, fmt.Errorf("sync: unsupported delta operation %q for %s", d.Op, d.Path)
		}
		abs, perr := safeNotePath(vaultDir, d.Path)
		if perr != nil {
			slog.Warn("sync: refusing hub delta path", "path", d.Path, "err", perr)
			//safe-skip: this is INBOUND hub content we are refusing to write, not a
			// local note. Nothing of the user's is dropped; the local file is untouched.
			continue
		}
		onDisk, readErr := readDeltaFile(abs)
		sentHash, wasSent := sentHashes[d.Path]
		// A local change during the window is either an edit (present but
		// different) or a delete (absent now, but present at send time).
		locallyChanged := (readErr == nil && contentHash(onDisk) != sentHash) ||
			(readErr != nil && wasSent)
		// A protected path is locally changed by definition in the state named by
		// its protection. Rejected/deferred upserts protect bytes; their deletes
		// protect absence; an unparked conflict protects only bytes that exist.
		//
		// syncproto states the contract plainly: "The client keeps its local copy; the
		// edit simply did not land upstream." Without this, it did the opposite. The
		// comparison above is against sentHashes, i.e. the disk state at OUTBOX time, so
		// an edit made BEFORE the sync (which is the normal order: you edit, then you
		// sync) hashes equal and reads as unchanged. If the same round also carried a
		// delta for that path, the hub version was written straight over the user's
		// bytes, with no conflict, no sibling and no curation job, while Summary.Rejected
		// told them it had been kept and to retry.
		//
		// Treating it as changed routes it into the park-and-keep branch below, which is
		// what Protected and keepParkedDirty already exist for: the local bytes stay, the
		// hub version lands in a sibling, and the path re-pushes next round.
		protection := protected[d.Path]
		if (readErr == nil && protection.present) || (readErr != nil && protection.missing) {
			locallyChanged = true
		}

		if d.Op == "delete" {
			if readErr != nil {
				// A read error is not evidence that the pathname is absent. A note
				// created during the request may be unreadable, a broken symlink, or
				// another non-regular object; passing hash(nil) to the guarded remover
				// can relocate it and can discard an empty file. Only a confirming
				// Lstat absence makes the delete already satisfied.
				if _, lerr := os.Lstat(abs); os.IsNotExist(lerr) {
					//safe-skip: the pathname is confirmed absent, so there are no
					// local bytes to remove or preserve.
					continue
				} else if lerr != nil {
					return parked, fmt.Errorf("sync: inspect unreadable delete path %s: %w", d.Path, lerr)
				}
				return parked, fmt.Errorf("sync: refusing to delete unreadable path %s: %w", d.Path, readErr)
			}
			if locallyChanged {
				continue // local edit/recreate after send: keep it, skip the delete
			}
			removed, sibling, err := removeFileDurable(vaultDir, d.Path, contentHash(onDisk))
			if err != nil {
				return parked, err
			}
			if !removed && sibling != "" {
				parked = append(parked, park{note: d.Path, sibling: sibling})
			}
			//safe-skip: the delete was applied above. Nothing is being dropped here.
			continue
		}

		b, err := base64.StdEncoding.DecodeString(d.ContentB64)
		if err != nil {
			return parked, err
		}
		if locallyChanged && (readErr != nil || contentHash(onDisk) != contentHash(b)) {
			// External-editor race: park the incoming version, keep the local
			// change (a local delete keeps the path absent; a local edit keeps it).
			// The sibling is derived from the already-validated delta path, so a
			// validation failure here is a local invariant break (e.g. a symlink
			// planted mid-sync), not hub input: fail the round rather than skip it,
			// which leaves the base unadvanced and self-heals on retry.
			sib := merge.SiblingPath(d.Path, time.Now(), "hub", b)
			sibAbs, perr := safeSiblingPath(vaultDir, sib)
			if perr != nil {
				return parked, perr
			}
			created, err := writeNewFileDurable(sibAbs, b, 0o644)
			if err != nil {
				return parked, err
			}
			if !created {
				existing, rerr := os.ReadFile(sibAbs)
				if rerr != nil || contentHash(existing) != contentHash(b) {
					// The deterministic sibling is user-owned once it exists. Keep
					// it and park this incoming version under an unpredictable,
					// no-clobber name instead.
					sib, err = parkContent(vaultDir, d.Path, "hub-protected", b, 0o644)
					if err != nil {
						return parked, err
					}
				}
			}
			parked = append(parked, park{note: d.Path, sibling: sib})
			continue
		}
		applied, sibling, err := replaceFileDurable(vaultDir, d.Path, readErr == nil, contentHash(onDisk), b)
		if err != nil {
			return parked, err
		}
		if !applied && sibling != "" {
			parked = append(parked, park{note: d.Path, sibling: sibling})
		}
	}
	return parked, nil
}

// validateDeltas rejects a malformed response before the first filesystem
// mutation. The collision key models the aliases common client filesystems
// resolve to one pathname (case, trailing dots/spaces, and Unicode normalization),
// even when the hub runs on case-sensitive Linux and can represent both names.
func validateDeltas(vaultDir string, deltas []syncproto.Delta) error {
	seen := make(map[string]string, len(deltas))
	for _, d := range deltas {
		if d.Op != "upsert" && d.Op != "delete" {
			return fmt.Errorf("sync: unsupported delta operation %q for %s", d.Op, d.Path)
		}
		if _, err := safeNotePath(vaultDir, d.Path); err != nil {
			continue // unsafe inbound paths are skipped, preserving the existing contract
		}
		key, ok := clientPathCollisionKey(d.Path)
		if !ok {
			continue
		}
		if first, duplicate := seen[key]; duplicate {
			return fmt.Errorf("sync: duplicate delta paths %q and %q alias on the client filesystem", first, d.Path)
		}
		seen[key] = d.Path
		if d.Op == "upsert" {
			b, err := base64.StdEncoding.DecodeString(d.ContentB64)
			if err != nil {
				return fmt.Errorf("sync: invalid base64 delta for %s: %w", d.Path, err)
			}
			if !merge.IsText(b) {
				return fmt.Errorf("sync: refusing non-text or oversize delta for %s", d.Path)
			}
		}
	}
	return nil
}

// validateSyncResponse rejects contradictions in a successful-looking response
// before conflict siblings, deltas, or tombstone drops mutate the filesystem.
// Tombstones name client notes only; the two hub-owned root files can be sent as
// upserts but can never be deleted by a client-side drop list.
func validateSyncResponse(vaultDir string, resp syncproto.SyncResponse, sentOutbox []syncproto.OutboxItem) error {
	if err := validateDeltas(vaultDir, resp.Deltas); err != nil {
		return err
	}
	live := make(map[string]string, len(resp.Deltas))
	for _, d := range resp.Deltas {
		if d.Op != "upsert" {
			continue
		}
		if _, err := safeNotePath(vaultDir, d.Path); err != nil {
			continue // the delta itself is safely skipped by applyDeltas
		}
		if key, ok := clientPathCollisionKey(d.Path); ok {
			live[key] = d.Path
		}
	}
	seenTombstones := make(map[string]string, len(resp.Tombstones))
	for _, rel := range resp.Tombstones {
		if _, ok := vault.SafeSyncNotePath(rel); !ok {
			return fmt.Errorf("sync: unsafe tombstone path %q", rel)
		}
		if _, err := safeNotePath(vaultDir, rel); err != nil {
			return fmt.Errorf("sync: unsafe tombstone path %q: %w", rel, err)
		}
		key, ok := clientPathCollisionKey(rel)
		if !ok {
			return fmt.Errorf("sync: unsafe tombstone path %q", rel)
		}
		if first, duplicate := seenTombstones[key]; duplicate {
			return fmt.Errorf("sync: duplicate tombstone paths %q and %q alias on the client filesystem", first, rel)
		}
		if livePath, contradiction := live[key]; contradiction {
			return fmt.Errorf("sync: response describes client path %q as live and %q as tombstoned", livePath, rel)
		}
		seenTombstones[key] = rel
	}

	// Rejected is an acknowledgement set for this exact request, not an
	// independent path stream. Requiring exact membership lets all downstream
	// protection/state logic use the sent path key safely; accepting a case or
	// Unicode alias would miss those maps on a case-sensitive runtime while still
	// naming the same file on APFS/Windows. Uniqueness also keeps Pushed from
	// becoming negative on a duplicated response entry.
	sent := make(map[string]bool, len(sentOutbox))
	for _, item := range sentOutbox {
		sent[item.Path] = true
	}
	seenRejected := make(map[string]string, len(resp.Rejected))
	for _, rel := range resp.Rejected {
		if _, ok := vault.SafeSyncNotePath(rel); !ok {
			return fmt.Errorf("sync: unsafe rejected path %q", rel)
		}
		if _, err := safeNotePath(vaultDir, rel); err != nil {
			return fmt.Errorf("sync: unsafe rejected path %q: %w", rel, err)
		}
		key, ok := clientPathCollisionKey(rel)
		if !ok {
			return fmt.Errorf("sync: unsafe rejected path %q", rel)
		}
		if first, duplicate := seenRejected[key]; duplicate {
			return fmt.Errorf("sync: duplicate rejected paths %q and %q alias on the client filesystem", first, rel)
		}
		if !sent[rel] {
			return fmt.Errorf("sync: rejected path %q was not in the transmitted outbox", rel)
		}
		seenRejected[key] = rel
	}
	return nil
}

func clientPathCollisionKey(rel string) (string, bool) {
	clean, ok := safeRelPath(rel)
	if !ok {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(clean), "/")
	for i := range parts {
		parts[i] = strings.TrimRight(norm.NFC.String(cases.Fold().String(parts[i])), ". ")
	}
	return strings.Join(parts, "/"), true
}

// keepParkedDirty rewrites current so each guard-parked path keeps its pre-sync
// base hash instead of its (just-applied) disk hash, so the next computeOutbox
// detects the kept local change and re-pushes it (SPEC 6.6: enqueue the local
// change). Without this the local change would be recorded as synced and lost.
func keepParkedDirty(current, base map[string]string, parked []park) {
	for _, p := range parked {
		if old, ok := base[p.note]; ok {
			current[p.note] = old
		} else {
			delete(current, p.note)
		}
	}
}

// keepDeferredDirty rewrites current so each path the bounded push left OUT of this
// batch keeps its pre-sync base hash (or no entry at all if it had none), exactly like
// keepParkedDirty does for a guard-parked path. The new base must describe what was
// actually SENT, not what is on disk: a deferred path recomputed from disk reads as
// synced, so the next computeOutbox emits nothing for it and the note is never pushed.
// A deferred delete keeps its base hash while the file is absent, which is what makes
// the next round re-emit the delete rather than forget it.
func keepDeferredDirty(current, base map[string]string, deferred []string) {
	for _, rel := range deferred {
		if old, ok := base[rel]; ok {
			current[rel] = old
		} else {
			delete(current, rel)
		}
	}
}

// keepWindowEditsDirty stops a local write made DURING the sync window from being
// silently recorded as already synced. current is recomputed from disk after the
// round trip, so any file the user (or the indexer, or another agent) touched
// while the request was in flight would be baselined as if the hub had it: the
// edit is never pushed, never retried, and a later inbound delta for that path
// overwrites it with no conflict sibling, because sentHashes then matches disk.
//
// sentHashes is the on-disk state at send time, so a path whose hash differs from
// it changed during the window. Response deltas establish the new authoritative
// base for their note paths, while dropped paths establish absence; every other
// changed path is reset to its send-time state. That makes an edit re-push as an
// upsert and a window delete re-push as a delete.
func keepWindowEditsDirty(_ string, current, sentHashes map[string]string, deltas []syncproto.Delta, dropped []string) {
	// A response delta describes the hub at the new HeadSHA, so its state is the
	// base for that path regardless of what is currently on disk. This is more
	// precise than excluding every delta path from the window check: an editor can
	// save after applyDeltas and before sync.json is written, and a protected local
	// edit/delete deliberately makes disk differ from the delta. Persisting the
	// delta state keeps either change dirty for the next round.
	expected := make(map[string]*string, len(deltas)+len(dropped))
	for _, d := range deltas {
		// Root config files are valid inbound deltas but are hub-owned, never
		// client notes. Admitting them to the note hash base makes the next
		// computeOutbox infer a client-side delete because Walk omits them.
		if _, ok := vault.SafeSyncNotePath(d.Path); !ok {
			continue
		}
		switch d.Op {
		case "upsert":
			b, err := base64.StdEncoding.DecodeString(d.ContentB64)
			if err != nil {
				continue // applyDeltas would have failed the round before this point
			}
			h := contentHash(b)
			expected[d.Path] = &h
		case "delete":
			expected[d.Path] = nil
		}
	}
	for _, rel := range dropped {
		expected[rel] = nil
	}
	for rel, want := range expected {
		if want == nil {
			delete(current, rel)
		} else {
			current[rel] = *want
		}
	}
	restore := func(rel string) {
		if _, handled := expected[rel]; handled {
			return
		}
		if old, ok := sentHashes[rel]; ok {
			current[rel] = old
		} else {
			delete(current, rel) // created during the window: no base, so it pushes as new
		}
	}
	for rel, h := range current {
		if sentHashes[rel] != h {
			restore(rel)
		}
	}
	for rel := range sentHashes {
		if _, ok := current[rel]; !ok {
			restore(rel) // deleted during the window: keep the base so the delete pushes
		}
	}
}

// dropFullReconcileOrphans removes local notes that a full reconcile proves are
// gone from the team vault. A full reconcile's deltas carry the live snapshot as
// upserts with NO deletes (the client's base was empty or too old to diff), so
// without this a stale client keeps, and can later resurrect, every note deleted
// while it was away (the offline-past-horizon resurrection bug).
//
// The deletion is content-safe: it only ever removes a file that is byte-identical
// to the EXACT version we last synced (base[rel]), which is the version the hub
// then deleted. The snapshot tells us the path is no longer live; matching base
// tells us the on-disk bytes are that dead version and not something the user has
// since edited or recreated at the same path. So a local recreate with any
// different content, including content the hub silently rejected as non-text, is
// never destroyed: its hash differs from base and it is kept (and re-pushes). The
// hub tombstone list is a confirming signal only; the base-hash match is the
// load-bearing safety, which also means tombstone GC can never cause data loss.
//
// Known limitation (safe direction): a client whose sync state was reset so base
// no longer knows a path cannot prove the on-disk bytes are the dead version, so
// it keeps the file rather than risk destroying local content. Such a note can
// re-share on the next push (a re-share, never a loss). The horizon GC assumes
// base is intact for offline clients.
func dropFullReconcileOrphans(vaultDir string, deltas []syncproto.Delta, tombstones []string, base map[string]string, protected ...*[]park) ([]string, error) {
	keep := make(map[string]bool, len(deltas))
	for _, d := range deltas {
		if d.Op == "upsert" {
			keep[d.Path] = true
		}
	}
	tomb := make(map[string]bool, len(tombstones))
	for _, p := range tombstones {
		tomb[p] = true
	}
	files, err := vault.Walk(vaultDir) // excludes .mesh + conflict siblings already
	if err != nil {
		return nil, err
	}
	var dropped []string
	for _, f := range files {
		rel, err := filepath.Rel(vaultDir, f)
		if err != nil {
			rel = f
		}
		rel = filepath.ToSlash(rel)
		if keep[rel] {
			continue // present in the authoritative snapshot: alive
		}
		baseHash, syncedBefore := base[rel]
		// A TOMBSTONE IS REQUIRED, not merely "absent from the snapshot".
		//
		// Absence plus a matching base hash proves "these bytes are the version the hub
		// last had". It does NOT prove the hub deleted the note, because a hub whose
		// history rolled BACKWARDS produces byte-identical evidence. Restore the hub
		// volume from a backup (the most common DR action there is) and every client
		// would delete everything created since that backup: no tombstone exists, because
		// nobody deleted anything. The paths were then dropped from the outbox too, so
		// the clients could not even re-push to heal the hub.
		//
		// Requiring the tombstone inverts that outcome: on a rollback the clients keep
		// their notes and push them back. The cost is that a note whose tombstone has
		// been GC'd can linger locally, which this design already accepts, because data
		// loss outranks a rare resurrection.
		//
		// Safe against the full path specifically: on a full reconcile the hub sends
		// AllTombstones() (internal/hub/sync.go), not a since-seq slice, so a genuine
		// delete always carries its tombstone here.
		if !tomb[rel] {
			//safe-skip: KEEP decision. Skipping here preserves the local file.
			continue
		}
		// SAFETY GATE: only remove bytes identical to the exact version we last
		// synced. A path with no base hash (state reset) or any local change since
		// fails here and is kept, so we never destroy unacknowledged local content.
		if !syncedBefore {
			//safe-skip: KEEP decision. Skipping here preserves the local file.
			continue
		}
		onDisk, readErr := os.ReadFile(f)
		if readErr != nil {
			//safe-skip: KEEP decision. Unreadable means we cannot prove the bytes match
			// the synced version, so the file stays. Failing closed preserves data.
			continue
		}
		if contentHash(onDisk) != baseHash {
			continue // locally edited or recreated since last sync: keep it
		}
		removed, sibling, err := removeFileDurable(vaultDir, rel, baseHash)
		if err != nil {
			return dropped, err
		}
		recordDeletePark(protected, rel, sibling)
		if removed {
			dropped = append(dropped, rel)
		}
	}
	return dropped, nil
}

// dropTombstoned removes local notes named in an incremental tombstone drop-list,
// content-safely: only a file byte-identical to the EXACT version we last synced
// (base[rel]) is removed. A path never synced locally, locally edited, or recreated
// since fails the gate and is kept, so an unacknowledged local change is never lost.
// This is the incremental sibling of dropFullReconcileOrphans' safety gate.
func dropTombstoned(vaultDir string, tombstones []string, base map[string]string, protected ...*[]park) ([]string, error) {
	var dropped []string
	for _, rel := range tombstones {
		baseHash, ok := base[rel]
		if !ok {
			continue // never synced here: nothing to prune
		}
		// The drop-list is hub-controlled, so validate before touching the disk.
		abs, perr := safeNotePath(vaultDir, rel)
		if perr != nil {
			slog.Warn("sync: refusing hub tombstone path", "path", rel, "err", perr)
			//safe-skip: KEEP decision. We refuse a hub-supplied delete path we cannot
			// validate, so the local file survives.
			continue
		}
		onDisk, err := os.ReadFile(abs)
		if err != nil {
			continue // already gone or unreadable
		}
		if contentHash(onDisk) != baseHash {
			continue // locally edited or recreated since last sync: keep it
		}
		removed, sibling, err := removeFileDurable(vaultDir, rel, baseHash)
		if err != nil {
			return dropped, err
		}
		recordDeletePark(protected, rel, sibling)
		if removed {
			dropped = append(dropped, rel)
		}
	}
	return dropped, nil
}

// quarantineUntrustedTombstones handles the one case contentHash gates cannot:
// an unknown base has no trusted hash for a stale local file. Positive evidence
// from the full-reconcile tombstone list means the live sync pathname must stay
// deleted, but the local bytes are moved into a visible conflict sibling rather
// than destroyed. A user can explicitly take-mine to recreate it; sync never does
// so automatically merely because sync.json was lost.
func quarantineUntrustedTombstones(vaultDir string, deltas []syncproto.Delta, tombstones []string) ([]park, []string, error) {
	live := make(map[string]bool, len(deltas))
	for _, d := range deltas {
		if d.Op == "upsert" {
			if key, ok := clientPathCollisionKey(d.Path); ok {
				live[key] = true
			}
		}
	}
	var parked []park
	var removed []string
	for _, rel := range tombstones {
		key, _ := clientPathCollisionKey(rel)
		if live[key] {
			continue
		}
		abs, err := safeNotePath(vaultDir, rel)
		if err != nil {
			slog.Warn("sync: refusing hub tombstone path during unknown-base quarantine", "path", rel, "err", err)
			continue
		}
		b, err := os.ReadFile(abs)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return parked, removed, err
		}
		captured, err := captureGuardedFile(vaultDir, rel, contentHash(b), "tombstone-quarantine", nil)
		if err != nil {
			return parked, removed, err
		}
		if captured == nil {
			continue
		}
		parked = append(parked, park{note: rel, sibling: captured.rel})
		removed = append(removed, rel)
	}
	return parked, removed, nil
}

func recordDeletePark(dst []*[]park, note, sibling string) {
	if sibling == "" || len(dst) == 0 || dst[0] == nil {
		return
	}
	*dst[0] = append(*dst[0], park{note: note, sibling: sibling})
}

// writeConflictSiblings preserves the client's losing version of each conflicted
// path in a local sibling BEFORE applyDeltas overwrites the path with the hub's
// winning version. Siblings are local resolution artifacts, never pushed.
//
// Both fields are hub-controlled. Path must be a safe note path and SiblingPath
// must be a safe path that actually carries the sync-conflict marker, so the hub
// cannot use the sibling field to write a local copy over an arbitrary note (or
// over a file outside the vault entirely).
// Returns the paths whose local version could NOT be parked. The caller must protect
// those from applyDeltas, or the hub's winning version silently overwrites the user's
// losing version with no copy anywhere.
var beforeConflictSiblingPublish func(string)

func writeConflictSiblings(vaultDir string, conflicts []syncproto.Conflict) ([]string, error) {
	var unparked []string
	for _, cf := range conflicts {
		noteAbs, perr := safeNotePath(vaultDir, cf.Path)
		if perr != nil {
			// Refuse the hostile path, but do NOT fail the round: a malicious or buggy
			// hub could otherwise wedge sync entirely with one bad path. Report it so
			// the caller protects the local bytes from being overwritten instead.
			slog.Warn("sync: refusing hub conflict path", "path", cf.Path, "err", perr)
			unparked = append(unparked, cf.Path)
			continue
		}
		sibAbs, perr := safeSiblingPath(vaultDir, cf.SiblingPath)
		if perr != nil {
			// Same reasoning: no sibling to park it in, so protect the local bytes
			// rather than losing them or wedging the client.
			slog.Warn("sync: refusing hub conflict sibling path", "path", cf.SiblingPath, "err", perr)
			unparked = append(unparked, cf.Path)
			continue
		}
		local, err := os.ReadFile(noteAbs)
		if err != nil {
			//safe-skip: nothing local to preserve (e.g. we deleted it), so there are no
			// bytes at risk.
			continue
		}
		if existing, rerr := os.ReadFile(sibAbs); rerr == nil {
			if contentHash(existing) == contentHash(local) {
				continue // idempotent replay of the same preserved loser
			}
			// A conflict sibling is user-owned resolution work once it exists. Never
			// overwrite different bytes merely because a replayed or hostile response
			// named the same path; protect the live loser as well because no new sibling
			// was written for it in this round.
			slog.Warn("sync: refusing to overwrite an existing conflict sibling", "path", cf.SiblingPath)
			unparked = append(unparked, cf.Path)
			continue
		} else if !os.IsNotExist(rerr) {
			// Unreadable means unequal cannot be disproved. Fail in the data-safe
			// direction and protect the live note rather than replacing either copy.
			slog.Warn("sync: cannot verify existing conflict sibling", "path", cf.SiblingPath, "err", rerr)
			unparked = append(unparked, cf.Path)
			continue
		}
		if beforeConflictSiblingPublish != nil {
			beforeConflictSiblingPublish(sibAbs)
		}
		created, err := writeNewFileDurable(sibAbs, local, 0o644)
		if err != nil {
			return unparked, err
		}
		if !created {
			existing, rerr := os.ReadFile(sibAbs)
			if rerr == nil && contentHash(existing) == contentHash(local) {
				continue // the same loser won the publication race
			}
			slog.Warn("sync: conflict sibling appeared during publication; preserving it", "path", cf.SiblingPath, "err", rerr)
			unparked = append(unparked, cf.Path)
		}
	}
	return unparked, nil
}

// writeFileAtomic writes b to path via a temp file in the same directory then
// rename, so a reader never sees a partially written note, and fsyncs both the
// data and the parent directory so the note survives a power cut.
//
// Temp+rename alone buys crash-atomicity for a concurrent READER, not durability:
// the rename can reach the disk while the data blocks it points at have not, and
// the note write and the sync.json write can land in either order. If sync.json
// survives and the note's bytes do not, the note reverts locally while state.Hashes
// records the new hash, and the next round pushes the OLD bytes as an upsert that
// the hub fast-forwards, reverting the team's copy with no conflict and no sibling.
//
// There are FOUR copies of this write in the tree, not two. The comment here used to
// name only internal/hub/repo.go, and that undercount is exactly how this estate
// keeps fixing one twin and shipping the other: the durability pass landed on this
// file and repo.go, and the other two kept writing note bytes with no fsync at all.
// The full list, all four fsync-before-rename plus syncDir-after-rename now:
//
//	pkg/meshclient/vault.go     writeFileAtomic  (this one; applies the hub's deltas)
//	internal/hub/repo.go        writeFileAtomic  (lands a note in the hub worktree)
//	cmd/mesh/conflicts.go       writeFileAtomic  (mesh conflicts resolve, take-mine)
//	internal/curator/merge_note.go writeAtomic   (writes the curator's LLM merge)
//
// Keep all four in step. Do not trust this list on its own: it is a comment and a
// comment cannot fail a build. cmd/mesh/atomic_write_durability_test.go DISCOVERS
// every temp-create + rename writer in the tree from the AST and fails on any one
// that does not fsync, so a fifth copy is caught the day it is written.
//
// The destination's current permission bits are preserved when it already exists. A
// rename INSTALLS the temp file, mode and all, and os.CreateTemp makes its file 0600,
// so without this the durability fix silently narrowed every note it touched to
// owner-only: nothing fails visibly, it just surfaces later as the indexer, a backup
// agent, an editor or a container on another uid no longer being able to read a note
// it could read yesterday. Same three lines as internal/vault/migrate.go
// WriteNoteAtomic, on purpose; there is one pattern for this, not two.
// TestEveryTempRenameWriterRestoresTheMode in cmd/mesh/conflicts_mode_test.go is the
// discovery guard for it, built like the fsync one: it names no file, so a sixth copy
// is caught the day it is written rather than three rounds later.
//
// newPerm is the mode a file that does not exist yet lands with, and it is a parameter
// rather than a constant 0644 because not every caller writes a note. Notes pass 0644,
// the mode every other note writer in the estate uses. writeState passes 0600, the mode
// its os.WriteFile used before it became atomic: .mesh/sync.json lists every note path
// in the vault with a content hash for each, .mesh is 0755 whenever internal/ingest
// created it first, and a default of 0644 here would have published that listing to
// every uid on the box on the next fresh join. A durability fix that widens a file
// somebody deliberately made private is worse than the gap it closes.
func writeFileAtomic(path string, b []byte, newPerm os.FileMode, forceMode ...bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	perm := newPerm
	force := len(forceMode) > 0 && forceMode[0]
	if !force {
		if fi, err := os.Stat(path); err == nil {
			perm = fi.Mode().Perm()
		}
	}
	tmp, err := os.CreateTemp(dir, ".mesh-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(b)
	// CreateTemp makes the file 0600; match the mode the note is meant to land with,
	// BEFORE the rename publishes it.
	perr := tmp.Chmod(perm)
	// Flush the data to the device BEFORE the rename publishes the name.
	serr := tmp.Sync()
	cerr := tmp.Close()
	if werr != nil {
		os.Remove(tmpName)
		return werr
	}
	if perr != nil {
		os.Remove(tmpName)
		return perr
	}
	if serr != nil {
		os.Remove(tmpName)
		return serr
	}
	if cerr != nil {
		os.Remove(tmpName)
		return cerr
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	syncDir(dir)
	return nil
}

// writeFileAtomicPrivate is the credentials-only variant: an accidentally
// permissive legacy mode must not survive token rotation. The temp is chmod'd
// before publication, so there is no post-rename window with a readable token.
func writeFileAtomicPrivate(path string, b []byte, perm os.FileMode) error {
	return writeFileAtomic(path, b, perm, true)
}

// The two hooks are deterministic test seams for an editor's atomic-save rename
// between applyDeltas/drop*'s first hash check and the guarded atomic capture.
var beforeDurableRemove func(string)

// beforeGuardedRename exercises the narrower race after the guard has fsynced
// the live inode but before it renames that pathname into its sibling.
var beforeGuardedRename func(string)

// guardedFile is a pathname atomically captured into a visible conflict sibling.
// If the process crashes mid-operation, bytes remain discoverable by `mesh
// conflicts`; a temporary hidden file would turn a crash-safety fix into loss by
// another name.
type guardedFile struct {
	rel     string
	path    string
	content []byte
	perm    os.FileMode
}

func guardBasePath(rel string) string {
	if strings.EqualFold(filepath.Ext(rel), ".md") {
		return rel
	}
	// The two non-note paths the protocol admits (mesh.toml and .gitattributes)
	// still need a safe, walker-excluded capture name. Give them a markdown base
	// solely for the conflict marker; the Summary carries the exact sibling path.
	dir := filepath.Dir(rel)
	base := strings.TrimLeft(filepath.Base(rel), ".")
	if base == "" {
		base = "root-file"
	}
	return filepath.ToSlash(filepath.Join(dir, base+"-root.md"))
}

func newGuardSibling(vaultDir, rel, role string, content []byte) (string, string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		now := time.Now()
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", "", err
		}
		// A cryptographically unpredictable 128-bit component makes the only
		// remaining check+rename destination race unreachable in practice. The
		// process/file sync lock already excludes other Mesh writers; an editor
		// cannot guess this pathname before the atomic rename.
		user := fmt.Sprintf("%s-%s-%d", role, hex.EncodeToString(nonce[:]), attempt)
		candidate := merge.SiblingPath(guardBasePath(rel), now, user, content)
		abs, err := safeSiblingPath(vaultDir, candidate)
		if err != nil {
			return "", "", err
		}
		if _, err := os.Lstat(abs); os.IsNotExist(err) {
			return candidate, abs, nil
		} else if err != nil {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("sync: could not allocate a conflict sibling for %s", rel)
}

func captureGuardedFile(vaultDir, rel, expectedHash, role string, before func(string)) (*guardedFile, error) {
	path, err := safeNotePath(vaultDir, rel)
	if err != nil {
		return nil, err
	}
	if before != nil {
		before(path)
	}

	// The live pathname may name editor-owned bytes that were never fsynced. Make
	// that inode durable while it still has its original name, then verify the
	// pathname still names the same inode before capturing it. Atomic-save editors
	// replace the inode; retrying makes their replacement durable too rather than
	// letting our rename become its sole pathname first.
	var syncedContent []byte
	var syncedInfo os.FileInfo
	for attempt := 0; attempt < 8; attempt++ {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("open live guard path %s: %w", rel, err)
		}
		content, rerr := io.ReadAll(f)
		fi, sterr := f.Stat()
		serr := f.Sync()
		cerr := f.Close()
		if err := errors.Join(rerr, sterr, serr, cerr); err != nil {
			return nil, fmt.Errorf("make live guard path %s durable: %w", rel, err)
		}
		if beforeGuardedRename != nil {
			beforeGuardedRename(path)
		}
		current, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("verify live guard path %s: %w", rel, err)
		}
		if !os.SameFile(fi, current) || fi.Size() != current.Size() || !fi.ModTime().Equal(current.ModTime()) {
			continue
		}
		syncedContent = content
		syncedInfo = fi
		break
	}
	if syncedInfo == nil {
		return nil, fmt.Errorf("sync: %s kept changing while preparing a guarded update", rel)
	}

	backupRel, backup, err := newGuardSibling(vaultDir, rel, role, []byte(expectedHash))
	if err != nil {
		return nil, err
	}
	backupDir := filepath.Dir(backup)
	if err := vault.PrepareSiblingDirectory(path, backup); err != nil {
		return nil, err
	}
	if err := os.Rename(path, backup); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	dir := filepath.Dir(path)
	// Re-open and verify what the atomic rename actually captured. The stable
	// pre-rename identity check closes deterministic atomic-save races; this second
	// check handles the irreducible final stat->rename window without ever trusting
	// stale bytes. A different/modified inode is fsynced here and returned to the
	// caller, whose expected-hash gate restores or parks it rather than deleting it.
	f, err := os.OpenFile(backup, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open guarded copy %s: %w", backupRel, err)
	}
	captured, rerr := io.ReadAll(f)
	fi, sterr := f.Stat()
	var serr error
	if sterr == nil && (!os.SameFile(syncedInfo, fi) || contentHash(captured) != contentHash(syncedContent)) {
		serr = f.Sync()
	}
	cerr := f.Close()
	if err := errors.Join(rerr, sterr, serr, cerr); err != nil {
		return nil, fmt.Errorf("make guarded copy %s durable: %w", backupRel, err)
	}
	syncDir(dir)
	syncDir(backupDir)
	perm := fi.Mode().Perm()
	return &guardedFile{rel: backupRel, path: backup, content: captured, perm: perm}, nil
}

var linkFile = os.Link

// writeNewFileDurable publishes a fully-written same-directory temp only if path
// is still absent. The hard link is atomic and no-clobber: concurrent readers see
// either no path or complete bytes, and an editor that installed a newer inode
// wins. Filesystems without hard-link support fail the round safely; no partial
// live markdown is ever published.
func writeNewFileDurable(path string, b []byte, perm os.FileMode) (bool, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, ".mesh-new-*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	perr := f.Chmod(perm)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, perr, serr, cerr); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := linkFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	// The live link now names already-fsynced data. Make that directory entry
	// durable before cleaning up the staging link.
	syncDir(dir)
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return true, err
	}
	syncDir(dir)
	return true, nil
}

func discardGuardedFile(g *guardedFile, dir string) error {
	if g == nil {
		return nil
	}
	if err := os.Remove(g.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	syncDir(dir)
	syncDir(filepath.Dir(g.path))
	return nil
}

func restoreGuardedFile(g *guardedFile, path string) (bool, error) {
	if g == nil {
		return false, nil
	}
	restored, err := writeNewFileDurable(path, g.content, g.perm)
	if err != nil || !restored {
		return restored, err // sibling remains; a newer live path, if any, is untouched
	}
	if err := discardGuardedFile(g, filepath.Dir(path)); err != nil {
		return true, err
	}
	return true, nil
}

func parkContent(vaultDir, rel, role string, content []byte, perm os.FileMode) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		sibling, path, err := newGuardSibling(vaultDir, rel, fmt.Sprintf("%s-%d", role, attempt), content)
		if err != nil {
			return "", err
		}
		created, err := writeNewFileDurable(path, content, perm)
		if err != nil {
			return "", err
		}
		if created {
			return sibling, nil
		}
	}
	return "", fmt.Errorf("sync: could not park content for %s", rel)
}

// removeFileDurable atomically captures the pathname, then verifies the MOVED
// inode against expectedHash. A late editor save is restored without clobbering
// anything newer; a matching capture is removed and the directory fsynced before
// sync.json may advance.
func removeFileDurable(vaultDir, rel, expectedHash string) (removed bool, sibling string, err error) {
	path, err := safeNotePath(vaultDir, rel)
	if err != nil {
		return false, "", err
	}
	captured, err := captureGuardedFile(vaultDir, rel, expectedHash, "delete-guard", beforeDurableRemove)
	if err != nil {
		return false, "", err
	}
	if captured == nil {
		return false, "", nil
	}
	dir := filepath.Dir(path)
	if contentHash(captured.content) != expectedHash {
		restored, err := restoreGuardedFile(captured, path)
		if err != nil {
			return false, captured.rel, err
		}
		if restored {
			return false, "", nil
		}
		slog.Warn("sync: local edit raced a delete; preserved beside a newer live version",
			"path", rel, "sibling", captured.rel)
		return false, captured.rel, nil
	}
	if err := discardGuardedFile(captured, dir); err != nil {
		return false, captured.rel, err
	}
	return true, "", nil
}

// replaceFileDurable is the upsert twin of removeFileDurable. It installs the
// hub bytes only while the live pathname still describes the version we checked;
// otherwise the local editor wins and the hub bytes are parked for resolution.
func replaceFileDurable(vaultDir, rel string, expectedExists bool, expectedHash string, incoming []byte) (applied bool, sibling string, err error) {
	path, err := safeNotePath(vaultDir, rel)
	if err != nil {
		return false, "", err
	}
	if !expectedExists {
		if beforeDurableUpsert != nil {
			beforeDurableUpsert(path)
		}
		created, err := writeNewFileDurable(path, incoming, 0o644)
		if err != nil {
			return false, "", err
		}
		if created {
			return true, "", nil
		}
		sibling, err := parkContent(vaultDir, rel, "hub-race", incoming, 0o644)
		return false, sibling, err
	}

	captured, err := captureGuardedFile(vaultDir, rel, expectedHash, "upsert-guard", beforeDurableUpsert)
	if err != nil {
		return false, "", err
	}
	if captured == nil {
		// The editor deleted the note after our first read. Keep that deletion and
		// park the incoming hub version.
		sibling, err := parkContent(vaultDir, rel, "hub-race", incoming, 0o644)
		return false, sibling, err
	}
	if contentHash(captured.content) != expectedHash {
		restored, rerr := restoreGuardedFile(captured, path)
		if rerr != nil {
			return false, captured.rel, rerr
		}
		if !restored {
			slog.Warn("sync: local edit raced an upsert; preserved beside a newer live version",
				"path", rel, "sibling", captured.rel)
		}
		sibling, perr := parkContent(vaultDir, rel, "hub-race", incoming, 0o644)
		return false, sibling, perr
	}

	created, err := writeNewFileDurable(path, incoming, captured.perm)
	if err != nil {
		return false, captured.rel, err
	}
	if created {
		if err := discardGuardedFile(captured, filepath.Dir(path)); err != nil {
			return false, captured.rel, err
		}
		return true, "", nil
	}
	// A still-later editor save occupied the path after capture. The old captured
	// bytes are already in hub history, so discard them and park the incoming hub
	// bytes; the new live bytes remain untouched and dirty.
	if err := discardGuardedFile(captured, filepath.Dir(path)); err != nil {
		return false, captured.rel, err
	}
	sibling, err = parkContent(vaultDir, rel, "hub-race", incoming, 0o644)
	return false, sibling, err
}

// syncDir fsyncs a directory so a rename that just landed in it survives a power
// cut. Best effort on purpose: opening or fsyncing a directory handle is a no-op or
// an error on some platforms and network filesystems (Windows cannot open one at
// all), and that is not a failed write, so the bytes still count as written and the
// refusal is only logged. The data itself is already fsynced by the caller.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		slog.Debug("sync: cannot open the note directory to fsync it", "dir", dir, "err", err)
		return
	}
	if err := d.Sync(); err != nil {
		slog.Debug("sync: directory fsync not supported here", "dir", dir, "err", err)
	}
	d.Close()
}

// SyncVault runs one reconcile round against the joined hub: push local edits,
// apply the hub's deltas, and persist the new base. It does not reindex; the
// caller (cmd/mesh) runs index.Reconcile afterward.
func SyncVault(vaultDir string) (Summary, error) {
	return SyncVaultContext(context.Background(), vaultDir)
}

// SyncVaultContext cancels lock waits, tree enumeration and hub requests. Once a
// verified response enters local publication, its durability phase completes
// before releasing the existing same-vault lock. An OS filesystem syscall itself
// cannot be interrupted by a Go context.
func SyncVaultContext(ctx context.Context, vaultDir string) (Summary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := acquireVaultSyncLockContext(ctx, vaultDir)
	if err != nil {
		return Summary{}, err
	}
	defer release()
	return syncVaultRoundContext(ctx, vaultDir, true, nil)
}

// syncVaultLocked is SyncVault with the same-vault process/file lock already
// held. JoinVault uses it so no sync can observe credentials half-transitioned
// to another hub.
func syncVaultLocked(vaultDir string) (Summary, error) {
	return syncVaultRound(vaultDir, true, nil)
}

func syncVaultRound(vaultDir string, allowPullFirst bool, suppressTombstones []string) (Summary, error) {
	return syncVaultRoundContext(context.Background(), vaultDir, allowPullFirst, suppressTombstones)
}
func syncVaultRoundContext(ctx context.Context, vaultDir string, allowPullFirst bool, suppressTombstones []string) (Summary, error) {
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	creds, err := readCredentials(vaultDir)
	if err != nil {
		return Summary{}, err
	}
	creds = hydrateLegacyVaultIDContext(ctx, vaultDir, creds)
	state := readState(vaultDir)
	// Recover from an interrupted join produced by this or an older client. A base
	// from another hub is never evidence about what the credential's hub has; the
	// only safe baseline is empty, which forces a full reconcile and re-push.
	hubBase := strings.TrimRight(creds.HubURL, "/")
	if joinTargetsAnotherVault(state, state.HubURL, hubBase, creds.VaultID) {
		state = syncState{Hashes: map[string]string{}, VaultID: creds.VaultID, HubURL: hubBase}
	}
	// Missing/corrupt/reset state is an UNKNOWN base, not proof every local note
	// is new. Sending those bytes before reading the hub's delete ledger can
	// resurrect every stale tombstoned path and clear its tombstone. Pull the
	// authoritative snapshot first; genuine survivors are pushed in a second
	// round below, still within this one locked SyncVault call.
	pullFirst := allowPullFirst && state.HeadSHA == ""
	outbox, sentHashes, err := computeOutboxContext(ctx, vaultDir, state.Hashes)
	if err != nil {
		return Summary{}, err
	}
	// Phase two remembers every authoritative tombstone learned by the unknown-
	// base pull. An editor can atomically save a stale buffer back after phase one
	// quarantined it; capture those bytes again and, independently, exclude the
	// aliased pathname from this call's outbox. That preserves the edit without
	// automatically clearing the hub's tombstone.
	var preParked []park
	var preDropped []string
	if len(suppressTombstones) > 0 {
		preParked, preDropped, err = quarantineUntrustedTombstones(vaultDir, nil, suppressTombstones)
		if err != nil {
			return Summary{}, err
		}
		blocked := make(map[string]bool, len(suppressTombstones))
		for _, rel := range suppressTombstones {
			if key, ok := clientPathCollisionKey(rel); ok {
				blocked[key] = true
			}
		}
		kept := outbox[:0]
		for _, item := range outbox {
			key, ok := clientPathCollisionKey(item.Path)
			if ok && blocked[key] {
				delete(sentHashes, item.Path) // quarantine made it absent at send time
				continue
			}
			kept = append(kept, item)
		}
		outbox = kept
	}
	outboxOps := make(map[string]string, len(outbox))
	for _, item := range outbox {
		outboxOps[item.Path] = item.Op
	}
	// Content that cannot pass the hub's deterministic gate need not cross the
	// network on every tick. Keep its operation and baseline: neither filtering
	// nor an inbound delta may turn an unshared local edit into "synced" bytes.
	// Unknown-base pulls quarantine tombstones first; validate only the survivors.
	var blocked []BlockedNote
	if !pullFirst {
		outbox, blocked, err = preflightOutbox(outbox)
		if err != nil {
			return Summary{}, err
		}
	}
	// Bound the push. The hub caps one request at maxSyncOutboxItems and answers 413 with
	// "split the push into smaller batches", but the client had no batching, so a vault
	// with more dirty notes than the cap 413'd on EVERY round and no state ever advanced:
	// permanently unsyncable, with the remedy addressed to a caller that could not follow
	// it. Reachable by `mesh join` into a directory that already holds a large vault, or a
	// deleted .mesh/sync.json on a vault that has since grown.
	//
	// Truncating rather than looping keeps this change small and each round durable: the
	// batch lands, state advances, and the next round picks up the remainder. A watch
	// daemon converges on its own; a manual `mesh sync` reports what is left.
	//
	// The remainder only comes back if it stays DIRTY. The new base below is recomputed
	// from the whole disk, and the truncated notes are still on disk, so their current
	// hashes would become the base: the next computeOutbox sees prev[rel] == h and emits
	// nothing, and keepWindowEditsDirty cannot rescue them either (sentHashes is the full
	// on-disk map, so current[rel] == sentHashes[rel] and no restore fires). The deferred
	// notes were then never pushed to the team while Summary reported convergence, and
	// since deletes are appended last they truncate FIRST, so a locally deleted note stayed
	// alive on the hub forever. deferred carries those paths to keepDeferredDirty below.
	var deferred []string
	if pullFirst {
		for _, it := range outbox {
			deferred = append(deferred, it.Path)
		}
		outbox = nil
	} else if len(outbox) > maxOutboxPerSync {
		for _, it := range outbox[maxOutboxPerSync:] {
			deferred = append(deferred, it.Path)
		}
		outbox = outbox[:maxOutboxPerSync]
		slog.Info("sync: pushing a bounded batch; the rest follow next round",
			"batch", len(outbox), "remaining", len(deferred))
	}
	reuseEvents, err := teamtelemetry.Pending(vaultDir, creds.VaultID, teamtelemetry.MaxPerSync)
	if err != nil {
		slog.Warn("sync: could not read the private team-reuse outbox; note sync continues", "err", err)
	}
	client := NewWithContext(ctx, creds.HubURL, creds.Token)
	client.UseZstdSyncRequests(state.RequestZstd)
	resp, err := client.Sync(syncproto.SyncRequest{
		BaseSHA: state.HeadSHA, Outbox: outbox, TombstoneSeq: state.TombSeq, ReuseEvents: reuseEvents,
	})
	if err != nil {
		return Summary{}, err
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	if err := validateSyncResponse(vaultDir, resp, outbox); err != nil {
		return Summary{}, err
	}
	if err := teamtelemetry.Ack(vaultDir, creds.VaultID, resp.AckedReuseEventIDs); err != nil {
		// The hub deduplicates by event id, so retaining an acknowledged event only
		// causes a harmless retry. Note convergence must not fail for telemetry cleanup.
		slog.Warn("sync: could not remove acknowledged team-reuse events", "err", err)
	}
	// Build the protected set BEFORE applying deltas. Three things go in it, for the same
	// reason: applyDeltas must not overwrite local bytes that nothing else is holding.
	//   - paths the hub REJECTED (the contract says the client keeps its local copy)
	//   - paths the bounded batch DEFERRED (the hub never saw those bytes this round)
	//   - paths whose conflict sibling could not be written (nothing holds the loser)
	protected := make(map[string]pathProtection, len(resp.Rejected)+len(deferred))
	protectOutbox := func(rel string) {
		p := protected[rel]
		switch outboxOps[rel] {
		case "delete":
			p.missing = true
		case "upsert":
			p.present = true
		}
		protected[rel] = p
	}
	for _, rel := range resp.Rejected {
		protectOutbox(rel)
	}
	if pullFirst {
		for rel := range outboxOps {
			protectOutbox(rel)
		}
	}
	// A deferred path sits in exactly the position a rejected one does: the hub did not
	// see our version, so any delta it sends for that path is built on a base that does
	// not contain the local change. Unprotected, applyDeltas compares disk against
	// sentHashes, finds them equal (sentHashes is the on-disk map, dirty files included),
	// concludes nothing changed locally, and writes the hub version straight over the
	// deferred edit with no conflict and no sibling. keepDeferredDirty cannot undo that:
	// the local bytes are already gone. Reachable whenever a truncated round is also a
	// full reconcile (a reset sync.json on a grown vault), because then the hub sends a
	// delta for every path while our push is capped.
	for _, rel := range deferred {
		protectOutbox(rel)
	}
	for _, note := range blocked {
		protectOutbox(note.Path)
	}
	// Preserve our losing versions locally before deltas overwrite the paths.
	unparked, err := writeConflictSiblings(vaultDir, resp.Conflicts)
	if err != nil {
		return Summary{}, err
	}
	for _, rel := range unparked {
		p := protected[rel]
		p.present = true // no sibling holds this local version: protect its bytes
		protected[rel] = p
	}
	parked, err := applyDeltas(vaultDir, resp.Deltas, sentHashes, protected)
	if err != nil {
		return Summary{}, err
	}
	parked = append(preParked, parked...)
	// Catch a save that landed while phase two's request was in flight. A live
	// upsert in this response supersedes the old tombstone and is deliberately
	// excluded from quarantine.
	if len(suppressTombstones) > 0 {
		lateParked, lateDropped, qerr := quarantineUntrustedTombstones(vaultDir, resp.Deltas, suppressTombstones)
		if qerr != nil {
			return Summary{}, qerr
		}
		parked = append(parked, lateParked...)
		preDropped = append(preDropped, lateDropped...)
	}
	// A full reconcile's deltas are upserts-only, so remove locals the snapshot
	// proves were deleted upstream (else they linger and can resurrect). Must run
	// before the recompute below so the dropped paths leave the persisted hashes.
	dropped := append([]string(nil), preDropped...)
	if resp.FullReconcile {
		var roundDropped []string
		roundDropped, err = dropFullReconcileOrphans(vaultDir, resp.Deltas, resp.Tombstones, state.Hashes, &parked)
		if err != nil {
			return Summary{}, err
		}
		dropped = append(dropped, roundDropped...)
		if pullFirst {
			quarantined, removed, qerr := quarantineUntrustedTombstones(vaultDir, resp.Deltas, resp.Tombstones)
			if qerr != nil {
				return Summary{}, qerr
			}
			parked = append(parked, quarantined...)
			dropped = append(dropped, removed...)
		}
	} else if len(resp.Tombstones) > 0 {
		// Incremental drop-list: prune notes the hub deleted since our last seq, content-
		// safely. A scoped client may never receive the delete delta, so without this it
		// keeps the deleted note locally (serving stale knowledge) and can resurrect it.
		var roundDropped []string
		roundDropped, err = dropTombstoned(vaultDir, resp.Tombstones, state.Hashes, &parked)
		if err != nil {
			return Summary{}, err
		}
		dropped = append(dropped, roundDropped...)
	}
	// Recompute hashes from disk so the next outbox reflects the canonical (post-
	// merge) hub state, not what we optimistically pushed; then keep any
	// guard-parked path dirty so its kept local change re-pushes next sync.
	_, current, err := computeOutbox(vaultDir, map[string]string{})
	if err != nil {
		return Summary{}, err
	}
	keepParkedDirty(current, state.Hashes, parked)
	keepWindowEditsDirty(vaultDir, current, sentHashes, resp.Deltas, dropped)
	// AFTER keepWindowEditsDirty on purpose: that one resets a path to its send-time
	// hash, which for a deferred path is exactly the hash that would mark it synced.
	// Proven by ablation: swapping these two lines puts the never-pushed remainder back.
	keepDeferredDirty(current, state.Hashes, deferred)
	for _, note := range blocked {
		keepDeferredDirty(current, state.Hashes, []string{note.Path})
	}
	// Hub-rejected paths (viewer role, folder ACL, out-of-scope, or oversize/binary
	// content) were NOT landed by the hub. Keep them dirty exactly like a parked path
	// so the next sync re-attempts the push, and never record the local (un-landed)
	// bytes as the synced base. Without this the rejected edit is silently treated as
	// synced, is invisible to the team forever, and is never retried when the user
	// later gains write permission.
	for _, rel := range resp.Rejected {
		if old, ok := state.Hashes[rel]; ok {
			current[rel] = old
		} else {
			delete(current, rel)
		}
	}
	// sync.json is exclusively the PUSH baseline. Keep hub-owned root files and
	// any legacy/hostile non-note key out even if an earlier helper restored one.
	for rel := range current {
		if _, ok := vault.SafeSyncNotePath(rel); !ok {
			delete(current, rel)
		}
	}
	// Carry the vault identity forward. It is what makes a later `mesh join` able to
	// tell "same hub, keep the base" from "different hub, this base is a lie", so a
	// round that dropped it would re-open that hole on the next sync. HubURL is
	// backfilled from the credentials when the state predates the field, which is how a
	// vault joined before this shipped starts being protected without a re-join.
	hubURL := state.HubURL
	if hubURL == "" {
		hubURL = hubBase
	}
	vaultID := state.VaultID
	if vaultID == "" {
		vaultID = creds.VaultID
	}
	if err := writeState(vaultDir, syncState{
		HeadSHA:     resp.HeadSHA,
		Hashes:      current,
		TombSeq:     resp.TombstoneSeq,
		VaultID:     vaultID,
		HubURL:      hubURL,
		RequestZstd: resp.RequestZstd,
	}); err != nil {
		return Summary{}, err
	}
	sum := Summary{Pushed: len(outbox) - len(resp.Rejected), Pulled: len(resp.Deltas), Conflicts: len(resp.Conflicts), Head: resp.HeadSHA, Dropped: dropped, Rejected: resp.Rejected}
	sum.Blocked = blocked
	// Tell the caller work remains, rather than letting a bounded batch look complete.
	sum.Remaining = len(deferred)
	sum.Deferred = append([]string(nil), deferred...)
	for _, c := range resp.Conflicts {
		sum.ConflictSiblings = append(sum.ConflictSiblings, c.SiblingPath)
	}
	for _, p := range parked {
		sum.Protected = append(sum.Protected, p.sibling)
	}
	if pullFirst {
		if afterUntrustedTombstoneQuarantine != nil {
			afterUntrustedTombstoneQuarantine()
		}
		pending, _, perr := computeOutboxContext(ctx, vaultDir, current)
		if perr != nil {
			return sum, perr
		}
		if len(pending) == 0 {
			sum.Remaining = 0
			sum.Deferred = nil
			return sum, nil
		}
		next, nerr := syncVaultRoundContext(ctx, vaultDir, false, resp.Tombstones)
		if nerr != nil {
			return sum, fmt.Errorf("sync: authoritative pull succeeded but survivor push failed: %w", nerr)
		}
		return combineSummaries(sum, next), nil
	}
	return sum, nil
}

// hydrateLegacyVaultID upgrades credentials written before the hub returned a stable
// vault identity. Without it, those long-lived joins never create a team-reuse outbox:
// the privacy boundary scopes every event by vault id and deliberately drops events
// when that id is unknown. Identification is best effort so an old or temporarily
// unavailable /v1/vault endpoint cannot block ordinary note sync. The id becomes live
// only after the private credential file is durably replaced.
func hydrateLegacyVaultID(vaultDir string, creds credentials) credentials {
	return hydrateLegacyVaultIDContext(context.Background(), vaultDir, creds)
}
func hydrateLegacyVaultIDContext(ctx context.Context, vaultDir string, creds credentials) credentials {
	if creds.VaultID != "" {
		return creds
	}
	vi, err := NewWithContext(ctx, creds.HubURL, creds.Token).Vault()
	if err != nil {
		slog.Warn("sync: could not identify this legacy join; cross-user reuse telemetry remains disabled", "err", err)
		return creds
	}
	vaultID := strings.TrimSpace(vi.VaultID)
	if vaultID == "" {
		slog.Warn("sync: hub returned no vault identity; cross-user reuse telemetry remains disabled")
		return creds
	}
	next := creds
	next.VaultID = vaultID
	if err := writeCredentials(vaultDir, next); err != nil {
		slog.Warn("sync: could not persist the joined vault identity; cross-user reuse telemetry remains disabled", "err", err)
		return creds
	}
	return next
}

func combineSummaries(first, second Summary) Summary {
	head := second.Head
	if head == "" {
		head = first.Head
	}
	return Summary{
		Pushed:           first.Pushed + second.Pushed,
		Pulled:           first.Pulled + second.Pulled,
		Conflicts:        first.Conflicts + second.Conflicts,
		Head:             head,
		ConflictSiblings: append(first.ConflictSiblings, second.ConflictSiblings...),
		Protected:        append(first.Protected, second.Protected...),
		Dropped:          append(first.Dropped, second.Dropped...),
		Rejected:         append(first.Rejected, second.Rejected...),
		Blocked:          append([]BlockedNote(nil), second.Blocked...), // second round rechecks all surviving dirty notes
		Remaining:        second.Remaining,
		Deferred:         append([]string(nil), second.Deferred...),
	}
}

// JoinVault redeems an invite, stores credentials, verifies embedding
// homogeneity against the hub-authoritative mesh.toml, and clones the vault via a
// reconcile from an empty base.
func JoinVault(hubURL, invite, vaultDir string) (Summary, error) {
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		return Summary{}, err
	}
	release, err := acquireVaultSyncLock(vaultDir)
	if err != nil {
		return Summary{}, err
	}
	defer release()
	// Read what this directory was joined to BEFORE writeCredentials overwrites it.
	// The old hub URL is one of the two ways we can recognise a re-join that switches
	// hubs, and it only exists until the next line runs.
	prevCreds, _ := readCredentials(vaultDir) // missing file = never joined; zero value is right
	prevState := readState(vaultDir)

	c := New(hubURL, "")
	jr, err := c.Join(invite)
	if err != nil {
		return Summary{}, err
	}
	hubBase := strings.TrimRight(hubURL, "/")
	c.Token = jr.ClientToken
	vi, err := c.Vault()
	if err != nil {
		return Summary{}, err
	}
	if err := checkHomogeneity(vi.MeshToml); err != nil {
		return Summary{}, err
	}
	vaultID := jr.VaultID
	if vaultID == "" {
		vaultID = vi.VaultID
	}
	// Point the sync BASE at the hub we just joined, not the one we used to be joined
	// to. Join rewrote .mesh/credentials and left .mesh/sync.json alone, so re-joining
	// an already-joined directory made hub A's hash map the push baseline for hub B:
	// computeOutbox diffs against it, so every note not edited since was seen as
	// already synced and skipped, the full reconcile kept the local files, and the
	// recompute wrote those same hashes back as the new base. Recorded as synced,
	// never sent, and it does not self-heal, because nothing ever re-dirties a path
	// that both sides believe is settled. TombSeq is the same shape and worse: the hub
	// echoes the client's floor back (internal/hub/sync.go), so hub B's own deletes
	// stay invisible until its sequence passes hub A's. Reachable by doing the
	// documented thing on a rebuilt hub, a hub migration, or a hub.db re-bootstrap.
	//
	// A zero base is the safe answer: the hub replies with a full snapshot and the
	// client pushes everything it holds, which is what "join and clone" already means.
	// The identity is stamped either way, so a state file written before this existed
	// starts carrying it after one join and the comparison gets sharper, not weaker.
	switching := joinTargetsAnotherVault(prevState, prevCreds.HubURL, hubBase, vaultID)
	if switching {
		// Neutralise the old hub's base BEFORE publishing the new credentials. A
		// crash between these two durable renames then leaves either the intact old
		// pair or old credentials with an empty (safe) base, never new credentials
		// paired with the old hub's hashes/cursor.
		prevState = syncState{Hashes: map[string]string{}}
		if err := writeState(vaultDir, prevState); err != nil {
			return Summary{}, err
		}
	}
	if err := writeCredentials(vaultDir, credentials{HubURL: hubBase, Token: jr.ClientToken, VaultID: vaultID}); err != nil {
		return Summary{}, err
	}
	prevState.VaultID = vaultID
	prevState.HubURL = hubBase
	if err := writeState(vaultDir, prevState); err != nil {
		return Summary{}, err
	}
	// Fresh directory: base "" -> the hub returns a full snapshot.
	return syncVaultLocked(vaultDir)
}

// joinTargetsAnotherVault reports whether a join is pointing an ALREADY-joined
// directory at a different vault than the one .mesh/sync.json describes, which makes
// the stored base (HeadSHA, Hashes, TombSeq) wrong rather than merely stale.
//
// The vault id is the authority when both sides have one: it survives a hub moving to
// a new URL, and it changes when a hub is rebuilt at the same URL. The URL is the
// fallback for a state file written before the id was recorded, and for a hub that
// reports no id at all. An empty stored base is nothing to invalidate, so a first join
// into an empty directory is never treated as a switch.
func joinTargetsAnotherVault(prev syncState, prevHubURL, hubURL, vaultID string) bool {
	if prev.HeadSHA == "" && len(prev.Hashes) == 0 && prev.TombSeq == 0 {
		return false
	}
	// A newly known identity cannot authenticate a legacy base that carried no
	// identity at all. The URL may be unchanged across an in-place hub rebuild;
	// preserving its hashes/cursor would then silently skip notes and deletes.
	if prev.VaultID == "" && vaultID != "" {
		return true
	}
	if prev.VaultID != "" && vaultID != "" {
		return prev.VaultID != vaultID
	}
	known := prev.HubURL
	if known == "" {
		known = strings.TrimRight(prevHubURL, "/")
	}
	return known != "" && known != hubURL
}

// checkHomogeneity fails closed if the vault's canonical embedding space (from the
// synced mesh.toml [embedding] section) conflicts with the operator's configured
// MESH_EMBED_MODEL / MESH_EMBED_DIM (SPEC 8: one embedding space per team). Both
// axes matter: two endpoints can serve the same model NAME at different widths (a
// requantized or truncated variant), which passes the name check but would later
// cosine across incompatible dimensions and emit a silent uniform garbage signal.
func checkHomogeneity(meshToml string) error {
	model := tomlSectionString(meshToml, "embedding", "model")
	env := strings.TrimSpace(os.Getenv("MESH_EMBED_MODEL"))
	if model != "" && env != "" && model != env {
		return fmt.Errorf("embedding model mismatch: this vault uses %q but MESH_EMBED_MODEL=%q; align them before joining (fail closed)", model, env)
	}
	if vd := tomlSectionString(meshToml, "embedding", "dim"); vd != "" && vd != "0" {
		if ed := strings.TrimSpace(os.Getenv("MESH_EMBED_DIM")); ed != "" && ed != "0" && ed != vd {
			return fmt.Errorf("embedding dim mismatch: this vault uses dim %s but MESH_EMBED_DIM=%s; align them before joining (fail closed)", vd, ed)
		}
	}
	return nil
}

// tomlSectionString pulls a simple `key = "value"` from inside a named [section]
// of the small, hub-written mesh.toml (keys before any section header are the
// top-level scope; pass section "" for those). Section-aware so a future section
// reusing a key name (e.g. a [rerank] model) cannot shadow the [embedding] one.
// Not a general TOML parser.
func tomlSectionString(toml, section, key string) string {
	cur := ""
	for _, line := range strings.Split(toml, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if cur != section {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"`)
	}
	return ""
}

// maxOutboxPerSync mirrors the hub's maxSyncOutboxItems. Kept slightly under it so a
// client a version ahead of its hub still fits.
const maxOutboxPerSync = 4000
