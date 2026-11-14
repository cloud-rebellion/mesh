// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
// Run with node --test. Executes the real shell against a small DOM adapter;
// this covers action/state handling, not a browser or a live SSO journey.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const shell = readFileSync(new URL("../internal/web/assets/shell.js", import.meta.url), "utf8");
const html = readFileSync(new URL("../internal/web/assets/index.html", import.meta.url), "utf8");
const response = (status, data = {}) => ({
  status, ok: status >= 200 && status < 300,
  headers: { get: () => "application/json" },
  json: async () => data, text: async () => "",
});

async function boot({ authRequired = true, readOnly = false, logout = async () => response(204) } = {}) {
  const elements = new Map();
  for (const id of ["mesh-signout", "mesh-signout-status", "rail-status", "login", "login-account", "login-key", "panel"]) {
    const classes = new Set(["hidden"]);
    const events = new Map();
    elements.set(id, {
      hidden: true, disabled: false, textContent: "", innerHTML: "", events,
      classList: { contains: x => classes.has(x), add: x => classes.add(x), remove: x => classes.delete(x),
        toggle(x, on) { if (on) classes.add(x); else classes.delete(x); } },
      addEventListener: (name, action) => events.set(name, action), setAttribute() {}, focus() {},
    });
  }
  const requests = [], navigations = [];
  let status = response(200, { authRequired, counts: { notes: 2 } });
  const location = { hash: "#/graph", pathname: "/app/", search: "?sso=1", href: "https://mesh.example.test/app/?sso=1#/graph",
    replace: target => navigations.push(target) };
  const window = { Mesh: { readOnlyViewer: readOnly }, addEventListener() {} };
  vm.runInNewContext(shell, {
    window, location, URL, setTimeout() {},
    document: { baseURI: "https://mesh.example.test/app/", body: { classList: { add() {}, remove() {} } },
      getElementById: id => elements.get(id) || null, querySelectorAll: () => [] },
    fetch: async (path, options) => {
      requests.push({ path, options });
      if (path === "api/status") return status;
      if (path === "api/logout") return logout();
      throw new Error("Unexpected route");
    },
  });
  await new Promise(setImmediate);
  return { button: elements.get("mesh-signout"), message: elements.get("mesh-signout-status"),
    login: elements.get("login"), requests, navigations, location,
    click: () => elements.get("mesh-signout").events.get("click")(),
    refresh: async next => { status = next; await window.Mesh.refreshStatus(); } };
}

test("the actual rail contains the initially hidden sign-out control and accessible status", () => {
  assert.match(html, /id="mesh-signout"[^>]*aria-describedby="mesh-signout-status"[^>]*hidden/);
  assert.match(html, /id="mesh-signout-status"[^>]*role="status"[^>]*aria-live="polite"[^>]*hidden/);
});

test("sign out is shown only for an authenticated web surface", async () => {
  assert.equal((await boot()).button.hidden, false);
  for (const authRequired of [false, null, "true", 1]) {
    assert.equal((await boot({ authRequired })).button.hidden, true);
  }
  assert.equal((await boot({ readOnly: true })).button.hidden, true);
  const ui = await boot();
  await ui.refresh(response(401));
  assert.equal(ui.button.hidden, true);
  assert.equal(ui.login.classList.contains("hidden"), false);
});

test("confirmed logout posts once with cookies and replaces the document at the clean app entry", async () => {
  let complete;
  const ui = await boot({ logout: () => new Promise(resolve => { complete = resolve; }) });
  const pending = ui.click();
  assert.equal(ui.button.disabled, true);
  await ui.click();
  assert.equal(ui.requests.filter(x => x.path === "api/logout").length, 1);
  const request = ui.requests.find(x => x.path === "api/logout");
  assert.equal(request.options.method, "POST");
  assert.equal(request.options.credentials, "same-origin");
  assert.deepEqual(ui.navigations, []);
  complete(response(204));
  await pending;
  assert.deepEqual(ui.navigations, ["/app/"]);
  assert.equal(ui.message.hidden, true);
});

test("HTTP failures and unexpected acknowledgements retain the page and allow an honest retry", async () => {
  for (const status of [200, 401, 403, 500]) {
    let calls = 0;
    const ui = await boot({ logout: async () => response(++calls === 1 ? status : 204) });
    await ui.click();
    assert.deepEqual(ui.navigations, []);
    assert.equal(ui.button.disabled, false);
    assert.equal(ui.button.textContent, "Sign out");
    assert.equal(ui.message.hidden, false);
    assert.equal(ui.message.textContent, "Could not confirm sign out. Please try again.");
    await ui.click();
    assert.equal(calls, 2);
    assert.deepEqual(ui.navigations, ["/app/"]);
  }
});

test("a lost response does not claim that sign out succeeded or that a session is still active", async () => {
  const ui = await boot({ logout: async () => { throw new Error("network interruption"); } });
  await ui.click();
  assert.deepEqual(ui.navigations, []);
  assert.equal(ui.button.disabled, false);
  assert.equal(ui.message.textContent, "Could not confirm sign out. Please try again.");
});
