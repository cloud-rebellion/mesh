// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// PrepareSnapshotUpdateContext renders through the existing authoring writer
// against one already-authorized immutable snapshot in a private temporary root.
// It never publishes to a real vault, its outbox, history or index. Callers must
// still recheck authority/revision before using the returned candidate.
func PrepareSnapshotUpdateContext(ctx context.Context, before *NoteSnapshot, spec NewNoteSpec) (*PreparedNote, error) {
	if before == nil || before.Frontmatter == nil || before.Revision != ContentRevision(before.Content) {
		return nil, fmt.Errorf("%w: invalid authorized snapshot", ErrInvalidSpec)
	}
	rel, ok := SafeSyncNotePath(before.Path)
	if !ok {
		return nil, fmt.Errorf("%w: invalid authorized note path", ErrInvalidSpec)
	}
	if spec.UpdateID != before.Frontmatter.ID || spec.UpdatePath != before.Path || spec.UpdateRevision != before.Revision {
		return nil, fmt.Errorf("%w: candidate identity differs from snapshot", ErrInvalidSpec)
	}
	root, err := os.MkdirTemp("", "mesh-curation-private-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	target := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(before.Content); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	syncDir(filepath.Dir(target))
	p, err := PrepareNoteContext(ctx, root, spec)
	if err != nil {
		return nil, err
	}
	// Do not expose a deleted temporary-root path as a publication destination.
	p.Result.Path = before.Path
	return p, nil
}
