// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package desktop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/syncproto"
	"github.com/bright-interaction/mesh/internal/vault"
)

func desktopFixture(t *testing.T) *Engine {
	t.Helper()
	t.Setenv("MESH_NO_UPDATE_CHECK", "1")
	t.Setenv("MESH_EMBED_ENDPOINT", "")
	t.Setenv("MESH_RERANK_ENDPOINT", "")
	e, err := New(context.Background(), filepath.Join(t.TempDir(), "vault"))
	if err != nil {
		t.Fatal(err)
	}
	if _, apiErr := e.Dispatch(t.Context(), "init", json.RawMessage(`{"name":"Desktop fixture"}`)); apiErr != nil {
		t.Fatal(apiErr)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// The real ordered NDJSON dispatcher enrolls through a private synthetic TLS
// hub, downloads an approved note, then reopens offline. No production endpoint,
// invitation, credential, native shell override or trust-policy bypass is used.
func TestDesktopStdioTLSJoinDownloadOfflineRestartAndNoRepeatedRedemption(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("MESH_NO_UPDATE_CHECK", "1")
	remoteRoot := t.TempDir()
	note, err := vault.CreateNote(remoteRoot, vault.NewNoteSpec{Title: "Synthetic joined desktop finding", Template: "finding", Summary: "A synthetic TLS peer download remains available offline.", Sections: map[string]string{"question": "Can the desktop reopen offline?", "findings": "This fixture supplies local content.", "evidence": "Synthetic TLS transport only.", "limitations": "No live authority or human native enrollment was exercised.", "next_steps": "Review native installation separately."}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(note.Path)
	if err != nil {
		t.Fatal(err)
	}
	var joins, syncs atomic.Int32
	var offline atomic.Bool
	hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/v1/join":
			joins.Add(1)
			var request syncproto.JoinRequest
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || json.NewDecoder(r.Body).Decode(&request) != nil || request.Invite != "synthetic-stdio-invitation" {
				t.Error("invalid synthetic enrollment request")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(syncproto.JoinResponse{ClientToken: "synthetic-stdio-private-bearer", User: "Synthetic native user", VaultID: "synthetic-stdio-vault"})
		case "/v1/vault", "/v1/sync":
			if r.Header.Get("Authorization") != "Bearer synthetic-stdio-private-bearer" {
				t.Error("saved synthetic identity was not used")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/v1/vault" {
				json.NewEncoder(w).Encode(syncproto.VaultInfo{VaultID: "synthetic-stdio-vault"})
				return
			}
			var request syncproto.SyncRequest
			if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Outbox) != 0 {
				t.Error("empty team staging uploaded unsolicited notes")
			}
			syncs.Add(1)
			json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: strings.Repeat("a", 40), FullReconcile: true, Deltas: []syncproto.Delta{{Path: "findings/" + note.ID + ".md", Op: "upsert", ContentB64: base64.StdEncoding.EncodeToString(raw)}}})
		default:
			t.Error("unexpected synthetic TLS endpoint")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hub.Close()
	// Only this synthetic in-process fixture supplies its private CA to the
	// standard client. Production still uses normal system trust and no redirects.
	previousTransport := http.DefaultTransport
	http.DefaultTransport = hub.Client().Transport
	defer func() { http.DefaultTransport = previousTransport }()
	root := filepath.Join(t.TempDir(), "native-team-vault")
	for launch := 0; launch < 2; launch++ {
		e, err := New(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		input, parentInput := io.Pipe()
		parentOutput, output := io.Pipe()
		t.Cleanup(func() {
			parentInput.Close()
			parentOutput.Close()
			e.Close()
			input.Close()
			output.Close()
		})
		var transcript bytes.Buffer
		done := make(chan error, 1)
		go func() {
			err := e.Serve(t.Context(), input, io.MultiWriter(output, &transcript))
			output.Close()
			done <- err
		}()
		decoder := json.NewDecoder(parentOutput)
		var id int64
		exchange := func(method string, params any) Response {
			t.Helper()
			id++
			if json.NewEncoder(parentInput).Encode(Request{Protocol: Protocol, ID: id, Method: method, Params: mustMarshal(params)}) != nil {
				t.Fatal("synthetic stdio request unavailable")
			}
			var response Response
			if decoder.Decode(&response) != nil || response.ID != id || response.Protocol != Protocol {
				t.Fatal("synthetic stdio framing failed")
			}
			return response
		}
		if launch == 0 {
			if r := exchange("init", map[string]any{"name": "Synthetic team desktop", "for_join": true}); r.Error != nil {
				t.Fatal(r.Error)
			}
			if r := exchange("join", map[string]any{"hub_url": hub.URL, "invite": "synthetic-stdio-invitation"}); r.Error != nil {
				t.Fatal(r.Error)
			}
			deadline := time.Now().Add(4 * time.Second)
			for {
				r := exchange("status", map[string]any{})
				if r.Error != nil {
					t.Fatal(r.Error)
				}
				var status Status
				json.Unmarshal(mustMarshal(r.Result), &status)
				if status.Sync.State == "synced" {
					if status.Identity == nil || status.Identity.User != "Synthetic native user" || status.Identity.Role != "unknown" || status.Identity.Verified {
						t.Fatal("enrollment fabricated role authority")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("synthetic TLS enrollment did not settle: %+v", status)
				}
				time.Sleep(20 * time.Millisecond)
			}
		} else {
			if r := exchange("join", map[string]any{"hub_url": hub.URL, "invite": "synthetic-repeat"}); r.Error == nil || r.Error.Code != "ALREADY_JOINED" {
				t.Fatal("cold restart permitted repeated redemption")
			}
		}
		// The owner's real watcher must make the peer write visible through the
		// same shared MCP reader used by the app, without a manual rebuild.
		deadline := time.Now().Add(4 * time.Second)
		for {
			r := exchange("tool", map[string]any{"name": "mesh_search", "arguments": map[string]any{"query": "Synthetic joined desktop finding", "limit": 5, "budget": 2000}})
			if r.Error == nil && bytes.Contains(mustMarshal(r.Result), []byte(note.ID)) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("peer note was not searchable through stdio")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if r := exchange("close", map[string]any{}); r.Error != nil {
			t.Fatal(r.Error)
		}
		parentInput.Close()
		parentOutput.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if strings.Contains(transcript.String(), root) || strings.Contains(transcript.String(), "synthetic-stdio-private-bearer") || strings.Contains(transcript.String(), "synthetic-stdio-invitation") {
			t.Fatal("desktop protocol exposed a credential, invitation or selected path")
		}
		offline.Store(true)
	}
	if joins.Load() != 1 || syncs.Load() != 1 {
		t.Fatal("native TLS fixture retried enrollment or created extra sync writes")
	}
}
func desktopTool(t *testing.T, e *Engine, name string, args any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	result, apiErr := e.Dispatch(t.Context(), "tool", raw)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	data, _ := json.Marshal(result)
	var text struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(data, &text) != nil || len(text.Content) != 1 {
		t.Fatalf("unexpected shared result: %s", data)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text.Content[0].Text), &out); err != nil {
		t.Fatalf("shared authoring object unavailable: %v %s", err, text.Content[0].Text)
	}
	return out
}
func TestDesktopOfflineTemplateAuthoringRestartAndStaleEdit(t *testing.T) {
	e := desktopFixture(t)
	sections := map[string]string{"question": "Can a synthetic offline note be reopened?", "findings": "Local Markdown and the local index remain available without a hub.", "evidence": "This fixture writes and reopens the note through the shared authoring API.", "limitations": "This test does not prove native packaging or real team sync.", "next_steps": "Keep an offline copy; verify team sync independently."}
	catalog := desktopTool(t, e, "mesh_templates", map[string]any{})
	if len(catalog["templates"].([]any)) < 13 {
		t.Fatal("template library missing")
	}
	first := desktopTool(t, e, "mesh_author_note", map[string]any{"action": "publish", "note": map[string]any{"title": "Offline field findings", "template": "finding", "summary": "An offline authoring fixture with explicit limits.", "sections": sections, "tags": []string{"offline"}}})
	id := first["id"].(string)
	before := desktopTool(t, e, "mesh_prepare_update", map[string]any{"id": id})
	note := before["note"].(map[string]any)
	note["summary"] = "Offline edits are kept when the application restarts."
	updated := desktopTool(t, e, "mesh_author_note", map[string]any{"action": "publish", "note": note})
	if updated["id"] != id || updated["path"] != first["path"] || updated["revision"] == first["revision"] {
		t.Fatal("edit identity/revision not preserved")
	}
	raw, _ := json.Marshal(map[string]any{"name": "mesh_author_note", "arguments": map[string]any{"action": "publish", "note": note}})
	if _, err := e.Dispatch(t.Context(), "tool", raw); err == nil {
		t.Fatal("stale overwrite accepted")
	}
	root := e.root
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	current := desktopTool(t, reopened, "mesh_prepare_update", map[string]any{"id": id})
	if current["note"].(map[string]any)["summary"] != note["summary"] || current["revision"] != updated["revision"] {
		t.Fatal("offline restart lost edit")
	}
	search := desktopTool(t, reopened, "mesh_search", map[string]any{"query": "Offline field findings"})
	if !strings.Contains(string(mustMarshal(search)), id) {
		t.Fatal("offline search lacks updated note")
	}
	status := reopened.status(t.Context())
	if status.Sync.Pending < 1 || status.Identity != nil || status.State != "ready" {
		t.Fatalf("offline state fabricated: %+v", status)
	}
}
func mustMarshal(v any) []byte { raw, _ := json.Marshal(v); return raw }
func TestDesktopDraftRetrievalCompletesSharedNoteWithoutLosingIdentity(t *testing.T) {
	e := desktopFixture(t)
	saved := desktopTool(t, e, "mesh_author_note", map[string]any{"action": "draft", "note": map[string]any{"title": "Marketing evidence draft", "template": "plan", "summary": "A strategy fixture awaits evidence.", "sections": map[string]string{"objective": "Reach the fixture audience."}, "tags": []string{"marketing"}}})
	id := saved["id"].(string)
	prepared := desktopTool(t, e, "mesh_prepare_update", map[string]any{"id": id, "draft": true})
	note := prepared["note"].(map[string]any)
	if note["draft_id"] != id || note["draft_revision"] != saved["revision"] || note["update_id"] != nil {
		t.Fatal("draft identity not fenced")
	}
	template := desktopTool(t, e, "mesh_note_template", map[string]any{"template": "plan"})["template"].(map[string]any)
	sections := note["sections"].(map[string]any)
	for _, row := range template["sections"].([]any) {
		key := row.(map[string]any)["key"].(string)
		if key != "objective" {
			sections[key] = "Synthetic strategy context; real campaign results remain unverified."
		}
	}
	note["status"] = "active"
	published := desktopTool(t, e, "mesh_author_note", map[string]any{"action": "publish", "note": note})
	if published["id"] != id || published["path"] != saved["path"] {
		t.Fatal("draft completion made another note")
	}
	inbox := desktopTool(t, e, "mesh_drafts", map[string]any{})
	if len(inbox["drafts"].([]any)) != 0 {
		t.Fatal("completed note still in draft inbox")
	}
}
func TestDesktopSecondWindowUsesExistingOwnerAndWritesThroughNormalAcknowledgement(t *testing.T) {
	first := desktopFixture(t)
	second, err := New(t.Context(), first.root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	saved := desktopTool(t, second, "mesh_author_note", map[string]any{"action": "draft", "note": map[string]any{"title": "Two window draft", "template": "procedure", "summary": "A synthetic sales procedure; additional steps are pending."}})
	drafts := desktopTool(t, first, "mesh_drafts", map[string]any{})
	if !strings.Contains(string(mustMarshal(drafts)), saved["id"].(string)) {
		t.Fatal("existing owner did not acknowledge second window write")
	}
}
func TestDesktopViewerPrivacyAndOperationAllowlist(t *testing.T) {
	e := desktopFixture(t)
	for _, p := range []string{"/graph.json", "/api/status"} {
		result, apiErr := e.Dispatch(t.Context(), "web", mustMarshal(map[string]any{"method": "GET", "path": p}))
		if apiErr != nil {
			t.Fatal(apiErr)
		}
		m := result.(map[string]any)
		body, err := base64.StdEncoding.DecodeString(m["body_base64"].(string))
		if err != nil || bytes.Contains(body, []byte(e.root)) {
			t.Fatal("viewer leaked selected absolute vault path")
		}
		var value map[string]any
		if json.Unmarshal(body, &value) != nil {
			t.Fatal("viewer JSON lost")
		}
		if _, ok := value["vault"]; ok {
			t.Fatal("status filesystem key retained")
		}
	}
	for _, p := range []string{"https://foreign.test/", "//foreign.test/", "/assets/../../.mesh/credentials", "/api/config", "/api/ask", "/api/note/../config", "/api/note/%2e%2e/config", "/graph.json?fixture=1", "/api/status?fixture=1", "/api/dashboard", "/api/mcp-tools", "/api/note/a/b", "/api/note/a?q=x", "/assets/a.js?x=y", "/api/search?q=a&q=b", "/api/search?limit=-1", "/api/search?other=1"} {
		if _, err := e.Dispatch(t.Context(), "web", mustMarshal(map[string]any{"method": "GET", "path": p})); err == nil {
			t.Fatalf("unexpected viewer capability %s", p)
		}
	}
	if !allowedWebPath("/api/search?q=offline%20findings") {
		t.Fatal("normal encoded query denied")
	}
	for _, name := range []string{"mesh_setup_hooks", "mesh_run", "mesh_secret_lookup", "mesh_write_entity", "mesh_fetch_many"} {
		if _, err := e.Dispatch(t.Context(), "tool", mustMarshal(map[string]any{"name": name, "arguments": map[string]any{}})); err == nil {
			t.Fatalf("unapproved tool %s", name)
		}
	}
}
func TestDesktopNDJSONBoundsIDsAndCancellationDrain(t *testing.T) {
	e := desktopFixture(t)
	var out bytes.Buffer
	input := strings.NewReader("{\"protocol\":1,\"id\":1,\"method\":\"status\",\"params\":{}}\n{\"protocol\":1,\"id\":2,\"method\":\"status\",\"params\":{},\"method\":\"sync\"}\n{\"protocol\":1,\"id\":3,\"method\":\"close\",\"params\":{}}\n")
	if err := e.Serve(t.Context(), input, &out); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'})
	if len(lines) != 3 {
		t.Fatal("stdout contained nonprotocol output")
	}
	var last Response
	if json.Unmarshal(lines[2], &last) != nil || last.ID != 3 || last.Error != nil {
		t.Fatal("close reply missing")
	}
	if err := strictJSON([]byte(`{"protocol":1,"id":9007199254740992,"method":"status","params":{}}`), &Request{}); err != nil {
		t.Fatal("fixture parse should reach numeric fence")
	}
	for _, raw := range []string{`{"x":1,"x":2}`, `{"outer":{"x":1,"x":2}}`, `{} {}`} {
		if strictJSON([]byte(raw), &map[string]any{}) == nil {
			t.Fatal("ambiguous JSON admitted")
		}
	}
	e2 := desktopFixture(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e2.Serve(ctx, r, io.Discard) }()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled idle stdio did not drain")
	}
}

func TestDesktopJoinStagingCreatesNoUnsolicitedNoteAndReopensEmpty(t *testing.T) {
	root := filepath.Join(t.TempDir(), "join-vault")
	e, err := New(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, apiErr := e.Dispatch(t.Context(), "init", json.RawMessage(`{"name":"Team workspace","for_join":true}`)); apiErr != nil {
		t.Fatal(apiErr)
	}
	files, err := vault.Walk(root)
	if err != nil || len(files) != 0 {
		t.Fatal("join staging created an unsolicited note")
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if status := reopened.status(t.Context()); status.State != "ready" || status.Sync.Pending != 0 || status.Vault.ID == "" {
		t.Fatalf("empty staged vault cannot reopen: %+v", status)
	}
}

func TestDesktopPreservesVaultPathsInsideKnowledgeAcrossFetchPrepareAndEdit(t *testing.T) {
	e := desktopFixture(t)
	code := "const chosenVault = \"" + e.root + "\";"
	sections := map[string]string{"question": "Does the opaque content survive?", "findings": "Selected path remains substantive knowledge: " + e.root, "evidence": "This fixture checks a full edit round trip.", "limitations": "Synthetic private fixture path, not a secret.", "next_steps": "Keep code, examples and provenance unchanged."}
	block := map[string]any{"template": "code", "id": "path-example", "fields": map[string]string{"purpose": "Preserve an illustrative local path.", "language": "javascript", "code": code, "source": e.root + "/illustrative.js", "revision": "unknown", "explanation": "The path is deliberately part of the example.", "verification": "Round trip checked by this fixture.", "limitations": "Illustrative code only.", "illustrative": "true"}}
	saved := desktopTool(t, e, "mesh_author_note", map[string]any{"action": "publish", "note": map[string]any{"title": "Path preservation example", "template": "finding", "summary": "Example includes " + e.root, "sections": sections, "blocks": []any{block}, "tags": []string{"offline"}}})
	id := saved["id"].(string)
	raw, apiErr := e.Dispatch(t.Context(), "tool", mustMarshal(map[string]any{"name": "mesh_fetch", "arguments": map[string]any{"id": id}}))
	if apiErr != nil || !bytes.Contains(mustMarshal(raw), []byte(e.root)) {
		t.Fatal("fetch rewrote substantive content")
	}
	prepared := desktopTool(t, e, "mesh_prepare_update", map[string]any{"id": id})
	note := prepared["note"].(map[string]any)
	fields := note["blocks"].([]any)[0].(map[string]any)["fields"].(map[string]any)
	if fields["code"] != code || fields["source"] != e.root+"/illustrative.js" || note["summary"] != "Example includes "+e.root {
		t.Fatal("structured edit corrupted code/provenance")
	}
	note["summary"] = "Edited summary retains " + e.root
	desktopTool(t, e, "mesh_author_note", map[string]any{"action": "publish", "note": note})
	disk, err := os.ReadFile(filepath.Join(e.root, saved["path"].(string)))
	if err != nil || !bytes.Contains(disk, []byte(code)) || !bytes.Contains(disk, []byte(e.root+"/illustrative.js")) {
		t.Fatal("edit persisted scrubbed content")
	}
}

func legacyJoinedDesktopFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("MESH_NO_UPDATE_CHECK", "1")
	t.Setenv("MESH_EMBED_ENDPOINT", "")
	t.Setenv("MESH_RERANK_ENDPOINT", "")
	root := t.TempDir()
	if os.Chmod(root, 0700) != nil || os.Mkdir(filepath.Join(root, ".mesh"), 0700) != nil {
		t.Fatal("fixture setup")
	}
	files := map[string]string{"credentials": `{"hub_url":"https://127.0.0.1:1","token":"synthetic-offline-device","vault_id":"legacy-empty-team"}`, "sync.json": `{"hub_url":"https://127.0.0.1:1","vault_id":"legacy-empty-team","head_sha":"","hashes":{}}`}
	for name, raw := range files {
		if os.WriteFile(filepath.Join(root, ".mesh", name), []byte(raw), 0600) != nil {
			t.Fatal("fixture metadata")
		}
	}
	return root
}
func TestDesktopExistingEmptyCLIJoinOpensReopensAndReportsUnknownTeamIdentity(t *testing.T) {
	root := legacyJoinedDesktopFixture(t)
	for range 2 {
		e, err := New(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		status := e.status(t.Context())
		if status.State != "ready" || status.Vault.ID != "legacy-empty-team" || status.Identity == nil || status.Identity.User != "Unknown team identity" || status.Identity.Role != "unknown" || status.Identity.Verified || (status.Sync.State != "offline" && status.Sync.State != "syncing") {
			t.Fatalf("legacy team misrepresented: %+v", status)
		}
		if _, apiErr := e.Dispatch(t.Context(), "join", mustMarshal(map[string]any{"hub_url": "https://127.0.0.1:1", "invite": "synthetic-rejoin"})); apiErr == nil || apiErr.Code != "ALREADY_JOINED" {
			t.Fatal("existing CLI join allowed another enrollment")
		}
		if _, apiErr := e.Dispatch(t.Context(), "sync", json.RawMessage(`{}`)); apiErr != nil {
			t.Fatal("empty joined vault cannot sync", apiErr)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		files, _ := vault.Walk(root)
		if len(files) != 0 {
			t.Fatal("opening joined vault added unsolicited notes")
		}
	}
}
func TestDesktopBrokenJoinedPairIsRefusedWithoutPersonalFallback(t *testing.T) {
	for _, scenario := range []string{"missing credentials", "missing state", "corrupt state", "different vault", "credential privacy"} {
		t.Run(scenario, func(t *testing.T) {
			root := legacyJoinedDesktopFixture(t)
			switch scenario {
			case "missing credentials":
				os.Remove(filepath.Join(root, ".mesh", "credentials"))
			case "missing state":
				os.Remove(filepath.Join(root, ".mesh", "sync.json"))
			case "corrupt state":
				os.WriteFile(filepath.Join(root, ".mesh", "sync.json"), []byte(`{`), 0600)
			case "different vault":
				os.WriteFile(filepath.Join(root, ".mesh", "sync.json"), []byte(`{"hub_url":"https://127.0.0.1:1","vault_id":"different","hashes":{}}`), 0600)
			case "credential privacy":
				os.Chmod(filepath.Join(root, ".mesh", "credentials"), 0644)
			}
			if e, err := New(t.Context(), root); err == nil {
				e.Close()
				t.Fatal("broken joined pair silently opened as personal")
			}
		})
	}
}

func TestDesktopAcceptedEnrollmentStateFailureReopensWithoutRedeemingOrRewriting(t *testing.T) {
	for _, scenario := range []string{"missing state", "corrupt prior state"} {
		t.Run(scenario, func(t *testing.T) {
			root := legacyJoinedDesktopFixture(t)
			var calls atomic.Int32
			hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer hub.Close()
			cred := map[string]string{"hub_url": hub.URL, "token": "synthetic-accepted-device", "vault_id": "accepted-empty-team"}
			if os.WriteFile(filepath.Join(root, ".mesh", "credentials"), mustMarshal(cred), 0600) != nil {
				t.Fatal("fixture credentials")
			}
			receipt := map[string]string{"state": "accepted", "hub_url": hub.URL, "invite_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte("synthetic-invite"))), "credential_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(cred["token"]))), "vault_id": cred["vault_id"], "user": "Saved team identity", "accepted_at": "2026-10-08T00:00:00Z"}
			if os.WriteFile(filepath.Join(root, ".mesh", "native-join.json"), mustMarshal(receipt), 0600) != nil {
				t.Fatal("fixture accepted receipt")
			}
			statePath := filepath.Join(root, ".mesh", "sync.json")
			prior := []byte(`{corrupt prior state: preserve these bytes`)
			if scenario == "missing state" {
				os.Remove(statePath)
			} else {
				os.WriteFile(statePath, prior, 0600)
			}
			for range 2 {
				e, err := New(t.Context(), root)
				if err != nil {
					t.Fatal("accepted enrollment became startup failure", err)
				}
				s := e.status(t.Context())
				if s.State != "joined-preparing" || s.Sync.State != "metadata-pending" || s.Identity == nil || s.Identity.User != receipt["user"] || s.Identity.Role != "unknown" || s.Identity.Verified {
					t.Fatalf("accepted identity lost: %+v", s)
				}
				if _, apiErr := e.Dispatch(t.Context(), "sync", json.RawMessage(`{}`)); apiErr == nil || apiErr.Code != "SYNC_METADATA_PENDING" {
					t.Fatal("metadata-pending sync was not refused synchronously")
				}
				if _, apiErr := e.Dispatch(t.Context(), "join", mustMarshal(map[string]any{"hub_url": hub.URL, "invite": "synthetic-repeat"})); apiErr == nil || apiErr.Code != "ALREADY_JOINED" {
					t.Fatal("accepted enrollment became a rejoin candidate")
				}
				after := e.status(t.Context())
				e.mu.Lock()
				worker := e.worker
				e.mu.Unlock()
				if after.State != s.State || after.Sync.State != s.Sync.State || worker {
					t.Fatal("refused operation mutated enrollment state or started worker")
				}
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 0 {
					t.Fatal("metadata fault contacted hub or redeemed invitation")
				}
				disk, err := os.ReadFile(statePath)
				if scenario == "missing state" && !os.IsNotExist(err) || scenario != "missing state" && (err != nil || !bytes.Equal(disk, prior)) {
					t.Fatal("recovery destructively replaced prior sync state")
				}
			}
		})
	}
}

func TestDesktopAutomaticColdOpenSyncThenOfflineRetentionWithoutRedemption(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("MESH_NO_UPDATE_CHECK", "1")
	remoteRoot := t.TempDir()
	note, err := vault.CreateNote(remoteRoot, vault.NewNoteSpec{Title: "Automatic cold desktop peer", Template: "finding", Summary: "Synthetic cold-open background sync fixture.", Sections: map[string]string{"question": "Does opening a saved enrollment resume sync?", "findings": "The TLS fixture supplies one approved note.", "evidence": "Synthetic transport only.", "limitations": "No actual account or live permission proof.", "next_steps": "Review separately."}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(note.Path)
	if err != nil {
		t.Fatal(err)
	}
	var joins, syncs, denied atomic.Int32
	var offline atomic.Bool
	hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/join" {
			joins.Add(1)
			w.WriteHeader(500)
			return
		}
		if offline.Load() {
			denied.Add(1)
			w.WriteHeader(503)
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-cold-open-bearer" {
			t.Error("normal saved identity not used")
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/v1/vault" {
			json.NewEncoder(w).Encode(syncproto.VaultInfo{VaultID: "synthetic-cold-vault"})
			return
		}
		if r.URL.Path != "/v1/sync" {
			t.Error("unsupported automatic endpoint")
			w.WriteHeader(404)
			return
		}
		var req syncproto.SyncRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Outbox) != 0 {
			t.Error("cold-open uploaded an unsolicited starter")
		}
		syncs.Add(1)
		json.NewEncoder(w).Encode(syncproto.SyncResponse{HeadSHA: strings.Repeat("c", 40), FullReconcile: true, Deltas: []syncproto.Delta{{Path: "findings/" + note.ID + ".md", Op: "upsert", ContentB64: base64.StdEncoding.EncodeToString(raw)}}})
	}))
	defer hub.Close()
	transport := http.DefaultTransport
	http.DefaultTransport = hub.Client().Transport
	defer func() { http.DefaultTransport = transport }()
	root := legacyJoinedDesktopFixture(t)
	if os.WriteFile(filepath.Join(root, ".mesh", "credentials"), mustMarshal(map[string]string{"hub_url": hub.URL, "token": "synthetic-cold-open-bearer", "vault_id": "synthetic-cold-vault"}), 0600) != nil || os.WriteFile(filepath.Join(root, ".mesh", "sync.json"), mustMarshal(map[string]any{"hub_url": hub.URL, "vault_id": "synthetic-cold-vault", "hashes": map[string]string{}}), 0600) != nil {
		t.Fatal("synthetic saved enrollment")
	}
	for launch := 0; launch < 2; launch++ {
		e, err := New(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { e.Close() })
		deadline := time.Now().Add(4 * time.Second)
		for {
			status := e.status(t.Context())
			settled := status.Sync.State == "synced"
			if launch == 1 {
				settled = status.Sync.State == "offline" && denied.Load() > 0
			}
			if settled {
				if status.Identity == nil || status.Identity.Role != "unknown" || status.Identity.Verified {
					t.Fatal("automatic sync invented authority")
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("automatic startup worker did not settle: %+v", status)
			}
			time.Sleep(20 * time.Millisecond)
		}
		deadline = time.Now().Add(4 * time.Second)
		for {
			result, apiErr := e.Dispatch(t.Context(), "tool", mustMarshal(map[string]any{"name": "mesh_search", "arguments": map[string]any{"query": "Automatic cold desktop peer", "limit": 5, "budget": 2000}}))
			if apiErr == nil && bytes.Contains(mustMarshal(result), []byte(note.ID)) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("automatic peer download or offline cache unavailable")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		offline.Store(true)
	}
	if joins.Load() != 0 || syncs.Load() != 1 || denied.Load() != 1 {
		t.Fatalf("automatic cold-open retried redemption or sync: joins=%d sync=%d offline=%d", joins.Load(), syncs.Load(), denied.Load())
	}
}
