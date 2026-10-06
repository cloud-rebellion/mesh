// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareNoteContextRendersWithoutPublishing(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.md"), []byte("# Vault\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := PrepareNoteContext(context.Background(), root, completeFixtureSpec(NewNoteSpec{
		Type: TypeDecision, Title: "Committed by hub", Do: "ship", Dont: "drop", Why: "team",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Result.ID != "committed-by-hub" {
		t.Fatalf("id = %q", p.Result.ID)
	}
	if _, err := os.Stat(p.Result.Path); !os.IsNotExist(err) {
		t.Fatalf("prepare published a file: %v", err)
	}
	fmText, _, had := SplitFrontmatter(string(p.Content))
	fm, _, err := ParseFrontmatter([]byte(fmText))
	if err != nil || !had || fm.ID != p.Result.ID {
		t.Fatalf("prepared bytes do not round-trip: had=%v id=%q err=%v", had, fm.ID, err)
	}
}
