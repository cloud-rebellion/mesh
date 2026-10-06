// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package guards

import (
	"context"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/llm"
	"github.com/bright-interaction/mesh/internal/vault"
)

func TestParseGuard(t *testing.T) {
	g, err := parseGuard("```json\n{\"applies\":true,\"pattern\":\"npm install\",\"globs\":\"Dockerfile\",\"message\":\"use bun\",\"severity\":\"block\"}\n```")
	if err != nil || !g.Applies || g.Pattern != "npm install" || g.Severity != "block" {
		t.Fatalf("parseGuard fenced = %+v, %v", g, err)
	}
	g2, _ := parseGuard(`{"applies": false, "reason": "architectural"}`)
	if g2.Applies {
		t.Fatalf("applies should be false: %+v", g2)
	}
	if g2.Severity != "block" { // default when unspecified
		t.Fatalf("default severity = %q", g2.Severity)
	}
}

func TestShellSnippetSkipsNonApplicable(t *testing.T) {
	gs := []Guard{
		{Title: "bun not npm", Applies: true, Pattern: "npm install", Globs: "Dockerfile,*.sh", Message: "use bun", Severity: "block"},
		{Title: "architecture call", Applies: false, Pattern: "", Message: "n/a"},
	}
	out := ShellSnippet(gs)
	// The globs now go through shellpath.Quote, the module's single shell-quoting
	// helper, instead of a hand-written 'wrapper' this package kept to itself. A glob
	// that needs no quoting loses its quotes (--include=Dockerfile), and one the shell
	// would expand keeps them (--include='*.sh'), which is the half that has to hold:
	// unquoted, the shell would glob *.sh against the current directory before grep
	// ever saw it.
	if !strings.Contains(out, "npm install") || !strings.Contains(out, "--include=Dockerfile") || !strings.Contains(out, "use bun") {
		t.Fatalf("snippet missing the applicable guard:\n%s", out)
	}
	if !strings.Contains(out, "--include='*.sh'") {
		t.Fatalf("a glob the shell would expand must stay quoted:\n%s", out)
	}
	if strings.Contains(out, "architecture call") {
		t.Fatalf("snippet included a non-applicable guard:\n%s", out)
	}
}

func TestSuggestWithStub(t *testing.T) {
	stub := llm.Func(func(_ context.Context, _, _ string) (string, error) {
		return `{"applies":true,"pattern":"chi\\.RealIP","globs":"*.go","message":"use ClientIP middleware with trusted proxies","severity":"block","reason":"textual"}`, nil
	})
	g, err := Suggest(context.Background(), stub, index.GotchaRow{ID: "g1", Title: "chi RealIP takeover", Legacy: &vault.LegacyContent{Dont: "use chi RealIP"}})
	if err != nil || !g.Applies || g.GotchaID != "g1" || g.Pattern == "" {
		t.Fatalf("Suggest = %+v, %v", g, err)
	}
}

func TestSuggestUsesAuthoredTemplateContentsAndRejectsIncompleteEvidence(t *testing.T) {
	called := 0
	stub := llm.Func(func(_ context.Context, system, request string) (string, error) {
		called++
		if !strings.Contains(system, "Incident impact") || !strings.Contains(request, "Cause or hypothesis") || !strings.Contains(request, "verification: source review passed") || strings.Contains(request, "dont:") {
			t.Fatalf("template evidence was reinterpreted as a legacy prohibition: %s\n%s", system, request)
		}
		return `{"applies":false,"reason":"runtime-only evidence"}`, nil
	})
	row := index.GotchaRow{ID: "modern", Title: "A modern gotcha", Template: "troubleshooting", TemplateVersion: 1, Summary: "A bounded diagnosis.", Content: "## Cause or hypothesis\nImpact followed a stale cache.\n## Remedy\nUse a supported cache refresh.\nverification: source review passed"}
	if _, err := Suggest(context.Background(), stub, row); err != nil || called != 1 {
		t.Fatalf("suggestion failed: %v calls=%d", err, called)
	}
	row.MissingSections = []string{"verification"}
	if result, err := Suggest(context.Background(), stub, row); err != nil || result.Applies || called != 1 {
		t.Fatalf("incomplete evidence reached generator: %+v %v calls=%d", result, err, called)
	}
	row.MissingSections = nil
	row.ContentTruncated = true
	if result, err := Suggest(context.Background(), stub, row); err != nil || result.Applies || called != 1 {
		t.Fatalf("truncated evidence reached generator: %+v %v calls=%d", result, err, called)
	}
}
