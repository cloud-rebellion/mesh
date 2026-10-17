//go:build linux || darwin

// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func admissionMeta(info os.FileInfo, directory bool, private bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || directory != info.IsDir() {
		return false
	}
	uid := uint32(os.Geteuid())
	if private {
		return stat.Uid == uid && (directory && info.Mode().Perm() == 0700 || !directory && info.Mode().IsRegular() && info.Mode().Perm() == 0600 && stat.Nlink == 1)
	}
	// Public ancestors must be trusted; root-owned sticky /tmp is allowed, but
	// the final ledger directory is always private. No ancestor links are used.
	return directory && (stat.Uid == 0 || stat.Uid == uid) && (info.Mode().Perm()&0022 == 0 || stat.Uid == 0 && info.Mode()&os.ModeSticky != 0)
}

func admissionOpenRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, spendRefused()
	}
	r, err := os.OpenRoot("/")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		if part == "" {
			r.Close()
			return nil, spendRefused()
		}
		before, err := r.Lstat(part)
		if err != nil || !admissionMeta(before, true, i == len(parts)-1) {
			r.Close()
			return nil, spendRefused()
		}
		next, err := r.OpenRoot(part)
		if err != nil {
			r.Close()
			return nil, err
		}
		after, err := next.Stat(".")
		r.Close()
		if err != nil || !os.SameFile(before, after) || !admissionMeta(after, true, i == len(parts)-1) {
			next.Close()
			return nil, spendRefused()
		}
		r = next
	}
	return r, nil
}

func admissionOpenFile(r *os.Root, name string, flags int) (*os.File, error) {
	before, err := r.Lstat(name)
	if err != nil || !admissionMeta(before, false, true) {
		return nil, spendRefused()
	}
	f, err := r.OpenFile(name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !admissionMeta(after, false, true) {
		f.Close()
		return nil, spendRefused()
	}
	return f, nil
}

func admissionOpenLocked(ctx context.Context, path string) (*os.Root, func(), error) {
	r, err := admissionOpenRoot(path)
	if err != nil {
		return nil, nil, err
	}
	lock, err := admissionOpenFile(r, "lock", os.O_RDWR)
	if err != nil {
		r.Close()
		return nil, nil, err
	}
	info, err := lock.Stat()
	if err != nil || info.Size() != 0 {
		lock.Close()
		r.Close()
		return nil, nil, spendRefused()
	}
	// Nonblocking flock plus caller cancellation. The file is pre-provisioned,
	// never replaced/deleted, and all participants must use this same inode.
	for {
		if ctx.Err() != nil {
			lock.Close()
			r.Close()
			return nil, nil, ctx.Err()
		}
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			lock.Close()
			r.Close()
			return nil, nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			lock.Close()
			r.Close()
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
	current, err := r.Lstat("lock")
	if err != nil || !os.SameFile(info, current) || !admissionMeta(current, false, true) {
		lock.Close()
		r.Close()
		return nil, nil, spendRefused()
	}
	return r, func() { lock.Close(); r.Close() }, nil // closing releases the kernel lock even on process death
}

func admissionInventory(r *os.Root) error {
	f, err := r.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	entries, err := f.ReadDir(5)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) != 3 {
		return spendRefused()
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Name() != "policy.json" && e.Name() != "state.json" && e.Name() != "lock" {
			return spendRefused()
		}
		seen[e.Name()] = true
	}
	if len(seen) != 3 {
		return spendRefused()
	}
	return nil
}

func admissionRead(r *os.Root, name string, limit int64) ([]byte, error) {
	f, err := admissionOpenFile(r, name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || before.Size() <= 0 || before.Size() > limit {
		return nil, spendRefused()
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) != before.Size() {
		return nil, spendRefused()
	}
	after, err := f.Stat()
	current, e := r.Lstat(name)
	if err != nil || e != nil || !os.SameFile(before, current) || !admissionMeta(after, false, true) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, spendRefused()
	}
	return b, nil
}

func admissionSyncDirectory(r *os.Root) error {
	f, err := r.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func admissionWriteNew(r *os.Root, name string, data []byte) error {
	f, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func admissionCommit(r *os.Root, state responsesSpendState) error {
	return admissionCommitWithDirectorySync(r, state, admissionSyncDirectory)
}

// A per-invocation seam exercises real file write/rename/intent phases without
// mutable global hooks. Production always supplies admissionSyncDirectory.
func admissionCommitWithDirectorySync(r *os.Root, state responsesSpendState, syncDirectory func(*os.Root) error) error {
	if syncDirectory == nil {
		return spendRefused()
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// Before state+directory sync succeeds, failures retain intent/temporary
	// entries and fresh processes refuse them. The final cleanup error below
	// must distinguish already durable state from an uncertain state write.
	if err = admissionWriteNew(r, "intent", []byte("mesh-openai-spend-v1\n")); err != nil {
		return err
	}
	if err = syncDirectory(r); err != nil {
		return err
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	tmp := "state.tmp-" + hex.EncodeToString(random[:])
	if err = admissionWriteNew(r, tmp, b); err != nil {
		return err
	}
	if err = r.Rename(tmp, "state.json"); err != nil {
		return err
	}
	if err = syncDirectory(r); err != nil {
		return err
	}
	if err = r.Remove("intent"); err != nil {
		return err
	}
	if err = syncDirectory(r); err != nil {
		return &admissionCommittedCleanupError{cause: err}
	}
	return nil
}
