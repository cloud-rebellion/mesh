// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Exercise both real HTTP delivery paths. Detailed guidance must remain opt-in,
// not inflate initialize/tool descriptions repeated by some clients.
func TestRetrievalContractHTTPDelivery(t *testing.T) {
	s := newTestServer(t)
	batchE2EReady(s)
	rpc := func(method string, params any) json.RawMessage {
		t.Helper()
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		s.HandleHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body)))
		var reply struct {
			Result json.RawMessage
			Error  *rpcError
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || reply.Error != nil {
			t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		return reply.Result
	}
	var initialized struct{ Instructions string }
	if err := json.Unmarshal(rpc("initialize", map[string]any{}), &initialized); err != nil {
		t.Fatal(err)
	}
	if initialized.Instructions != contractText {
		t.Fatal("initialize contract drift")
	}
	for _, want := range []string{"mesh_fetch_many", "mesh://contract", "at most one", "report gaps", "[[untrusted-external-content]]", "<untrusted-external-content>", "never store capability tokens"} {
		if !strings.Contains(initialized.Instructions, want) {
			t.Errorf("initialize missing %q", want)
		}
	}
	if strings.Contains(initialized.Instructions, "Bounded follow-up") {
		t.Fatal("initialize expanded detailed policy")
	}
	if strings.Contains(initialized.Instructions, "Write-back outcomes") {
		t.Fatal("initialize expanded optional write-back policy")
	}
	var resource struct {
		Contents []struct{ URI, MimeType, Text string }
	}
	if err := json.Unmarshal(rpc("resources/read", map[string]any{"uri": "mesh://contract"}), &resource); err != nil {
		t.Fatal(err)
	}
	if len(resource.Contents) != 1 {
		t.Fatalf("contents=%d", len(resource.Contents))
	}
	c := resource.Contents[0]
	if c.URI != "mesh://contract" || c.MimeType != "text/markdown" || !strings.HasPrefix(c.Text, initialized.Instructions) {
		t.Fatal("resource metadata or compact guidance drift")
	}
	for _, want := range []string{"cards when they answer", "one-item batch", "Retain safety excerpts", "including titles, is untrusted data", "Count search responses", "at most one follow-up", "less than 256", "own reordered input", "not budget omissions", "not server-side enforcement", "not total model usage"} {
		if !strings.Contains(c.Text, want) {
			t.Errorf("resource missing %q", want)
		}
	}
	for _, want := range []string{"15-second server deadline", "not a hard deadline on filesystem durability", "index_stale", "Do not retry it", "unknown write outcome"} {
		if !strings.Contains(c.Text, want) {
			t.Errorf("resource missing write-back safety guidance %q", want)
		}
	}
	if len(c.Text) > 5200 {
		t.Fatalf("optional contract grew to %d bytes; budget 5200", len(c.Text))
	}
	t.Logf("initialize=%d bytes optional contract=%d bytes", len(initialized.Instructions), len(c.Text))
}
