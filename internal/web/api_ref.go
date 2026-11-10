// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"net/http"
	"strconv"

	"github.com/bright-interaction/mesh/internal/mcp"
)

// handleMCPTools returns the agent (MCP) tool reference: the same tool specs the
// MCP server advertises (single-sourced from internal/mcp) plus the retrieval
// contract and a ready-to-paste agent config.
func (s *Server) handleMCPTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"tools":    mcp.ToolSpecs(),
		"contract": mcp.Contract(),
		"vault":    s.exposedVaultRoot(),
		"config": map[string]any{
			"command": "mesh",
			"args":    []string{"mcp", "--vault", s.exposedVaultRoot(), "--watch"},
		},
		// Say what this config does about indexing. It is the whole setup a user pastes,
		// and for four days it was a server that could not index anything, with nothing
		// anywhere saying so.
		"note": "This is the whole setup. The server claims the vault's owning-writer role when nothing " +
			"else holds it, so write-backs are searchable at once and editor changes are picked up live. " +
			"Beside a running `mesh watch` or `mesh sync --watch` it reads that owner's index instead. " +
			"`mesh doctor <vault>` names the owner, or prints `owner: NONE` with the fix when there is none.",
	})
}

// handleOpenAPI serves a hand-authored OpenAPI 3 description of the /api surface.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, openAPISpec())
}

func openAPISpec() map[string]any {
	get := func(summary string, params []map[string]any) map[string]any {
		op := map[string]any{"summary": summary, "responses": map[string]any{"200": map[string]any{"description": "ok"}}}
		if params != nil {
			op["parameters"] = params
		}
		return map[string]any{"get": op}
	}
	qparam := func(name, desc string, required bool) map[string]any {
		return map[string]any{"name": name, "in": "query", "required": required, "schema": map[string]any{"type": "string"}, "description": desc}
	}
	pparam := func(name, desc string) map[string]any {
		return map[string]any{"name": name, "in": "path", "required": true, "schema": map[string]any{"type": "string"}, "description": desc}
	}
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Mesh viewer API",
			"version":     "0.1.0",
			"description": "Local HTTP API behind `mesh ui`. Loopback binds need no auth; a non-loopback bind requires a bearer token.",
		},
		"servers":  []map[string]any{{"url": "/"}},
		"security": []map[string]any{{"bearerAuth": []string{}}},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer", "description": "Required only when the viewer is bound beyond loopback (--token / MESH_UI_TOKEN)."},
			},
		},
		"paths": map[string]any{
			"/api/status":      get("Index counts + active retrieval signals.", nil),
			"/api/config":      mergeOps(get("Effective config per field (value, source, editable). Members receive a redacted read-only view; shared configuration writes require an admin role. Secrets are never returned.", nil), putConfigOp()),
			"/api/reindex":     map[string]any{"post": map[string]any{"summary": "Re-read the vault and rebuild the graph.", "responses": map[string]any{"200": map[string]any{"description": "ok"}}}},
			"/api/search":      get("Fused retrieval; returns ranked cards.", []map[string]any{qparam("q", "query; at most "+strconv.Itoa(mcp.SearchQueryMaxBytes)+" bytes, read as at most 64 distinct terms", true), qparam("limit", "candidates per signal", false), qparam("budget", "token budget for packing", false)}),
			"/api/note/{id}":   get("A note's raw markdown by frontmatter id.", []map[string]any{pparam("id", "frontmatter id")}),
			"/api/docs":        get("List the embedded doc pages.", nil),
			"/api/docs/{slug}": get("A doc page rendered to HTML.", []map[string]any{pparam("slug", "doc slug")}),
			"/api/mcp-tools":   get("The MCP tool reference + retrieval contract + agent config.", nil),
			"/graph.json":      get("The full graph (nodes, edges, communities) the viewer renders.", nil),
		},
	}
}

func putConfigOp() map[string]any {
	return map[string]any{"put": map[string]any{
		"summary": "Update editable config fields (writes .mesh/config.toml).",
		"requestBody": map[string]any{
			"required": true,
			"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"updates": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}},
			}}},
		},
		"responses": map[string]any{
			"200": map[string]any{"description": "saved"},
			"400": map[string]any{"description": "validation error"},
			"409": map[string]any{"description": "field is env-locked"},
		},
	}}
}

func mergeOps(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}
