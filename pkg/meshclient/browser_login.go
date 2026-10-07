// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// BrowserLogin is a short-lived, one-use browser confirmation capability. URL
// must never be logged or printed except for an explicit user's --print-url.
// It contains no long-lived device credential.
type BrowserLogin struct {
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expires_at"`
	ClientID  int64  `json:"client_id"`
	User      string `json:"user"`
	Role      string `json:"role"`
}

const browserLoginLimit = 4096

var errBrowserLogin = errors.New("browser sign-in unavailable; update the Mesh client and hub, then retry mesh open")

func browserOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(raw, "\r\n\t\\#") {
		return "", errors.New("browser sign-in requires a saved HTTPS hub origin without a path, query or credentials")
	}
	if u.Hostname() == "" || strings.ContainsAny(u.Host, " \t\r\n") {
		return "", errBrowserLogin
	}
	return "https://" + strings.ToLower(u.Host), nil
}

func validBrowserBearer(token string) bool {
	if len(token) == 0 || len(token) > 4096 {
		return false
	}
	for _, b := range []byte(token) {
		if b < 33 || b > 126 {
			return false
		}
	}
	return true
}

func validBrowserIdentity(s string) bool {
	if s == "" || len(s) > 256 || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ValidateBrowserLogin refuses foreign origins, alternate routes, encoded
// fragments, malformed identities and expired/overlong capabilities before a
// browser or manual handoff can receive them.
func ValidateBrowserLogin(savedHub string, login BrowserLogin, now time.Time) error {
	origin, err := browserOrigin(savedHub)
	if err != nil {
		return err
	}
	u, err := url.Parse(login.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || "https://"+strings.ToLower(u.Host) != origin || u.Path != "/team" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.RawFragment != "" {
		return errBrowserLogin
	}
	const prefix = "signin=mesh_login_"
	if !strings.HasPrefix(u.Fragment, prefix) {
		return errBrowserLogin
	}
	code := strings.TrimPrefix(u.Fragment, prefix)
	decoded, e := hex.DecodeString(code)
	if e != nil || len(decoded) != 32 || len(code) != 64 || hex.EncodeToString(decoded) != code || login.URL != origin+"/team#"+prefix+code {
		return errBrowserLogin
	}
	if login.ExpiresAt <= now.Unix() || login.ExpiresAt > now.Add(2*time.Minute).Unix() || login.ClientID <= 0 || !validBrowserIdentity(login.User) {
		return errBrowserLogin
	}
	switch login.Role {
	case "viewer", "member", "admin", "owner", "curator":
	default:
		return errBrowserLogin
	}
	return nil
}

// BrowserLogin creates one handoff with the existing device bearer only. The
// isolated client refuses all redirects, cookies, response echoes and retries.
func (c *Client) BrowserLogin(ctx context.Context, destination string) (BrowserLogin, error) {
	var login BrowserLogin
	if destination != "vault" && destination != "team" {
		return login, errors.New("browser destination must be vault or team")
	}
	origin, err := browserOrigin(c.HubURL)
	if err != nil {
		return login, err
	}
	if !validBrowserBearer(c.Token) {
		return login, errBrowserLogin
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(struct {
		Destination string `json:"destination"`
	}{destination})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/v1/browser-login", bytes.NewReader(body))
	if err != nil {
		return login, errBrowserLogin
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	client := http.Client{Timeout: 10 * time.Second}
	if c.HTTP != nil {
		client = *c.HTTP
		if client.Timeout <= 0 || client.Timeout > 10*time.Second {
			client.Timeout = 10 * time.Second
		}
	}
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return login, errBrowserLogin
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return login, fmt.Errorf("browser sign-in refused by hub (HTTP %d); retry mesh open after resolving access or updating the hub", response.StatusCode)
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return login, errBrowserLogin
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return login, errBrowserLogin
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, browserLoginLimit+1))
	if err != nil || len(data) > browserLoginLimit {
		return login, errBrowserLogin
	}
	login, err = decodeBrowserLogin(data)
	if err != nil {
		return BrowserLogin{}, errBrowserLogin
	}
	if err = ValidateBrowserLogin(c.HubURL, login, time.Now()); err != nil {
		return BrowserLogin{}, err
	}
	return login, nil
}

func decodeBrowserLogin(data []byte) (BrowserLogin, error) {
	var out BrowserLogin
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return out, errBrowserLogin
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return out, errBrowserLogin
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return out, errBrowserLogin
		}
		seen[key] = true
		switch key {
		case "url":
			err = d.Decode(&out.URL)
		case "expires_at":
			err = d.Decode(&out.ExpiresAt)
		case "client_id":
			err = d.Decode(&out.ClientID)
		case "user":
			err = d.Decode(&out.User)
		case "role":
			err = d.Decode(&out.Role)
		default:
			return out, errBrowserLogin
		}
		if err != nil {
			return out, errBrowserLogin
		}
	}
	if _, err = d.Token(); err != nil || len(seen) != 5 {
		return out, errBrowserLogin
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return out, errBrowserLogin
	}
	return out, nil
}

// PrepareBrowserLogin reads only the selected joined vault's private credential
// pair. It never redeems an invite, syncs, rotates credentials or repairs files.
func PrepareBrowserLogin(ctx context.Context, vaultDir, destination string) (BrowserLogin, error) {
	return prepareBrowserLogin(ctx, vaultDir, destination, New)
}

func prepareBrowserLogin(ctx context.Context, vaultDir, destination string, newClient func(string, string) *Client) (BrowserLogin, error) {
	var empty BrowserLogin
	root, err := os.OpenRoot(vaultDir)
	if err != nil {
		return empty, errors.New("cannot open joined vault")
	}
	defer root.Close()
	rootFile, err := root.Open(".")
	if err != nil {
		return empty, errBrowserLogin
	}
	defer rootFile.Close()
	if err = browserPrivateRoot(rootFile); err != nil {
		return empty, err
	}
	before, err := root.Lstat(".mesh")
	if err != nil || !before.IsDir() {
		return empty, errors.New("joined vault needs a private .mesh directory")
	}
	private, err := root.OpenRoot(".mesh")
	if err != nil {
		return empty, errBrowserLogin
	}
	defer private.Close()
	dir, err := private.Open(".")
	if err != nil {
		return empty, errBrowserLogin
	}
	defer dir.Close()
	after, err := dir.Stat()
	if err != nil || !os.SameFile(before, after) {
		return empty, errBrowserLogin
	}
	if err = browserPrivateFile(dir, true); err != nil {
		return empty, err
	}
	// Share join/sync serialization, but refuse unsafe existing lock files before
	// the older lock helper can follow them. No credential/state writes occur.
	if info, e := private.Lstat("sync.lock"); e == nil {
		if !info.Mode().IsRegular() {
			return empty, errBrowserLogin
		}
		lock, e := private.Open("sync.lock")
		if e != nil {
			return empty, errBrowserLogin
		}
		opened, e := lock.Stat()
		if e != nil || !os.SameFile(info, opened) {
			lock.Close()
			return empty, errBrowserLogin
		}
		e = browserPrivateFile(lock, false)
		lock.Close()
		if e != nil {
			return empty, e
		}
	} else if !os.IsNotExist(e) {
		return empty, errBrowserLogin
	}
	release, err := acquireVaultSyncLock(vaultDir)
	if err != nil {
		return empty, errors.New("cannot lock joined vault for browser sign-in")
	}
	defer release()
	current, err := root.Lstat(".mesh")
	if err != nil || !os.SameFile(before, current) {
		return empty, errBrowserLogin
	}
	credBytes, err := readBrowserPrivate(private, "credentials", 16<<10)
	if err != nil {
		return empty, err
	}
	stateBytes, err := readBrowserPrivate(private, "sync.json", 16<<20)
	if err != nil {
		return empty, err
	}
	var creds credentials
	var state syncState
	if json.Unmarshal(credBytes, &creds) != nil || json.Unmarshal(stateBytes, &state) != nil {
		return empty, errors.New("joined vault credential/sync metadata is invalid; do not repeat mesh join")
	}
	origin, err := browserOrigin(creds.HubURL)
	if err != nil {
		return empty, err
	}
	stateOrigin, err := browserOrigin(state.HubURL)
	if err != nil || stateOrigin != origin || creds.VaultID == "" || state.VaultID != creds.VaultID {
		return empty, errors.New("joined vault credential/sync identities differ; resolve with mesh sync before browser sign-in")
	}
	if !validBrowserBearer(creds.Token) {
		return empty, errBrowserLogin
	}
	return newClient(creds.HubURL, creds.Token).BrowserLogin(ctx, destination)
}

func readBrowserPrivate(root *os.Root, name string, limit int64) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit {
		return nil, errors.New("joined vault requires private credentials and sync metadata")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errBrowserLogin
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errBrowserLogin
	}
	if err = browserPrivateFile(file, false); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errBrowserLogin
	}
	return data, nil
}

// browserPrivatePath is confined to this selected vault; platform checks never
// inspect a user's other vaults, browser stores, keychains or credentials.
func browserPrivatePath(file *os.File) string {
	absolute, _ := filepath.Abs(file.Name())
	return absolute
}
