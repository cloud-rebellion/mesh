// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
)

var skipDirs = map[string]bool{
	".git": true, ".mesh": true, "node_modules": true,
	"_archive": true,
}

// skipDir reports whether a directory (by name, relative to root) should be
// pruned from a vault traversal. Shared by Walk and Dirs so the watcher watches
// exactly the directories the indexer reads.
func skipDir(path, root, name string) bool {
	if path != root && strings.HasPrefix(name, ".") {
		return true
	}
	return skipDirs[name]
}

// Walk returns every markdown file under root, skipping noise and hidden dirs.
// This is the M0 walker; .meshignore support lands with the index step.
func Walk(root string) ([]string, error) {
	return WalkContext(context.Background(), root)
}

// WalkContext is Walk with caller-controlled cancellation. Cancellation is checked at
// every visited entry and after the traversal, and a cancelled walk never returns a
// partial file set that a caller could mistake for the whole vault.
func WalkContext(ctx context.Context, root string) ([]string, error) {
	return walkContext(ctx, root, filepath.WalkDir)
}

func walkContext(ctx context.Context, root string, walkDir func(string, fs.WalkDirFunc) error) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// filepath.WalkDir has no context and may stall between callbacks while reading an
	// unhealthy FUSE/network directory. Keep the traversal and its partial slice private
	// to a read-only worker so a cancellable owner can stop waiting and release its lock.
	// The callback below observes ctx once the filesystem returns; the buffered result
	// lets that late worker exit without touching caller-owned state.
	if ctx.Done() == nil {
		return scanWalkContext(ctx, root, walkDir)
	}
	type result struct {
		paths []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		paths, err := scanWalkContext(ctx, root, walkDir)
		done <- result{paths: paths, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case got := <-done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return got.paths, got.err
	}
}

func scanWalkContext(ctx context.Context, root string, walkDir func(string, fs.WalkDirFunc) error) ([]string, error) {
	var out []string
	err := walkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(path, root, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".md") && !IsConflictSibling(d.Name()) {
			// A markdown-shaped directory entry is not necessarily a note. Known FIFOs,
			// symlinks, sockets, and devices are excluded without calling Info: Info is a
			// separate filesystem operation that can itself block on an unhealthy mount.
			// Type()==0 also represents "unknown" on some filesystems; those entries are
			// admitted here and protected by the indexer's isolated cancellable read.
			if d.Type().IsRegular() {
				out = append(out, path)
			}
		}
		return nil
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// IsConflictSibling reports whether a filename is a sync-conflict artifact
// (e.g. note.sync-conflict-20260616-bob-1a2b3c4d.md). These are a copy of a note
// the team-sync hub parked for a human to resolve; they duplicate the original's
// frontmatter id, so the indexer, watcher, and linter must skip them rather than
// trip on a duplicate id or surface a half-merged version in retrieval.
func IsConflictSibling(name string) bool {
	return strings.Contains(name, ".sync-conflict-")
}

// WalkConflictSiblings returns every sync-conflict sibling under root (the
// inverse of Walk's filter), pruning the same noise/hidden dirs. Used by
// `mesh conflicts` to find the parked loser copies that Walk deliberately hides.
func WalkConflictSiblings(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(path, root, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".md") && IsConflictSibling(d.Name()) {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// Dirs returns every directory under root that Walk traverses (root included,
// skipped/hidden dirs pruned). The fsnotify watcher needs these because kqueue
// and inotify watch a single directory at a time, not a tree, so it adds one
// watch per indexed directory and skips the same noise Walk does.
func Dirs(root string) ([]string, error) {
	return DirsContext(context.Background(), root)
}

// DirsContext follows WalkContext's isolated read-only traversal contract. A
// cancelled caller receives no partial directory set; an in-flight OS read may
// finish later, without registering watches or mutating caller-owned state.
func DirsContext(ctx context.Context, root string) ([]string, error) {
	return dirsContext(ctx, root, filepath.WalkDir)
}

func dirsContext(ctx context.Context, root string, walkDir func(string, fs.WalkDirFunc) error) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ctx.Done() == nil {
		return scanDirsContext(ctx, root, walkDir)
	}
	type result struct {
		paths []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		paths, err := scanDirsContext(ctx, root, walkDir)
		done <- result{paths: paths, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case got := <-done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return got.paths, got.err
	}
}

func scanDirsContext(ctx context.Context, root string, walkDir func(string, fs.WalkDirFunc) error) ([]string, error) {
	var out []string
	err := walkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if skipDir(path, root, d.Name()) {
			return fs.SkipDir
		}
		out = append(out, path)
		return nil
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, err
	}
	return out, err
}
