// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package meshclient is the Mesh team-sync client: the low-level RPC transport to
// a mesh-hub plus the high-level vault orchestration (join + reconcile) that
// reads and writes a vault's local sync state. It never runs git; it speaks the
// pull-based reconcile protocol over HTTPS. cmd/mesh drives it, and the future
// TUI RemoteBackend will reuse it.
package meshclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/syncproto"
	"github.com/bright-interaction/mesh/internal/syncwire"
)

// Client is the HTTP transport to one hub.
type Client struct {
	requestContext context.Context
	HubURL         string
	Token          string
	HTTP           *http.Client

	// syncRequestZstd is learned from a previous successful sync response and
	// persisted by the vault orchestrator. It is deliberately opt-in so a new
	// client can talk to an old hub without a failed probe.
	syncRequestZstd bool
}

// New builds a client for a hub base URL and (optional) bearer token.
func New(hubURL, token string) *Client {
	return &Client{
		HubURL: strings.TrimRight(hubURL, "/"),
		Token:  token,
		HTTP:   &http.Client{Timeout: 60 * time.Second},
	}
}

// NewWithContext preserves the existing transport API while making requests
// cancelable. Context clients refuse redirects so a bearer/invitation cannot
// leave the selected hub through a redirect.
func NewWithContext(ctx context.Context, hubURL, token string) *Client {
	if ctx == nil {
		ctx = context.Background()
	}
	c := New(hubURL, token)
	c.requestContext = ctx
	c.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// Join redeems a one-time invite for a client token (no bearer needed).
func (c *Client) Join(invite string) (syncproto.JoinResponse, error) {
	var jr syncproto.JoinResponse
	err := c.rpc("POST", "/v1/join", syncproto.JoinRequest{Invite: invite}, false, &jr)
	return jr, err
}

// Vault fetches vault metadata (authed).
func (c *Client) Vault() (syncproto.VaultInfo, error) {
	var vi syncproto.VaultInfo
	err := c.rpc("GET", "/v1/vault", nil, true, &vi)
	return vi, err
}

// Sync runs one reconcile round (authed). It always declares the protocol this
// build speaks, so the hub can refuse a client that predates a response field
// instead of sending one it would silently mishandle (see syncproto.ProtoVersion).
func (c *Client) Sync(req syncproto.SyncRequest) (syncproto.SyncResponse, error) {
	req.Proto = syncproto.ProtoVersion
	var sr syncproto.SyncResponse
	opts := rpcOptions{acceptZstd: true, requestZstd: c.syncRequestZstd}
	headers, err := c.rpcHeadersOptions("POST", "/v1/sync", req, true, &sr, opts)
	if err != nil && c.syncRequestZstd && safeZstdDowngradeRetry(err) {
		// A rolled-back/older hub rejects the content coding before it can decode or
		// mutate the sync request. Retrying plain JSON is therefore not a duplicate
		// write. No other error is retried.
		sr = syncproto.SyncResponse{}
		headers, err = c.rpcHeadersOptions("POST", "/v1/sync", req, true, &sr, rpcOptions{acceptZstd: true})
	}
	if err == nil {
		sr.RequestZstd = syncwire.AcceptsZstd(headers.Get(syncproto.SyncEncodingHeader))
	}
	return sr, err
}

// UseZstdSyncRequests enables request compression after a hub has advertised it.
// Response compression is negotiated independently on every Sync call.
func (c *Client) UseZstdSyncRequests(enabled bool) { c.syncRequestZstd = enabled }

func (c *Client) rpc(method, path string, body any, authed bool, out any) error {
	_, err := c.rpcHeaders(method, path, body, authed, out)
	return err
}

// rpcHeaders is rpc plus the response headers, for the one contract that puts part of
// its answer in a header: the curation activity trail returns its paging position in
// one, because the response BODY type is shared with the pending-jobs route and adding
// a field there would change what every existing client decodes. A caller that cannot
// see the headers cannot page, which is exactly how the client ended up able to read
// only the newest page of an endpoint that had a cursor.
//
// The headers come back only on success; every error path returns nil, so a caller can
// never read a paging position off a response the hub refused.
func (c *Client) rpcHeaders(method, path string, body any, authed bool, out any) (http.Header, error) {
	return c.rpcHeadersOptions(method, path, body, authed, out, rpcOptions{})
}

type rpcOptions struct {
	acceptZstd  bool
	requestZstd bool
}

type hubError struct {
	status     int
	statusLine string
	body       string
}

func (e *hubError) Error() string { return fmt.Sprintf("hub %s: %s", e.statusLine, e.body) }

func safeZstdDowngradeRetry(err error) bool {
	var he *hubError
	if !errors.As(err, &he) {
		return false
	}
	if he.status == http.StatusUnsupportedMediaType {
		return true
	}
	// Pre-zstd Mesh reaches JSON decoding with compressed bytes and emits this
	// exact error before entering the ref lock. Restricting the fallback to that
	// body avoids replaying any request that could have reached mutation.
	return he.status == http.StatusBadRequest && he.body == `{"error":"bad request"}`
}

func (c *Client) rpcHeadersOptions(method, path string, body any, authed bool, out any, opts rpcOptions) (http.Header, error) {
	var rdr io.Reader
	if body != nil {
		if opts.requestZstd {
			pr, pw := io.Pipe()
			rdr = pr
			go func() {
				zw, err := syncwire.NewZstdWriter(pw)
				if err != nil {
					_ = pw.CloseWithError(err)
					return
				}
				encodeErr := json.NewEncoder(zw).Encode(body)
				closeErr := zw.Close()
				if encodeErr != nil {
					_ = pw.CloseWithError(encodeErr)
					return
				}
				_ = pw.CloseWithError(closeErr)
			}()
		} else {
			var buf bytes.Buffer
			if err := json.NewEncoder(&buf).Encode(body); err != nil {
				return nil, err
			}
			rdr = &buf
		}
	}
	ctx := c.requestContext
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, c.HubURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if opts.acceptZstd {
		req.Header.Set("Accept-Encoding", syncwire.EncodingZstd)
	}
	if opts.requestZstd {
		req.Header.Set("Content-Encoding", syncwire.EncodingZstd)
	}
	// Every request carries the protocol version, including the ones with no body,
	// so the hub's audit log can spot a stale client before it corrupts anything.
	req.Header.Set(syncproto.ProtoHeader, strconv.Itoa(syncproto.ProtoVersion))
	if authed {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var responseBody io.Reader = resp.Body
	switch encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); encoding {
	case "", "identity":
	case syncwire.EncodingZstd:
		zr, zerr := syncwire.NewZstdReader(resp.Body)
		if zerr != nil {
			return nil, fmt.Errorf("decode hub zstd response: %w", zerr)
		}
		responseBody = zr
		defer zr.Close()
	default:
		return nil, fmt.Errorf("hub response uses unsupported content encoding %q", encoding)
	}
	data, readErr := syncwire.ReadAllLimited(responseBody, syncwire.ResponseLimit)
	if readErr != nil {
		return nil, fmt.Errorf("read hub response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &hubError{status: resp.StatusCode, statusLine: resp.Status, body: strings.TrimSpace(string(data))}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, err
		}
	}
	return resp.Header, nil
}
