// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

func TestNewAuthoringScopesRequireRegisteredAccessPartitions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spec       vault.NewNoteSpec
		registered []string
		wantErr    bool
	}{
		{"implicit dev", vault.NewNoteSpec{}, []string{"dev"}, false},
		{"registered scope", vault.NewNoteSpec{Scope: []string{"sales"}}, []string{"dev", "sales"}, false},
		{"multiple registered scopes", vault.NewNoteSpec{Scope: []string{"dev", "sales"}}, []string{"dev", "sales"}, false},
		{"one unknown in multiple", vault.NewNoteSpec{Scope: []string{"dev", "invented"}}, []string{"dev", "sales"}, true},
		{"descriptive scope", vault.NewNoteSpec{Scope: []string{"repair the graph"}}, []string{"dev"}, true},
		{"case remains significant", vault.NewNoteSpec{Scope: []string{"Sales"}}, []string{"dev", "sales"}, true},
		{"new draft", vault.NewNoteSpec{Scope: []string{"invented"}, Status: "draft"}, []string{"dev"}, true},
		{"empty registry fails closed", vault.NewNoteSpec{}, nil, true},
		{"existing publication preserved", vault.NewNoteSpec{UpdateID: "historical", Scope: []string{"legacy label"}}, []string{"dev"}, false},
		{"existing draft preserved", vault.NewNoteSpec{DraftID: "historical-draft", Scope: []string{"legacy label"}}, []string{"dev"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNewAuthoringScopes(tc.spec, tc.registered)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v; want error=%v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, vault.ErrInvalidSpec) {
				t.Fatalf("wrong error kind: %v", err)
			}
		})
	}
}

func TestAuthoringScopeRegistryIsIndependentOfReadScopeFilter(t *testing.T) {
	s := newTestServer(t)
	registered := []string{"dev"}
	ctx := WithAuthoringScopeRegistry(WithWriteCapability(context.Background(), true), registered)
	registered[0] = "invented"
	if scopeFromCtx(ctx) != nil {
		t.Fatal("fixture unexpectedly enabled read scoping")
	}
	args := authoringArgs{Title: "Synthetic scope fixture", Template: "finding", Scope: "invented"}
	if _, err := s.authoringSpec(ctx, args, ""); err == nil || !strings.Contains(err.Message, "not registered") {
		t.Fatalf("unscoped hosted author accepted a new label: %v", err)
	}
	privileged := WithScopeFilter(ctx, &ScopeFilter{WriteScope: "dev", CanWrite: func(string) bool { return true }})
	if _, err := s.authoringSpec(privileged, args, ""); err == nil || !strings.Contains(err.Message, "not registered") {
		t.Fatalf("all-scope write capability bypassed the registry: %v", err)
	}
	args.Scope = ""
	if spec, err := s.authoringSpec(ctx, args, ""); err != nil || len(spec.Scope) != 0 {
		t.Fatalf("omitted dev default changed: %+v %v", spec.Scope, err)
	}
	// The registry is a hosted publication contract, not a new restriction on a
	// standalone operator's locally owned vault.
	args.Scope = "operator-owned-label"
	if _, err := s.authoringSpec(context.Background(), args, ""); err != nil {
		t.Fatalf("standalone authoring changed: %v", err)
	}
}
