// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"errors"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

// readerVersion is called only under reloadMu. Probe failure is a cache miss,
// not a reason to stop refreshing. Reconnection invalidates the old stamp because
// SQLite data_version values from different connections cannot be compared.
func (s *Server) readerVersion(ctx context.Context) (index.ReaderVersion, bool) {
	if s.refreshClosed {
		return index.ReaderVersion{}, false
	}
	if s.changeMonitor == nil {
		s.viewReusable = false
		monitor, err := s.store.NewChangeMonitor(ctx)
		if err != nil {
			return index.ReaderVersion{}, false
		}
		s.changeMonitor = monitor
	}
	version, err := s.changeMonitor.ReaderVersion(ctx)
	if err != nil {
		s.viewReusable = false
		if errors.Is(err, index.ErrMonitorIndexReplaced) {
			return index.ReaderVersion{}, false // do not relabel pooled old-file readers with a new monitor
		}
		_ = s.changeMonitor.Close()
		s.changeMonitor = nil
		return index.ReaderVersion{}, false
	}
	return version, true
}

// The monitor brackets ALL database reads in graph/retriever construction. A
// retrieval commit during construction leaves the installed snapshot usable but
// uncached; the next pass must reload. No later stamp may bless an older graph.
// Configuration is different: the fingerprint names the immutable inputs actually
// consumed by construction, so a racing A -> B -> A file edit cannot label a B
// retriever as A. Config read failures also disable reuse.
func (s *Server) rememberReaderVersion(ctx context.Context, before index.ReaderVersion, valid bool, in *retrieve.ConfigInputs) {
	s.viewReusable = false
	if !valid || !s.viewBuildComplete {
		return
	}
	fingerprint, reusable := in.Fingerprint()
	if !reusable {
		return
	}
	after, ok := s.readerVersion(ctx)
	if !ok || before != after {
		return
	}
	s.viewVersion, s.viewConfig, s.viewReusable = after, fingerprint, true
}

// reuseAcknowledgement runs only under reloadMu. The installed graph/retriever
// was built wholly inside viewVersion (see rememberReaderVersion). If that same
// monitor still reports viewVersion before AND after the exact path/hash query,
// the query describes the already-installed snapshot, without rebuilding it.
// Notes, code/graph links, vectors, schema and retrieval configuration all retain
// the ordinary refresh invalidation contract. Probe failure is never success.
// The caller must still verify the final database row and current file bytes.
func (s *Server) reuseAcknowledgement(ctx context.Context, before index.ReaderVersion, valid bool, noteID, notePath, noteHash string) (matched, reused bool, err error) {
	if !valid || !s.viewReusable || before != s.viewVersion {
		return false, false, nil
	}
	inputs, err := retrieve.LoadConfigInputsWithOptions(ctx, s.store.MeshDir(), s.retrievalOptions)
	if err != nil {
		return false, false, err
	}
	fingerprint, reusable := inputs.Fingerprint()
	if !reusable || fingerprint != s.viewConfig {
		return false, false, nil
	}
	monitor := s.changeMonitor
	matched, err = s.store.NoteVersionMatchesContext(ctx, noteID, notePath, noteHash)
	if err != nil {
		return false, false, err
	}
	if s.afterCachedNoteVersion != nil {
		s.afterCachedNoteVersion()
	}
	after, valid := s.readerVersion(ctx)
	if !valid || !s.viewReusable || s.changeMonitor != monitor || after != before {
		return false, false, ctx.Err()
	}
	return matched, true, ctx.Err()
}
