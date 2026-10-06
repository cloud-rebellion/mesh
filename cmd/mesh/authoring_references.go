// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/mcp"
	"github.com/bright-interaction/mesh/internal/vault"
)

// Local authoring checks current confined files rather than depending on a fresh
// index. Drafts use the same reference contract as publication: unresolved links
// stay ordinary prose until there is an existing, audience-compatible target.
func validateLocalAuthoringReferences(ctx context.Context, root string, spec vault.NewNoteSpec) error {
	refs, err := mcp.AuthoringReferences(spec)
	if err != nil || len(refs) == 0 {
		return err
	}
	if err := vault.RequireRoot(root); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	claims, err := vault.ClaimedIDsContext(ctx, root)
	if err != nil {
		return fmt.Errorf("cannot verify reference targets: %w", err)
	}
	audiences := spec.Scope
	if len(audiences) == 0 {
		audiences = []string{vault.DefaultScope}
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		id, anchor, _ := strings.Cut(strings.TrimSpace(ref), "#")
		rel, exists := claims[id]
		if id == "" || len(id) > 256 || !exists || !filepath.IsLocal(rel) {
			return fmt.Errorf("unknown or inaccessible reference")
		}
		data, err := vault.ReadConfinedFileContext(ctx, root, rel, 4<<20)
		if err != nil {
			return fmt.Errorf("unknown or inaccessible reference")
		}
		header, _, had := vault.SplitFrontmatter(string(data))
		fm, _, err := vault.ParseFrontmatter([]byte(header))
		if err != nil || !had || vault.UnterminatedFrontmatter(string(data)) || fm.ID != id {
			return fmt.Errorf("unknown or inaccessible reference")
		}
		for _, audience := range audiences {
			if !vault.ScopeAllows(fm.EffectiveScopes(), map[string]bool{audience: true}) {
				return fmt.Errorf("reference audience mismatch")
			}
		}
		if !mcp.AuthoringReferenceAnchorValid(data, anchor) {
			return fmt.Errorf("invalid reference anchor")
		}
	}
	return nil
}
