// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/spf13/cobra"
)

func draftsCmd() *cobra.Command {
	var root string
	var limit, offset int
	var asJSON bool
	c := &cobra.Command{Use: "drafts [vault]", Short: "Browse incomplete drafts with their current revision", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		root = vaultArgOr(args, root)
		store, err := index.OpenReadOnly(root)
		if err != nil {
			return err
		}
		defer store.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		type draft struct {
			ID              string   `json:"id"`
			Path            string   `json:"path"`
			Title           string   `json:"title"`
			Template        string   `json:"template"`
			TemplateVersion int      `json:"template_version"`
			Revision        string   `json:"revision"`
			Summary         string   `json:"summary,omitempty"`
			Missing         []string `json:"missing_content,omitempty"`
		}
		current := map[string]draft{}
		checkCurrent := func(path string) bool {
			data, err := vault.ReadConfinedFileContext(ctx, root, path, 4<<20)
			if err != nil {
				return false
			}
			fm, _, err := vault.ParseFrontmatter(data)
			if err != nil || fm == nil || fm.ID == "" || fm.Status != "draft" || vault.UnterminatedFrontmatter(string(data)) {
				return false
			}
			_, body, _ := vault.SplitFrontmatter(string(data))
			authored, err := vault.ReadAuthoring(fm, body)
			if err != nil {
				return false
			}
			current[path] = draft{ID: fm.ID, Path: path, Title: fm.Title, Template: fm.Template, TemplateVersion: fm.TemplateVersion, Revision: vault.ContentRevision(data), Summary: authored.Summary, Missing: authored.MissingSections}
			return true
		}
		notes, err := store.DraftNotesContext(ctx, nil, checkCurrent, limit, offset)
		if err != nil {
			return err
		}
		out := make([]draft, 0, len(notes))
		for _, m := range notes {
			if d, ok := current[m.Path]; ok && d.ID == m.NoteID {
				out = append(out, d)
			}
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"drafts": out, "offset": offset, "limit": min(max(limit, 1), 100)})
		}
		for _, d := range out {
			fmt.Fprintf(cmd.OutOrStdout(), "%s (%s v%d) %s\n  revision: %s\n", d.ID, d.Template, d.TemplateVersion, d.Path, d.Revision)
			if len(d.Missing) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  missing: %s\n", strings.Join(d.Missing, ", "))
			}
		}
		return nil
	}}
	c.Flags().StringVar(&root, "vault", ".", "vault root")
	c.Flags().IntVar(&limit, "limit", 20, "maximum drafts (1..100)")
	c.Flags().IntVar(&offset, "offset", 0, "drafts to skip after current-file validation")
	c.Flags().BoolVar(&asJSON, "json", false, "emit bounded draft cards as JSON")
	return c
}
