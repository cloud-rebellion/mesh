// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/bright-interaction/mesh/internal/vault"
)

type authoringScopeRegistryKey struct{}

type authoringScopeRegistry struct {
	registered []string
}

// WithAuthoringScopeRegistry binds hosted authoring to the team's registered
// access scopes independently of whether scope-based read filtering is enabled.
// A missing registry preserves standalone authoring; a present empty registry
// fails closed. The copy keeps later mutations by the caller from changing it.
func WithAuthoringScopeRegistry(ctx context.Context, registered []string) context.Context {
	return context.WithValue(ctx, authoringScopeRegistryKey{}, authoringScopeRegistry{
		registered: append([]string{}, registered...),
	})
}

func validateAuthoringScopeRegistry(ctx context.Context, spec vault.NewNoteSpec) error {
	registry, ok := ctx.Value(authoringScopeRegistryKey{}).(authoringScopeRegistry)
	if !ok {
		return nil
	}
	return ValidateNewAuthoringScopes(spec, registry.registered)
}

// ValidateNewAuthoringScopes rejects invented access partitions on new hosted
// notes, including new drafts. It grants no read or write authority. Existing
// published updates and draft resumptions retain historical labels: the vault
// publication path separately verifies their current identity/revision and
// refuses any change to their scopes before writing.
//
// The hub repeats this check against the current registry under its publication
// lock so a request prepared before a scope was removed cannot publish into it.
func ValidateNewAuthoringScopes(spec vault.NewNoteSpec, registered []string) error {
	if spec.UpdateID != "" || spec.DraftID != "" {
		return nil
	}
	known := make(map[string]bool, len(registered))
	for _, scope := range registered {
		if scope = strings.TrimSpace(scope); scope != "" {
			known[scope] = true
		}
	}
	scopes := (&vault.Frontmatter{Scope: spec.Scope}).EffectiveScopes()
	for _, scope := range scopes {
		if !known[scope] {
			return fmt.Errorf("%w: access scope is not registered; use an existing authorized scope or omit it for your configured default", vault.ErrInvalidSpec)
		}
	}
	return nil
}
