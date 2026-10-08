// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package desktop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/mcp"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/bright-interaction/mesh/internal/web"
	"github.com/bright-interaction/mesh/pkg/meshclient"
)

type Status struct {
	State string `json:"state"`
	Vault struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"vault"`
	Sync struct {
		State       string  `json:"state"`
		Pending     int     `json:"pending"`
		Conflicts   int     `json:"conflicts"`
		LastSuccess *string `json:"last_success"`
	} `json:"sync"`
	Identity *Identity `json:"identity"`
	Version  string    `json:"version"`
}
type Identity struct {
	User     string `json:"user"`
	Role     string `json:"role"`
	Verified bool   `json:"verified"`
}
type localInfo struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	ForJoin bool   `json:"for_join,omitempty"`
}
type Engine struct {
	root        string
	ctx         context.Context
	cancel      context.CancelFunc
	web         *web.Server
	mcp         *mcp.Server
	mu          sync.Mutex
	info        localInfo
	state       string
	syncState   string
	lastSuccess *string
	conflicts   int
	identity    *Identity
	worker      bool
	wg          sync.WaitGroup
	closeOnce   sync.Once
	closeErr    error
}

func New(ctx context.Context, root string) (*Engine, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return nil, errors.New("invalid selected vault")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(root); err == nil {
		if !info.IsDir() {
			return nil, errors.New("invalid selected vault")
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("selected vault unavailable")
	}
	lifetime, cancel := context.WithCancel(ctx)
	e := &Engine{root: root, ctx: lifetime, cancel: cancel, state: "uninitialized", syncState: "unjoined", info: localInfo{Name: filepath.Base(root)}}
	if info, err := os.Lstat(filepath.Join(root, ".mesh")); err == nil && !info.IsDir() {
		cancel()
		return nil, errors.New("unsafe vault metadata")
	}
	if raw, err := readLocalInfo(root); err == nil {
		var info localInfo
		if json.Unmarshal(raw, &info) == nil && validName(info.Name) && len(info.ID) <= 256 {
			e.info = info
		}
	}
	if r, err := meshclient.ReadNativeJoinReceipt(root); err == nil {
		if r.State == "pending" {
			e.state = "join-uncertain"
		} else {
			e.identity = &Identity{User: r.User, Role: "unknown"}
			if r.VaultID != "" {
				e.info.ID = r.VaultID
			}
			e.syncState = "offline"
			infoCtx, infoCancel := context.WithTimeout(ctx, 2*time.Second)
			joined, joinedErr := meshclient.ReadNativeJoinedInfo(infoCtx, root)
			infoCancel()
			if joinedErr != nil || joined.MetadataPending {
				e.state, e.syncState = "joined-preparing", "metadata-pending"
			}
		}
	} else if errors.Is(err, meshclient.ErrNativeJoinUncertain) {
		e.state, e.syncState = "join-uncertain", "offline"
	} else {
		joinInfoCtx, joinInfoCancel := context.WithTimeout(ctx, 2*time.Second)
		joined, err := meshclient.ReadNativeJoinedInfo(joinInfoCtx, root)
		joinInfoCancel()
		if err != nil {
			cancel()
			return nil, err
		}
		if joined.Joined {
			user := joined.User
			if user == "" {
				user = "Unknown team identity"
			}
			e.identity = &Identity{User: user, Role: "unknown"}
			e.info.ID, e.syncState = joined.VaultID, "offline"
		}
	}
	files, err := vault.WalkContext(ctx, root)
	if ctx.Err() != nil {
		cancel()
		return nil, ctx.Err()
	}
	if err == nil && (len(files) > 0 || e.info.ID != "") {
		priorState := e.state
		if err = e.open(); err != nil {
			cancel()
			return nil, err
		}
		if priorState == "join-uncertain" || priorState == "joined-preparing" {
			e.state = priorState
		}
	}

	// An already-enrolled vault resumes the normal cancelable sync worker on
	// open. Personal/staging vaults and uncertain or metadata-pending enrollment
	// have no established usable pair here and must remain locally gated.
	if e.state == "ready" && e.identity != nil && e.syncState == "offline" {
		e.startWorker(false, "", "")
	}
	return e, nil
}

func (e *Engine) open() error {
	if e.web != nil {
		return nil
	}
	if info, err := os.Lstat(filepath.Join(e.root, ".mesh")); err == nil && !info.IsDir() {
		return errors.New("unsafe vault metadata")
	}
	s, err := web.NewOwningServerWithRetrievalOptions(e.ctx, e.root, retrieve.ConstructionOptions{LocalOnly: true})
	if errors.Is(err, index.ErrOwnerHeld) {
		s, err = web.NewServerWithRetrievalOptions(e.ctx, e.root, retrieve.ConstructionOptions{LocalOnly: true})
	}
	if err != nil {
		return err
	}
	reader, err := mcp.NewServerWithRetrievalOptions(e.ctx, e.root, retrieve.ConstructionOptions{LocalOnly: true})
	if err != nil {
		s.Close()
		return err
	}
	e.web = s
	e.mcp = reader
	e.state = "ready"
	return nil
}

func (e *Engine) Dispatch(ctx context.Context, method string, params json.RawMessage) (any, *APIError) {
	if ctx == nil {
		ctx = context.Background()
	}
	if method == "close" {
		var empty struct{}
		if strictJSON(params, &empty) != nil {
			return nil, fail("INVALID_PARAMS", "Invalid close parameters")
		}
		if err := e.Close(); err != nil {
			return nil, fail("CLOSE_FAILED", "Desktop shutdown incomplete")
		}
		return map[string]bool{"closed": true}, nil
	}
	if e.ctx.Err() != nil {
		return nil, fail("CLOSED", "Desktop core is closed")
	}
	// Requests are dispatched serially by Serve. Background worker state has its
	// own mutex; local publication continues to use Mesh's normal note locks.
	switch method {
	case "status":
		var empty struct{}
		if strictJSON(params, &empty) != nil {
			return nil, fail("INVALID_PARAMS", "Invalid status parameters")
		}
		return e.status(ctx), nil
	case "init":
		var p struct {
			Name    string `json:"name"`
			ForJoin bool   `json:"for_join,omitempty"`
		}
		if strictJSON(params, &p) != nil || !validName(p.Name) {
			return nil, fail("INVALID_PARAMS", "A vault name is required")
		}
		if e.web != nil {
			return nil, fail("ALREADY_INITIALIZED", "Vault already exists")
		}
		entries, err := os.ReadDir(e.root)
		if err != nil && !os.IsNotExist(err) {
			return nil, fail("VAULT_UNAVAILABLE", "Selected vault unavailable")
		}
		if len(entries) > 0 {
			return nil, fail("VAULT_NOT_EMPTY", "Choose a new empty managed vault")
		}
		if os.MkdirAll(e.root, 0700) != nil || os.Mkdir(filepath.Join(e.root, ".mesh"), 0700) != nil {
			return nil, fail("VAULT_UNAVAILABLE", "Cannot initialize selected vault")
		}
		id := make([]byte, 16)
		if _, err = rand.Read(id); err != nil {
			return nil, fail("VAULT_UNAVAILABLE", "Cannot initialize selected vault")
		}
		e.info = localInfo{Name: p.Name, ID: hex.EncodeToString(id), ForJoin: p.ForJoin}
		raw, _ := json.Marshal(e.info)
		if err = writePrivate(filepath.Join(e.root, ".mesh", "desktop.json"), raw); err != nil {
			return nil, fail("VAULT_UNAVAILABLE", "Cannot initialize selected vault")
		}
		if !p.ForJoin {
			_, err = vault.CreateNoteContext(ctx, e.root, vault.NewNoteSpec{Title: p.Name + " index " + e.info.ID[:8], Template: "index", Type: vault.TypeMap, Summary: "Start here to browse " + p.Name + ".", Sections: map[string]string{"scope": "Knowledge in " + p.Name + ".", "start_here": "Create a note from an approved template, or browse the local graph and Topics.", "grouped_links": "Links will appear as notes are added."}, Tags: []string{"index"}, Author: "desktop user"})
			if err != nil {
				return nil, fail("INIT_FAILED", "Vault initialization requires recovery")
			}
		}
		if err = e.open(); err != nil {
			return nil, fail("INDEX_UNAVAILABLE", "Vault created; index unavailable")
		}
		return e.status(ctx), nil
	case "web":
		if e.web == nil {
			return nil, fail("NOT_INITIALIZED", "Create or open a vault first")
		}
		var p struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		}
		if strictJSON(params, &p) != nil || p.Method != http.MethodGet || !allowedWebPath(p.Path) {
			return nil, fail("INVALID_PARAMS", "Unsupported viewer request")
		}
		return e.serveWeb(ctx, p.Path)
	case "tool":
		if e.mcp == nil {
			return nil, fail("NOT_INITIALIZED", "Create or open a vault first")
		}
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if strictJSON(params, &p) != nil || !allowedTool(p.Name) || len(p.Arguments) == 0 || p.Arguments[0] != '{' {
			return nil, fail("INVALID_PARAMS", "Unsupported Mesh tool")
		}
		return e.serveTool(ctx, p.Name, p.Arguments)
	case "sync":
		var empty struct{}
		if strictJSON(params, &empty) != nil {
			return nil, fail("INVALID_PARAMS", "Invalid sync parameters")
		}
		if e.web == nil {
			return nil, fail("NOT_INITIALIZED", "Create or open a vault first")
		}
		e.mu.Lock()
		metadataPending := e.state == "joined-preparing" || e.syncState == "metadata-pending"
		e.mu.Unlock()
		if metadataPending {
			return nil, fail("SYNC_METADATA_PENDING", "Enrollment is saved; initial sync metadata requires recovery. Do not redeem the invitation again")
		}
		e.startWorker(false, "", "")
		return map[string]bool{"started": true}, nil
	case "join":
		var p struct {
			Hub    string `json:"hub_url"`
			Invite string `json:"invite"`
		}
		if strictJSON(params, &p) != nil || !validJoin(p.Hub, p.Invite) {
			return nil, fail("INVALID_PARAMS", "Join requires a valid HTTPS hub and invitation")
		}
		if e.web == nil {
			return nil, fail("NOT_INITIALIZED", "Create a new managed vault first")
		}
		e.mu.Lock()
		busy, joined, uncertain := e.worker, e.identity != nil, e.state == "join-uncertain"
		e.mu.Unlock()
		if joined {
			return nil, fail("ALREADY_JOINED", "Enrollment is already saved; use the existing vault without redeeming another invitation")
		}
		if uncertain {
			return nil, fail("JOIN_UNCERTAIN", "Enrollment outcome requires recovery; do not redeem another invitation")
		}
		if busy {
			return nil, fail("BUSY", "A vault operation is already running")
		}
		e.startWorker(true, p.Hub, p.Invite)
		return map[string]string{"state": "joining"}, nil
	default:
		return nil, fail("METHOD_NOT_ALLOWED", "Unsupported desktop operation")
	}
}

func validName(s string) bool {
	if s == "" || strings.TrimSpace(s) != s || !utf8.ValidString(s) || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func validJoin(hub, invite string) bool {
	u, err := url.Parse(hub)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Opaque == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawPath == "" && (u.Path == "" || u.Path == "/") && len(invite) > 0 && len(invite) <= 4096 && !strings.ContainsAny(hub+invite, " \t\r\n\\")
}

func (e *Engine) status(ctx context.Context) Status {
	e.mu.Lock()
	s := Status{State: e.state, Version: Version}
	s.Vault.Name = e.info.Name
	s.Vault.ID = e.info.ID
	s.Sync.State = e.syncState
	s.Sync.LastSuccess = e.lastSuccess
	s.Sync.Conflicts = e.conflicts
	if e.identity != nil {
		copyIdentity := *e.identity
		s.Identity = &copyIdentity
	}
	e.mu.Unlock()
	countCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if pending, err := meshclient.NativePendingCount(countCtx, e.root); err == nil {
		s.Sync.Pending = pending
	}
	if files, err := vault.WalkConflictSiblings(e.root); err == nil {
		s.Sync.Conflicts = len(files)
	}
	return s
}

func (e *Engine) startWorker(join bool, hub, invite string) {
	e.mu.Lock()
	if e.worker {
		e.mu.Unlock()
		return
	}
	e.worker = true
	e.syncState = "syncing"
	if join {
		e.state = "joining"
	}
	e.wg.Add(1)
	e.mu.Unlock()
	go func() {
		defer e.wg.Done()
		defer func() { e.mu.Lock(); e.worker = false; e.mu.Unlock() }()
		if join {
			receipt, err := meshclient.JoinNativeVaultContext(e.ctx, hub, invite, e.root)
			metadataPending := false
			if receipt.State == "accepted" && err != nil {
				infoCtx, infoCancel := context.WithTimeout(e.ctx, 2*time.Second)
				joinedInfo, infoErr := meshclient.ReadNativeJoinedInfo(infoCtx, e.root)
				infoCancel()
				metadataPending = infoErr != nil || joinedInfo.MetadataPending
			}
			e.mu.Lock()
			if receipt.State == "accepted" {
				e.identity = &Identity{User: receipt.User, Role: "unknown"}
				if receipt.VaultID != "" {
					e.info.ID = receipt.VaultID
				}
				e.state = "ready"
			}
			if err != nil {
				if receipt.State == "accepted" {
					e.state, e.syncState = "ready", "offline"
					if metadataPending {
						e.state, e.syncState = "joined-preparing", "metadata-pending"
					}
				} else if errors.Is(err, meshclient.ErrNativeJoinUncertain) {
					e.state = "join-uncertain"
					e.syncState = "offline"
				} else {
					e.state = "ready"
					e.syncState = "offline"
				}
				e.mu.Unlock()
				return
			}
			e.mu.Unlock()
		}
		for {
			sum, err := meshclient.SyncNativeVaultContext(e.ctx, e.root)
			e.mu.Lock()
			if err != nil {
				e.syncState = "offline"
				if e.state == "joined-preparing" {
					e.syncState = "metadata-pending"
				}
				if _, missing := os.Lstat(filepath.Join(e.root, ".mesh", "credentials")); os.IsNotExist(missing) {
					e.syncState = "unjoined"
				}
			} else {
				e.state = "ready"
				e.conflicts = sum.Conflicts + len(sum.Protected)
				if e.conflicts > 0 {
					e.syncState = "conflicted"
				} else if sum.Remaining > 0 || len(sum.Rejected) > 0 || len(sum.Blocked) > 0 {
					e.syncState = "pending"
				} else {
					e.syncState = "synced"
				}
				now := time.Now().UTC().Format(time.RFC3339)
				e.lastSuccess = &now
			}
			e.mu.Unlock()
			if errors.Is(err, meshclient.ErrNativePrivate) || e.ctx.Err() != nil {
				return
			}
			timer := time.NewTimer(15 * time.Second)
			select {
			case <-e.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.cancel()
		e.wg.Wait()
		if e.mcp != nil {
			e.closeErr = e.mcp.Close()
		}
		if e.web != nil {
			e.closeErr = errors.Join(e.closeErr, e.web.Close())
		}
		e.mu.Lock()
		e.state = "closed"
		e.mu.Unlock()
	})
	return e.closeErr
}

func allowedTool(name string) bool {
	switch name {
	case "mesh_templates", "mesh_note_template", "mesh_block_template", "mesh_author_note", "mesh_prepare_update", "mesh_drafts", "mesh_search", "mesh_fetch":
		return true
	}
	return false
}
func (e *Engine) serveTool(ctx context.Context, name string, args json.RawMessage) (any, *APIError) {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": json.RawMessage(args)}})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://desktop.invalid/mcp", bytes.NewReader(raw))
	if err != nil {
		return nil, fail("TOOL_FAILED", "Mesh tool unavailable")
	}
	recorder := newBoundedRecorder()
	e.mcp.HandleHTTP(recorder, request)
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if recorder.overflow || json.Unmarshal(recorder.body.Bytes(), &rpc) != nil || len(rpc.Result) == 0 || len(rpc.Error) > 0 {
		return nil, fail("TOOL_FAILED", "Mesh tool refused or unavailable")
	}
	// Shared MCP already bounds and scrubs diagnostics. Content is opaque here:
	// replacing strings in Markdown/code/provenance would corrupt a later edit.
	return rpc.Result, nil
}

// A durable private metadata write; never truncates an existing credential.
func readLocalInfo(root string) ([]byte, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	info, err := r.Lstat(".mesh/desktop.json")
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return nil, errors.New("desktop metadata unavailable")
	}
	f, err := r.Open(".mesh/desktop.json")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("desktop metadata changed")
	}
	return io.ReadAll(io.LimitReader(f, 4097))
}

func writePrivate(target string, raw []byte) error {
	dir := filepath.Dir(target)
	f, err := os.CreateTemp(dir, ".desktop-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, target); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir makes the renamed private metadata entry durable. A failure after
// rename is returned to the caller even though the new bytes may be visible.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	return errors.Join(syncErr, d.Close())
}

type boundedRecorder struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	overflow bool
}

func newBoundedRecorder() *boundedRecorder {
	return &boundedRecorder{header: make(http.Header), status: 200}
}
func (r *boundedRecorder) Header() http.Header    { return r.header }
func (r *boundedRecorder) WriteHeader(status int) { r.status = status }
func (r *boundedRecorder) Write(b []byte) (int, error) {
	if r.body.Len()+len(b) > MaxResponse/2 {
		r.overflow = true
		return 0, io.ErrShortWrite
	}
	return r.body.Write(b)
}

var assetPath = regexp.MustCompile(`^/assets/[a-zA-Z0-9_-]+(?:/[a-zA-Z0-9_-]+)*\.[a-z0-9]+$`)
var noteDocPath = regexp.MustCompile(`^/api/(?:note|docs)/[a-zA-Z0-9_-]+$`)
var numericQuery = regexp.MustCompile(`^\d{1,6}$`)

func allowedWebPath(raw string) bool {
	if len(raw) > 4096 || strings.ContainsAny(raw, "\\\r\n\x00#") || strings.Contains(strings.ToLower(raw), "%2e") || strings.Contains(strings.ToLower(raw), "%2f") || strings.Contains(strings.ToLower(raw), "%5c") || strings.Contains(strings.ToLower(raw), "%00") {
		return false
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || strings.ContainsAny(u.Path, "\\\r\n\x00") || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || path.Clean(u.Path) != u.Path {
		return false
	}
	if u.Path == "/api/search" {
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(q) > 4 {
			return false
		}
		for key, values := range q {
			if len(values) != 1 {
				return false
			}
			switch key {
			case "q":
				if len(values[0]) > 1024 {
					return false
				}
			case "limit", "budget", "offset":
				if !numericQuery.MatchString(values[0]) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if u.RawQuery != "" || u.ForceQuery {
		return false
	}
	switch u.Path {
	case "/", "/graph.json", "/api/status", "/api/docs":
		return true
	}
	return assetPath.MatchString(u.Path) || noteDocPath.MatchString(u.Path)
}
func (e *Engine) serveWeb(ctx context.Context, path string) (any, *APIError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://desktop.invalid"+path, nil)
	if err != nil {
		return nil, fail("WEB_FAILED", "Viewer unavailable")
	}
	recorder := newBoundedRecorder()
	e.web.Handler().ServeHTTP(recorder, req)
	if recorder.overflow {
		return nil, fail("RESPONSE_TOO_LARGE", "Viewer response exceeds desktop limit")
	}
	body := recorder.body.Bytes()
	if req.URL.Path == "/graph.json" || req.URL.Path == "/api/status" {
		var value map[string]any
		if json.Unmarshal(body, &value) == nil {
			// These are filesystem diagnostics, not note content or graph labels.
			if req.URL.Path == "/api/status" {
				delete(value, "vault")
			}
			if meta, ok := value["meta"].(map[string]any); ok && req.URL.Path == "/graph.json" {
				delete(meta, "vault")
			}
			body, _ = json.Marshal(value)
		}
	}
	return map[string]any{"status": recorder.status, "headers": map[string]string{"Content-Type": recorder.header.Get("Content-Type"), "Cache-Control": "no-store"}, "body_base64": base64.StdEncoding.EncodeToString(body)}, nil
}
