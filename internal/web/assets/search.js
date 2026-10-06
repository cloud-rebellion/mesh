// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
// search.js: the Search view. A query box runs the same fused retrieval the agent
// gets over MCP (GET /api/search) and renders ranked cards; opening a card fetches
// the note (GET /api/note/{id}) into a reading pane, with a jump back to the graph.
// Registers on Mesh.views.search. Vanilla, no deps.
(function () {
  "use strict";
  const Mesh = (window.Mesh = window.Mesh || {});
  Mesh.views = Mesh.views || {};

  function esc(s) { return (s == null ? "" : String(s)).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }
  // the retriever wraps matched terms in [brackets]; render them as marks.
  function snippet(s) { return esc(s).replace(/\[([^\]]+)\]/g, "<mark>$1</mark>"); }

  // Only use the reader-authorized pointer on the card, never infer one from
  // titles/prose/graph data. A missing pointer must not reveal a fenced note.
  function replacementID(c) {
    return c && typeof c.SupersededBy === "string" ? c.SupersededBy : "";
  }

  function guidanceHTML(c) {
    const missing = c && Array.isArray(c.MissingContent) ? c.MissingContent : (c && Array.isArray(c.MissingGuidance) ? c.MissingGuidance : []);
    return missing.length ? '<div class="rc-warning">Incomplete authored content: missing ' + esc(missing.join(", ")) + '; verify before relying on this note.</div>' : "";
  }

  function supersededHTML(c) {
    return replacementID(c) ? '<div class="rc-warning rc-superseded"><strong>Superseded: historical note.</strong> Review the replacement before relying on this guidance.</div>' : "";
  }

  function replacementHTML(c) {
    const id = replacementID(c);
    return id ? '<button type="button" class="btn ghost rc-replacement" data-id="' + esc(id) + '">Read replacement</button>' : "";
  }

  function cardHTML(c) {
    const tier = c.Tier0 ? '<span class="t0">tier-0</span>' : "";
    const score = typeof c.Score === "number" ? c.Score.toFixed(2) : "";
    return (
      '<div class="rc-result"><button type="button" class="rcard" data-id="' + esc(c.NoteID) + '">' +
      '<div class="rc-head"><span class="rc-title">' + esc(c.Title || c.NoteID) + "</span>" + tier + '<span class="rc-score">' + score + "</span></div>" +
      '<div class="rc-path">' + esc(c.Path) + "</div>" +
      supersededHTML(c) + guidanceHTML(c) +
      (c.Snippet ? '<div class="rc-snip">' + snippet(c.Snippet) + "</div>" : "") +
      (c.Reason ? '<div class="rc-reason">' + esc(c.Reason) + "</div>" : "") +
      "</button>" + replacementHTML(c) + "</div>"
    );
  }

  Mesh.views.search = function (el, M) {
    const inner = document.createElement("div");
    inner.className = "panel-inner";
    inner.innerHTML =
      '<p class="panel-h">Retrieve</p>' +
      '<h1 class="panel-title">Search</h1>' +
      '<p class="panel-lead">Searches the full text of every note and ranks by relevance. This is the same engine your AI agent uses. (The box on the Graph view is different: it only hides or shows nodes by name.)</p>' +
      '<div class="srch-box"><input id="srch-q" type="search" placeholder="search the whole vault" autocomplete="off" spellcheck="false"></div>' +
      '<div id="srch-results" class="srch-results"><p class="srch-hint">Type to search. Results are the fused full-text + graph + semantic ranking, with decisions, gotchas, and post-mortems surfaced first.</p></div>';
    el.replaceChildren(inner);

    const q = inner.querySelector("#srch-q");
    const results = inner.querySelector("#srch-results");
    if (M.searchOnSubmit) {
      const submit = document.createElement("button");
      submit.className = "btn";
      submit.type = "button";
      submit.textContent = "Search";
      submit.addEventListener("click", run);
      q.parentElement.appendChild(submit);
      results.textContent = "Press Enter or Search. Your existing Mesh retrieval settings apply.";
    }
    let timer, lastSeq = 0;
    let visibleCards = [];

    function bindReplacements() {
      results.querySelectorAll(".rc-replacement").forEach((b) => b.addEventListener("click", () => openNote(b.dataset.id, true)));
    }

    async function run() {
      clearTimeout(timer);
      const seq = ++lastSeq;
      const term = q.value.trim();
      visibleCards = [];
      if (!term) {
        results.innerHTML = '<p class="srch-hint">Type to search.</p>';
        return;
      }
      results.innerHTML = '<p class="srch-hint">Searching...</p>';
      try {
        const data = await M.api("/api/search?q=" + encodeURIComponent(term) + "&limit=15");
        if (seq !== lastSeq) return; // a newer query superseded this one
        const cards = data.cards || [];
        visibleCards = cards;
        if (!cards.length) {
          results.innerHTML = '<p class="srch-hint">No matches for "' + esc(term) + '".</p>';
          return;
        }
        results.innerHTML = '<p class="srch-count">' + cards.length + " results &middot; " + (data.tokens || 0) + " tokens</p>" + cards.map(cardHTML).join("");
        results.querySelectorAll(".rcard").forEach((b) => b.addEventListener("click", () => openNote(b.dataset.id)));
        bindReplacements();
      } catch (e) {
        if (seq === lastSeq) results.innerHTML = '<p class="srch-hint">Search failed: ' + esc(e.message) + "</p>";
      }
    }

    async function openNote(id, isReplacement = false) {
      clearTimeout(timer);
      const seq = ++lastSeq;
      const card = visibleCards.find(c => c.NoteID === id);
      results.innerHTML = '<p class="srch-hint">Loading...</p>';
      try {
        // Fetch on click through the normal authorized endpoint. Search-time
        // permission is not a grant: revocation/deletion may have happened since.
        const n = await M.api("/api/note/" + encodeURIComponent(id));
        if (seq !== lastSeq) return;
        const bodyHTML = n.html || ('<pre class="note-md">' + esc(n.markdown) + "</pre>");
        // /api/note authorizes the current read, but does not supply fresh
        // retrieval-status metadata. Never call an unlisted replacement current
        // or silently transfer the historical note's status onto it.
        const statusHTML = card
          ? '<p class="srch-hint">Guidance status is from the last search. Search this note again to refresh it.</p>' + supersededHTML(card) + guidanceHTML(card) + replacementHTML(card)
          : '<p class="rc-warning">Guidance status not checked for this note. Search for it before relying on it.</p>';
        results.innerHTML =
          '<div class="note-pane">' +
          '<div class="note-bar"><button class="btn ghost" id="note-back">&larr; results</button>' +
          '<span class="note-path">' + esc(n.path) + "</span>" +
          '<button class="btn ghost" id="note-graph">show in graph</button></div>' +
          statusHTML + '<button type="button" class="btn ghost rc-replacement-search" id="note-search">Search this note</button>' +
          '<div class="note-body prose">' + bodyHTML + "</div></div>";
        results.querySelector("#note-back").addEventListener("click", run);
        results.querySelector("#note-search").addEventListener("click", () => {
          q.value = (n.meta && typeof n.meta.title === "string" && n.meta.title.trim()) || id;
          run();
        });
        bindReplacements();
        results.querySelector("#note-graph").addEventListener("click", () => {
          if (Mesh.route) Mesh.route("graph");
          location.hash = "";
          const gq = document.getElementById("q");
          if (gq) { gq.value = id; gq.dispatchEvent(new Event("input", { bubbles: true })); }
        });
      } catch (e) {
        if (seq !== lastSeq) return;
        const message = isReplacement ? "Replacement unavailable. It may have changed or you may no longer have access. Return to results and search again." : "Could not open note: " + e.message;
        results.innerHTML = '<p class="srch-hint">' + esc(message) + '</p><button type="button" class="btn ghost" id="note-back">&larr; results</button>';
        results.querySelector("#note-back").addEventListener("click", run);
      }
    }

    q.addEventListener("keydown", (event) => {
      if (M.searchOnSubmit && event.key === "Enter") { event.preventDefault(); run(); }
    });
    q.addEventListener("input", () => {
      if (M.searchOnSubmit) return;
      clearTimeout(timer);
      timer = setTimeout(run, 250);
    });
    q.focus();
  };
})();
