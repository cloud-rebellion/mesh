// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Same-vault sync is a transaction over the note tree, credentials, and
// sync.json. It must be serial across both goroutines and processes: mesh sync,
// mesh sync --watch, and mesh-curator can all target the same directory.
//
// The process mutex avoids relying on platform-specific same-process advisory
// lock semantics. The OS lock closes the cross-process gap and is released by
// the kernel on crash, so no stale lock file can wedge restart recovery.
type localVaultLock struct {
	gate chan struct{}
	refs int
}

var localVaultLocks = struct {
	sync.Mutex
	byPath map[string]*localVaultLock
}{byPath: make(map[string]*localVaultLock)}

func acquireVaultSyncLock(vaultDir string) (func(), error) {
	return acquireVaultSyncLockContext(context.Background(), vaultDir)
}

func acquireVaultSyncLockContext(ctx context.Context, vaultDir string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := filepath.Abs(vaultDir)
	if err != nil {
		return nil, err
	}
	if resolved, rerr := filepath.EvalSymlinks(key); rerr == nil {
		key = resolved
	}
	key = filepath.Clean(key)

	localVaultLocks.Lock()
	local := localVaultLocks.byPath[key]
	if local == nil {
		local = &localVaultLock{gate: make(chan struct{}, 1)}
		local.gate <- struct{}{}
		localVaultLocks.byPath[key] = local
	}
	local.refs++
	localVaultLocks.Unlock()
	dropReference := func() {
		localVaultLocks.Lock()
		local.refs--
		if local.refs == 0 {
			delete(localVaultLocks.byPath, key)
		}
		localVaultLocks.Unlock()
	}
	select {
	case <-ctx.Done():
		dropReference()
		return nil, ctx.Err()
	case <-local.gate:
	}

	releaseLocal := func() {
		local.gate <- struct{}{}
		localVaultLocks.Lock()
		local.refs--
		if local.refs == 0 {
			delete(localVaultLocks.byPath, key)
		}
		localVaultLocks.Unlock()
	}

	meshDir := filepath.Join(key, ".mesh")
	if err := os.MkdirAll(meshDir, 0o700); err != nil {
		releaseLocal()
		return nil, err
	}
	lockPath := filepath.Join(meshDir, "sync.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		releaseLocal()
		return nil, err
	}
	if err := lockSyncFileContext(ctx, f); err != nil {
		_ = f.Close()
		releaseLocal()
		return nil, fmt.Errorf("lock sync for %s: %w", key, err)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			if err := unlockSyncFile(f); err != nil {
				slog.Warn("sync: failed to release vault file lock", "path", lockPath, "err", err)
			}
			if err := f.Close(); err != nil {
				slog.Warn("sync: failed to close vault file lock", "path", lockPath, "err", err)
			}
			releaseLocal()
		})
	}, nil
}

// No detached blocking-lock goroutine survives a canceled join/sync. Platform
// nonblocking attempts preserve the same lock and wait with caller cancellation.
func lockSyncFileContext(ctx context.Context, f *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, err := tryLockSyncFile(f)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
