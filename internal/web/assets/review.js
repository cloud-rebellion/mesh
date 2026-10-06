// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
// Review extracted drafts with the same templates used by the canonical writer.
(function () {
  "use strict";
  const Mesh = (window.Mesh = window.Mesh || {});
  Mesh.views = Mesh.views || {};
  function esc(s) { return (s == null ? "" : String(s)).replace(/[&<>\"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }
  function csv(s) { return s.split(",").map((v) => v.trim()).filter(Boolean); }
  function refreshBadge(M) {
    const el = document.getElementById("review-badge");
    if (!el) return;
    (M || Mesh).api("/api/pending").then((d) => {
      const n = (d.pending || []).length;
      el.hidden = !n;
      el.textContent = String(n);
    }).catch(() => {});
  }
  Mesh.refreshReviewBadge = refreshBadge;
  function sectionFields(template, values) {
    return (template.sections || []).map((s) => '<label style="display:block;margin:.8rem 0">' +
      '<strong>' + esc(s.heading) + (s.required ? " (required)" : "") + '</strong>' +
      '<span style="display:block;color:#9da7b3;font-size:.85rem">' + esc(s.guidance) + '</span>' +
      '<textarea data-section="' + esc(s.key) + '" rows="4" style="display:block;width:100%;margin-top:.3rem">' + esc(values[s.key] || "") + '</textarea></label>').join("");
  }
  function supportingContent(p) {
    const blocks = (p.blocks || []).map((b) => '<details><summary>' + esc(b.template) + ': ' + esc(b.id) + '</summary>' +
      Object.keys(b.fields || {}).map((key) => '<p><strong>' + esc(key.replace(/_/g, " ")) + '</strong></p><pre style="white-space:pre-wrap">' + esc(b.fields[key]) + '</pre>').join("") + '</details>').join("");
    const references = [p.related && p.related.length ? "Related: " + p.related.join(", ") : "", p.supersedes && p.supersedes.length ? "Replaces: " + p.supersedes.join(", ") : ""].filter(Boolean).join("; ");
    return (blocks ? '<div><h3>Supporting material</h3>' + blocks + '</div>' : "") + (references ? '<p>' + esc(references) + '</p>' : "");
  }
  function card(p, templates) {
    const selected = p.template || "finding";
    const template = templates.find((t) => t.id === selected) || templates[0];
    const options = templates.map((t) => '<option value="' + esc(t.id) + '"' + (t.id === selected ? " selected" : "") + '>' + esc(t.id) + '</option>').join("");
    return '<article class="rev-card" data-id="' + esc(p.id) + '" style="border:1px solid var(--hair,#21262d);border-radius:12px;padding:1rem;margin-bottom:1rem">' +
      '<h2>' + esc(p.title) + '</h2>' +
      '<p style="color:#9da7b3">' + esc(p.confidence ? "Confidence: " + p.confidence : "Confidence unspecified") + (p.source ? " · " + esc(p.source) : "") + '</p>' +
      (p.legacy_review_required ? '<p>Historical suggestion: choose its purpose and write supported sections before publishing.</p><details><summary>Original content</summary><pre style="white-space:pre-wrap">' + esc(p.legacy_content) + '</pre></details>' : "") +
      ((p.missing_content || []).length ? '<p>Draft needs: ' + esc(p.missing_content.join(", ")) + '</p>' : "") +
      '<label>Purpose <select class="rev-template">' + options + '</select></label>' +
      '<label style="display:block;margin:.6rem 0">Summary<textarea class="rev-summary" rows="2" style="display:block;width:100%">' + esc(p.summary) + '</textarea></label>' +
      '<div class="rev-sections">' + sectionFields(template, p.sections || {}) + '</div>' + supportingContent(p) +
      '<label style="display:block">Collections (verified note IDs)<input class="rev-collections" style="display:block;width:100%" value="' + esc((p.collections || []).join(", ")) + '"></label>' +
      '<label style="display:block;margin-top:.5rem">Topics<input class="rev-tags" style="display:block;width:100%" value="' + esc((p.tags || []).join(", ")) + '"></label>' +
      '<p class="rev-error" role="status" style="color:#f85149"></p>' +
      '<button class="rev-act" data-act="promote">Publish note</button> ' +
      '<button class="rev-act" data-act="discard">Discard</button></article>';
  }
  Mesh.views.review = function (el, M) {
    const inner = document.createElement("div");
    inner.className = "panel-inner";
    inner.innerHTML = '<h1 class="panel-title">Review queue</h1><p class="panel-lead">Review extracted knowledge, fill missing facts with supported prose, and publish when complete. Unknown facts belong in the draft.</p><div id="rev-body">Loading...</div>';
    el.replaceChildren(inner);
    const body = inner.querySelector("#rev-body");
    let items = [], templates = [];
    function load() {
      M.api("/api/pending").then((d) => {
        items = d.pending || []; templates = d.templates || [];
        body.innerHTML = items.length ? items.map((p) => card(p, templates)).join("") : '<p>Nothing to review.</p>';
        refreshBadge(M);
      }).catch((e) => { body.textContent = "Could not load review queue: " + e.message; });
    }
    body.addEventListener("change", (e) => {
      if (!e.target.matches(".rev-template")) return;
      const cardEl = e.target.closest(".rev-card");
      const values = {};
      cardEl.querySelectorAll("[data-section]").forEach((field) => { values[field.dataset.section] = field.value; });
      const template = templates.find((t) => t.id === e.target.value);
      if (template) cardEl.querySelector(".rev-sections").innerHTML = sectionFields(template, values);
    });
    body.addEventListener("click", (e) => {
      const button = e.target.closest("button.rev-act");
      if (!button) return;
      const cardEl = button.closest(".rev-card"), id = cardEl.dataset.id, act = button.dataset.act;
      const p = items.find((item) => item.id === id);
      const request = { id };
      if (act === "promote") {
        const sections = {};
        cardEl.querySelectorAll("[data-section]").forEach((field) => { sections[field.dataset.section] = field.value; });
        request.authoring = { title:p.title, template:cardEl.querySelector(".rev-template").value, template_version:1,
          summary:cardEl.querySelector(".rev-summary").value, sections, blocks:p.blocks || [],
          collections:csv(cardEl.querySelector(".rev-collections").value), tags:csv(cardEl.querySelector(".rev-tags").value),
          related:p.related || [], supersedes:p.supersedes || [] };
      }
      button.disabled = true;
      M.api("/api/pending/" + act, { method:"POST", body:JSON.stringify(request) }).then((receipt) => {
        cardEl.remove();
        if (receipt.index_stale) {
          const notice = document.createElement("p"); notice.textContent = "Note saved. Search indexing is pending."; body.prepend(notice);
        }
        if (!body.querySelector(".rev-card") && !receipt.index_stale) load();
        refreshBadge(M);
      }).catch((err) => { button.disabled = false; cardEl.querySelector(".rev-error").textContent = err.message; });
    });
    load();
  };
  if (document.readyState !== "loading") setTimeout(() => refreshBadge(Mesh), 800);
  else document.addEventListener("DOMContentLoaded", () => setTimeout(() => refreshBadge(Mesh), 800));
})();
