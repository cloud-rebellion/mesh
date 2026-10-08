// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrNativeInvalid         = errors.New("invalid native join")
	ErrNativePrivate         = errors.New("private vault metadata unavailable")
	ErrNativeAlreadyJoined   = errors.New("vault already joined")
	ErrNativeJoinUncertain   = errors.New("invitation redemption uncertain; do not redeem again")
	ErrNativeMetadataPending = errors.New("joined successfully; initial metadata is pending")
)

// NativeJoinReceipt deliberately contains no bearer or invitation. A persisted
// pending intent fences uncertain remote success; an accepted receipt survives
// metadata, first sync and index failures. Role is unknown: JoinResponse proves
// no role, and a browser handoff is not an enrollment/role discovery API.
type NativeJoinReceipt struct {
	State            string `json:"state"`
	HubURL           string `json:"hub_url"`
	InviteSHA256     string `json:"invite_sha256"`
	VaultID          string `json:"vault_id,omitempty"`
	User             string `json:"user,omitempty"`
	AcceptedAt       string `json:"accepted_at,omitempty"`
	CredentialSHA256 string `json:"credential_sha256,omitempty"`
}

func nativePrivate(vaultDir string) (*os.Root, error) {
	root, err := os.OpenRoot(vaultDir)
	if err != nil {
		return nil, ErrNativePrivate
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return nil, ErrNativePrivate
	}
	err = browserPrivateRoot(f)
	f.Close()
	if err != nil {
		return nil, ErrNativePrivate
	}
	before, err := root.Lstat(".mesh")
	if err != nil || !before.IsDir() {
		return nil, ErrNativePrivate
	}
	private, err := root.OpenRoot(".mesh")
	if err != nil {
		return nil, ErrNativePrivate
	}
	d, err := private.Open(".")
	if err != nil {
		private.Close()
		return nil, ErrNativePrivate
	}
	after, e := d.Stat()
	err = browserPrivateFile(d, true)
	d.Close()
	if e != nil || err != nil || !os.SameFile(before, after) {
		private.Close()
		return nil, ErrNativePrivate
	}
	if info, e := private.Lstat("sync.lock"); e == nil {
		if !info.Mode().IsRegular() {
			private.Close()
			return nil, ErrNativePrivate
		}
		lock, e := private.Open("sync.lock")
		if e != nil {
			private.Close()
			return nil, ErrNativePrivate
		}
		opened, e := lock.Stat()
		valid := e == nil && os.SameFile(info, opened) && browserPrivateFile(lock, false) == nil
		lock.Close()
		if !valid {
			private.Close()
			return nil, ErrNativePrivate
		}
	} else if !os.IsNotExist(e) {
		private.Close()
		return nil, ErrNativePrivate
	}
	return private, nil
}

func nativeCredentials(private *os.Root) (credentials, error) {
	var c credentials
	raw, err := readBrowserPrivate(private, "credentials", 16<<10)
	if err != nil || json.Unmarshal(raw, &c) != nil || !validBrowserBearer(c.Token) {
		return c, ErrNativePrivate
	}
	origin, err := browserOrigin(c.HubURL)
	if err != nil || origin != c.HubURL {
		return c, ErrNativeInvalid
	}
	return c, nil
}

// NativeJoinedInfo exposes only saved non-secret team metadata. Legacy CLI
// credentials never stored a user, so User may be empty; no role is inferred.
type NativeJoinedInfo struct {
	Joined          bool   `json:"joined"`
	VaultID         string `json:"vault_id,omitempty"`
	User            string `json:"user,omitempty"`
	MetadataPending bool   `json:"metadata_pending,omitempty"`
}

// ReadNativeJoinedInfo validates the selected private credential/sync pair
// without contacting the hub or exposing a bearer. An absent pair is unjoined local;
// a partial, corrupt or contradictory pair is an error unless a matching accepted
// native receipt proves enrollment. That exception remains metadata-pending and
// must not sync or rewrite existing state until a reviewed recovery is performed.
func ReadNativeJoinedInfo(ctx context.Context, vaultDir string) (NativeJoinedInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	root, err := os.OpenRoot(vaultDir)
	if os.IsNotExist(err) {
		return NativeJoinedInfo{}, nil
	}
	if err != nil {
		return NativeJoinedInfo{}, ErrNativePrivate
	}
	_, credErr := root.Lstat(".mesh/credentials")
	_, stateErr := root.Lstat(".mesh/sync.json")
	root.Close()
	if os.IsNotExist(credErr) && os.IsNotExist(stateErr) {
		return NativeJoinedInfo{}, nil
	}
	if credErr != nil || stateErr != nil && !os.IsNotExist(stateErr) {
		return NativeJoinedInfo{}, ErrNativePrivate
	}
	private, err := nativePrivate(vaultDir)
	if err != nil {
		return NativeJoinedInfo{}, err
	}
	defer private.Close()
	release, err := acquireVaultSyncLockContext(ctx, vaultDir)
	if err != nil {
		return NativeJoinedInfo{}, err
	}
	defer release()
	c, err := nativeCredentials(private)
	if err != nil {
		return NativeJoinedInfo{}, err
	}
	info := NativeJoinedInfo{Joined: true, VaultID: c.VaultID}
	accepted := false
	if r, err := readNativeReceipt(private); err == nil && r.State == "accepted" && r.HubURL == c.HubURL && r.VaultID == c.VaultID && r.CredentialSHA256 == contentHash([]byte(c.Token)) {
		info.User, accepted = r.User, true
	}
	if err := nativeSyncPair(private, c); err != nil {
		if !accepted {
			return NativeJoinedInfo{}, err
		}
		// Successful enrollment survived a later local-state failure. Preserve it
		// visibly; do not synthesize/overwrite a base or redeem the invitation again.
		info.MetadataPending = true
	}
	return info, nil
}

func nativeSyncPair(private *os.Root, c credentials) error {
	raw, err := readBrowserPrivate(private, "sync.json", 16<<20)
	var state syncState
	if err != nil || json.Unmarshal(raw, &state) != nil {
		return ErrNativePrivate
	}
	origin, err := browserOrigin(state.HubURL)
	if err != nil || origin != c.HubURL || c.VaultID == "" || state.VaultID != c.VaultID {
		return ErrNativePrivate
	}
	return nil
}

// ReadNativeJoinReceipt returns non-secret enrollment information. Missing
// receipts (legacy joins) do not fabricate identity or role evidence.
func ReadNativeJoinReceipt(vaultDir string) (NativeJoinReceipt, error) {
	private, err := nativePrivate(vaultDir)
	if err != nil {
		return NativeJoinReceipt{}, err
	}
	defer private.Close()
	r, err := readNativeReceipt(private)
	if err == nil && r.State == "accepted" {
		c, e := nativeCredentials(private)
		if e != nil || c.HubURL != r.HubURL || c.VaultID != r.VaultID || contentHash([]byte(c.Token)) != r.CredentialSHA256 {
			return NativeJoinReceipt{}, ErrNativeJoinUncertain
		}
	}
	return r, err
}
func readNativeReceipt(private *os.Root) (NativeJoinReceipt, error) {
	var r NativeJoinReceipt
	info, err := private.Lstat("native-join.json")
	if os.IsNotExist(err) {
		return r, os.ErrNotExist
	}
	if err != nil || !info.Mode().IsRegular() {
		return r, ErrNativePrivate
	}
	raw, err := readBrowserPrivate(private, "native-join.json", 4096)
	if err != nil || json.Unmarshal(raw, &r) != nil {
		return r, ErrNativePrivate
	}
	if (r.State != "pending" && r.State != "accepted") || len(r.InviteSHA256) != 64 {
		return r, ErrNativePrivate
	}
	if _, err := browserOrigin(r.HubURL); err != nil {
		return r, ErrNativePrivate
	}
	return r, nil
}
func writeNativeReceipt(vaultDir string, r NativeJoinReceipt) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeFileAtomicPrivate(filepath.Join(vaultDir, ".mesh", "native-join.json"), raw, 0600)
}

// JoinNativeVaultContext enrolls once and publishes the existing private local
// credential before any fallible metadata fetch. It does not synchronously clone
// or index. The desktop starts those cancellable phases separately. Existing
// joined directories never switch identity through this entry point.
func JoinNativeVaultContext(ctx context.Context, hubURL, invite, vaultDir string) (NativeJoinReceipt, error) {
	return joinNativeVaultContext(ctx, hubURL, invite, vaultDir, NewWithContext)
}
func joinNativeVaultContext(ctx context.Context, hubURL, invite, vaultDir string, newClient func(context.Context, string, string) *Client) (NativeJoinReceipt, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	origin, err := browserOrigin(hubURL)
	if err != nil || len(invite) == 0 || len(invite) > 4096 || strings.ContainsAny(invite, " \t\r\n") {
		return NativeJoinReceipt{}, ErrNativeInvalid
	}
	private, err := nativePrivate(vaultDir)
	if err != nil {
		return NativeJoinReceipt{}, err
	}
	defer private.Close()
	release, err := acquireVaultSyncLockContext(ctx, vaultDir)
	if err != nil {
		return NativeJoinReceipt{}, err
	}
	defer release()
	if old, e := readNativeReceipt(private); e == nil {
		if old.State == "accepted" && old.HubURL == origin && old.InviteSHA256 == contentHash([]byte(invite)) {
			c, e := nativeCredentials(private)
			if e != nil || c.HubURL != old.HubURL || c.VaultID != old.VaultID || contentHash([]byte(c.Token)) != old.CredentialSHA256 {
				return old, ErrNativeJoinUncertain
			}
			return old, nil
		}
		return old, ErrNativeJoinUncertain
	} else if !os.IsNotExist(e) {
		return NativeJoinReceipt{}, e
	}
	if _, e := private.Lstat("credentials"); e == nil {
		return NativeJoinReceipt{}, ErrNativeAlreadyJoined
	} else if !os.IsNotExist(e) {
		return NativeJoinReceipt{}, ErrNativePrivate
	}
	r := NativeJoinReceipt{State: "pending", HubURL: origin, InviteSHA256: contentHash([]byte(invite))}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if err = writeNativeReceipt(vaultDir, r); err != nil {
		return r, ErrNativePrivate
	}
	c := newClient(ctx, origin, "")
	// Clone the supplied transport policy (test fixtures may use a private CA),
	// while retaining the native no-cookie/no-redirect execution boundary.
	nativeClientPolicy(c)
	jr, err := c.Join(invite)
	if err != nil {
		return r, ErrNativeJoinUncertain
	}
	if !validBrowserBearer(jr.ClientToken) || !validBrowserIdentity(jr.User) || len(jr.VaultID) > 256 {
		return r, ErrNativeJoinUncertain
	}
	// Do not cancel between an accepted response and its durable credential/receipt.
	// An accepted enrollment must survive later network/index failure.
	if err = writeCredentials(vaultDir, credentials{HubURL: origin, Token: jr.ClientToken, VaultID: jr.VaultID}); err != nil {
		return r, ErrNativeJoinUncertain
	}
	r.State = "accepted"
	r.CredentialSHA256 = contentHash([]byte(jr.ClientToken))
	r.VaultID = jr.VaultID
	r.User = jr.User
	r.AcceptedAt = time.Now().UTC().Format(time.RFC3339)
	if err = writeNativeReceipt(vaultDir, r); err != nil {
		return r, ErrNativeJoinUncertain
	}
	if err = writeState(vaultDir, syncState{Hashes: map[string]string{}, VaultID: jr.VaultID, HubURL: origin}); err != nil {
		return r, ErrNativeMetadataPending
	}
	// A canceled shutdown does not undo successful enrollment.
	if ctx.Err() != nil {
		return r, ErrNativeMetadataPending
	}
	c.Token = jr.ClientToken
	vi, err := c.Vault()
	if err != nil || checkHomogeneity(vi.MeshToml) != nil || (jr.VaultID != "" && vi.VaultID != "" && jr.VaultID != vi.VaultID) {
		return r, ErrNativeMetadataPending
	}
	if r.VaultID == "" && vi.VaultID != "" {
		r.VaultID = vi.VaultID
		if writeCredentials(vaultDir, credentials{HubURL: origin, Token: jr.ClientToken, VaultID: r.VaultID}) != nil || writeNativeReceipt(vaultDir, r) != nil || writeState(vaultDir, syncState{Hashes: map[string]string{}, VaultID: r.VaultID, HubURL: origin}) != nil {
			return r, ErrNativeMetadataPending
		}
	}
	return r, nil
}

func nativeClientPolicy(c *Client) {
	copyHTTP := *c.HTTP
	copyHTTP.Jar = nil
	copyHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.HTTP = &copyHTTP
}

// SyncNativeVaultContext authenticates the private selected credential pair and
// HTTPS origin before using the normal cancellation/locking/durability pipeline.
func SyncNativeVaultContext(ctx context.Context, vaultDir string) (Summary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	private, err := nativePrivate(vaultDir)
	if err != nil {
		return Summary{}, err
	}
	defer private.Close()
	release, err := acquireVaultSyncLockContext(ctx, vaultDir)
	if err != nil {
		return Summary{}, err
	}
	defer release()
	c, err := nativeCredentials(private)
	if err != nil {
		return Summary{}, err
	}
	if err := nativeSyncPair(private, c); err != nil {
		return Summary{}, err
	}
	vi, err := NewWithContext(ctx, c.HubURL, c.Token).Vault()
	if ctx.Err() != nil {
		return Summary{}, ctx.Err()
	}
	if err != nil || checkHomogeneity(vi.MeshToml) != nil || (c.VaultID != "" && vi.VaultID != "" && c.VaultID != vi.VaultID) {
		return Summary{}, ErrNativeMetadataPending
	}
	return syncVaultRoundContext(ctx, vaultDir, true, nil)
}

// NativePendingCount counts local operations without revealing paths or tokens.
func NativePendingCount(ctx context.Context, vaultDir string) (int, error) {
	outbox, _, err := computeOutboxContext(ctx, vaultDir, readState(vaultDir).Hashes)
	return len(outbox), err
}
