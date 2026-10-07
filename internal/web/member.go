// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// memberAuth backs the web app with the hub's per-member client tokens, used when the
// app is served for a team via `mesh ui --hub-db`. Each member signs in with their own
// client token; the browser then carries a client-bound HMAC cookie. This replaces the
// single shared MESH_UI_TOKEN so the graph/search/note surfaces can be scoped to the
// signed-in member (via the Server's scopeResolver).
type memberAuth struct {
	browser *BrowserSignIn
	// verify resolves a raw client token to its client id (ok=false for an unknown token).
	verify func(token string) (clientID int64, user string, ok bool)
	// scopesFor returns the member's readable scopes; nil = unrestricted (no scoping configured).
	scopesFor func(clientID int64) map[string]bool
	// pathsFor returns the member's per-path folder-ACL read predicate; nil = unrestricted
	// (no folder ACL configured). Folder ACLs and scopes are independent partitions, so
	// this is not derivable from scopesFor.
	pathsFor func(clientID int64) func(path string) bool
	// roleFor returns the member's role, their immutable created_at, and whether the
	// client still exists. ok=false means the client was removed/revoked, so a
	// still-valid cookie must stop working immediately instead of at cookie expiry.
	// createdAt is signed into the cookie so a REUSED client rowid cannot revive a
	// revoked member's cookie as whoever now holds that id.
	roleFor func(clientID int64) (role string, createdAt int64, ok bool)
	key     []byte // per-process HMAC key for the client-bound cookie
}

const memberCookie = "mesh_member"

func newMemberAuth(verify func(string) (int64, string, bool), scopesFor func(int64) map[string]bool, pathsFor func(int64) func(string) bool, roleFor func(int64) (string, int64, bool)) *memberAuth {
	return &memberAuth{verify: verify, scopesFor: scopesFor, pathsFor: pathsFor, roleFor: roleFor, key: stableMemberKey()}
}

// Hub role order: owner > admin > member/curator > viewer. Curation is an
// explicit pro capability and does not give curator browser administration.
// Duplicated minimally here because
// internal/web is the open core and must not import the pro internal/hub package; the
// pro side hands us the role string via roleFor and we only need to rank it.
func roleRank(role string) int {
	switch strings.TrimSpace(role) {
	case "owner":
		return 4
	case "admin":
		return 3
	case "member", "curator":
		return 2
	case "viewer":
		return 1
	}
	return 0
}

// stableMemberKey derives the cookie-signing key from a STABLE server secret so a
// signed-in member survives a server restart/redeploy instead of being logged out
// every time (the 30-day cookie then actually lasts 30 days). It is HMAC-derived and
// domain-separated, so the secret itself is never exposed and a member cookie cannot be
// forged without it. Priority: MESH_UI_COOKIE_SECRET, then MESH_UI_TOKEN (set on a
// hosted hub). With neither, it falls back to a random per-process key (sessions then
// end on restart) and logs how to make them persist.
func stableMemberKey() []byte {
	for _, env := range []string{"MESH_UI_COOKIE_SECRET", "MESH_UI_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			mac := hmac.New(sha256.New, []byte(v))
			mac.Write([]byte("mesh-ui-member-cookie-v1"))
			return mac.Sum(nil)
		}
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	slog.Warn("mesh ui: member cookies use an ephemeral signing key, so members re-login after every restart; set MESH_UI_COOKIE_SECRET (or MESH_UI_TOKEN) to keep them signed in")
	return key
}

// memberCookieTTL is how long an issued member cookie stays valid SERVER side. It
// matches the browser-side MaxAge, but unlike MaxAge it is signed into the value, so a
// copied cookie value stops working on its own schedule instead of living forever.
const memberCookieTTL = 30 * 24 * time.Hour

// sign returns the client-bound cookie value "<id>.<exp>.<hmac>", signed with the
// stable key so it stays valid across restarts.
//
// Two things beyond the rowid are INSIDE the signed payload:
//
//   - the expiry. The v1 format signed the client rowid alone, which made the cookie a
//     permanent bearer credential: nothing but the browser's own MaxAge (which the
//     holder controls) ever retired it.
//   - the client's immutable created_at. sqlite hands the deleted maximum rowid back to
//     the next INSERT on a table declared without AUTOINCREMENT, so a revoked member's
//     id can be reissued to a brand-new member. Binding created_at means the old
//     cookie's HMAC no longer verifies against the new occupant of that id.
//
// The version string is bumped to v3 so every cookie minted under v1/v2 stops verifying
// at once.
func (m *memberAuth) sign(id, createdAt int64) string {
	return m.signAt(id, createdAt, time.Now().Add(memberCookieTTL).Unix())
}

// signAt is sign with an explicit expiry, split out so the expiry check is testable
// without waiting or faking the clock globally.
func (m *memberAuth) signAt(id, createdAt, exp int64) string {
	ids, exps := strconv.FormatInt(id, 10), strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte("mesh-member-v3:" + ids + ":" + strconv.FormatInt(createdAt, 10) + ":" + exps))
	return ids + "." + exps + "." + hex.EncodeToString(mac.Sum(nil))
}

// cookieClaims parses the UNVERIFIED id and expiry out of a cookie value. The caller
// must still run clientFromCookie to authenticate it; this only exists so the caller can
// look up the createdAt that the signature is bound to.
func cookieClaims(v string) (id, exp int64, ok bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	exp, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return id, exp, true
}

// clientFromCookie validates a cookie value (constant-time) against the client's current
// createdAt and returns its client id. A well-signed but expired value is refused, and so
// is a value signed for a DIFFERENT createdAt on the same rowid (the reuse case).
func (m *memberAuth) clientFromCookie(v string, createdAt int64) (int64, bool) {
	id, exp, ok := cookieClaims(v)
	if !ok {
		return 0, false
	}
	if subtle.ConstantTimeCompare([]byte(v), []byte(m.signAt(id, createdAt, exp))) != 1 {
		return 0, false
	}
	// Signature checked first so an expired-vs-forged cookie is indistinguishable in
	// timing; only then does the expiry decide.
	if time.Now().Unix() >= exp {
		return 0, false
	}
	return id, true
}

// member resolves the requesting member from the cookie, or a bearer client token,
// and returns their id + role. It re-checks that the client still exists (roleFor) on
// every request, so a removed member's still-valid cookie stops working immediately
// instead of lingering until the 30-day cookie expires. ok=false when unauthenticated
// or when the client has been revoked.
func (m *memberAuth) member(r *http.Request) (id int64, role string, ok bool) {
	if identity, valid := r.Context().Value(connectionContextKey{}).(connectionIdentity); valid {
		role, created, exists := m.roleFor(identity.id)
		return identity.id, role, exists && !identity.shared && created == identity.created && roleRank(role) > 0
	}
	// A fresh SSO identity must not be shadowed by an expired legacy viewer cookie.
	if m.browser != nil {
		if id, _, valid := m.browser.Resolve(r); valid && id > 0 {
			if role, _, exists := m.roleFor(id); exists && roleRank(role) > 0 {
				return id, role, true
			}
		}
	}
	if c, err := r.Cookie(memberCookie); err == nil && c.Value != "" {
		// The claimed id is read first (unverified) purely to look up the createdAt the
		// signature is bound to. Nothing is trusted until clientFromCookie verifies the
		// HMAC over id+createdAt+exp, so a forged id resolves either to a non-existent
		// client (refused here) or to a createdAt the forger cannot sign against.
		if cid, _, parsed := cookieClaims(c.Value); parsed {
			role, createdAt, exists := m.roleFor(cid)
			if !exists {
				return 0, "", false // client was revoked; the cookie dies with it
			}
			if _, valid := m.clientFromCookie(c.Value, createdAt); valid {
				return cid, role, true
			}
			return 0, "", false // bad signature, expired, or a reused rowid
		}
	}
	// Bearer header only; the ?token= query fallback was removed so a member's client
	// token never lands in access/proxy logs, history, or a Referer header.
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	if tok != "" {
		if cid, _, valid := m.verify(tok); valid {
			if role, _, exists := m.roleFor(cid); exists {
				return cid, role, true
			}
		}
	}
	return 0, "", false
}

// clientFromRequest resolves the requesting member id (existence-checked). ok=false
// when unauthenticated or revoked.
func (m *memberAuth) clientFromRequest(r *http.Request) (int64, bool) {
	id, _, ok := m.member(r)
	return id, ok
}

// SetMemberAuth puts the web app in per-member mode: requests authenticate as a hub
// client and the graph/search/note surfaces are scoped to that member. verify checks a
// client token; scopesFor returns a member's readable scopes (nil = unrestricted);
// pathsFor returns their folder-ACL read predicate (nil = unrestricted); roleFor returns
// the member's role, their immutable created_at (bound into the session cookie so a
// reused client rowid cannot revive a revoked member's cookie), and whether the client
// still exists.
func (s *Server) SetMemberAuth(verify func(token string) (int64, string, bool), scopesFor func(int64) map[string]bool, pathsFor func(int64) func(string) bool, roleFor func(int64) (string, int64, bool)) {
	s.member = newMemberAuth(verify, scopesFor, pathsFor, roleFor)
	s.scopeResolver = func(r *http.Request) map[string]bool {
		id, ok := s.member.clientFromRequest(r)
		if !ok {
			return map[string]bool{} // deny-all for an unresolved request (the guard blocks these anyway)
		}
		return s.member.scopesFor(id) // nil here = unrestricted (scoping not configured)
	}
	s.pathResolver = func(r *http.Request) func(string) bool {
		if s.member.pathsFor == nil {
			return nil // no folder ACLs wired (standalone-shaped host)
		}
		id, ok := s.member.clientFromRequest(r)
		if !ok {
			return func(string) bool { return false } // deny-all, same as the scope resolver
		}
		return s.member.pathsFor(id) // nil here = unrestricted (no folder ACL configured)
	}
}

// requireAdmin gates a state-changing handler on an admin-or-higher role. In standalone
// mode (no member auth) the single trusted token already gates every route, so it is a
// no-op. In member mode a viewer/member is refused: config edits, reindex, and the
// pending review-queue promote/discard are admin actions, mirroring the hub's own
// admin-gated Integrations config. Returns false (and writes 403) when refused.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.member == nil {
		return true // standalone single-token viewer: the token is the gate
	}
	_, role, ok := s.member.member(r)
	if !ok || roleRank(role) < roleRank("admin") {
		http.Error(w, "forbidden: an admin role is required for this action", http.StatusForbidden)
		return false
	}
	return true
}

// memberGuard gates every non-open path on a resolved member, replacing the single
// shared-token guard when member mode is active.
func (s *Server) memberGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOpenPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if _, _, ok := s.browserAccount(r); ok {
			if r.Host != s.connections.host || (r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != s.connections.origin) {
				http.Error(w, "browser origin not allowed", http.StatusForbidden)
				return
			}
		}
		if _, ok := s.member.clientFromRequest(r); !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
