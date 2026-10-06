// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/spf13/cobra"
)

func updateCmd() *cobra.Command {
	var root, specFile, by string
	var validate bool
	c := &cobra.Command{Use: "update ID", Short: "Prepare or publish a revision-checked update to an existing note", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
		defer cancel()
		if specFile == "" {
			before, err := vault.NoteSnapshotContext(ctx, root, args[0])
			if err != nil {
				return err
			}
			// A complete editable object, not a patch. Identity, scopes, evidence,
			// existing links and selected blocks are prefilled from current bytes.
			return json.NewEncoder(cmd.OutOrStdout()).Encode(before.Spec)
		}
		var spec vault.NewNoteSpec
		if err := readAuthoringJSON(specFile, &spec); err != nil {
			return err
		}
		if spec.UpdateID != args[0] {
			return fmt.Errorf("update_id must match the requested ID; prepare the current note first")
		}
		spec.Agent, spec.By, spec.Author = "mesh-cli", "mesh-cli", by
		if err := vault.ValidateSpec(spec); err != nil {
			return err
		}
		if err := validateLocalAuthoringReferences(ctx, root, spec); err != nil {
			return err
		}
		if validate {
			if _, err := vault.PrepareNoteContext(ctx, root, spec); err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"valid": true, "saved": false, "factual_correctness": "not assessed"})
		}
		result, err := vault.CreateNoteContext(ctx, root, spec)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"id": result.ID, "path": result.Path, "revision": result.Revision, "updated": true, "previous_revision": spec.UpdateRevision})
	}}
	c.Flags().StringVar(&root, "vault", ".", "vault root")
	c.Flags().StringVar(&specFile, "spec", "", "edited JSON authoring object returned by mesh update ID")
	c.Flags().StringVar(&by, "by", "", "latest editor; original author remains unchanged")
	c.Flags().BoolVar(&validate, "validate", false, "validate the edited object without saving")
	return c
}
