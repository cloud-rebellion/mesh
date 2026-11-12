// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
)

func TestUntaggedAuthoringUsesOnlyReadableDistinctiveEntityEvidence(t *testing.T) {
	root := t.TempDir()
	seedVaultFiles(t, root)
	files := map[string]string{
		"crm":                "---\nid: crm\ntype: entity\ntitle: CRM\nscope: [dev]\n---\n# CRM\nTeam CRM application.\n",
		"crm-peer":                 "---\nid: crm-peer\ntype: note\ntitle: CRM member administration\nscope: [dev]\n---\n# CRM member administration\nCRM member administration for the team.\n",
		"crm-private":              "---\nid: crm-private\ntype: note\ntitle: CRM member administration private\nscope: [sales]\n---\n# CRM private notes\nCRM private notes.\n",
		"other":                    "---\nid: other\ntype: note\ntitle: OtherProduct member administration\nscope: [dev]\n---\n# OtherProduct\nOtherProduct administration.\n",
		"private-product":          "---\nid: private-product\ntype: entity\ntitle: PrivateProduct\nscope: [sales]\n---\n# PrivateProduct\nPrivate canonical entity record.\n",
		"private-name-public-peer": "---\nid: private-name-public-peer\ntype: note\ntitle: PrivateProduct public mention\nscope: [dev]\n---\n# PrivateProduct public mention\nPrivateProduct is named without access to its private entity record.\n",
	}
	for id, text := range files {
		if err := os.WriteFile(filepath.Join(root, id+".md"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	seedIndex(t, root)
	s, err := NewServerWithRetrievalOptions(context.Background(), root, retrieve.ConstructionOptions{LocalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.WaitReady(); err != nil {
		t.Fatal(err)
	}
	ctx := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"dev": true}})
	spec := vault.NewNoteSpec{Title: "CRM team administration", Summary: "CRM team administration knowledge", Scope: []string{"dev"}}
	s.deriveAuthoringRelated(ctx, &spec)
	if len(spec.Related) == 0 {
		t.Fatal("untagged note stayed disconnected despite explicit readable entity evidence")
	}
	for _, id := range spec.Related {
		if id != "crm" && id != "crm-peer" {
			t.Fatalf("unrelated or private edge derived: %v", spec.Related)
		}
	}
	private := vault.NewNoteSpec{Title: "PrivateProduct public mention", Scope: []string{"dev"}}
	s.deriveAuthoringRelated(ctx, &private)
	if len(private.Related) != 0 {
		t.Fatalf("inaccessible canonical entity became corroboration: %v", private.Related)
	}
}
