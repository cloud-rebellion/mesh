// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
// settings.js: the Settings view. Reads /api/config (effective config, per-field
// source + editable), renders grouped forms, writes changes back with PUT
// /api/config, and offers Status + Reindex. Env-overridden fields are shown
// read-only with a badge (env wins over the file). Registers on Mesh.views.settings.
(function () {
  "use strict";
  const Mesh = (window.Mesh = window.Mesh || {});
  Mesh.views = Mesh.views || {};

  // a transient toast, shared with later views.
  Mesh.toast = Mesh.toast || function (msg, kind) {
    let t = document.getElementById("mesh-toast");
    if (!t) {
      t = document.createElement("div");
      t.id = "mesh-toast";
      document.body.appendChild(t);
    }
    t.textContent = msg;
    t.className = "show " + (kind || "");
    clearTimeout(Mesh._toastT);
    Mesh._toastT = setTimeout(() => (t.className = ""), 2600);
  };

  function esc(s) { return (s == null ? "" : String(s)).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }

  function row(f) {
    const dis = f.editable ? "" : "disabled";
    const badge = f.source === "env"
      ? '<span class="src env" title="set by an environment variable">env</span>'
      : f.source === "file" ? '<span class="src file">file</span>'
      : f.source === "admin" ? '<span class="src def" title="managed by an administrator">admin</span>'
      : '<span class="src def">default</span>';
    const type = f.kind === "number" ? "number" : "text";
    const ph = f.kind === "keyref" ? "MESH_..._KEY" : "";
    const step = f.kind === "number" ? 'step="any" min="0"' : "";
    return (
      '<div class="set-row">' +
      '<label>' + esc(f.label) + ' ' + badge + '</label>' +
      '<input data-key="' + esc(f.key) + '" type="' + type + '" ' + step + ' value="' + esc(f.value) + '" placeholder="' + esc(ph) + '" ' + dis + ' autocomplete="off" spellcheck="false">' +
      (f.help ? '<p class="set-help">' + esc(f.help) + '</p>' : "") +
      '</div>'
    );
  }

  Mesh.views.settings = async function (el, M) {
    el.innerHTML = '<div class="panel-inner"><p class="panel-h">Settings</p><h1 class="panel-title">Settings</h1><div class="panel-soon">Loading config...</div></div>';
    let cfg, status;
    try {
      cfg = await M.api("/api/config");
      status = M.status || (await M.api("/api/status"));
    } catch (e) {
      el.querySelector(".panel-soon").textContent = "Could not load settings: " + e.message;
      return;
    }
    const groups = {};
    (cfg.fields || []).forEach((f) => (groups[f.group] = groups[f.group] || []).push(f));
    const canAdminister = !cfg.permissions || cfg.permissions.admin !== false;

    // One-line description per group, shown in the collapsed summary so a user knows
    // whether they need to open it at all.
    const groupDesc = {
      "Semantic search (optional)": "Add \"find by meaning\" search by pointing Mesh at an embedding model.",
      "Ranking (advanced)": "Fine-tune how results are scored. Defaults are tuned; most people skip this.",
      "Reranker (optional)": "Re-score the top hits with a reranker for a sharper #1 result.",
      "Large-vault scale (pro)": "Speed knobs that only matter on very large vaults (pro build).",
    };

    const c = (status && status.counts) || {};
    const sig = (status && status.signals) || {};
    const sigChip = (k, label) => '<span class="chip ' + (sig[k] ? "on" : "") + '">' + label + "</span>";

    const inner = document.createElement("div");
    inner.className = "panel-inner";
    inner.innerHTML =
      '<p class="panel-h">Configure</p>' +
      '<h1 class="panel-title">Settings</h1>' +
      '<p class="panel-lead">Mesh works with no setup. Everything below is optional tuning, grouped and collapsed. Open a section only if you need it. Settings set by an environment variable show an "env" badge and are read-only here.</p>' +
      (canAdminister ? "" : '<p class="panel-lead">Some settings are managed by an administrator. Your account can view the effective safe settings, but cannot change shared vault configuration or reindex.</p>') +
      '<section class="set-group">' +
      '<h2 class="set-h">Index</h2>' +
      '<div class="set-status">' +
      "<div><b>" + (c.notes || 0) + "</b> notes &middot; <b>" + (c.nodes || 0) + "</b> nodes &middot; <b>" + (c.edges || 0) + "</b> links &middot; <b>" + (c.vectors || 0) + "</b> vectors</div>" +
      '<div class="set-sigs">' + sigChip("fts", "full-text") + sigChip("graph", "graph") + sigChip("vector", "semantic") + sigChip("rerank", "rerank") + sigChip("ann", "ANN") + "</div>" +
      '<button class="btn ghost" id="set-reindex">Reindex now</button>' +
      "</div></section>" +
      Object.keys(groups).map((g) =>
        '<details class="set-acc"><summary class="set-acc-sum"><span class="set-acc-name">' + esc(g) + "</span>" +
        (groupDesc[g] ? '<span class="set-acc-desc">' + esc(groupDesc[g]) + "</span>" : "") + "</summary>" +
        '<div class="set-acc-body">' + groups[g].map(row).join("") + "</div></details>"
      ).join("") +
      '<div class="set-bar"><span class="set-note">Environment variables override these. Secrets stay in env vars; fields name the var, never the key.</span>' +
      (canAdminister ? '<button class="btn" id="set-save">Save changes</button>' : '') + '</div>';

    el.replaceChildren(inner);

    const reindexButton = inner.querySelector("#set-reindex");
    if (!canAdminister) {
      reindexButton.disabled = true;
      reindexButton.title = "An administrator manages reindexing";
    }
    reindexButton.addEventListener("click", async (ev) => {
      ev.target.disabled = true;
      ev.target.textContent = "Reindexing...";
      try {
        const r = await M.api("/api/reindex", { method: "POST" });
        Mesh.toast("Reindexed: " + r.counts.notes + " notes", "ok");
        if (Mesh.refreshStatus) Mesh.refreshStatus();
        Mesh.views.settings(el, M); // re-render with fresh counts
      } catch (e) {
        Mesh.toast("Reindex failed: " + e.message, "err");
        ev.target.disabled = false;
        ev.target.textContent = "Reindex now";
      }
    });

    const saveButton = inner.querySelector("#set-save");
    if (!saveButton) return;
    saveButton.addEventListener("click", async (ev) => {
      const updates = {};
      inner.querySelectorAll("input[data-key]:not([disabled])").forEach((i) => (updates[i.dataset.key] = i.value));
      ev.target.disabled = true;
      ev.target.textContent = "Saving...";
      try {
        await M.api("/api/config", {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ updates }),
        });
        Mesh.toast("Settings saved", "ok");
        Mesh.views.settings(el, M); // re-render with fresh sources
      } catch (e) {
        Mesh.toast("Save failed: " + e.message, "err");
        ev.target.disabled = false;
        ev.target.textContent = "Save changes";
      }
    });
  };
})();
