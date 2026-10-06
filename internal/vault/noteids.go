// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// idScanBytes bounds how much of a file NoteIDForFile reads looking for the closing
// frontmatter marker. Frontmatter sits at the very top of a note and Mesh's own writer
// renders well under a kilobyte of it, so this reads the whole block for every real note
// while a large vault costs a bounded head read per file instead of a full slurp.
const idScanBytes = 64 << 10

// NoteIDForFile returns the note id a markdown file claims: its frontmatter `id:` when it
// declares one, otherwise the lowercased basename without the extension. This MUST agree
// with index.effectiveID, which is what the indexer keys notes.id and the graph's
// "note:"+id node on; internal/index/dupid_consistency_test.go enumerates a vault and
// asserts the two answer the same thing for every file in it.
//
// A file whose frontmatter will not parse, or whose block is not closed within the head
// read, falls back to the basename. That is deliberately conservative for the caller that
// matters (CreateNote, claiming a free id): the fallback can only make an id look TAKEN
// that might have been free, never free that is taken.
func NoteIDForFile(path string) string {
	id, _ := noteIDForFile(path)
	return id
}

// noteIDForFile is NoteIDForFile plus whether the file DECLARED that id in its frontmatter
// or merely fell back to its filename.
//
// The two are not interchangeable when something has to move. A declared id is the
// author's ground truth and no tool here rewrites one; a fallback id is an accident of
// where the file was saved, and it is the only one a migration can safely change. So when
// a declared id and a fallback id land on the same string, the declarer is the holder and
// the fallback is what has to yield, regardless of walk order.
func noteIDForFile(path string) (id string, declared bool) {
	id, declared, _ = noteIDForFileContext(context.Background(), path)
	return id, declared
}

// noteIDForFileContext is noteIDForFile with cancellation threaded through the bounded
// head read. Ordinary read failures retain noteIDForFile's conservative filename
// fallback; cancellation is different because silently falling back would let an
// expensive whole-vault scan continue after its caller has shut down.
func noteIDForFileContext(ctx context.Context, path string) (id string, declared bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	key := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	head, err := ReadFileHeadContext(ctx, path, idScanBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, ctxErr
		}
		return key, false, nil
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	id, declared = noteIDFromHead(key, head)
	return id, declared, nil
}

func noteIDFromHead(key string, head []byte) (string, bool) {
	fmText, _, had := SplitFrontmatter(string(head))
	if !had {
		return key, false
	}
	// Only the id is decoded. A note with, say, an unquoted colon in `updated:` fails to
	// unmarshal as a whole, and asking for the full Frontmatter would throw away the id
	// of every note that has any other schema problem.
	var probe struct {
		ID string `yaml:"id"`
	}
	if err := yaml.Unmarshal([]byte(fmText), &probe); err != nil {
		return key, false
	}
	if id := strings.TrimSpace(probe.ID); id != "" {
		return id, true
	}
	return key, false
}

// createNoteDirs is the complete, fixed set of destinations in which CreateNote can
// publish. The pre-publication ClaimedIDs scan already catches notes in every custom
// vault folder; the post-publication check only has to find another CreateNote that raced
// that scan, and such a racer can appear only in a canonical type directory or inbox.
var createNoteDirs = [...]string{
	"decisions",
	"gotchas",
	"post-mortems",
	"entities",
	"concepts",
	"maps",
	"notes",
	"statuses",
	"inbox",
}

// otherFileNamedForID retains the non-context compatibility surface used by existing
// package callers.
func otherFileNamedForID(root, id, ownPath string) string {
	other, _ := otherFileNamedForIDContext(context.Background(), root, id, ownPath)
	return other
}

// otherFileNamedForIDContext returns the vault-relative path of a regular <id>.md
// published by a concurrent CreateNote in another type directory, or "" when ownPath is
// the only one. It checks the fixed destination set instead of a second
// full-vault traversal after O_EXCL. Each stat is isolated so a cancelled caller does not
// wait on a stalled FUSE/network filesystem; the goroutine is read-only and publishes
// only to its private buffered channel, so it can never mutate the vault after return.
func otherFileNamedForIDContext(ctx context.Context, root, id, ownPath string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, dir := range createNoteDirs {
		p := filepath.Join(root, dir, id+".md")
		if p == ownPath {
			continue
		}
		info, err := LstatContext(ctx, p)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if err == nil && info.Mode().IsRegular() {
			if rel, rerr := filepath.Rel(root, p); rerr == nil {
				return rel, nil
			}
			return p, nil
		}
	}
	return "", nil
}

// candidateID renders the n-th id in the sequence base, base-2, base-3 ... that every
// writer in this package mints ids from.
//
// It is one function on purpose. CreateNote walks the sequence while claiming files with
// O_EXCL, IDClaims walks it while rewriting notes in place, and the two have to agree on
// the shape: if one of them stepped around "readme-2" and the other minted "readme_2",
// each would read the id the other reserved as free and the collision they both exist to
// prevent would come straight back the first time a vault saw both writers.
func candidateID(base string, n int) string {
	if n <= 1 {
		return base
	}
	return base + "-" + strconv.Itoa(n)
}

// fallbackIDBase is the id base used when a title or a filename slugs to nothing (a note
// called "..." or "___"). Without it every such file mints the SAME empty id, which is
// the collision this whole file exists to prevent, dressed as a formatting quirk.
const fallbackIDBase = "note"

// IDClaims is a vault's note ids and the files holding them, carried across a pass that
// mints ids into MANY files so each one gets an id no other file holds.
//
// It exists because a note id is vault-GLOBAL while the thing a writer looks at is one
// file. `mesh migrate --apply` synthesized an id per file from that file's basename alone,
// so a vault with two README.md in different folders came out of a migration with `id:
// readme` written into BOTH, and the migration is precisely what `mesh doctor` tells the
// operator to run when it finds that collision: the remedy cemented the defect on disk and
// doctor then printed STALE over an index no reindex could change.
//
// The whole vault is scanned ONCE, up front, so a pass over 1200 notes costs one walk
// rather than one per file. Claims made during the pass are recorded as they are handed
// out, so two files that would mint the same id inside a single pass step around each
// other too, not just around the notes that were already on disk.
type IDClaims struct {
	root    string
	claimed map[string]idHolder // note id -> the file holding it
}

// LoadIDClaims scans root for every note id already claimed anywhere in it.
//
// An unreadable corner of the vault is skipped rather than failing the scan, which is
// ClaimedIDs' behavior and the right one here for the same reason: an incomplete id scan
// degrades to the old per-file guess for the notes it could not see, while a failed scan
// would refuse to migrate the entire vault.
func LoadIDClaims(root string) (*IDClaims, error) {
	claimed, err := claimedIDHolders(root)
	if err != nil {
		return nil, err
	}
	return &IDClaims{root: root, claimed: claimed}, nil
}

// owner returns the key IDClaims records a file under: its vault-relative path, matching
// what ClaimedIDs stores, so "this id is already held by me" can be told apart from "this
// id is held by some other file".
func (c *IDClaims) owner(path string) string {
	if rel, err := filepath.Rel(c.root, path); err == nil {
		return rel
	}
	return path
}

// Claim hands path the first id in base's candidate sequence that no OTHER file in the
// vault holds, and records the claim so the rest of the pass steps around it.
//
// A file that already holds the id it is asking for keeps it. That is what makes a
// migration idempotent AND leaves the incumbent alone: the indexer gives a contested id to
// the first file in walk order, a migration pass walks in the same order, so the note that
// is already indexed under the id keeps it and only the shadowed file moves.
//
// The one case where holding an id is not enough to keep it is a file that only holds it
// by filename fallback while some OTHER note declares the same id in its frontmatter.
// Nothing here rewrites a declared id, so the fallback is the only side that can move, and
// claimedIDHolders has already recorded the declarer as the holder for exactly that reason.
//
// Every id this hands out is recorded as DECLARED, because the caller is about to write it
// into the file's frontmatter. A second file asking for the same slug later in the pass
// therefore yields to it instead of taking it back.
func (c *IDClaims) Claim(path, base string) (string, error) {
	if base == "" {
		base = fallbackIDBase
	}
	me := c.owner(path)
	for n := 1; n <= maxIDAttempts; n++ {
		id := candidateID(base, n)
		if holder, taken := c.claimed[id]; !taken || holder.path == me {
			c.claimed[id] = idHolder{path: me, declared: true}
			return id, nil
		}
	}
	return "", fmt.Errorf("could not claim a free note id for %q after %d attempts; %d notes in this vault already hold ids starting with that slug, so give this note an explicit id in its frontmatter",
		base, maxIDAttempts, maxIDAttempts)
}

// idHolder is the file recorded as holding one note id, and how strong its hold is.
type idHolder struct {
	path     string // vault-relative
	declared bool   // the frontmatter says this id, rather than the filename implying it
}

// ClaimedIDs maps every note id currently claimed anywhere in the vault to the
// vault-relative path holding it: the first file in walk order, except that a file which
// DECLARES the id in its frontmatter takes it from one that only falls back to its
// filename (see noteIDForFile).
//
// It exists because a note id is vault-GLOBAL while a note file lives in one type
// directory. CreateNote used to prove an id free by creating decisions/<id>.md with
// O_EXCL, which says nothing about gotchas/<id>.md, so a gotcha and a decision with the
// same title produced two files with one id and a success receipt for both. Which of the
// two you could then retrieve flipped depending on whether the watcher or `mesh index`
// ran last, and one of them was invisible either way.
// An unreadable file or directory is SKIPPED rather than failing the scan. Walk returns
// the first such error, and using it here made an unreadable folder anywhere in the vault
// fail every note write, which is a strictly worse outcome than the id scan being
// incomplete: the write is the thing the flywheel cannot afford to lose, the note file
// itself is written atomically either way, and a duplicate that slips through an
// unreadable corner is still caught and quarantined by the indexer.
func ClaimedIDs(root string) (map[string]string, error) {
	return ClaimedIDsContext(context.Background(), root)
}

// ClaimedIDsContext is ClaimedIDs with cancellation checked during traversal and each
// bounded note-head read. On cancellation it returns no partial claim set: treating a
// prefix of the vault as complete could let a writer publish a duplicate id.
func ClaimedIDsContext(ctx context.Context, root string) (map[string]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	holders, err := claimedIDHoldersContext(ctx, root)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	out := make(map[string]string, len(holders))
	for id, h := range holders {
		out[id] = h.path
	}
	// Preserve ClaimedIDs' historical partial-map behavior for ordinary traversal
	// errors. Cancellation alone suppresses the partial set, because publishing from
	// one would be unsafe.
	return out, err
}

// claimedIDHolders is ClaimedIDs keeping the declared/fallback distinction its callers in
// this package need to decide which of two files holding one id is the one that must move.
func claimedIDHolders(root string) (map[string]idHolder, error) {
	return claimedIDHoldersContext(context.Background(), root)
}

func claimedIDHoldersContext(ctx context.Context, root string) (map[string]idHolder, error) {
	return claimedIDHoldersWith(ctx, root, filepath.WalkDir, noteIDForFileContext)
}

type idHoldersResult struct {
	holders map[string]idHolder
	err     error
}

func claimedIDHoldersWith(
	ctx context.Context,
	root string,
	walkDir func(string, fs.WalkDirFunc) error,
	readID func(context.Context, string) (string, bool, error),
) (map[string]idHolder, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// WalkDir itself has no context and may block between callbacks while reading an
	// unhealthy FUSE/network directory. Keep the complete scan and its partial map in a
	// read-only worker so cancellation can stop the caller's wait. The buffered result
	// lets that worker exit later without publishing into caller-owned state.
	if ctx.Done() == nil {
		return scanClaimedIDHolders(ctx, root, walkDir, readID)
	}
	result := make(chan idHoldersResult, 1)
	go func() {
		holders, err := scanClaimedIDHolders(ctx, root, walkDir, readID)
		result <- idHoldersResult{holders: holders, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case got := <-result:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return got.holders, got.err
	}
}

func scanClaimedIDHolders(
	ctx context.Context,
	root string,
	walkDir func(string, fs.WalkDirFunc) error,
	readID func(context.Context, string) (string, bool, error),
) (map[string]idHolder, error) {
	return scanClaimedIDHoldersWorkers(ctx, root, walkDir, readID, idScanWorkers)
}

// Four readers overlap local filesystem latency without scaling open files or
// concurrent YAML decoders with vault size. There is deliberately no metadata or
// index cache: preserved mtimes, unindexed notes and custom folders still count.
const idScanWorkers = 4

func scanClaimedIDHoldersWorkers(
	ctx context.Context,
	root string,
	walkDir func(string, fs.WalkDirFunc) error,
	readID func(context.Context, string) (string, bool, error),
	workers int,
) (map[string]idHolder, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var paths []string
	err := walkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// Unreadable directory or vanished entry: skip it, keep scanning.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if skipDir(path, root, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") || IsConflictSibling(d.Name()) {
			return nil
		}
		// WalkDir also reports symlinks, sockets, and named pipes as non-directories.
		// Type uses the directory entry Mesh already has and cannot add a blocking stat;
		// a zero/unknown type proceeds to the isolated cancellable head read, while every
		// definitely non-regular entry is skipped without opening it.
		if !d.Type().IsRegular() {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	type result struct {
		id       string
		declared bool
		err      error
	}
	results := make([]result, len(paths))
	workers = min(max(workers, 1), len(paths))
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			for i := start; i < len(paths); i += workers {
				if ctx.Err() != nil {
					return
				}
				id, declared, readErr := readID(ctx, paths[i])
				results[i] = result{id, declared, readErr}
				if readErr != nil {
					return
				}
			}
		}(worker)
	}
	// Each worker owns disjoint result slots; publish nothing before they finish.
	// claimedIDHoldersWith isolates this whole read-only pass when cancellable,
	// so a stalled filesystem cannot keep its caller waiting after cancellation.
	wg.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	out := map[string]idHolder{}
	for i, path := range paths {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		id, declared, readErr := results[i].id, results[i].declared, results[i].err
		if readErr != nil {
			return out, readErr
		}
		if id == "" {
			continue
		}
		rel := path
		if r, rerr := filepath.Rel(root, path); rerr == nil {
			rel = r
		}
		// First in walk order wins, matching the indexer's incumbent rule, EXCEPT that a
		// declared id outranks a filename fallback whenever the two collide. Without that
		// exception a vault with README.md (no frontmatter) and keyed/overview.md
		// (`id: readme`) records README.md as the holder, a migration then leaves
		// README.md's fallback id alone as "already mine", and the vault comes out of the
		// migration with two notes claiming readme: exactly the state the migration was
		// run to clear. The declared id is the one nothing here may rewrite, so it has to
		// be the one that stays put.
		if prev, taken := out[id]; !taken || (declared && !prev.declared) {
			out[id] = idHolder{path: rel, declared: declared}
		}
		// Merge in traversal order, NEVER completion order. A faster reader
		// must not change which declared duplicate is the incumbent.
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return out, err
	}
	return out, nil
}
