// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/spf13/cobra"
)

type sharedScopeEntry struct {
	Path   string `json:"path"`
	Before string `json:"before_sha256"`
	After  string `json:"after_sha256"`
}

type sharedScopePlan struct {
	Schema    int                `json:"schema"`
	CreatedAt time.Time          `json:"created_at"`
	Vault     string             `json:"vault"`
	Target    string             `json:"target_scope"`
	Entries   []sharedScopeEntry `json:"entries"`
}

func scopeNormalizeSharedCmd() *cobra.Command {
	var shared, apply bool
	var planPath, manifestPath, digest string
	c := &cobra.Command{
		Use:   "normalize-shared [vault]",
		Short: "Plan or apply an explicitly authorized shared-dev scope correction with retained provenance",
		Long:  "Correct legacy product/topic scope labels to dev only after the operator has verified the vault is shared, dev is its sole registered access scope, folder ACLs are absent, and the migration is authorized. Stop all writers and retain a verified backup before apply. This command neither changes grants nor infers authorization from note content. Published note updates cannot change scopes; this is a separate local operator migration.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !shared {
				return fmt.Errorf("explicit --shared-vault attestation is required; do not use this command on a partitioned vault")
			}
			root, err := filepath.Abs(vaultArg(args))
			if err != nil {
				return err
			}
			root, err = filepath.EvalSymlinks(root)
			if err != nil {
				return err
			}
			if apply {
				if planPath != "" || manifestPath == "" || digest == "" {
					return fmt.Errorf("apply requires --manifest and --manifest-sha256, without --plan")
				}
				plan, err := readSharedScopePlan(root, manifestPath, digest)
				if err != nil {
					return err
				}
				// Validate the entire pinned cohort before the first write. Each write
				// then checks its current identity and bytes again under confinement.
				for _, entry := range plan.Entries {
					if err := verifySharedScopeEntry(cmd.Context(), root, entry); err != nil {
						return err
					}
				}
				written := 0
				for _, entry := range plan.Entries {
					result, err := vault.NormalizeSharedScopeFile(cmd.Context(), root, entry.Path, entry.Before, false)
					if err != nil {
						_ = json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"state": "stopped-reconcile-required", "completed_writes": written, "current_entry": result, "manifest_sha256": digest})
						return fmt.Errorf("scope migration stopped after %d completed writes; current entry written=%v; preserve backup and reconcile before retry: %w", written, result.Written, err)
					}
					current, err := vault.ReadConfinedFileContext(cmd.Context(), root, entry.Path, 1<<20)
					if err != nil || evidenceRevision(current) != entry.After {
						return fmt.Errorf("scope migration outcome needs reconciliation after %d writes", written+1)
					}
					written++
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"state": "completed", "notes_changed": written, "target_scope": "dev", "manifest_sha256": digest, "grants_changed": false, "reindex_required": true})
			}
			if planPath == "" || manifestPath != "" || digest != "" {
				return fmt.Errorf("planning requires --plan in a private directory; no manifest arguments")
			}
			plan, err := planSharedScopes(cmd.Context(), root)
			if err != nil {
				return err
			}
			data, err := json.MarshalIndent(plan, "", "  ")
			if err != nil {
				return err
			}
			data = append(data, '\n')
			if err := writePrivateScopePlan(planPath, data); err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"state": "planned-not-applied", "notes_to_change": len(plan.Entries), "manifest_sha256": evidenceRevision(data), "target_scope": "dev", "authorization": "operator-attested-shared-vault"})
		},
	}
	c.Flags().BoolVar(&shared, "shared-vault", false, "attest the authorized shared-dev configuration, exclusive writer window and preservation prerequisites")
	c.Flags().BoolVar(&apply, "apply", false, "apply the exact fresh pinned plan; planning is the default")
	c.Flags().StringVar(&planPath, "plan", "", "new private plan file (planning only)")
	c.Flags().StringVar(&manifestPath, "manifest", "", "fresh private plan file (apply only)")
	c.Flags().StringVar(&digest, "manifest-sha256", "", "independently pinned plan SHA256 (apply only)")
	return c
}

func planSharedScopes(ctx context.Context, root string) (sharedScopePlan, error) {
	plan := sharedScopePlan{Schema: 1, CreatedAt: time.Now().UTC(), Vault: root, Target: "dev"}
	files, err := vault.Walk(root)
	if err != nil {
		return plan, err
	}
	for _, name := range files {
		path, err := filepath.Rel(root, name)
		if err != nil || !filepath.IsLocal(path) {
			return plan, fmt.Errorf("scope plan contains a nonlocal note")
		}
		data, err := vault.ReadConfinedFileContext(ctx, root, path, 1<<20)
		if err != nil {
			return plan, err
		}
		header, _, _ := vault.SplitFrontmatter(string(data))
		fm, _, err := vault.ParseFrontmatter([]byte(header))
		if err != nil {
			return plan, err
		}
		if vault.IsDraft(fm) {
			continue
		}
		out, changed, err := vault.PlanSharedScopeNormalization(data)
		if err != nil {
			return plan, fmt.Errorf("cannot plan scope migration for %s: %w", path, err)
		}
		if changed {
			plan.Entries = append(plan.Entries, sharedScopeEntry{Path: filepath.ToSlash(path), Before: evidenceRevision(data), After: evidenceRevision(out)})
		}
		if len(plan.Entries) > 20000 || ctx.Err() != nil {
			return plan, fmt.Errorf("scope planning cancelled or cohort limit exceeded")
		}
	}
	return plan, nil
}

func verifySharedScopeEntry(ctx context.Context, root string, entry sharedScopeEntry) error {
	if !filepath.IsLocal(entry.Path) || filepath.ToSlash(filepath.Clean(entry.Path)) != entry.Path || !evidenceSHA.MatchString(entry.Before) || !evidenceSHA.MatchString(entry.After) {
		return fmt.Errorf("invalid scope plan entry")
	}
	data, err := vault.ReadConfinedFileContext(ctx, root, entry.Path, 1<<20)
	if err != nil || evidenceRevision(data) != entry.Before {
		return fmt.Errorf("scope plan source changed; no further writes authorized")
	}
	out, changed, err := vault.PlanSharedScopeNormalization(data)
	if err != nil || !changed || evidenceRevision(out) != entry.After {
		return fmt.Errorf("scope plan transformation changed; no further writes authorized")
	}
	preview, err := vault.NormalizeSharedScopeFile(ctx, root, entry.Path, entry.Before, true)
	if err != nil || !preview.Changed || preview.AfterSHA != entry.After {
		return fmt.Errorf("scope plan source is not eligible for publication; no further writes authorized")
	}
	return nil
}

func privateScopePlanPath(name string) error {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return fmt.Errorf("scope plan must be an absolute private file")
	}
	parent := filepath.Dir(name)
	real, err := filepath.EvalSymlinks(parent)
	st, statErr := os.Lstat(parent)
	if err != nil || statErr != nil || real != parent || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("scope plan parent must be a nonsymlink private directory")
	}
	return nil
}

func writePrivateScopePlan(name string, data []byte) error {
	if err := privateScopePlanPath(name); err != nil {
		return err
	}
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func readSharedScopePlan(root, name, digest string) (sharedScopePlan, error) {
	var plan sharedScopePlan
	if err := privateScopePlanPath(name); err != nil || !evidenceSHA.MatchString(digest) {
		return plan, fmt.Errorf("scope plan location or digest is invalid")
	}
	st, err := os.Lstat(name)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 8<<20 {
		return plan, fmt.Errorf("scope plan must be a bounded private regular file")
	}
	file, err := os.Open(name)
	if err != nil {
		return plan, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return plan, fmt.Errorf("scope plan identity changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if err != nil || len(data) > 8<<20 || evidenceRevision(data) != digest {
		return plan, fmt.Errorf("scope plan digest changed")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&plan) != nil || decoder.Decode(new(any)) != io.EOF {
		return plan, fmt.Errorf("invalid scope plan JSON")
	}
	age := time.Since(plan.CreatedAt)
	if plan.Schema != 1 || plan.Vault != root || plan.Target != "dev" || age < -30*time.Second || age > 5*time.Minute || len(plan.Entries) > 20000 {
		return plan, fmt.Errorf("scope plan is stale or belongs to another vault")
	}
	seen := map[string]bool{}
	for _, entry := range plan.Entries {
		if seen[entry.Path] {
			return plan, fmt.Errorf("duplicate scope plan source")
		}
		seen[entry.Path] = true
	}
	return plan, nil
}
