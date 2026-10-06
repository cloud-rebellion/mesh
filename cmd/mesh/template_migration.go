// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/spf13/cobra"
)

func authoringMigrationPreviewCmd() *cobra.Command {
	var root, requestsPath, outPath string
	c := &cobra.Command{Use: "migration-preview", Short: "Preview a reviewed migration without changing historical notes", RunE: func(cmd *cobra.Command, args []string) error {
		var requests []vault.MigrationRequest
		if err := readAuthoringJSONBounded(requestsPath, &requests, 8<<20); err != nil {
			return err
		}
		preview, err := vault.PreviewAuthoringMigration(cmd.Context(), root, requests)
		if err != nil {
			return err
		}
		raw, err := json.MarshalIndent(preview, "", "  ")
		if err != nil {
			return err
		}
		if len(raw) > 8<<20 {
			return fmt.Errorf("preview exceeds 8 MiB")
		}
		if err := saveMigrationPreview(outPath, append(raw, '\n')); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "preview saved to %s\npreview hash: %s\nReview the content, actions, issues and caveats before applying exact IDs.\n", outPath, preview.Hash)
		return nil
	}}
	c.Flags().StringVar(&root, "vault", ".", "vault root")
	c.Flags().StringVar(&requestsPath, "requests", "", "JSON file with up to 32 path/id/spec migration requests")
	c.Flags().StringVar(&outPath, "out", "", "new file for the reviewable preview artifact")
	_ = c.MarkFlagRequired("requests")
	_ = c.MarkFlagRequired("out")
	return c
}

func authoringMigrationApplyCmd() *cobra.Command {
	var root, previewPath, reviewedIDs, previewHash string
	c := &cobra.Command{Use: "migration-apply", Short: "Apply exact reviewed migration IDs against an unchanged preview", RunE: func(cmd *cobra.Command, args []string) error {
		var preview vault.MigrationPreview
		if err := readAuthoringJSONBounded(previewPath, &preview, 8<<20); err != nil {
			return err
		}
		ids := splitCSV(reviewedIDs)
		if len(ids) == 0 || strings.TrimSpace(previewHash) == "" {
			return fmt.Errorf("exact reviewed IDs and preview hash are required")
		}
		receipts, err := vault.ApplyAuthoringMigration(cmd.Context(), root, &preview, vault.MigrationApproval{PreviewHash: previewHash, IDs: ids})
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(receipts)
	}}
	c.Flags().StringVar(&root, "vault", ".", "vault root")
	c.Flags().StringVar(&previewPath, "preview", "", "reviewed preview JSON file")
	c.Flags().StringVar(&reviewedIDs, "reviewed-ids", "", "comma-separated IDs actually reviewed")
	c.Flags().StringVar(&previewHash, "preview-hash", "", "exact hash of the reviewed preview")
	_ = c.MarkFlagRequired("preview")
	_ = c.MarkFlagRequired("reviewed-ids")
	_ = c.MarkFlagRequired("preview-hash")
	return c
}

// saveMigrationPreview claims a new local artifact, without replacing a prior review.
func saveMigrationPreview(path string, raw []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	saved := false
	defer func() {
		if !saved {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(raw); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	saved = true
	return nil
}
