// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
package meshclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const browserFixtureToken = "mesh_client_SYNTHETIC_BROWSER_FIXTURE_ONLY"
const browserFixtureCode = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func fixtureBrowserLogin(origin string) BrowserLogin {
	return BrowserLogin{origin + "/team#signin=mesh_login_" + browserFixtureCode, time.Now().Add(90 * time.Second).Unix(), 7, "fixture-user", "member"}
}
func writeBrowserFixture(t *testing.T, dir, origin string) {
	t.Helper()
	if e := writeCredentials(dir, credentials{origin, browserFixtureToken, "fixture-vault"}); e != nil {
		t.Fatal(e)
	}
	if e := writeState(dir, syncState{VaultID: "fixture-vault", HubURL: origin, Hashes: map[string]string{}}); e != nil {
		t.Fatal(e)
	}
}
func assertNoBrowserSecret(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("wanted refusal")
	}
	if strings.Contains(err.Error(), browserFixtureToken) || strings.Contains(err.Error(), browserFixtureCode) {
		t.Fatal("error exposed synthetic credential/capability")
	}
}
func TestBrowserLoginBoundedBearerRequestAndPrivateReceipt(t *testing.T) {
	var origin string
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/browser-login" || r.Host != strings.TrimPrefix(origin, "https://") {
			t.Error("wrong target")
		}
		if r.Header.Get("Authorization") != "Bearer "+browserFixtureToken || r.Header.Get("Cookie") != "" {
			t.Error("not bearer-only")
		}
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["destination"] != "team" {
			t.Error("wrong body")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(fixtureBrowserLogin(origin))
	}))
	defer server.Close()
	origin = server.URL
	client := New(origin, browserFixtureToken)
	client.HTTP = server.Client()
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(origin)
	jar.SetCookies(u, []*http.Cookie{{Name: "existing", Value: "must-not-be-sent"}})
	client.HTTP.Jar = jar
	login, err := client.BrowserLogin(context.Background(), "team")
	if err != nil {
		t.Fatal(err)
	}
	if login.ClientID != 7 || login.Role != "member" || hits.Load() != 1 {
		t.Fatal("receipt mismatch")
	}
	if client.HTTP.Jar != jar {
		t.Fatal("mutated existing HTTP client")
	}
}
func TestBrowserLoginRedirectsNeverForwardBearerOrRetry(t *testing.T) {
	var foreign atomic.Int32
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1); t.Error("redirect reached") }))
	defer sink.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits atomic.Int32
			source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Location", sink.URL+"/capture?token="+browserFixtureToken)
				w.WriteHeader(status)
				w.Write([]byte(browserFixtureCode))
			}))
			defer source.Close()
			client := New(source.URL, browserFixtureToken)
			client.HTTP = source.Client()
			client.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { t.Fatal("existing redirect policy used"); return nil }
			_, err := client.BrowserLogin(context.Background(), "vault")
			assertNoBrowserSecret(t, err)
			if hits.Load() != 1 || foreign.Load() != 0 {
				t.Fatal("redirect or retry")
			}
		})
	}
}
func TestBrowserLoginResponseRefusalsDoNotEchoSecrets(t *testing.T) {
	for _, which := range []string{"foreign", "http", "userinfo", "query", "route", "encoded-path", "encoded-fragment", "uppercase-code", "short-code", "expired", "long-expiry", "bad-client", "bad-role", "control-user", "duplicate", "unknown", "missing", "trailing", "oversized", "not-json", "status"} {
		t.Run(which, func(t *testing.T) {
			var origin string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				login := fixtureBrowserLogin(origin)
				w.Header().Set("Content-Type", "application/json")
				switch which {
				case "foreign":
					login.URL = "https://foreign.example/team#signin=mesh_login_" + browserFixtureCode
				case "http":
					login.URL = strings.Replace(login.URL, "https:", "http:", 1)
				case "userinfo":
					login.URL = strings.Replace(login.URL, "https://", "https://user@", 1)
				case "query":
					login.URL = strings.Replace(login.URL, "/team#", "/team?token="+browserFixtureToken+"#", 1)
				case "route":
					login.URL = strings.Replace(login.URL, "/team#", "/invite#", 1)
				case "encoded-path":
					login.URL = strings.Replace(login.URL, "/team#", "/%74eam#", 1)
				case "encoded-fragment":
					login.URL = strings.Replace(login.URL, "signin=", "signin%3D", 1)
				case "uppercase-code":
					login.URL = strings.Replace(login.URL, browserFixtureCode, strings.ToUpper(browserFixtureCode), 1)
				case "short-code":
					login.URL = strings.TrimSuffix(login.URL, "f")
				case "expired":
					login.ExpiresAt = time.Now().Unix()
				case "long-expiry":
					login.ExpiresAt = time.Now().Add(10 * time.Minute).Unix()
				case "bad-client":
					login.ClientID = 0
				case "bad-role":
					login.Role = browserFixtureToken
				case "control-user":
					login.User = "bad\n" + browserFixtureToken
				case "not-json":
					w.Header().Set("Content-Type", "text/html")
					w.Write([]byte(browserFixtureToken))
					return
				case "status":
					w.WriteHeader(401)
					w.Write([]byte(browserFixtureToken + browserFixtureCode))
					return
				}
				data, _ := json.Marshal(login)
				switch which {
				case "duplicate":
					data = []byte(`{"user":"shadow",` + string(data[1:]))
				case "unknown":
					data = []byte(`{"extra":"` + browserFixtureToken + `",` + string(data[1:]))
				case "missing":
					data = []byte(`{"url":"` + login.URL + `"}`)
				case "trailing":
					data = append(data, []byte(` {"token":"`+browserFixtureToken+`"}`)...)
				case "oversized":
					data = []byte(strings.Repeat(browserFixtureToken, 200))
				}
				w.Write(data)
			}))
			defer server.Close()
			origin = server.URL
			client := New(origin, browserFixtureToken)
			client.HTTP = server.Client()
			_, err := client.BrowserLogin(context.Background(), "vault")
			assertNoBrowserSecret(t, err)
		})
	}
}
func TestBrowserLoginLocalInputAndCancellationRefusal(t *testing.T) {
	for _, origin := range []string{"http://example.test", "https://example.test/path", "https://user@example.test", "https://example.test?x=1", "https://example.test/#wrong", "//example.test"} {
		client := New(origin, browserFixtureToken)
		_, err := client.BrowserLogin(context.Background(), "vault")
		assertNoBrowserSecret(t, err)
	}
	client := New("https://example.test", browserFixtureToken)
	_, err := client.BrowserLogin(context.Background(), "other")
	assertNoBrowserSecret(t, err)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
	}))
	defer server.Close()
	client = New(server.URL, browserFixtureToken)
	client.HTTP = server.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = client.BrowserLogin(ctx, "vault")
	assertNoBrowserSecret(t, err)
	if time.Since(started) > time.Second {
		t.Fatal("cancellation unbounded")
	}
}
func TestBrowserLoginCapabilityExpiryBoundary(t *testing.T) {
	now := time.Unix(1000000, 0)
	good := fixtureBrowserLogin("https://example.test")
	good.ExpiresAt = now.Unix() + 120
	if err := ValidateBrowserLogin("https://example.test", good, now); err != nil {
		t.Fatal(err)
	}
	good.ExpiresAt++
	if ValidateBrowserLogin("https://example.test", good, now) == nil {
		t.Fatal("accepted121seconds")
	}
}
func TestPrepareBrowserLoginRefusesUnsafeCredentialPairsBeforeHTTP(t *testing.T) {
	for _, which := range []string{"private-parent", "vault-writable", "credential-mode", "state-mode", "credential-symlink", "state-symlink", "private-symlink", "lock-symlink", "wrong-vault", "wrong-hub", "missing-vault", "missing-state", "corrupt-state", "insecure-hub", "empty-token"} {
		t.Run(which, func(t *testing.T) {
			dir := t.TempDir()
			writeBrowserFixture(t, dir, "https://fixture.example")
			switch which {
			case "private-parent":
				os.Chmod(filepath.Join(dir, ".mesh"), 0755)
			case "vault-writable":
				os.Chmod(dir, 0777)
			case "credential-mode":
				os.Chmod(credPath(dir), 0644)
			case "state-mode":
				os.Chmod(statePath(dir), 0644)
			case "credential-symlink", "state-symlink", "lock-symlink":
				path := credPath(dir)
				if which == "state-symlink" {
					path = statePath(dir)
				}
				if which == "lock-symlink" {
					path = filepath.Join(dir, ".mesh", "sync.lock")
				}
				os.Remove(path)
				target := filepath.Join(t.TempDir(), "synthetic")
				os.WriteFile(target, []byte(browserFixtureToken), 0600)
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "private-symlink":
				mesh := filepath.Join(dir, ".mesh")
				moved := filepath.Join(dir, "moved")
				if os.Rename(mesh, moved) != nil {
					t.Fatal("rename")
				}
				if os.Symlink(moved, mesh) != nil {
					t.Fatal("symlink")
				}
			case "wrong-vault":
				writeState(dir, syncState{VaultID: "another", HubURL: "https://fixture.example"})
			case "wrong-hub":
				writeState(dir, syncState{VaultID: "fixture-vault", HubURL: "https://foreign.example"})
			case "missing-vault":
				writeCredentials(dir, credentials{HubURL: "https://fixture.example", Token: browserFixtureToken})
			case "missing-state":
				os.Remove(statePath(dir))
			case "corrupt-state":
				os.WriteFile(statePath(dir), []byte(browserFixtureToken), 0600)
			case "insecure-hub":
				writeBrowserFixture(t, dir, "http://fixture.example")
			case "empty-token":
				writeCredentials(dir, credentials{HubURL: "https://fixture.example", VaultID: "fixture-vault"})
			}
			var calls int
			_, err := prepareBrowserLogin(context.Background(), dir, "vault", func(string, string) *Client { calls++; return nil })
			assertNoBrowserSecret(t, err)
			if calls != 0 {
				t.Fatal("unsafe pair reached HTTP factory")
			}
		})
	}
}
func TestPrepareBrowserLoginPreservesDurableJoinedPair(t *testing.T) {
	var origin string
	var posts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if r.URL.Path != "/v1/browser-login" {
			t.Error("unexpected join/sync")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(fixtureBrowserLogin(origin))
	}))
	defer server.Close()
	origin = server.URL
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	writeBrowserFixture(t, dir, origin)
	lockPath := filepath.Join(dir, ".mesh", "sync.lock")
	if err := os.WriteFile(lockPath, []byte("synthetic existing lock bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	creds, _ := os.ReadFile(credPath(dir))
	state, _ := os.ReadFile(statePath(dir))
	login, err := prepareBrowserLogin(context.Background(), dir, "vault", func(hub, token string) *Client {
		if hub != origin || token != browserFixtureToken {
			t.Error("wrong identity")
		}
		client := New(hub, token)
		client.HTTP = server.Client()
		return client
	})
	if err != nil {
		t.Fatal(err)
	}
	if login.ClientID != 7 || posts.Load() != 1 {
		t.Fatal("receipt")
	}
	afterCreds, _ := os.ReadFile(credPath(dir))
	afterState, _ := os.ReadFile(statePath(dir))
	if string(creds) != string(afterCreds) || string(state) != string(afterState) {
		t.Fatal("rewrote private pair")
	}
	ci, _ := os.Stat(credPath(dir))
	si, _ := os.Stat(statePath(dir))
	lockBytes, e := os.ReadFile(lockPath)
	if e != nil || string(lockBytes) != "synthetic existing lock bytes" {
		t.Fatal("existing lock bytes changed")
	}
	lockInfo, e := os.Stat(lockPath)
	if e != nil || lockInfo.Mode().Perm() != 0600 {
		t.Fatal("existing private lock mode changed")
	}
	if ci.Mode().Perm() != 0600 || si.Mode().Perm() != 0600 {
		t.Fatal("private modes changed")
	}
}

func TestBrowserLoginRoleAllowlistKeepsCuratorSeparate(t *testing.T) {
	now := time.Now()
	for _, role := range []string{"viewer", "member", "admin", "owner", "curator"} {
		login := fixtureBrowserLogin("https://example.test")
		login.Role = role
		if err := ValidateBrowserLogin("https://example.test", login, now); err != nil {
			t.Fatalf("supported role %s refused:%v", role, err)
		}
	}
	login := fixtureBrowserLogin("https://example.test")
	login.Role = "superadmin"
	if ValidateBrowserLogin("https://example.test", login, now) == nil {
		t.Fatal("unknown elevated role accepted")
	}
}
