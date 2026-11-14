// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
// shell.js: the app-shell router. Owns the left-nav sections (Graph / Search /
// Settings / Docs / API), the auth-aware /api fetch helper, and the rail status.
// The Graph section is driven by app.js (the canvas engine); the other sections are
// rendered by per-view modules that register on window.Mesh.views (filled in by
// settings.js / search.js / docs.js / api.js as those phases land). Vanilla, no deps.
(function () {
  "use strict";
  const Mesh = (window.Mesh = window.Mesh || {});
  Mesh.views = Mesh.views || {};

  // api: fetch /api/* relying on the HttpOnly session cookie (set once via the login
  // card / magic link). No token is ever held in JS. On 401 it reveals the login
  // card so the user can re-authenticate.
  Mesh.api = async function (path, opts) {
    opts = opts || {};
    // Resolve relative to <base href> so the app works under a path (e.g. /app/).
    // Callers pass "/api/...": strip the leading slash so it is relative.
    path = path.replace(/^\//, "");
    const res = await fetch(path, Object.assign({ credentials: "same-origin" }, opts));
    if (res.status === 401) {
      showLogin();
      throw new Error(path + ": 401");
    }
    if (!res.ok) throw new Error(path + ": " + res.status);
    const ct = res.headers.get("content-type") || "";
    return ct.includes("application/json") ? res.json() : res.text();
  };

  // --- Note reader: a shared in-app markdown viewer (no editor needed) ----------
  // Any view calls Mesh.openNote(id); it fetches the note and shows server-rendered
  // HTML in the slide-in drawer. Used by graph clicks, the clusters explorer, etc.
  const noteDrawer = document.getElementById("note-drawer");
  Mesh.openNote = async function (id) {
    if (!noteDrawer || !id) return;
    const body = document.getElementById("nd-body"), pathEl = document.getElementById("nd-path");
    noteDrawer.classList.remove("hidden");
    noteDrawer.setAttribute("aria-hidden", "false");
    pathEl.textContent = "";
    body.innerHTML = '<p class="srch-hint">Loading...</p>';
    try {
      const n = await Mesh.api("/api/note/" + encodeURIComponent(id));
      pathEl.textContent = n.path || id;
      body.innerHTML = n.html || ("<pre>" + (n.markdown || "").replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c])) + "</pre>");
      body.scrollTop = 0;
    } catch (e) {
      body.innerHTML = '<p class="srch-hint">Could not load note: ' + e.message + "</p>";
    }
  };
  Mesh.closeNote = function () {
    if (!noteDrawer) return;
    noteDrawer.classList.add("hidden");
    noteDrawer.setAttribute("aria-hidden", "true");
    if (typeof Mesh.onNoteClose === "function") Mesh.onNoteClose();
  };
  if (noteDrawer) {
    const x = document.getElementById("nd-close");
    if (x) x.addEventListener("click", Mesh.closeNote);
    window.addEventListener("keydown", (e) => { if (e.key === "Escape" && !noteDrawer.classList.contains("hidden")) Mesh.closeNote(); });
  }

  // --- Login (HttpOnly cookie session) ----------------------------------------
  const signoutButton = document.getElementById("mesh-signout");
  const signoutStatus = document.getElementById("mesh-signout-status");
  let signingOut = false;

  function renderSignout(status) {
    if (signoutButton) signoutButton.hidden = Mesh.readOnlyViewer || !status || status.authRequired !== true;
    if (signoutStatus && (!signoutButton || signoutButton.hidden)) signoutStatus.hidden = true;
  }

  if (signoutButton) signoutButton.addEventListener("click", async () => {
    if (signingOut) return;
    signingOut = true;
    signoutButton.disabled = true;
    signoutButton.textContent = "Signing out...";
    if (signoutStatus) signoutStatus.hidden = true;
    try {
      const response = await fetch("api/logout", { method: "POST", credentials: "same-origin" });
      if (response.status !== 204) throw new Error("Sign out was not confirmed");
      // A fresh document drops the previous account's in-memory notes and any
      // launcher query/fragment. The clean app entry shows normal sign-in.
      location.replace(new URL(document.baseURI).pathname);
    } catch (_) {
      signoutButton.disabled = false;
      signoutButton.textContent = "Sign out";
      if (signoutStatus) {
        signoutStatus.textContent = "Could not confirm sign out. Please try again.";
        signoutStatus.hidden = false;
      }
    } finally {
      signingOut = false;
    }
  });

  // POST the access key; the server validates it constant-time and sets the cookie.
  async function submitKey(key) {
    const res = await fetch("api/login", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ key: key }),
    });
    return res.ok;
  }

  function showLogin() {
    renderSignout(null);
    const el = document.getElementById("login");
    if (!el || !el.classList.contains("hidden")) return;
    el.classList.remove("hidden");
    el.setAttribute("aria-hidden", "false");
    const account = document.getElementById("login-account");
    const inp = account && !account.hidden ? document.getElementById("login-account-link") : document.getElementById("login-key");
    if (inp) setTimeout(() => inp.focus(), 60);
  }
  Mesh.showLogin = showLogin;

  function wireLogin() {
    const form = document.getElementById("login-form");
    if (!form) return;
    const link = document.getElementById("login-account-link");
    if (link && link.dataset.signIn) {
      const target = new URL(link.dataset.signIn, location.href);
      if (target.origin === location.origin && target.pathname === "/auth/oidc/login") {
        link.href = target.href;
        document.getElementById("login-account").hidden = false;
        document.getElementById("login-key-option").open = false;
        document.getElementById("login-sub").textContent = "Sign in with your team's account to continue.";
      }
    }
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const inp = document.getElementById("login-key");
      const err = document.getElementById("login-err");
      const btn = document.getElementById("login-btn");
      const key = ((inp && inp.value) || "").trim();
      if (!key) return;
      if (err) err.hidden = true;
      if (btn) { btn.disabled = true; btn.textContent = "Checking..."; }
      let ok = false;
      try { ok = await submitKey(key); } catch (_) { ok = false; }
      if (ok) {
        // Cookie is set: reload so every view loads authenticated, cleanly.
        location.reload();
        return;
      }
      if (btn) { btn.disabled = false; btn.textContent = "Unlock"; }
      if (err) { err.hidden = false; err.textContent = "That key was not accepted. Check it and try again."; }
      if (inp) inp.select();
    });
  }

  // A magic link (mesh.../app/#k=<key>) signs you in with one click. The key lives in
  // the URL fragment (never sent to the server, so it is not in access logs) and is
  // stripped from the address bar immediately, before the network call.
  async function consumeMagicLink() {
    const m = (location.hash || "").match(/[#&]k=([^&]+)/);
    if (!m) return false;
    const key = decodeURIComponent(m[1]);
    history.replaceState(null, "", location.pathname + location.search);
    let ok = false;
    try { ok = await submitKey(key); } catch (_) { ok = false; }
    if (ok) { location.reload(); return true; }
    showLogin();
    return false;
  }

  const panel = document.getElementById("panel");
  const overlay = document.getElementById("overlay");
  const navs = Array.from(document.querySelectorAll(".rail-nav .nav"));
  const panelViews = Array.from(document.querySelectorAll(".panel-view"));

  function panelEl(view) {
    return panelViews.find((p) => p.dataset.panel === view);
  }

  // Proper display labels (so a fallback never mangles an acronym like "API" -> "Api").
  const LABELS = { graph: "Graph", search: "Search", settings: "Settings", docs: "Docs", api: "API" };
  function labelFor(view) { return LABELS[view] || (view.charAt(0).toUpperCase() + view.slice(1)); }

  // placeholder for a view whose module did not load (e.g. a stale cache). It tells
  // the user how to recover rather than implying the feature is unfinished.
  function placeholder(el, view) {
    const title = labelFor(view);
    el.innerHTML = "";
    const inner = document.createElement("div");
    inner.className = "panel-inner";
    inner.innerHTML =
      '<p class="panel-h">' + title + '</p>' +
      '<h1 class="panel-title">' + title + '</h1>' +
      '<div class="panel-soon">This view could not load its script (often a stale cache). ' +
      'Try a hard refresh (Cmd-Shift-R).</div>';
    el.appendChild(inner);
  }

  let current = "graph";
  function route(view) {
    if (!view) view = "graph";
    current = view;
    navs.forEach((b) => {
      const on = b.dataset.view === view;
      b.classList.toggle("active", on);
      if (on) b.setAttribute("aria-current", "page");
      else b.removeAttribute("aria-current");
    });
    if (view === "graph") {
      document.body.classList.remove("panel-active");
      panel.hidden = true;
      panelViews.forEach((p) => p.classList.remove("active"));
      return;
    }
    // a feature section: cover the canvas with its panel.
    document.body.classList.add("panel-active");
    if (overlay) overlay.classList.add("hidden"); // the graph overlay is irrelevant here
    panel.hidden = false;
    panelViews.forEach((p) => p.classList.toggle("active", p.dataset.panel === view));
    const el = panelEl(view);
    if (!el) return;
    const render = Mesh.views[view];
    if (typeof render === "function") {
      try { render(el, Mesh); } catch (e) { el.innerHTML = '<div class="panel-inner"><div class="panel-soon">Failed to render: ' + e.message + "</div></div>"; }
    } else {
      placeholder(el, view);
    }
  }
  Mesh.route = route;

  navs.forEach((b) =>
    b.addEventListener("click", () => {
      const v = b.dataset.view;
      location.hash = v === "graph" ? "" : "#/" + v;
      route(v);
    })
  );
  // "search ->" beside the graph filter jumps to the full-text Search view.
  const toSearch = document.getElementById("to-search");
  if (toSearch) toSearch.addEventListener("click", () => { location.hash = "#/search"; route("search"); });

  window.addEventListener("hashchange", () => route(hashView()));
  function hashView() {
    const m = (location.hash || "").match(/^#\/(\w+)/);
    return m ? m[1] : "graph";
  }

  // Release strings are data, not commands or URLs. Build the only external
  // update link from a validated semver and our fixed official source origin.
  function releaseVersion(value) {
    if (typeof value !== "string" || value.length > 128) return "";
    const match = value.match(/^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/);
    if (!match || (match[4] && match[4].split(".").some(v => /^\d+$/.test(v) && /^0\d/.test(v)))) return "";
    return value;
  }

  // Inputs have already passed releaseVersion. Compare arbitrary-size numeric
  // identifiers without rounding, and ignore build metadata as semver requires.
  function compareRelease(a, b) {
    const parts = v => v.slice(1).split("+", 1)[0].match(/^([0-9.]+)(?:-(.*))?$/);
    const aa = parts(a), bb = parts(b);
    const ac = aa[1].split("."), bc = bb[1].split(".");
    for (let i = 0; i < 3; i++) {
      if (BigInt(ac[i]) !== BigInt(bc[i])) return BigInt(ac[i]) > BigInt(bc[i]) ? 1 : -1;
    }
    if (!aa[2] || !bb[2]) return aa[2] ? -1 : bb[2] ? 1 : 0;
    const ap = aa[2].split("."), bp = bb[2].split(".");
    for (let i = 0; i < Math.max(ap.length, bp.length); i++) {
      if (ap[i] === bp[i]) continue;
      if (ap[i] === undefined) return -1;
      if (bp[i] === undefined) return 1;
      const an = /^\d+$/.test(ap[i]), bn = /^\d+$/.test(bp[i]);
      if (an !== bn) return an ? -1 : 1;
      return (an ? BigInt(ap[i]) > BigInt(bp[i]) : ap[i] > bp[i]) ? 1 : -1;
    }
    return 0;
  }

  const updateDialog = document.getElementById("mesh-update-dialog");
  const updateRefresh = document.getElementById("mesh-update-refresh");
  let updateOpener;
  function openUpdates(event) {
    if (!updateDialog || updateDialog.open) return;
    updateOpener = event.currentTarget;
    updateDialog.showModal();
  }
  for (const id of ["mesh-update-open", "update-details"]) {
    const button = document.getElementById(id);
    if (button) button.addEventListener("click", openUpdates);
  }
  if (updateDialog) {
    document.getElementById("mesh-update-close").addEventListener("click", () => updateDialog.close());
    updateDialog.addEventListener("close", () => { if (updateOpener) updateOpener.focus(); });
  }
  if (updateRefresh) updateRefresh.addEventListener("click", loadStatus);

  // The top-level release identifies the actual server independently of update
  // discovery. Fall back to the notice only for old servers without that field.
  function renderUpdate(status) {
    // The IDE owns its coordinated native updater. Its bundled browser controls
    // are hidden, so do not reserve banner space in that read-only surface.
    if (Mesh.readOnlyViewer) {
      const banner = document.getElementById("update-banner");
      if (banner) banner.hidden = true;
      document.body.classList.remove("update-available");
      return;
    }
    const u = status && status.update;
    const current = releaseVersion(status && Object.prototype.hasOwnProperty.call(status, "release") ? status.release : u && u.current);
    const latest = releaseVersion(u && u.latest);
    const checked = Boolean(current && latest && releaseVersion(u && u.current) === current && typeof u.available === "boolean" && u.available === (compareRelease(latest, current) > 0));
    const available = checked && u.available === true;
    if (updateDialog) {
      document.getElementById("mesh-update-current").textContent = current || "Unknown";
      document.getElementById("mesh-update-latest").textContent = latest || "Unknown";
      document.getElementById("mesh-update-summary").textContent = available
        ? "A newer server release is available. Operator approval is required."
        : checked ? "No newer server release is reported by the last check."
          : "Release status unknown. Checks may be disabled, unavailable, or unsupported by this server.";
      const link = document.getElementById("mesh-update-release");
      link.hidden = !latest;
      if (latest) link.href = "https://github.com/bright-interaction/mesh/tree/" + encodeURIComponent(latest);
      else link.removeAttribute("href");
    }
    const banner = document.getElementById("update-banner");
    if (!banner) return;
    banner.hidden = true;
    document.body.classList.remove("update-available");
    if (!available) return;
    let dismissed = "";
    try { dismissed = sessionStorage.getItem("mesh-update-dismissed") || ""; } catch (_) {}
    if (dismissed === latest) return;
    const copy = document.getElementById("update-copy");
    if (copy) copy.textContent = "Mesh " + latest + " is available (server " + current + ").";
    banner.hidden = false;
    document.body.classList.add("update-available");
  }

  const updateDismiss = document.getElementById("update-dismiss");
  if (updateDismiss) updateDismiss.addEventListener("click", () => {
    const banner = document.getElementById("update-banner");
    const latest = Mesh.status && Mesh.status.update && Mesh.status.update.latest;
    if (latest) { try { sessionStorage.setItem("mesh-update-dismissed", latest); } catch (_) {} }
    if (banner) banner.hidden = true;
    document.body.classList.remove("update-available");
  });

  async function loadStatus() {
    const foot = document.getElementById("rail-status");
    if (!foot) return;
    if (updateRefresh && updateRefresh.disabled) return;
    if (updateRefresh) { updateRefresh.disabled = true; updateRefresh.textContent = "Checking..."; }
    try {
      const s = await Mesh.api("/api/status");
      Mesh.status = s;
      renderSignout(s);
      renderUpdate(s);
      const c = s.counts || {};
      const sig = s.signals || {};
      const dot = (k) => '<span class="sig ' + (sig[k] ? "on" : "") + '" title="' + k + (sig[k] ? " on" : " off") + '"></span>';
      foot.innerHTML =
        (c.notes || 0) + " notes &middot; " + (c.edges || 0) + " links<br>" +
        dot("fts") + dot("graph") + dot("vector") + dot("rerank") + dot("ann");
      // Surface the empty-state when the vault has no real notes yet (a fresh hub
      // carries a single seed index.md).
      const empty = document.getElementById("empty");
      if (empty) empty.classList.toggle("hidden", (c.notes || 0) > 1);
    } catch (e) {
      foot.textContent = "";
      Mesh.status = null;
      renderSignout(null);
      renderUpdate(null);
      const login = document.getElementById("login");
      if (updateDialog && updateDialog.open && login && !login.classList.contains("hidden")) updateDialog.close();
    } finally {
      if (updateRefresh) { updateRefresh.disabled = false; updateRefresh.textContent = "Refresh status"; }
    }
  }
  Mesh.refreshStatus = loadStatus;

  const emptyDocs = document.getElementById("empty-docs");
  if (emptyDocs) emptyDocs.addEventListener("click", () => { location.hash = "#/docs"; route("docs"); });

  wireLogin();
  (async function init() {
    if (await consumeMagicLink()) return; // a successful magic link reloads the page
    route(hashView());
    loadStatus();
  })();
})();
