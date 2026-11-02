// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRepositoryOperationLockCancellationAndRelease(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireRepositoryOperationLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if unlock, err := AcquireRepositoryOperationLock(ctx, dir); !errors.Is(err, context.DeadlineExceeded) || unlock != nil {
		t.Fatalf("queued acquisition did not honor cancellation: %v", err)
	}
	release()
	release() // repeated early-return/deferred cleanup must not close another FD
	unlock, err := AcquireRepositoryOperationLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestRepositoryOperationLockRefusesEscapedMetadata(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	if release, err := AcquireRepositoryOperationLock(context.Background(), dir); err == nil || release != nil {
		t.Fatal("symlink metadata admitted a different lock domain")
	}
	if _, err := os.Stat(filepath.Join(outside, RepositoryOperationLockName)); !os.IsNotExist(err) {
		t.Fatal("refusal touched the external directory")
	}
}
