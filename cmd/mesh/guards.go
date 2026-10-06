// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/bright-interaction/mesh/internal/guards"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/llm"
	"github.com/spf13/cobra"
)

// guardsCmd turns institutional gotchas into candidate pre-commit guards (the
// knowledge->enforcement loop): for each high-confidence gotcha with a concrete
// anti-pattern, the BYOAI LLM proposes a grep-style check; the human pastes the ones
// that fit into the hook. `list` is the deterministic checklist; `suggest` calls the LLM.
func guardsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "guards",
		Short: "Turn gotchas into candidate pre-commit guards (knowledge -> enforcement)",
	}
	c.AddCommand(guardsListCmd(), guardsSuggestCmd())
	return c
}

func guardsListCmd() *cobra.Command {
	var vaultRoot string
	var all bool
	c := &cobra.Command{
		Use:   "list [vault]",
		Short: "List authored troubleshooting evidence for reviewed guard candidates",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// READ-ONLY: these surface stored gotchas, nothing more.
			store, err := index.OpenReadOnly(vaultArgOr(args, vaultRoot))
			if err != nil {
				return err
			}
			defer store.Close()
			gs, err := store.Gotchas(!all)
			if err != nil {
				return err
			}
			for _, g := range gs {
				fmt.Printf("- [%s] %s\n    %s\n", orDash(g.Confidence), g.Title, g.Content)
			}
			fmt.Printf("(%d authored troubleshooting candidates%s)\n", len(gs), ifStr(!all, ", high-confidence only", ""))
			return nil
		},
	}
	c.Flags().StringVar(&vaultRoot, "vault", ".", "vault root")
	c.Flags().BoolVar(&all, "all", false, "include all confidences, not just high")
	return c
}

func guardsSuggestCmd() *cobra.Command {
	var vaultRoot string
	var all, asJSON bool
	var concurrency int
	c := &cobra.Command{
		Use:   "suggest [vault]",
		Short: "Propose pre-commit guards for high-confidence gotchas (BYOAI; review before enabling)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// READ-ONLY: these surface stored gotchas, nothing more.
			store, err := index.OpenReadOnly(vaultArgOr(args, vaultRoot))
			if err != nil {
				return err
			}
			defer store.Close()
			client, err := llm.NewFromEnv()
			if err != nil {
				return err
			}
			gs, err := store.Gotchas(!all)
			if err != nil {
				return err
			}
			if len(gs) == 0 {
				fmt.Println("no authored troubleshooting candidates to propose guards for.")
				return nil
			}
			if concurrency < 1 {
				concurrency = 4
			}
			fmt.Fprintf(os.Stderr, "proposing guards for %d gotchas via %s...\n", len(gs), client.Describe())
			results := make([]guards.Guard, len(gs))
			sem := make(chan struct{}, concurrency)
			var wg sync.WaitGroup
			for i, g := range gs {
				wg.Add(1)
				go func(i int, g index.GotchaRow) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					gd, e := guards.Suggest(cmd.Context(), client, g)
					if e != nil {
						gd = guards.Guard{GotchaID: g.ID, Title: g.Title, Applies: false, Reason: "llm error: " + e.Error()}
					}
					results[i] = gd
				}(i, g)
			}
			wg.Wait()

			var applicable []guards.Guard
			for _, g := range results {
				if g.Applies && g.Pattern != "" {
					applicable = append(applicable, g)
				}
			}
			if asJSON {
				b, _ := json.MarshalIndent(map[string]any{"guards": results, "applicable": len(applicable)}, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("\n%d of %d gotchas are mechanically enforceable:\n\n", len(applicable), len(results))
			for _, g := range applicable {
				fmt.Printf("- %s  [%s]\n    pattern: %s\n    globs:   %s\n    message: %s\n", g.Title, g.Severity, g.Pattern, g.Globs, g.Message)
			}
			fmt.Printf("\n--- paste-ready guard script (review first) ---\n%s", guards.ShellSnippet(applicable))
			return nil
		},
	}
	c.Flags().StringVar(&vaultRoot, "vault", ".", "vault root")
	c.Flags().BoolVar(&all, "all", false, "include all confidences, not just high")
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON (all proposals, including non-applicable)")
	c.Flags().IntVar(&concurrency, "concurrency", 4, "parallel LLM calls")
	return c
}

func vaultArgOr(args []string, flag string) string {
	if len(args) == 1 {
		return args[0]
	}
	return flag
}

func orDash(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func ifStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
