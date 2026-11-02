// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

var errMigrationReceiptOutput = errors.New("fixture output unavailable")

type migrationReceiptFailWriter struct{}

func (migrationReceiptFailWriter) Write([]byte) (int, error) {
	return 0, errMigrationReceiptOutput
}

// A batch can publish its first note and then fail while preparing a later
// journal. The command must expose the first receipt without hiding the failure
// or implying that the failed second entry was retried or published.
func TestAuthoringCLIMigrationPreservesPartialReceipts(t *testing.T) {
	for _, outputFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("stdout_failure_%t", outputFails), func(t *testing.T) {
			root := t.TempDir()
			var requests []vault.MigrationRequest
			for _, id := range []string{"first-reviewed", "second-reviewed"} {
				title := strings.ReplaceAll(id, "-", " ")
				original := fmt.Sprintf("---\nid: %s\ntype: note\ntitle: %s\nwhen: 2026-01-01\ncreated: 2026-01-01\nscope: [dev]\n---\n# %s\n\nAn illustrative local migration fixture.\n", id, title, title)
				if err := os.WriteFile(filepath.Join(root, id+".md"), []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
				requests = append(requests, vault.MigrationRequest{Path: id + ".md", ID: id, Spec: vault.NewNoteSpec{
					Template: "finding", TemplateVersion: 1, Title: title,
					Summary: "This illustrative fixture checks partial migration recovery.", Sections: completeFindingSections(),
				}})
			}
			preview, err := vault.PreviewAuthoringMigration(context.Background(), root, requests)
			if err != nil {
				t.Fatal(err)
			}
			previewRaw, err := json.Marshal(preview)
			if err != nil {
				t.Fatal(err)
			}
			previewPath := filepath.Join(t.TempDir(), "preview.json")
			if err := os.WriteFile(previewPath, previewRaw, 0o600); err != nil {
				t.Fatal(err)
			}
			// This failure is independent of OS-user privilege: the writer itself
			// refuses a noneditable destination. Both notes passed preview checks.
			journalDir := filepath.Join(root, ".mesh", "migrations", preview.Hash)
			if err := os.MkdirAll(journalDir, 0o700); err != nil {
				t.Fatal(err)
			}
			blockedJournal := filepath.Join(journalDir, preview.Entries[1].OriginalHash+"-receipt.json")
			const blockedContent = "illustrative blocked journal\n"
			if err := os.WriteFile(blockedJournal, []byte(blockedContent), 0o400); err != nil {
				t.Fatal(err)
			}
			var output, diagnostics bytes.Buffer
			cmd := rootCmd()
			cmd.SetOut(&output)
			if outputFails {
				cmd.SetOut(migrationReceiptFailWriter{})
			}
			cmd.SetErr(&diagnostics)
			cmd.SetArgs([]string{"templates", "migration-apply", "--vault", root,
				"--preview", previewPath, "--preview-hash", preview.Hash,
				"--reviewed-ids", "first-reviewed,second-reviewed"})
			err = cmd.Execute()
			if !errors.Is(err, os.ErrPermission) {
				t.Fatalf("partial apply failure was lost: %v", err)
			}
			for i, entry := range preview.Entries {
				want := entry.Content
				if i == 1 {
					want = entry.Original
				}
				current, readErr := os.ReadFile(filepath.Join(root, entry.Path))
				if readErr != nil || string(current) != want {
					t.Fatalf("entry %d has unexpected publication state: %v", i, readErr)
				}
			}
			blocked, readErr := os.ReadFile(blockedJournal)
			if readErr != nil || string(blocked) != blockedContent {
				t.Fatalf("failed entry's journal was replaced: %v", readErr)
			}
			if outputFails {
				if !errors.Is(err, errMigrationReceiptOutput) || !strings.Contains(err.Error(), "changes may already") {
					t.Fatalf("output failure lost the partial-apply warning or one of its errors: %v", err)
				}
				return
			}
			var receipts []vault.MigrationReceipt
			if err := json.Unmarshal(output.Bytes(), &receipts); err != nil {
				t.Fatalf("partial receipts unavailable on stdout: %v; output=%q", err, output.String())
			}
			if len(receipts) != 1 || receipts[0].ID != "first-reviewed" || receipts[0].State != "applied" || receipts[0].AlreadyApplied {
				t.Fatalf("partial receipt does not identify the completed entry: %+v", receipts)
			}
			original, readErr := os.ReadFile(filepath.Join(root, receipts[0].OriginalPath))
			if readErr != nil || string(original) != preview.Entries[0].Original {
				t.Fatalf("completed entry's exact original is unavailable: %v", readErr)
			}
			if !strings.Contains(diagnostics.String(), "inspect") || !strings.Contains(diagnostics.String(), "before retrying") {
				t.Fatalf("partial migration has no recovery warning: %q", diagnostics.String())
			}
		})
	}
}
