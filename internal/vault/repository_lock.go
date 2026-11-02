// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// RepositoryOperationLockName is shared by cooperating Hub and UI publishers.
const RepositoryOperationLockName = "mesh-operation.lock"

// AcquireRepositoryOperationLock serializes cooperating processes over an ordinary
// Git vault. It does not fence arbitrary editors or writers which do not call it.
// The caller must not already hold this lock, and must release it before waiting
// for indexing or other callbacks which may themselves need repository access.
func AcquireRepositoryOperationLock(ctx context.Context, dir string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(".git")
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("repository operation lock requires an ordinary .git directory")
	}
	f, err := root.OpenFile(".git/"+RepositoryOperationLockName, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0600)
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err := tryRepositoryOperationFile(f)
		if err == nil {
			if err := ctx.Err(); err != nil {
				_ = unlockRepositoryOperationFile(f)
				_ = f.Close()
				return nil, err
			}
			var once sync.Once
			return func() { once.Do(func() { _ = unlockRepositoryOperationFile(f); _ = f.Close() }) }, nil
		}
		if !repositoryOperationBusy(err) {
			_ = f.Close()
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
