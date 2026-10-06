// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
// Disposable rendered-browser acceptance. All HTTP is intercepted; no live vault,
// credentials, model, or hosted deployment is used. Run with bun or node and an
// installed Playwright module, e.g. MESH_BROWSER_MODULE=/path/to/playwright/index.mjs.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const { chromium } = await import(process.env.MESH_BROWSER_MODULE || "playwright");
const script = readFileSync(new URL("../internal/web/assets/search.js", import.meta.url), "utf8");
const css = readFileSync(new URL("../internal/web/assets/style.css", import.meta.url), "utf8");
const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 1100, height: 900 } });
  const errors = [];
  page.on("pageerror", e => errors.push(e.message));
  let cards = [
    { NoteID: "old", Title: "Historical procedure", Path: "notes/old.md", Tier0: true,
      SupersededBy: "replacement", MissingGuidance: ["verification", "limitations"], Score: 0.9 },
    { NoteID: "ordinary", Title: "Ordinary note", Path: "notes/ordinary.md", Score: 0.8 },
  ];
  let revoked = false;
  let pendingNote;
  let holdNote = false;
  const calls = [];
  await page.route("**/*", async route => {
    const url = new URL(route.request().url());
    assert.equal(url.origin, "https://mesh-fixture.invalid", "no external requests");
    if (url.pathname === "/") {
      return route.fulfill({ contentType: "text/html", body: '<!doctype html><html lang="en"><head><meta name="viewport" content="width=device-width, initial-scale=1"></head><body><main id="root"></main></body></html>' });
    }
    if (["/assets/fonts/jetbrains-mono.woff2", "/assets/fonts/geist.woff2"].includes(url.pathname)) {
      return route.fulfill({ contentType: "font/woff2", body: readFileSync(new URL("../internal/web" + url.pathname, import.meta.url)) });
    }
    calls.push(url.pathname + url.search);
    if (url.pathname === "/api/search") return route.fulfill({ json: { cards, tokens: 140 } });
    if (url.pathname.startsWith("/api/note/")) {
      const id = decodeURIComponent(url.pathname.slice("/api/note/".length));
      if (holdNote) {
        holdNote = false;
        await new Promise(resolve => { pendingNote = resolve; });
      }
      if (revoked && id === "replacement") return route.fulfill({ status: 404, body: "unknown note id" });
      return route.fulfill({ json: { path: "notes/" + id + ".md", html: "<p>Fixture note body</p>" } });
    }
    throw new Error("Unexpected request: " + url.pathname);
  });
  await page.goto("https://mesh-fixture.invalid/");
  await page.addStyleTag({ content: css });
  await page.addScriptTag({ content: script });
  const mount = async searchOnSubmit => page.evaluate(searchOnSubmit => {
    window.Mesh.views.search(document.getElementById("root"), {
      searchOnSubmit,
      api: async path => {
        const response = await fetch(path);
        if (!response.ok) throw new Error(await response.text());
        return response.json();
      },
    });
  }, searchOnSubmit);
  await mount(true);
  const run = async () => {
    await page.locator("#srch-q").fill("procedure");
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await page.locator(".rcard").first().waitFor();
  };
  await run();
  assert.deepEqual(await page.locator(".rcard").evaluateAll(nodes => nodes.map(n => n.dataset.id)), ["old", "ordinary"], "preserve rank and historical cards");
  assert.match(await page.locator('[data-id="old"].rcard').innerText(), /Superseded/);
  assert.match(await page.locator('[data-id="old"].rcard').innerText(), /Incomplete authored content: missing verification, limitations/);
  assert.doesNotMatch(await page.locator('[data-id="ordinary"].rcard').innerText(), /Superseded/);
  assert.equal(await page.locator(".rcard button, .rcard a").count(), 0, "no nested interactive elements");
  assert.equal(await page.getByRole("button", { name: "Read replacement", exact: true }).count(), 1);
  assert.equal(calls.length, 1, "no background replacement fetch");

  await page.locator('[data-id="old"].rcard').click();
  await page.locator(".note-body").waitFor();
  assert.match(await page.locator(".note-pane").innerText(), /Superseded/);
  assert.match(await page.locator(".note-pane").innerText(), /Incomplete authored content/);
  assert.match(await page.locator(".note-pane").innerText(), /status is from the last search/);
  assert.equal(calls.at(-1), "/api/note/old", "historical body remains accessible");
  await page.getByRole("button", { name: "Read replacement", exact: true }).focus();
  await page.keyboard.press("Enter");
  await page.waitForFunction(() => document.querySelector(".note-path")?.textContent === "notes/replacement.md");
  assert.doesNotMatch(await page.locator(".note-pane").innerText(), /Superseded|Incomplete authored content/, "do not transfer the old note's warnings to another note");
  assert.match(await page.locator(".note-pane").innerText(), /Guidance status not checked/);
  assert.equal(calls.at(-1), "/api/note/replacement");

  // If the replacement is itself superseded, use its own result metadata, not
  // the historical card's. Refresh is explicit rather than an automatic query.
  cards.push({ NoteID: "replacement", Title: "Replacement procedure", SupersededBy: "latest", MissingGuidance: ["evidence"] });
  await page.getByRole("button", { name: "Search this note", exact: true }).click();
  await page.locator(".rcard").first().waitFor();
  assert.equal(calls.at(-1), "/api/search?q=replacement&limit=15");
  await page.locator('.rc-replacement[data-id="replacement"]').click();
  await page.locator(".note-body").waitFor();
  assert.match(await page.locator(".note-pane").innerText(), /Incomplete authored content: missing evidence;/);
  assert.doesNotMatch(await page.locator(".note-pane").innerText(), /missing verification, limitations/);
  assert.equal(await page.locator(".note-pane .rc-replacement").getAttribute("data-id"), "latest");
  cards.pop();

  await page.locator("#note-back").click();
  await page.locator(".rcard").first().waitFor();
  revoked = true;
  await page.getByRole("button", { name: "Read replacement", exact: true }).click();
  await page.getByText("Replacement unavailable.", { exact: false }).waitFor();
  assert.equal(await page.locator(".note-body").count(), 0, "no stale body after denied fetch");
  await page.locator("#note-back").click();
  await page.locator(".rcard").first().waitFor();

  // Hidden/missing pointers arrive without SupersededBy. The UI must not infer
  // their existence from prose or invent a replacement request.
  cards = [{ NoteID: "ordinary", Title: "SupersededBy: classified is just prose", MissingGuidance: ["limitations"] }];
  await run();
  assert.equal(await page.locator(".rc-replacement, .rc-superseded").count(), 0);

  // Treat an ID as data, never an href, markup, selector, or executable handler.
  const hostileID = 'javascript:alert(1)/<img src=x onerror="boom">&?#';
  cards = [{ NoteID: "old", Title: "<img src=x onerror=boom>", SupersededBy: hostileID, MissingGuidance: ["<script>boom</script>"] }];
  await run();
  assert.equal(await page.locator("#srch-results img, #srch-results script").count(), 0);
  assert.equal(await page.locator(".rc-replacement").getAttribute("data-id"), hostileID);
  await page.locator(".rc-replacement").click();
  await page.locator(".note-body").waitFor();
  assert.equal(calls.at(-1), "/api/note/" + encodeURIComponent(hostileID));

  // An older note request must not overwrite a newly submitted result list.
  cards = [{ NoteID: "old", Title: "Historical procedure", SupersededBy: "replacement" }];
  await run();
  holdNote = true;
  await page.locator(".rcard").click();
  await page.waitForFunction(() => document.querySelector("#srch-results")?.textContent.includes("Loading"));
  for (let attempt = 0; !pendingNote && attempt < 200; attempt++) await new Promise(resolve => setTimeout(resolve, 5));
  assert.ok(pendingNote, "held note request arrived within one second");
  await run();
  const response = page.waitForResponse(r => r.url().endsWith("/api/note/old"));
  pendingNote();
  await (await response).finished();
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.locator(".rcard").count(), 1);
  assert.equal(await page.locator(".note-body").count(), 0);

  // Long unbroken titles/pointers still leave both actions usable on mobile.
  cards = [{ NoteID: "old", Title: "Historical".repeat(30), Path: "notes/" + "long".repeat(80), Tier0: true, Score: 0.92, SupersededBy: "replacement" }];
  await page.setViewportSize({ width: 390, height: 844 });
  await run();
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  assert.equal(await page.locator(".rc-replacement").isVisible(), true);
  // Repeat through ordinary web's debounced input path, not just IDE submit.
  cards = [{ NoteID: "old", Title: "Historical procedure", SupersededBy: "replacement" }];
  await mount(false);
  await page.locator("#srch-q").fill("debounced");
  await page.locator(".rcard").waitFor();
  assert.equal(calls.at(-1), "/api/search?q=debounced&limit=15");
  assert.equal(await page.locator(".rc-superseded").count(), 1);

  // A late failed replacement request must not overwrite the empty-query view.
  pendingNote = undefined;
  holdNote = true;
  await page.locator(".rc-replacement").click();
  for (let attempt = 0; !pendingNote && attempt < 200; attempt++) await new Promise(resolve => setTimeout(resolve, 5));
  assert.ok(pendingNote);
  await page.locator("#srch-q").fill("");
  await page.getByText("Type to search.", { exact: true }).waitFor();
  const failedResponse = page.waitForResponse(r => r.url().endsWith("/api/note/replacement"));
  pendingNote();
  assert.equal((await failedResponse).status(), 404);
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.getByText("Type to search.", { exact: true }).count(), 1);
  assert.equal(await page.locator("#note-back").count(), 0);
  await page.locator("#srch-q").fill("procedure");
  await page.locator(".rcard").waitFor();
  assert.deepEqual(errors, []);
  if (process.env.MESH_BROWSER_SCREENSHOT) await page.screenshot({ path: process.env.MESH_BROWSER_SCREENSHOT, fullPage: true });
  console.log(JSON.stringify({ passed: true, rendered_browser: true, fixture_http_only: true, live_deployment_tested: false, requests: calls.length, checks: ["historical warning", "preserved rank and authored-content warnings", "no eager fetch", "keyboard replacement navigation", "replacement status unknown or own warnings", "explicit status refresh", "opaque denied replacement and recovery", "absent pointer", "escaped IDs", "stale success and failure", "empty-query invalidation", "web debounce and IDE submit", "mobile layout"] }));
} finally {
  await browser.close();
}
