// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
// mesh ui: a sovereign canvas graph viewer. No dependencies. Two living views over
// one graph: an Obsidian-style force graph (a continuous velocity sim you can grab
// and fling) and a galaxy orbiting the index note. Nodes are additive glow blobs;
// the index is a small sun.
(() => {
  "use strict";
  // the shell modules merge into this same object, so making it here keeps the hooks
  // below registered even when graph.json arrives before they have run
  const Mesh = (window.Mesh = window.Mesh || {});
  const $ = (id) => document.getElementById(id);
  const canvas = $("stage"), ctx = canvas.getContext("2d");
  const canvas3d = $("stage3d");
  let gl3d = null; // lazy WebGL2 galaxy; null until the 3D tab is first opened (or if unavailable)
  let captureStill = false; // ?still=1 capture mode: skip the galaxy fly-in + hide the build overlay
  const overlay = $("overlay"), overlayMsg = $("overlay-msg");
  const TAU = Math.PI * 2;
  const HEX = /^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$/; // the only forms rgb() parses

  let G = null;
  const byId = new Map();
  const nodeIndex = new Map();
  const commColor = new Map();
  const sprites = new Map(); // community color -> offscreen glow sprite
  let sunSprite = null;
  let sp = [];               // reused screen-position buffer
  let view = "graph";
  const cam = { x: 0, y: 0, zoom: 1 };
  // 2D fly-into-node + lock-on (mirrors the 3D camera): while camFollow is set the
  // layout freezes and the camera eases onto the node; camReturn eases back out.
  let camFollow = null, camReturn = null, preFocus = null;
  const FOCUS_ZOOM = 2.6;
  // auto-fit: while on (and nothing else holds the camera) it glides to frame the whole
  // graph around the index, so the zoom follows the force layout as it blooms. Any
  // manual pan, zoom or drag hands control back; double-clicking empty space, "0", or an
  // Escape with nothing left to dismiss resumes it.
  let autoFit = true, fitTick = 0, minZoom = 0.08;
  const MAX_ZOOM = 8;
  let indexNode = null;      // the index note: the galaxy's sun, pinned at the origin in the force view
  let hover = null, selected = null, query = "";
  let dpr = Math.max(1, window.devicePixelRatio || 1);
  let W = 0, H = 0;
  let galaxyAngle = 0;
  let swirl = 0;             // force view: the same kind of render-time rigid turn (see pos)
  let alpha = 1;             // sim energy: cools to a floor, then the sim sleeps (see simStep)
  let simAwake = true, quiet = 0; // quiet: consecutive still steps at the floor
  let settle = null;         // the last dropped note: the sim stays awake until it is back (see simStep)
  let settleT = 0;           // ...for at most this many more steps
  let neighborSet = null;
  let spotlight = null; // community id to spotlight (dim the rest); set by the legend
  let drag = null;           // { node, vx, vy } while dragging/flinging a node
  let running = true;        // rAF gate; paused when the tab is hidden
  let lastInteract = 0;      // for easing galaxy rotation to a near-stop when idle
  let labelOrder = [];       // node indices by descending importance, for ambient labels
  const boxes = [];          // per-frame label rects, for the declutter pass
  let stars = [];
  const now = () => (window.performance && performance.now ? performance.now() : 0);
  let vignette = null;
  let t = 0;                 // clock in 60 Hz frames, for the sun pulse
  let CX = 0;                // screen x of the VISIBLE centre (the rail covers the left edge)
  let inset = 0;             // px of canvas hidden under the rail
  let dust = [];             // galaxy view: faint arm particles (not notes)
  let clock = 0;             // seconds, drives the float drift
  // Reduced motion: notes hold still (no drift, no idle orbit) and the camera jumps into a
  // note and back out instead of flying (as the 3D camera does); the auto-fit still glides.
  const calmMQ = window.matchMedia ? window.matchMedia("(prefers-reduced-motion: reduce)") : null;
  const calm = () => !!(calmMQ && calmMQ.matches);

  const safeColor = (c) => (typeof c === "string" && HEX.test(c) ? c : "#7c766e");

  // ---- topic domains: an alternate grouping to the emergent link communities ----
  // Each note is bucketed into a business-area domain from its tags, so the galaxy can
  // be grouped + colored by topic (Engineering, Infra, Marketing, ...) with stable
  // colors and clean labels. This is a VIEW only: retrieval + the community graph are
  // untouched. A note's domain = the domain its tags vote for most (ties: first listed).
  const DOMAIN_DEFS = [
    { label: "Engineering", color: "#6ea8ff", tags: ["mesh","frontend","backend","api","database","sql","sqlc","mcp","webgl","workflow","testing","refactor","go","typescript"] },
    { label: "Infra / DevOps", color: "#54c489", tags: ["deploy","self-hosted","docker","kubernetes","tailscale","infra","dns","proxy","monitoring","ci","backup","networking"] },
    { label: "Marketing / SEO", color: "#f0a93b", tags: ["marketing","seo","cro","content","social","geo","aeo","growth","newsletter","copywriting"] },
    { label: "Sales / Outreach", color: "#e8629a", tags: ["sales","outreach","leads","prospecting","legal","crm"] },
    { label: "Security / Compliance", color: "#e0524e", tags: ["security","gdpr","audit","compliance","owasp","pii","encryption","secrets","vault"] },
    { label: "Design", color: "#38c5d0", tags: ["design","branding","typography","visual","taste","css","font"] },
    { label: "Knowledge / Learning", color: "#b08cf0", tags: ["claude-skill","learning","fundamentals","reference","concept","skill","basics","guide","patterns"] },
    { label: "General", color: "#8a8f9a", tags: [] },
  ];
  const GENERAL_DOMAIN = DOMAIN_DEFS.length - 1;
  const DOMAIN_TAG = new Map();
  DOMAIN_DEFS.forEach((d, i) => d.tags.forEach((t) => DOMAIN_TAG.set(t, i)));
  const domainColor = new Map();
  let grouping = "community"; // "community" (emergent link clusters) | "domain" (topic)

  function domainIndexFor(n) {
    // vote from tags + tokens of the id/title (so an untagged note whose id names its
    // topic, e.g. "deploy-log", still classifies).
    const idt = ((n.id || "") + " " + (n.label || "")).toLowerCase().split(/[^a-z0-9]+/);
    const toks = (n.tags || []).map((t) => String(t).toLowerCase()).concat(idt);
    const score = {};
    let best = -1, bestS = 0;
    for (const t of toks) {
      const di = DOMAIN_TAG.get(t);
      if (di == null) continue;
      score[di] = (score[di] || 0) + 1;
      if (score[di] > bestS) { bestS = score[di]; best = di; }
    }
    return best < 0 ? GENERAL_DOMAIN : best;
  }
  function computeDomains() {
    for (const n of G.nodes) n.domain = domainIndexFor(n);
    // pass 2: a note that matched nothing inherits its community's dominant domain
    // (communities are topical, so an untagged note in an infra cluster is Infra).
    const byComm = new Map();
    for (const n of G.nodes) {
      if (n.domain === GENERAL_DOMAIN) continue;
      const m = byComm.get(n.community) || {};
      m[n.domain] = (m[n.domain] || 0) + 1; byComm.set(n.community, m);
    }
    const commDom = new Map();
    for (const [comm, m] of byComm) {
      let best = -1, bs = 0;
      for (const k in m) if (m[k] > bs) { bs = m[k]; best = +k; }
      if (best >= 0) commDom.set(comm, best);
    }
    for (const n of G.nodes) if (n.domain === GENERAL_DOMAIN && commDom.has(n.community)) n.domain = commDom.get(n.community);
    const counts = new Array(DOMAIN_DEFS.length).fill(0);
    for (const n of G.nodes) counts[n.domain]++;
    G.domains = DOMAIN_DEFS.map((d, i) => ({ id: i, color: d.color, label: d.label, size: counts[i] }))
      .filter((d) => d.size > 0).sort((a, b) => b.size - a.size);
    domainColor.clear();
    for (const d of G.domains) domainColor.set(d.id, d.color);
  }
  const activeGroups = () => (grouping === "domain" ? (G.domains || []) : G.communities);
  const groupKeyOf = (n) => (grouping === "domain" ? n.domain : n.community);
  const groupColorOf = (n) => (grouping === "domain" ? domainColor : commColor).get(groupKeyOf(n)) || "#7c766e";

  // the galaxy's spiral, arm packer and tap rules are gl3d.js's (Mesh3D, below), so
  // without it there is no graph to lay out: say so, rather than stall half-booted
  if (!window.Mesh3D || typeof window.Mesh3D.packArms !== "function") { fail("its 3D module (gl3d.js) did not load. Reload the page."); return; }
  fetch("graph.json").then((r) => {
    if (!r.ok) throw new Error("graph.json " + r.status);
    return r.json();
  }).then(boot).catch(fail);

  function boot(data) {
    G = data;
    G.communities = G.communities || []; // tolerate a graph indexed before community detection
    G.edges = G.edges || [];
    G.collection_edges = G.collection_edges || [];
    resize();
    if (!G.nodes || G.nodes.length === 0) return showEmpty();
    for (const c of G.communities) { c.color = safeColor(c.color); commColor.set(c.id, c.color); }
    G.nodes.forEach((n, i) => { byId.set(n.id, n); nodeIndex.set(n.id, i); n.vx = 0; n.vy = 0; n.dispX = 0; n.dispY = 0; seedFloat(n, i); });
    indexNode = byId.get(G.meta.index_id) || null;
    computeDomains(); // assign each note a topic domain for the alternate grouping
    sp = new Array(G.nodes.length);
    buildAdjacency();
    buildGroupIndex();
    labelOrder = G.nodes.map((_, i) => i).sort((a, b) =>
      G.nodes[b].degree - G.nodes[a].degree || nodeRadius(G.nodes[b]) - nodeRadius(G.nodes[a]) || (G.nodes[a].id < G.nodes[b].id ? -1 : 1));
    buildSprites();
    seedLayout();
    layoutGalaxy();
    setStats();
    buildLegend();
    buildExplorer();
    fitView();
    doneOverlay();
    wire();
    // closing the note reader eases the camera back out of the note it flew into. Both
    // views: the note may have been focused in one and closed in the other, and a lock
    // left behind would freeze that view on it later (each is a no-op when unlocked).
    // A galaxy a regroup disposed has only its camera left: that lets go of the note too,
    // back at the rest framing as clearFocus flies it, unless it was panned or zoomed on
    // the note (focusMoved): then, as clearFocus does, it only lets go where it is. A note
    // still selected keeps its star lit, as its card and its 2D neighborhood stay (and as
    // a view switch relights it).
    Mesh.onNoteClose = () => {
      if (gl3d) { gl3d.clearFocus(); gl3d.setHighlight(selected ? selected.id : null); }
      else if (gl3dCam && gl3dCam.focusId != null) {
        gl3dCam = gl3dCam.focusMoved ? { ...gl3dCam, focusId: null, focusMoved: false }
          : { yaw: gl3dCam.yaw, pitch: gl3dCam.pitch, spinTime: gl3dCam.spinTime, time: gl3dCam.time };
      }
      clearFocus2d();
    };
    lastInteract = now();
    requestAnimationFrame(loop);
    // Deep-link a view: ?v=galaxy3d (or galaxy) opens straight into it - shareable,
    // and lets a headless capture land on the 3D galaxy without a click.
    const params = new URLSearchParams(location.search);
    captureStill = params.get("still") === "1";
    const dv = params.get("v");
    if (dv === "galaxy3d" || dv === "galaxy") setView(dv);
    if (captureStill) overlay.style.display = "none"; // deterministic still capture
  }

  const adj = new Map();
  let edgeIdx = null;        // Int32Array of [a, b] node-index pairs, self-loops and dangling ends dropped
  let collectionPair = null; // visual membership lines; semantic edges win overlapping pairs
  const navigationEdges = () => [...G.edges, ...G.collection_edges];
  function buildAdjacency() {
    for (const n of G.nodes) adj.set(n.id, []);
    // one link per unordered pair (key min*N+max): A->B plus B->A, or a repeated link,
    // would double that spring and the degree the springs are normalised by
    const ei = [], memberships = [], seen = new Set(), N = G.nodes.length;
    for (const e of navigationEdges()) {
      if (e.source === e.target || !adj.has(e.source) || !adj.has(e.target)) continue;
      const a = nodeIndex.get(e.source), b = nodeIndex.get(e.target), k = a < b ? a * N + b : b * N + a;
      if (seen.has(k)) continue;
      seen.add(k);
      adj.get(e.source).push(e.target); adj.get(e.target).push(e.source);
      ei.push(a, b);
      memberships.push(e.rel === "collection-membership" ? 1 : 0);
    }
    edgeIdx = new Int32Array(ei);
    collectionPair = new Uint8Array(memberships);
    deg = new Int32Array(G.nodes.length);
    for (const i of edgeIdx) deg[i]++;
    // charge: a hub pushes harder, so its burst has room round it. It is the note's mass
    // too, so a hub moves little when its springs pull (see simStep)
    chg = new Float64Array(G.nodes.length);
    for (let i = 0; i < chg.length; i++) chg[i] = 1 + CHARGE * deg[i];
  }

  // ---- float: every note drifts on its own slow two-frequency loop ----
  // A render-space offset in SCREEN pixels, so the drift reads the same at any zoom
  // and never disturbs the layout (edges bend with it because they draw to the
  // offset positions). depth fakes a z: near notes are brighter, a touch bigger, and
  // drift further; hubs are heavy and drift less.
  function seedFloat(n, i) {
    n.depth = rand(i * 17 + 5);
    n.fp1 = rand(i * 29 + 1) * TAU; n.fp2 = rand(i * 31 + 2) * TAU;
    n.ff1 = TAU / (8 + rand(i * 37 + 3) * 7);   // 8-15 s per loop
    n.ff2 = TAU / (11 + rand(i * 41 + 4) * 9);  // 11-20 s, so the path never repeats exactly
    n.famp = (2.2 + 3.4 * n.depth) / Math.sqrt(Math.max(1, (n.size || 1) - 0.4));
  }

  // group index: dense 0..K-1 slot per group key, for the per-frame nebula centroids
  // and the colour-bucketed edge batches. Rebuilt when the grouping flips.
  let gSlot = new Map(), gCount = 0, gColors = [], nodeSlot = null, edgeBuckets = [], coreCol = [], nebOn = null;
  let gSx, gSy, gSxx, gN;
  function buildGroupIndex() {
    gSlot = new Map(); gColors = [];
    nodeSlot = new Int32Array(G.nodes.length);
    G.nodes.forEach((n, i) => {
      const k = groupKeyOf(n);
      if (!gSlot.has(k)) { gSlot.set(k, gColors.length); gColors.push(groupColorOf(n)); }
      nodeSlot[i] = gSlot.get(k);
    });
    // each dot's core colour depends only on its group hue + depth, so build the
    // string once here instead of per note per frame
    coreCol = G.nodes.map((n) => lighten(groupColorOf(n), 0.5 + 0.25 * n.depth));
    gCount = gColors.length;
    gSize = new Int32Array(gCount);
    for (const k of nodeSlot) gSize[k]++;
    gSx = new Float64Array(gCount); gSy = new Float64Array(gCount); gSxx = new Float64Array(gCount); gN = new Int32Array(gCount);
    // a nebula only around the 24 biggest groups of five or more notes (ties by group id),
    // the 3D galaxy's set: hundreds of mid-size clusters would fog the whole map
    nebOn = new Uint8Array(gCount);
    [...gSlot].filter(([, k]) => gSize[k] >= 5).sort((a, b) => gSize[b[1]] - gSize[a[1]] || byGroupId(a[0] || 0, b[0] || 0))
      .slice(0, 24).forEach(([, k]) => { nebOn[k] = 1; });
    // each note's hub: its most-linked neighbour in its own group, if that one has more
    // links than it (ties by index, so the hubs form a forest rooted at local maxima). Per
    // group, so a burst stays inside its island: the index links into most groups and
    // would otherwise haul every island's notes onto itself. Rebuilt on a regroup.
    par = new Int32Array(G.nodes.length).fill(-1);
    const over = (p, q) => deg[p] > deg[q] || (deg[p] === deg[q] && p < q);
    for (let j = 0; j < edgeIdx.length; j += 2) {
      const a = edgeIdx[j], b = edgeIdx[j + 1];
      if (nodeSlot[a] !== nodeSlot[b]) continue;
      if (over(b, a) && (par[a] < 0 || over(b, par[a]))) par[a] = b;
      if (over(a, b) && (par[b] < 0 || over(a, par[b]))) par[b] = a;
    }
    kids = new Int32Array(G.nodes.length);
    for (const p of par) if (p >= 0) kids[p]++;
    kidAt0 = new Int32Array(G.nodes.length + 1); kidAt = new Int32Array(G.nodes.length);
    for (let i = 0; i < kids.length; i++) kidAt0[i + 1] = kidAt0[i] + kids[i];
    const fill = kidAt0.slice(0, -1);
    par.forEach((p, i) => { if (p >= 0) kidAt[fill[p]++] = i; });
    // edges inside one group draw in that group's hue; bridges between groups stay
    // a cool neutral. One path per bucket keeps it to ~2K strokes a frame. The force view
    // draws them as a fine grey-tinted web (web), a note's link to its hub a touch
    // brighter, so a hub's burst shows its spokes, and the bridges fainter, so the space
    // between the islands reads dark; the galaxy keeps its faint threads.
    const buckets = new Map();
    for (let j = 0; j < edgeIdx.length; j += 2) {
      const a = edgeIdx[j], b = edgeIdx[j + 1];
      const slot = nodeSlot[a] === nodeSlot[b] ? nodeSlot[a] : -1, spoke = par[a] === b || par[b] === a;
      const membership = !!collectionPair[j >> 1], key = `${slot}/${spoke}/${membership}`;
      if (!buckets.has(key)) buckets.set(key, { slot, spoke, membership, list: [] });
      buckets.get(key).list.push(a, b);
    }
    edgeBuckets = [...buckets.values()].map(({ slot: k, spoke, membership, list }) => {
      const c = k < 0 ? null : rgb(gColors[k]);
      const wa = spoke ? WEB_A * SPOKE_A : k < 0 ? WEB_A * BRIDGE_A : WEB_A;
      return { slot: k, pairs: list, membership, stroke: c ? `rgba(${c.r},${c.gg},${c.b},0.075)` : "rgba(150,165,205,0.045)",
        web: c ? `rgba(${(c.r + 170) >> 1},${(c.gg + 175) >> 1},${(c.b + 195) >> 1},${wa})` : `rgba(165,172,195,${wa * 0.8})` };
    });
    // the springs (see simStep), per link: strength and rest length. A note's link to its
    // hub is strong; the rest of its links are a weak web over the lighter end's degree,
    // and a bridge between groups is weaker still and longer, so it ties islands together
    // without closing the dark between them. A burst's radius grows with sqrt of its notes. A
    // leaf rests somewhere across its hub's burst (sqrt of a hash, so they fill it as a disc,
    // not one hard ring), a hub rests clear of its parent's burst. Every rest length adds
    // both dots' own radii, so big dots never rest inside each other (a small vault zooms
    // in to 1.1, where a hub's dot is wider than SPACING).
    const E = edgeIdx.length >> 1, bR = (i) => BURST * Math.sqrt(kids[i]);
    eK = new Float64Array(E); eL = new Float64Array(E);
    for (let e = 0; e < E; e++) {
      const a = edgeIdx[e * 2], b = edgeIdx[e * 2 + 1], da = deg[a], db = deg[b];
      const c = par[a] === b ? a : par[b] === a ? b : -1, p = c === a ? b : a;
      eK[e] = c >= 0 ? K_TREE : K_LINK / Math.min(da, db) * (nodeSlot[a] === nodeSlot[b] ? 1 : K_BRIDGE);
      eL[e] = SPACING * (c < 0 ? (1 + 0.5 * (bR(a) + bR(b))) * (nodeSlot[a] === nodeSlot[b] ? 1 : BRIDGE_L) : kids[c] ? 1 + bR(p) + bR(c) : LEAF_L + bR(p) * Math.sqrt(rand(c * 53 + 7)))
        + nodeRadius(G.nodes[a]) + nodeRadius(G.nodes[b]);
    }
    // loners: notes with no link, or in a group too small to be a region, keep to homes
    // scattered across the disc (see seedLayout)
    loner = new Uint8Array(G.nodes.length);
    for (let i = 0; i < loner.length; i++) loner[i] = gSize[nodeSlot[i]] <= TINY || !deg[i] ? 1 : 0;
  }

  // buildInfluence weights the dragged node (1) and the rings around it, so a galaxy drag
  // pulls the local cluster elastically: 1-hop comes along strongly (0.72), 2-hop follows
  // clearly (0.42), 3-hop drifts a little (0.18). Breadth-first, so a ring is finished
  // before the next starts, and each ring is capped, so neither a hub nor a note next to
  // one hauls a third of the map. A note past a ring's cap is still that ring's (seen):
  // it stays put rather than coming along as the next ring out. The index never comes
  // along (it is the sun, pinned on the core), and a hub (over HUB_DEG links) joins its
  // ring but feeds no next one: its links from across the map would fill the capped ring
  // ahead of the dragged note's own cluster.
  const RING_W = [0.72, 0.42, 0.18], RING_CAP = 90, HUB_DEG = 40;
  function buildInfluence(root) {
    const m = new Map([[root.id, 1]]), seen = new Set([root.id, G.meta.index_id]);
    let ring = [root.id];
    for (const w of RING_W) {
      const next = [];
      for (const a of ring) {
        const nb = adj.get(a) || [];
        if (a !== root.id && nb.length > HUB_DEG) continue;
        for (const b of nb) {
          if (seen.has(b)) continue;
          seen.add(b);
          if (next.length < RING_CAP) { m.set(b, w); next.push(b); }
        }
      }
      ring = next;
    }
    return m;
  }

  // ---- glow sprites (built once per community color; drawn additively) ----
  function buildSprites() {
    for (const c of G.communities) sprites.set(c.color, haloSprite(c.color));
    for (const d of DOMAIN_DEFS) sprites.set(d.color, haloSprite(d.color)); // topic-grouping colors
    sprites.set("#7c766e", haloSprite("#7c766e"));
    sunSprite = haloSprite("#fff1dc", true);
    for (const c of sprites.keys()) nebSprites.set(c, nebulaSprite(c));
    galCore = galaxyCoreSprite();
  }
  // the galaxy's bulge: white-hot centre through warm peach and a pink ring into a
  // faint teal disc glow (the same palette as the 3D galaxy).
  let galCore = null;
  function galaxyCoreSprite() {
    const R = 128, cv = document.createElement("canvas");
    cv.width = cv.height = R * 2;
    const g = cv.getContext("2d"), grad = g.createRadialGradient(R, R, 0, R, R, R);
    grad.addColorStop(0, "rgba(255,250,240,0.95)");
    grad.addColorStop(0.06, "rgba(255,236,208,0.7)");
    grad.addColorStop(0.16, "rgba(255,196,170,0.3)");
    grad.addColorStop(0.3, "rgba(236,128,172,0.12)");
    grad.addColorStop(0.55, "rgba(80,170,210,0.06)");
    grad.addColorStop(1, "rgba(40,80,140,0)");
    g.fillStyle = grad; g.fillRect(0, 0, R * 2, R * 2);
    return cv;
  }
  // nebula: a very soft, wide colour cloud drawn behind each cluster, so a group
  // reads as a lit gas region instead of a pile of dots.
  const nebSprites = new Map();
  function nebulaSprite(color) {
    const R = 64, cv = document.createElement("canvas");
    cv.width = cv.height = R * 2;
    const g = cv.getContext("2d"), grad = g.createRadialGradient(R, R, 0, R, R, R);
    const { r, gg, b } = rgb(color);
    grad.addColorStop(0, `rgba(${r},${gg},${b},0.7)`);
    grad.addColorStop(0.28, `rgba(${r},${gg},${b},0.36)`);
    grad.addColorStop(0.62, `rgba(${r},${gg},${b},0.1)`);
    grad.addColorStop(1, `rgba(${r},${gg},${b},0)`);
    g.fillStyle = grad; g.fillRect(0, 0, R * 2, R * 2);
    return cv;
  }
  function haloSprite(color, sun) {
    const R = 48;
    const cv = document.createElement("canvas");
    cv.width = cv.height = R * 2;
    const g = cv.getContext("2d");
    const grad = g.createRadialGradient(R, R, 0, R, R, R);
    const { r, gg, b } = rgb(color);
    if (sun) {
      // a real star: hot bright core fading through the community hue (the "sun
      // pulling planets" look), bright but not the old nuclear white-out.
      grad.addColorStop(0, `rgba(255,252,246,0.95)`);
      grad.addColorStop(0.08, `rgba(255,243,224,0.78)`);
      grad.addColorStop(0.26, `rgba(${r},${gg},${b},0.4)`);
      grad.addColorStop(0.55, `rgba(${r},${gg},${b},0.14)`);
      grad.addColorStop(1, `rgba(${r},${gg},${b},0)`);
    } else {
      // glow back, a touch: a bit more deposit than the de-tackify low, still under
      // 1.0 for a moderately dense cluster (the force swirl keeps it from collapsing).
      grad.addColorStop(0, `rgba(${r},${gg},${b},0.18)`);
      grad.addColorStop(0.35, `rgba(${r},${gg},${b},0.06)`);
      grad.addColorStop(1, `rgba(${r},${gg},${b},0)`);
    }
    g.fillStyle = grad;
    g.beginPath(); g.arc(R, R, R, 0, TAU); g.fill();
    return cv;
  }

  // ---- layouts ----
  // Both 2D views grow with sqrt(N): the force sim spaces neighbours SPACING apart, so
  // its area tracks the note count, and the galaxy radius is set the same way. So the
  // two views fit to about the same zoom at any vault size and the dots match.
  const SPACING = 36;        // target world distance between neighbouring notes
  let GAL_R = 2300;          // galaxy disc radius, world units: 33.6 * sqrt(N), set in layoutGalaxy
  function seedLayout() {
    // Each group starts as a tight knot at its own spot on a sunflower spiral, the
    // biggest nearest the middle. The sim then blooms the knots out and the links weave
    // them into one web, so the opening seconds read as the map unfurling, not a random
    // cloud collapsing. The index's group goes first, seeded at the origin, round the
    // pinned index.
    const ig = indexNode ? nodeSlot[nodeIndex.get(indexNode.id)] : -1;
    const order = [...Array(gCount).keys()].sort((a, b) => (b === ig) - (a === ig) || gSize[b] - gSize[a]);
    const seedX = new Float64Array(gCount), seedY = new Float64Array(gCount);
    let cum = 0;
    order.forEach((g, j) => {
      const r = g === ig ? 0 : SPACING * 1.5 * Math.sqrt(cum + gSize[g] / 2), a = j * 2.399963;
      seedX[g] = Math.cos(a) * r; seedY[g] = Math.sin(a) * r;
      cum += gSize[g];
    });
    G.nodes.forEach((n, i) => {
      // a loner's home (in units of the map's radius), scattered across the disc so loners
      // read as stardust between the islands (a band round the rim read as a hard ring)
      const ha = rand(i * 43 + 21) * TAU, hr = HOME_R0 + HOME_DR * Math.sqrt(rand(i * 47 + 22));
      n.homeX = Math.cos(ha) * hr; n.homeY = Math.sin(ha) * hr;
      const g = nodeSlot[i], a = rand(i * 3 + 11) * TAU;
      const r = Math.sqrt(rand(i * 5 + 13)) * SPACING * 0.3 * Math.sqrt(gSize[g]);
      n.gx = seedX[g] + Math.cos(a) * r; n.gy = seedY[g] + Math.sin(a) * r;
      n.vx = 0; n.vy = 0;
      if (n === indexNode) { n.gx = 0; n.gy = 0; }
    });
  }

  // Galaxy: a face-on spiral. Every group owns a segment of one arm, sized by its note
  // count, so a big cluster streams along the arm instead of piling into a blob, and
  // groups are balanced across the arms (greedy: next-biggest onto the lightest arm).
  // The arms wind logarithmically, ~1.35 turns from the bulge to the rim, slim, spaced
  // wider toward the rim so they thin out there, with dark gaps between them; a few
  // notes stray into the gaps so the arms have soft edges. The whole pattern turns
  // rigidly (galaxyAngle) against the arms' wind, so they trail: differential rotation
  // would wind them into rings within minutes. Each note's float drift keeps it alive.
  let gSegA = null, gSeg0 = null, gSeg1 = null;
  // The arm count and the spiral (angle, where a stretch of arm sits, its width) are the
  // 3D galaxy's (Mesh3D), so a cluster deals onto the same stretch of the same arm in
  // both. gl3d.js always loads first (index.html: both deferred, from the same embed),
  // so these are the one copy, with no stand-ins to drift. u: radius over GAL_R.
  const M3 = window.Mesh3D;
  const GAL_ARMS = M3.GAL_ARMS, galArmAngle = M3.armAngle, armU = M3.armU, armW = M3.armW;
  // Deal groups onto arms: the next biggest onto the lightest arm (ties by group id,
  // numbers first, never input order), each owning the stretch [s0, s1) of its arm.
  // groups: [{id, size}] -> Map id -> {arm, s0, s1}.
  const { packArms, byGroupId } = M3;
  // a finger's tap follows the 3D galaxy's rules (slop, double-tap time and reach, hit
  // radius), so it behaves the same in every view. A pen taps by them in 2D but by the
  // mouse's in 3D (gl3d routes a pen through its mouse handlers)
  const { TAP_SLOP, DBL_TAP_MS, DBL_TAP_PX, TAP_R } = M3;
  function layoutGalaxy() {
    const N = G.nodes.length;
    GAL_R = 33.6 * Math.sqrt(N); // 2300 at ~4.7k notes; a small vault gets a small, dense disc
    // keyed as gl3d keys them (a note with no group is group 0), so both deal the same set
    const sizes = new Map();
    for (const [id, g] of gSlot) sizes.set(id || 0, (sizes.get(id || 0) || 0) + gSize[g]);
    const seg = packArms([...sizes].map(([id, size]) => ({ id, size })), GAL_ARMS);
    gSegA = new Int32Array(gCount); gSeg0 = new Float64Array(gCount); gSeg1 = new Float64Array(gCount);
    for (const [id, g] of gSlot) { const sg = seg.get(id || 0); gSegA[g] = sg.arm; gSeg0[g] = sg.s0; gSeg1[g] = sg.s1; }
    const idx = G.meta.index_id;
    G.nodes.forEach((n, i) => {
      if (n.id === idx) { n.gal0x = 0; n.gal0y = 0; return; }
      const g = nodeSlot[i];
      const s = gSeg0[g] + (gSeg1[g] - gSeg0[g]) * rand(i * 7 + 1);
      // on the ridge at its stretch of arm, then off it: across the arm by a radial step
      // (the arm is tight, so that is nearly square to it) and a little along it
      const u = armU(s);
      const gauss = (rand(i * 11 + 2) + rand(i * 13 + 3) + rand(i * 19 + 4) - 1.5) / 1.5; // ~normal in [-1,1]
      const stray = rand(i * 23 + 5) < 0.12 ? 2.8 : 1;
      const th = galArmAngle(gSegA[g], u) + (rand(i * 29 + 6) - 0.5) * 0.16 * stray;
      const rr = (u + gauss * armW(u) * stray) * GAL_R;
      n.gal0x = Math.cos(th) * rr; n.gal0y = Math.sin(th) * rr;
    });
    // dust: faint unlabelled arm particles so the disc reads as a galaxy between notes.
    // The count follows the disc area (so N), with a floor so a small vault still has arms.
    // Spaced wider toward the rim, as the notes are, with the odd brighter grain there (the
    // sparkle at the arm tips); one in eight drifts loose, fainter, into the gaps.
    dust = []; dustCv = null;
    const D = Math.min(4200, Math.max(600, Math.round(N * 0.9)));
    for (let i = 0; i < D; i++) {
      const u = 0.05 + 0.97 * Math.pow(rand(i * 3 + 101), 1.15);
      const gauss = rand(i * 5 + 102) + rand(i * 7 + 103) - 1;
      const loose = rand(i * 11 + 104) < 0.12;
      const th = galArmAngle(i % GAL_ARMS, u) + (rand(i * 29 + 109) - 0.5) * 0.3 + (loose ? (rand(i * 13 + 105) - 0.5) * 1.9 : 0);
      const rr = (u + gauss * armW(u) * 1.5) * GAL_R * 1.02;
      const roll = rand(i * 17 + 106), spark = rand(i * 23 + 108) > 0.97 - 0.05 * u;
      dust.push({
        x: Math.cos(th) * rr, y: Math.sin(th) * rr,
        a: Math.min(1, (0.16 + rand(i * 19 + 107) * 0.45) * (1.15 - 0.55 * u) * (loose ? 0.5 : 1) * (spark ? 1.8 : 1)), // a canvas ignores an alpha over 1
        s: spark ? 1.6 : 1,
        c: roll > 0.86 ? "#f4a3c8" : roll > 0.62 ? "#ffe6c7" : "#9fe3f2",
      });
    }
    // the bulge: a dense knot of warm stars round the sun, thickest at the centre, that
    // the arms grow out of (not one bright dot with a dark ring round it)
    const B = Math.min(1600, Math.max(300, Math.round(N * 0.3)));
    for (let i = 0; i < B; i++) {
      const u = -0.06 * Math.log(1 - 0.985 * rand(i * 3 + 201)), a = rand(i * 5 + 202) * TAU, roll = rand(i * 7 + 203);
      dust.push({
        x: Math.cos(a) * u * GAL_R, y: Math.sin(a) * u * GAL_R * 0.9,
        a: (0.36 + rand(i * 11 + 204) * 0.5) * Math.max(0.3, 1 - u * 3.5),
        s: rand(i * 13 + 205) > 0.9 ? 1.6 : 1,
        c: roll > 0.8 ? "#ffd0dc" : roll > 0.35 ? "#ffe9cf" : "#fff7ee",
      });
    }
  }
  // The dust turns rigidly with the disc, so it is baked once (per layout, viewport and
  // dpr) into a world-space image and drawn as one rotated drawImage, not ~4k fillRects
  // a frame. dustK (image px per world unit) is set so that at the galaxy's fitted zoom
  // one image px is one device px, so the grains match the old per-grain draw there;
  // the image is capped at 4096px, which only an 8K-class HiDPI window reaches. A resize
  // keeps drawing the old bake (it is in world units, so still in place) and rebakes once
  // the window has held still, not on every frame of a window-edge drag.
  let dustCv = null, dustK = 1, dustStale = 0;
  function buildDust() {
    dustStale = 0;
    const RD = GAL_R * 1.16 + 4; // the outermost grain: (u 1.02 + its arm scatter 0.11) x 1.02
    // the galaxy fit frames its 98th percentile radius, about GAL_R (see computeFit: a
    // touch under on a big vault, a touch over on some small ones), so about 1:1
    const zf = Math.min(1.1, Math.max(60, Math.min(W - inset - 120, H - 150)) / (2 * GAL_R));
    dustK = Math.min(zf * dpr, 4096 / (2 * RD));
    const S = Math.ceil(2 * RD * dustK), c = S / 2, px = dustK / zf; // image px per css px at the fit
    const cv = document.createElement("canvas");
    cv.width = cv.height = S;
    const g = cv.getContext("2d");
    g.globalCompositeOperation = "lighter"; // grains add, as they did drawn straight onto the frame
    for (const d of dust) {
      g.globalAlpha = d.a; g.fillStyle = d.c;
      g.fillRect(c + d.x * dustK, c + d.y * dustK, d.s * px, d.s * px);
    }
    dustCv = cv;
  }
  function rot(x, y, a) { const c = Math.cos(a), s = Math.sin(a); return { x: x * c - y * s, y: x * s + y * c }; }
  function galaxyPos(n) { return rot(n.gal0x || 0, n.gal0y || 0, galaxyAngle); }
  // Every screen-space consumer (draw, picking via sp, camFollow, the fit, search) reads
  // the force layout through the swirl, so what you click is what is drawn.
  function pos(n) { return view === "galaxy" ? galaxyPos(n) : rot(n.gx, n.gy, swirl); }

  // ---- force sim (graph view): Barnes-Hut repulsion + hub springs + a weak web ----
  // Floaty islands with Obsidian's structure inside. Each note hangs off its hub (its
  // most-linked neighbour in its group, see buildGroupIndex) on a strong spring, so a
  // hub's notes gather round it in a star burst: a leaf rests somewhere across the
  // burst, whose radius grows with sqrt of its notes, and a hub rests clear of its
  // parent's burst. Every other link in a group is a weak spring over its lighter end's
  // degree, the fine web inside an island; a bridge between groups is weaker still and
  // longer, so it ties islands together without closing the dark between them. A pull to
  // each group's centroid (between the faint one of a single web and the old uniform-
  // puffball one) makes each group its own island. A spring pulls both ends alike and
  // each moves by the pull over its mass (its charge), so a 300-link hub holds still
  // while its notes come to it. Repulsion grows with a note's links, so neighbouring
  // bursts push apart, and runs on a quadtree (theta 0.9): every note feels the whole
  // graph at O(N log N), with no cell-border force jumps (a cell grid froze ~4.7k notes
  // into a visible lattice). A gentle pull to the centre keeps the whole a disc. The
  // pools are reused every tick.
  const THETA2 = 0.81, REP = 1.6, SOFT2 = (SPACING * 0.3) ** 2;
  // centre gravity, group cohesion, a loner's pull home, the hub / web / bridge springs,
  // a bridge's rest length (x a web link's) and its draw opacity (x WEB_A)
  const GRAV = 0.0014, COH = 0.001, PULL = 0.02, K_TREE = 0.5, K_LINK = 0.03, K_BRIDGE = 0.04;
  const BRIDGE_L = 2.2, BRIDGE_A = 0.35, REGROUP_COH = 8;
  // charge per link, burst radius per sqrt(note) and a leaf's least rest length (both in
  // SPACING), the loners' home band (in web radii)
  const CHARGE = 0.15, BURST = 0.45, LEAF_L = 0.4, HOME_R0 = 0.25, HOME_DR = 0.8;
  // the cooling floor (see simStep's sleep) and the per-step velocity damping
  const ALPHA_MIN = 0.015, DAMP = 0.86;
  // the force view's link web opacity, and a note's link to its hub (x WEB_A): the spokes
  const WEB_A = 0.3, SPOKE_A = 2.3;
  const TINY = 4; // groups this small make no island: they keep to homes across the disc
  let qCap = 0, qN = 0, qCx, qCy, qHalf, qM, qX, qY, qBody, qKid;
  const qStack = new Int32Array(8192);
  let gSize = null, deg = null, chg = null, par = null, kids = null, loner = null, eK = null, eL = null;
  // a regroup's cohesion boost, easing back to 1: notes of a new group start spread over
  // the old islands, and at the resting cohesion they stay tangled in one web
  let cohK = 1;
  // a grab's local warmth: per-note weight (warmOn) times warmA, cooling as alpha does
  let warmW = null, warmA = 0;
  // the dropped note's 30-step window (see simStep): its gap to where it was grabbed at
  // the window's start (settleD), or, with no grab point, its position then
  let settleX = 0, settleY = 0, settleK = 0, settleHX = NaN, settleHY = NaN, settleD = 0;
  let kidAt0 = null, kidAt = null; // each hub's notes: kidAt[kidAt0[p] .. kidAt0[p + 1]]
  function qGrow() {
    const cap = qCap ? qCap * 2 : 16384;
    const f = (old) => { const a = new Float64Array(cap); if (old) a.set(old); return a; };
    qCx = f(qCx); qCy = f(qCy); qHalf = f(qHalf); qM = f(qM); qX = f(qX); qY = f(qY);
    const b = new Int32Array(cap); if (qBody) b.set(qBody); qBody = b;
    const k = new Int32Array(cap * 4); if (qKid) k.set(qKid); qKid = k;
    qCap = cap;
  }
  function qNew(cx, cy, half) {
    if (qN >= qCap) qGrow();
    const q = qN++;
    qCx[q] = cx; qCy[q] = cy; qHalf[q] = half; qM[q] = 0; qX[q] = 0; qY[q] = 0; qBody[q] = -1;
    qKid[q * 4] = qKid[q * 4 + 1] = qKid[q * 4 + 2] = qKid[q * 4 + 3] = -1;
    return q;
  }
  function qKidFor(q, x, y) {
    const k = (x >= qCx[q] ? 1 : 0) | (y >= qCy[q] ? 2 : 0);
    let c = qKid[q * 4 + k];
    if (c < 0) {
      const h = qHalf[q] / 2;
      c = qNew(qCx[q] + (k & 1 ? h : -h), qCy[q] + (k & 2 ? h : -h), h);
      qKid[q * 4 + k] = c; // qNew may have grown the pools, so index the live array
    }
    return c;
  }
  // body states: -1 empty leaf, >=0 leaf holding that body, -2 internal
  // a cell's mass is its notes' total charge, its centre their charge-weighted centre
  function qInsert(b, bx, by, w) {
    let q = 0;
    for (let depth = 0; ; depth++) {
      qM[q] += w; qX[q] += bx * w; qY[q] += by * w;
      const s = qBody[q];
      if (s === -1) { qBody[q] = b; return; }
      if (s >= 0) {
        if (depth > 40) return; // coincident pile: the aggregate keeps its mass
        qBody[q] = -2;
        const o = G.nodes[s], c = qKidFor(q, o.gx, o.gy), ws = chg[s];
        qM[c] += ws; qX[c] += o.gx * ws; qY[c] += o.gy * ws; qBody[c] = s;
      }
      q = qKidFor(q, bx, by);
    }
  }
  function simStep() {
    const nodes = G.nodes, N = nodes.length;
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    for (const v of nodes) { if (v.gx < x0) x0 = v.gx; if (v.gx > x1) x1 = v.gx; if (v.gy < y0) y0 = v.gy; if (v.gy > y1) y1 = v.gy; }
    qN = 0;
    qNew((x0 + x1) / 2, (y0 + y1) / 2, Math.max(x1 - x0, y1 - y0) / 2 + 1);
    for (let i = 0; i < N; i++) qInsert(i, nodes[i].gx, nodes[i].gy, chg[i]);

    gSx.fill(0); gSy.fill(0); gN.fill(0);
    let r2 = 0, rn = 0;
    for (let i = 0; i < N; i++) {
      const s = nodeSlot[i], v = nodes[i];
      gSx[s] += v.gx; gSy[s] += v.gy; gN[s]++;
      if (!loner[i]) { r2 += v.gx * v.gx + v.gy * v.gy; rn++; }
    }
    // Loners feel only repulsion and the centre, so they would be shoved out to one hard
    // ring at the balance point. Each keeps to a fixed home instead, in units of the map's
    // live radius (its rms radius is ~0.7 of its edge), as stardust between the islands.
    const R = Math.sqrt(r2 / Math.max(1, rn)) * 1.35;

    for (let i = 0; i < N; i++) {
      const v = nodes[i], x = v.gx, y = v.gy;
      let fx = 0, fy = 0, top = 0;
      qStack[top++] = 0;
      while (top) {
        const q = qStack[--top], m = qM[q];
        if (!m) continue;
        const dx = x - qX[q] / m, dy = y - qY[q] / m, d2 = dx * dx + dy * dy;
        const w = qHalf[q] * 2;
        if (qBody[q] !== -2 || w * w < THETA2 * d2) {
          if (qBody[q] === i) continue;
          const f = REP * m / (d2 + SOFT2);
          fx += dx * f; fy += dy * f;
        } else {
          for (let k = 0; k < 4; k++) { const c = qKid[q * 4 + k]; if (c >= 0 && top < qStack.length) qStack[top++] = c; }
        }
      }
      if (loner[i]) {
        v.fx = fx + (v.homeX * R - x) * PULL;
        v.fy = fy + (v.homeY * R - y) * PULL;
      } else {
        const s = nodeSlot[i], n = gN[s], coh = COH * cohK;
        v.fx = fx + (gSx[s] / n - x) * coh - x * GRAV;
        v.fy = fy + (gSy[s] / n - y) * coh - y * GRAV;
      }
    }
    // springs (per-link strength and rest length: buildGroupIndex). Each end moves by the
    // pull over its mass, so a heavy hub barely moves and its notes come to it.
    for (let j = 0, e = 0; j < edgeIdx.length; j += 2, e++) {
      const ai = edgeIdx[j], bi = edgeIdx[j + 1], a = nodes[ai], b = nodes[bi];
      const dx = b.gx - a.gx, dy = b.gy - a.gy, d = Math.sqrt(dx * dx + dy * dy) || 1;
      const f = (d - eL[e]) / d * eK[e], sa = f / chg[ai], sb = f / chg[bi];
      a.fx += dx * sa; a.fy += dy * sa; b.fx -= dx * sb; b.fy -= dy * sb;
    }
    // the slow swirl is not a force: it is a render-time rigid turn (swirl, in loop), so
    // a settled layout is truly still and the sim can sleep
    const damp = DAMP;
    let vSum = 0, vMax = 0;
    for (let i = 0; i < N; i++) {
      const v = nodes[i];
      if (drag && v === drag.node) continue;
      if (v === indexNode) { // pinned at the origin like the galaxy sun; a drag springs back
        v.gx *= 0.8; v.gy *= 0.8; v.vx = 0; v.vy = 0;
        if (v.gx * v.gx + v.gy * v.gy < 0.01) { v.gx = 0; v.gy = 0; }
        else vMax = Infinity; // still springing home: no sleep, or it would stay off the origin
        continue;
      }
      const a = warmW && warmW[i] ? alpha + warmW[i] * warmA : alpha;
      let vx = (v.vx + v.fx * a) * damp;
      let vy = (v.vy + v.fy * a) * damp;
      let sp2 = vx * vx + vy * vy;
      if (sp2 > 3600) { const k = 60 / Math.sqrt(sp2); vx *= k; vy *= k; sp2 = 3600; }
      v.vx = vx; v.vy = vy; v.gx += vx; v.gy += vy;
      const s = Math.sqrt(sp2); vSum += s; if (s > vMax) vMax = s;
    }
    if (settle && --settleT <= 0) settle = null; // one dropped note never holds the map awake for long
    // a dropped note is back once it stops closing on where it was grabbed (under half a
    // world unit closer over a 30-step window), or with no grab point once it all but stops:
    // its own convergence. Its leftover force says nothing (half the map sleeps over 0.3),
    // and nor does a slow creep at the floor, which held drops to the 900-step cap.
    if (settle && ++settleK % 30 === 0) {
      if (settleHX === settleHX) {
        const d = Math.hypot(settle.gx - settleHX, settle.gy - settleHY);
        if (settleD - d < 0.5) settle = null; else settleD = d;
      } else if (Math.hypot(settle.gx - settleX, settle.gy - settleY) < 0.5) settle = null;
      else { settleX = settle.gx; settleY = settle.gy; }
    }
    if (warmW && (warmA *= 0.993) < ALPHA_MIN) warmW = null;
    cohK = 1 + (cohK - 1) * 0.995;
    // at the floor it anneals on, slowly, to a third of it (nothing held): a few rim notes
    // pushed outward for tens of seconds then freeze rather than hold the sim awake (a
    // late creep kept it up 34 s). A dropped note's own warmth carries it back meanwhile.
    // wake() lifts it back above the floor.
    if (alpha <= ALPHA_MIN && !drag) alpha = Math.max(ALPHA_MIN * 0.3, alpha * 0.998);
    if (alpha > ALPHA_MIN) alpha *= 0.993; // cools to a floor
    // cooled and still for a whole second: sleep, skipping the Barnes-Hut pass that is
    // most of an idle frame. The max check keeps it awake while a flung note is still
    // moving (one note barely moves the mean), and a grab's warmth or a dropped note on
    // its way back hold it too. wake() restarts it. The floor is low (ALPHA_MIN, annealing
    // on below it): a link web relaxes slowly at its rim, and at 0.04 a few outer notes
    // still crept past the still line after 50 s. Still is judged on screen (under ~0.5 px
    // a second on average, ~3 at most) but never stricter than 0.05 / 0.2 world units a
    // step, so a fitted map sleeps rather than waiting out its last sub-pixel crawl.
    // Zoomed in past ~0.15 that floor is the gate (at zoom 1 about 3 px/s mean, 12 at
    // most): a close-up can stop mid-crawl rather than burn Barnes-Hut passes on the map's
    // slow relaxation, mostly off screen. The map sleeps with some force left (Barnes-Hut
    // never sums to zero), which is why a grab only warms its own neighbourhood (warmOn).
    else if (drag || warmW || settle || vSum / N >= Math.max(0.05, 0.0075 / cam.zoom) || vMax >= Math.max(0.2, 0.045 / cam.zoom)) quiet = 0;
    else if (++quiet >= 60) { simAwake = false; settle = null; }
  }
  // wake the force sim with at least this much energy (a real grab, a drop, a regroup, a
  // return to the view)
  function wake(a) { alpha = Math.max(alpha, a); simAwake = true; quiet = 0; }
  // hold the sim awake until this dropped note is back (home: where it was grabbed), for
  // at most 5 s (its warmth has cooled by then)
  function settleOn(n, home) {
    settle = n; settleT = 300; settleK = 0; settleX = n.gx; settleY = n.gy;
    const h = home && typeof home === "object" ? home : null;
    settleHX = h ? h.x : NaN; settleHY = h ? h.y : NaN;
    settleD = h ? Math.hypot(n.gx - h.x, n.gy - h.y) : 0;
  }
  // A grab warms the grabbed note's own neighbourhood, not the whole map: its burst (every
  // note hanging off it, down the forest), its hub, and its links two hops out in its
  // group, up to WARM_CAP notes. The rest keeps the energy it slept with: a whole-map
  // wake let every note's leftover force out at once, so each tug moved the whole map.
  const WARM_CAP = 1500;
  function warmOn(root) {
    const w = new Float32Array(G.nodes.length), r = nodeIndex.get(root.id), g = nodeSlot[r], q = [r];
    w[r] = 1;
    for (let h = 0; h < q.length && q.length < WARM_CAP; h++) {
      for (let k = kidAt0[q[h]]; k < kidAt0[q[h] + 1]; k++) { const c = kidAt[k]; if (!w[c]) { w[c] = 1; q.push(c); } }
    }
    if (par[r] >= 0 && !w[par[r]]) w[par[r]] = 0.8;
    let ring = [r], n = q.length;
    for (const wt of [0.8, 0.4]) {
      const next = [];
      for (const a of ring) for (const id of adj.get(G.nodes[a].id) || []) {
        const b = nodeIndex.get(id);
        if (nodeSlot[b] !== g || w[b] || n >= WARM_CAP) continue;
        w[b] = wt; next.push(b); n++;
      }
      ring = next;
    }
    return w;
  }
  function warm(n, a) { warmW = warmOn(n); warmA = Math.max(warmA, a); simAwake = true; quiet = 0; }

  // ---- render ----
  let lastFrame = 0, simAcc = 0;
  function loop() {
    if (document.hidden) { running = false; return; } // pause when the tab is hidden (battery)
    // a feature panel hides both canvases: draw and step nothing behind it, but keep the
    // loop ticking (one class check a frame) so the view is live again the moment it closes.
    // lastFrame resets so that first frame steps as one, not as the whole time away.
    if (document.body.classList.contains("panel-active")) { lastFrame = 0; requestAnimationFrame(loop); return; }
    // step by real time in 60 Hz frames (fk = 1 at 60 Hz, as every rate below was tuned),
    // so a 120 Hz display does not turn, cool or glide at double speed; clamped to 0.1 s
    // so a stall or a return from the 3D view does not leap (the same k as gl3d.frame)
    const tn = now(), fk = lastFrame ? Math.min(6, Math.max(0, (tn - lastFrame) * 0.06)) : 1;
    lastFrame = tn;
    if (view === "galaxy3d") { if (gl3d) gl3d.frame(); requestAnimationFrame(loop); return; } // 3D owns its own render; skip the 2D sim+draw
    t += fk;
    clock = tn / 1000;
    if (!camFollow) { // the layout freezes while locked onto a note, so it holds still
      const idle = (tn - lastInteract) > 4000;
      if (view === "graph") {
        // slow rigid swirl, ~5.4 min a revolution (the rate the old in-sim swirl settled
        // at), eased when idle. Rigid: an inner-faster shear tears the bursts apart. It
        // falls: counterclockwise on screen, the way the galaxy turns
        if (!calm()) swirl -= (idle ? 0.00016 : 0.00032) * fk;
        // a held note stays under the cursor: its world point is fixed, so as the swirl
        // turns, re-pin it in the unturned frame the sim works in (the sim skips it)
        if (drag) { const g = rot(drag.wx, drag.wy, -swirl); drag.node.gx = g.x; drag.node.gy = g.y; }
        // the sim's forces, damping and cooling are per step, so it steps at 60 Hz whatever
        // the display runs at (every other frame at 120 Hz). At most one step a frame, so
        // a slow frame never piles more Barnes-Hut work onto itself.
        simAcc += fk;
        if (simAcc >= 0.5) { simAcc = Math.max(-0.5, Math.min(0.5, simAcc - 1)); if (simAwake) simStep(); }
      } else {
        // rigid turn, ~3.5 min a revolution, easing when idle. It falls: on screen (y down)
        // that is counterclockwise, against the arms' clockwise outward wind, so they trail
        if (!calm()) galaxyAngle -= (idle ? 0.00025 : 0.0005) * fk;
        const back = Math.pow(0.86, fk), pull = 1 - Math.pow(0.68, fk);
        const decay = (n) => {
          if (n.dispX || n.dispY) {
            n.dispX *= back; n.dispY *= back;
            if (Math.abs(n.dispX) < 0.5 && Math.abs(n.dispY) < 0.5) { n.dispX = 0; n.dispY = 0; }
          }
        };
        if (drag && drag.influence) {
          // elastic pull: the held node + its neighborhood spring toward the cursor
          // (1-hop at 0.55, 2-hop at 0.22), the rest decay back, all keep orbiting.
          const o = galaxyPos(drag.node), tgtX = drag.wx - o.x, tgtY = drag.wy - o.y;
          for (const n of G.nodes) {
            const w = drag.influence.get(n.id);
            if (w) { n.dispX += (w * tgtX - n.dispX) * pull; n.dispY += (w * tgtY - n.dispY) * pull; }
            else decay(n);
          }
        } else for (const n of G.nodes) decay(n);
      }
    }
    // fly-into-node lock-on / ease-back-out (reduced motion: straight there, as in 3D)
    if (camFollow) {
      const p = pos(camFollow), g = calm() ? 1 : 1 - Math.pow(0.88, fk);
      cam.x += (p.x - cam.x) * g; cam.y += (p.y - cam.y) * g;
      cam.zoom += (FOCUS_ZOOM - cam.zoom) * g;
    } else if (camReturn) {
      const g = calm() ? 1 : 1 - Math.pow(0.9, fk);
      cam.x += (camReturn.x - cam.x) * g; cam.y += (camReturn.y - cam.y) * g;
      cam.zoom += (camReturn.zoom - cam.zoom) * g;
      if (Math.hypot(cam.x - camReturn.x, cam.y - camReturn.y) < 2 && Math.abs(cam.zoom - camReturn.zoom) < 0.02) camReturn = null;
    } else if (autoFit) {
      // the fit is O(N log N) (percentile sort), so retarget every 10 frames' worth of
      // time and glide between; zoom eases in log space so a big refit is as smooth as a
      // small one. Reduced motion eases too: the layout still blooms under it, and a jump
      // on every retarget reads as the zoom pulsing.
      if ((fitTick -= fk) <= 0) { computeFit(); fitTick = 10; }
      const k = 1 - Math.pow(0.94, fk);
      cam.x += (fit.x - cam.x) * k; cam.y += (fit.y - cam.y) * k;
      cam.zoom *= Math.pow(fit.zoom / cam.zoom, k);
    }
    draw();
    requestAnimationFrame(loop);
  }

  function draw() {
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.fillStyle = "#050409";
    ctx.fillRect(0, 0, W, H);
    const z = cam.zoom, ox = CX - cam.x * z, oy = H / 2 - cam.y * z;

    drawStars(z); // open deep-space starfield in BOTH views (no framed floor disc)

    const nodes = G.nodes, N = nodes.length, galaxy = view === "galaxy";
    const drift = calm() ? 0 : 1, tc = clock;
    const ang = galaxy ? galaxyAngle : swirl, ca = Math.cos(ang), sa = Math.sin(ang);
    gSx.fill(0); gSy.fill(0); gSxx.fill(0); gN.fill(0);
    for (let i = 0; i < N; i++) {
      const n = nodes[i];
      let px, py; // inline pos() to avoid N object allocations per frame
      if (galaxy) {
        const x = n.gal0x || 0, y = n.gal0y || 0;
        px = x * ca - y * sa + (n.dispX || 0); py = x * sa + y * ca + (n.dispY || 0); // + grab/spring-back offset
      } else { px = n.gx * ca - n.gy * sa; py = n.gx * sa + n.gy * ca; }
      const s = sp[i] || (sp[i] = { x: 0, y: 0 });
      const fa = n.famp * drift;
      s.x = px * z + ox + fa * (Math.sin(tc * n.ff1 + n.fp1) + 0.45 * Math.sin(tc * n.ff2 * 1.9 + n.fp2));
      s.y = py * z + oy + fa * (Math.cos(tc * n.ff2 + n.fp2) + 0.45 * Math.cos(tc * n.ff1 * 1.7 + n.fp1));
      const k = nodeSlot[i];
      gSx[k] += s.x; gSy[k] += s.y; gSxx[k] += s.x * s.x + s.y * s.y; gN[k]++;
    }
    const on = (s, m) => s.x >= -m && s.x <= W + m && s.y >= -m && s.y <= H + m;

    ctx.globalCompositeOperation = "lighter";
    // galaxy core + dust: the disc between the notes
    if (galaxy) {
      const c = { x: ox, y: oy }, pulse = 1 + 0.03 * drift * Math.sin(clock * 0.9);
      blit(galCore, c, GAL_R * z * 0.62 * pulse, 0.55);
      if (!dustCv || (dustStale && now() - dustStale > 250)) buildDust();
      // zoomed well in, the baked grains would smear into soft blobs: fade them out
      // instead (at that zoom the old 1px grains were already sparse and faint)
      const k = z / dustK, h = dustCv.width / 2, fade = Math.min(1, (4 - k * dpr) / 2.5);
      if (fade > 0) {
        ctx.save();
        ctx.translate(ox, oy); ctx.rotate(galaxyAngle); ctx.scale(k, k);
        // zoomed out, the default filter samples a few of every grain's image px and the
        // grains shimmer in and out as the disc turns; the high one averages them. Only
        // when well shrunk: from a little under 1:1 up (the fitted view among them, which
        // sits a hair either side of 1:1 by vault) the plain filter reads every grain, and
        // the high one there cost 30-40 frames over 50 ms in 4 s
        ctx.imageSmoothingQuality = k * dpr < 0.9 ? "high" : "low";
        ctx.globalAlpha = fade; ctx.drawImage(dustCv, -h, -h);
        ctx.restore();
      }
    }
    // nebulae: one soft colour cloud per group, sized by the group's on-screen spread
    const focusDim = neighborSet ? 0.35 : 1;
    for (let k = 0; k < gCount; k++) {
      const m = gN[k];
      if (m < 5 || !nebOn[k]) continue;
      const mx = gSx[k] / m, my = gSy[k] / m;
      const spread = Math.sqrt(Math.max(1, gSxx[k] / m - mx * mx - my * my));
      const rad = Math.min(Math.max(W, H) * 0.7, Math.max(28, spread * 1.7));
      let a = Math.min(0.34, 0.07 + 0.034 * Math.log(m)) * focusDim;
      if (spotlight != null) a = gSlot.get(spotlight) === k ? a * 1.6 : a * 0.15;
      if (galaxy) a *= 0.7;
      const c = { x: mx, y: my };
      if (on(c, rad)) blit(nebSprites.get(gColors[k]), c, rad, a);
    }
    ctx.globalCompositeOperation = "source-over";

    // edges: inside a group they carry the group's hue, bridges stay a cool neutral;
    // one path per bucket. The focused subgraph lights up bright on top.
    const emph = [];
    ctx.lineWidth = Math.max(0.5, 0.65 * z);
    for (const bk of edgeBuckets) {
      ctx.setLineDash(bk.membership ? [3, 4] : []);
      ctx.strokeStyle = galaxy ? bk.stroke : bk.web;
      ctx.globalAlpha = neighborSet ? 0.45 : (spotlight != null && gSlot.get(spotlight) !== bk.slot ? 0.25 : 1);
      ctx.beginPath();
      const L = bk.pairs;
      for (let j = 0; j < L.length; j += 2) {
        const a = sp[L[j]], b = sp[L[j + 1]];
        if (!on(a, 40) && !on(b, 40)) continue;
        if (neighborSet && (isFocus(nodes[L[j]].id) || isFocus(nodes[L[j + 1]].id))) { emph.push(a, b); continue; }
        if (galaxy) curve(a, b); else { ctx.moveTo(a.x, a.y); ctx.lineTo(b.x, b.y); } // straight spokes read as a burst
      }
      ctx.stroke();
    }
    ctx.setLineDash([]);
    ctx.globalAlpha = 1;
    if (emph.length) {
      ctx.strokeStyle = "rgba(245,242,236,0.55)"; ctx.lineWidth = Math.max(0.9, 1.2 * z); ctx.beginPath();
      for (let i = 0; i < emph.length; i += 2) curve(emph[i], emph[i + 1]);
      ctx.stroke();
    }

    // glow halos (additive, low-deposit so a dense cluster saturates to a hue, not white)
    ctx.globalCompositeOperation = "lighter";
    for (let i = 0; i < N; i++) {
      const n = nodes[i], s = sp[i];
      if (!on(s, 80)) continue;
      const dim = dimmed(n);
      const core = dotR(n, z);
      if (n.id === G.meta.index_id) {
        const pulse = 1 + 0.035 * drift * Math.sin(t * 0.02);
        blit(sunSprite, s, Math.max(26, core * 5.5) * pulse, dim ? 0.3 : 0.92); // the sun glow
      } else {
        const halo = Math.max(4 + 5 * n.depth, core * (2.5 + Math.min(1.6, n.degree * 0.022)));
        blit(sprites.get(groupColorOf(n)) || sprites.get("#7c766e"), s, halo, (dim ? 0.05 : 0.2 + 0.2 * n.depth));
      }
    }
    ctx.globalCompositeOperation = "source-over";

    // crisp cores: clean glowing dots, NO border ring. Depth sets brightness + size,
    // and a slow per-note shimmer keeps the field from reading as printed.
    for (let i = 0; i < N; i++) {
      const n = nodes[i], s = sp[i];
      if (!on(s, 10)) continue;
      const dim = dimmed(n);
      const sun = n.id === G.meta.index_id;
      const r = drawnR(n, z);
      const tw = drift ? 0.86 + 0.14 * Math.sin(tc * 1.3 + n.fp1 * 3) : 1;
      ctx.globalAlpha = dim ? 0.14 : (sun ? 1 : (galaxy ? 0.5 + 0.5 * n.depth : 0.68 + 0.32 * n.depth) * tw);
      ctx.fillStyle = sun ? "#fff6e8" : coreCol[i];
      ctx.beginPath(); ctx.arc(s.x, s.y, r, 0, TAU); ctx.fill();
    }
    ctx.globalAlpha = 1;

    if (vignette) { ctx.fillStyle = vignette; ctx.fillRect(0, 0, W, H); }

    // labels: focus first (always, decluttered), then a capped ambient set by
    // importance, gated by a zoom band. Never the old "every hub at once" soup.
    boxes.length = 0;
    const hoverId = hover && hover.id;
    for (let i = 0; i < N; i++) {
      const n = nodes[i], s = sp[i];
      if (!on(s, 10)) continue;
      if (n.id === hoverId || (selected && n.id === selected.id) || isFocus(n.id)) placeLabel(n, s, coreR(n, z), 1);
    }
    const band = z < 0.5 ? 0 : z < 1.4 ? 1 : 2;
    if (band > 0) {
      const cap = band === 1 ? 12 : 18;
      const a = 0.35 + 0.55 * Math.max(0, Math.min(1, (z - 0.5) / 1.0));
      let placed = 0;
      for (const idx of labelOrder) {
        if (placed >= cap) break;
        const n = nodes[idx], s = sp[idx];
        if (!s || !on(s, 10)) continue;
        if (dimmed(n)) continue;
        if (n.id === hoverId || (selected && n.id === selected.id) || isFocus(n.id)) continue;
        if (placeLabel(n, s, coreR(n, z), a)) placed++;
      }
    }
  }

  function coreR(n, z) { return n.id === G.meta.index_id ? Math.max(2, dotR(n, z)) * 1.6 : Math.max(2, dotR(n, z)); }
  // a dot as drawn: coreR, a non-sun dot sized by its depth (0.8 to 1.15x). Drawing and
  // picking (nodeAt) both read it, so the dot you see is the dot you hit, the sun's rim too
  function drawnR(n, z) { return coreR(n, z) * (n.id === G.meta.index_id ? 1 : 0.8 + 0.35 * n.depth); }
  // a note's radius on screen. Force view: never under a floor that grows with sqrt(links),
  // so a hub reads as a hub at the overview zoom (~0.2 at a few thousand notes), and with
  // sqrt(zoom), so dots keep pace with the spokes as you zoom in.
  function dotR(n, z) { const w = nodeRadius(n) * z; return view === "graph" ? Math.max(w, (1 + 0.55 * Math.sqrt(n.degree || 0)) * Math.sqrt(z / 0.2)) : w; }

  function measureLabel(n) {
    if (n._lw != null) return;
    ctx.font = "600 10.5px Geist, sans-serif";
    // cut to 80 chars (well past the 140px box) before measuring: trimming a huge title
    // one char at a time is O(L^2) and could freeze the tab
    const full = String(n.label || n.id || "");
    let txt = full.slice(0, 80);
    if (txt.length < full.length || ctx.measureText(txt).width > 140) {
      while (txt.length > 1 && ctx.measureText(txt + "…").width > 140) txt = txt.slice(0, -1);
      txt += "…";
    }
    n._ltxt = txt; n._lw = Math.min(140, ctx.measureText(txt).width);
  }
  function placeLabel(n, s, r, alpha) {
    measureLabel(n);
    const x = s.x + r + 4, y = s.y - 7, w = n._lw + 10, h = 14;
    for (const b of boxes) if (b.x < x + w && x < b.x + b.w && b.y < y + h && y < b.y + b.h) return false;
    boxes.push({ x, y, w, h });
    ctx.globalAlpha = alpha;
    ctx.fillStyle = "rgba(10,9,8,0.72)";
    ctx.beginPath();
    if (ctx.roundRect) ctx.roundRect(x, y, w, h, 4); else ctx.rect(x, y, w, h);
    ctx.fill();
    ctx.strokeStyle = "rgba(255,255,255,0.06)"; ctx.lineWidth = 1; ctx.stroke();
    ctx.fillStyle = "#e9e4da"; ctx.font = "600 10.5px Geist, sans-serif"; ctx.textBaseline = "middle";
    ctx.fillText(n._ltxt, x + 5, y + h / 2);
    ctx.globalAlpha = 1;
    return true;
  }

  function curve(a, b) {
    const dx = b.x - a.x, dy = b.y - a.y, len = Math.hypot(dx, dy) || 1;
    const off = Math.min(len * 0.10, 18); // flatter bow so dense short edges stay near-straight
    const mx = (a.x + b.x) / 2 - (dy / len) * off, my = (a.y + b.y) / 2 + (dx / len) * off;
    ctx.moveTo(a.x, a.y);
    ctx.quadraticCurveTo(mx, my, b.x, b.y);
  }
  function blit(spr, s, size, a) {
    if (!spr || size <= 0) return;
    ctx.globalAlpha = a;
    ctx.drawImage(spr, s.x - size, s.y - size, size * 2, size * 2);
  }
  function drawStars(z) {
    ctx.fillStyle = "#cdd6f4";
    const px = -cam.x * z * 0.25 + W / 2, py = -cam.y * z * 0.25 + H / 2;
    for (const st of stars) {
      const x = ((st.x + px) % W + W) % W, y = ((st.y + py) % H + H) % H;
      ctx.globalAlpha = st.a;
      ctx.fillRect(x, y, st.r, st.r);
    }
    ctx.globalAlpha = 1;
  }
  function nodeRadius(n) { return Math.max(1.4, (n.size || 1) * 1.7); }
  function isFocus(id) { if (!neighborSet) return false; const f = selected || hover; return neighborSet.has(id) || (f && id === f.id); }
  // a node is dimmed by a text filter, a focus neighborhood, or a legend spotlight.
  function dimmed(n) { return (query && !matches(n)) || (neighborSet && !isFocus(n.id)) || (spotlight != null && groupKeyOf(n) !== spotlight); }
  function clearSpotlight() { if (spotlight == null) return; spotlight = null; if (gl3d) gl3d.setSpotlight(null); buildLegend(); }

  // ---- camera ----
  // The fit frames the graph around the index note (the centre piece): the camera sits
  // on the index and zooms so the content fits symmetrically on both sides. The extent
  // is the 98th percentile of the offsets, not the max, so one stray orphan does not
  // shrink everything. The galaxy is a turning disc, so it is framed on the 98th
  // percentile radius, which does not change as it turns (per axis, the zoom breathed
  // ~18% a turn). Falls back to the bbox centre when there is no index note.
  const fit = { x: 0, y: 0, zoom: 1 };
  let fitDX = null, fitDY = null; // reused offset buffers
  function computeFit() {
    const N = G.nodes.length;
    if (!fitDX || fitDX.length !== N) { fitDX = new Float64Array(N); fitDY = new Float64Array(N); }
    const ok = (p) => Number.isFinite(p.x) && Number.isFinite(p.y);
    let cx, cy;
    const ip = indexNode && pos(indexNode);
    if (ip && ok(ip)) { cx = ip.x; cy = ip.y; }
    else {
      let a = Infinity, b = Infinity, c = -Infinity, d = -Infinity;
      for (const n of G.nodes) {
        const p = pos(n);
        if (!ok(p)) continue;
        a = Math.min(a, p.x); b = Math.min(b, p.y); c = Math.max(c, p.x); d = Math.max(d, p.y);
      }
      if (!isFinite(a)) { fit.x = fit.y = 0; fit.zoom = 1; return; }
      cx = (a + c) / 2; cy = (b + d) / 2;
    }
    let m = 0;
    const disc = view === "galaxy";
    for (const n of G.nodes) {
      const p = pos(n);
      if (!ok(p)) continue;
      if (disc) fitDX[m] = Math.hypot(p.x - cx, p.y - cy);
      else { fitDX[m] = Math.abs(p.x - cx); fitDY[m] = Math.abs(p.y - cy); }
      m++;
    }
    const q = Math.min(m - 1, Math.floor(m * 0.98));
    const ex = m ? fitDX.subarray(0, m).sort()[q] : 0, ey = disc ? ex : m ? fitDY.subarray(0, m).sort()[q] : 0;
    const z = Math.min(1.1, Math.max(60, W - inset - 120) / (2 * ex || 1), Math.max(60, H - 150) / (2 * ey || 1));
    fit.x = cx; fit.y = cy; fit.zoom = Number.isFinite(z) && z > 0 ? z : 1;
    minZoom = Math.min(0.08, fit.zoom * 0.3); // zoom-out limit follows the graph's size
  }
  function fitView() { computeFit(); cam.x = fit.x; cam.y = fit.y; cam.zoom = fit.zoom; fitTick = 10; }
  // hand the camera back to the auto-fit (double-click empty space, "0", a bare Escape);
  // reduced motion jumps there, as the 3D recenter does
  function refit() { dropHover(); autoFit = true; fitTick = 0; camFollow = null; camReturn = null; if (calm()) fitView(); }
  // zoom about a screen point, clamped to the size-adaptive limits
  function zoomAt(z, sx, sy) {
    const w = worldAt(sx, sy);
    // a clamp never reverses the step: a zoom-out that starts below the floor (the fit
    // target jumped, say after a resize, before the glide caught up) holds, never zooms in
    cam.zoom = Math.max(Math.min(minZoom, cam.zoom), Math.min(Math.max(MAX_ZOOM, cam.zoom), z));
    cam.x = w.x - (sx - CX) / cam.zoom; cam.y = w.y - (sy - H / 2) / cam.zoom;
  }
  // one wheel reading for every view (gl3d gets it as opts.wheel): line and page deltas
  // become px, and shift+wheel on a plain mouse, which can arrive as deltaY, becomes
  // sideways. A mostly sideways swipe (two-finger trackpad, shift+wheel) pans; the
  // vertical wheel and pinch (ctrlKey) zoom.
  function wheelIntent(e) {
    const u = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? H : 1;
    const sh = e.shiftKey && !e.ctrlKey && !e.deltaX;
    const dx = (sh ? e.deltaY : e.deltaX) * u, dy = (sh ? 0 : e.deltaY) * u;
    return { pan: !e.ctrlKey && Math.abs(dx) > Math.abs(dy), dx, dy };
  }

  // slop is the hit margin past the dot: generous for hover + click, tight for the
  // press that decides grab vs pan, so a drag across a dense cluster pans the view
  // unless it starts right on a dot. A tap passes TAP_R instead: the hit radius for a
  // dot of a given on-screen radius. prefer: a note that wins while it is still within reach
  // (the one whose card shows), else the nearest does.
  function nodeAt(sx, sy, slop = 6, prefer = null) {
    let best = null, bestD = Infinity;
    for (let i = 0; i < G.nodes.length; i++) {
      const n = G.nodes[i];
      if (query && !matches(n)) continue;
      const s = sp[i]; if (!s) continue;
      const dot = drawnR(n, cam.zoom), r = typeof slop === "function" ? slop(dot) : dot + slop;
      const dd = (s.x - sx) ** 2 + (s.y - sy) ** 2;
      if (dd <= r * r && n === prefer) return n;
      if (dd <= r * r && dd < bestD) { best = n; bestD = dd; }
    }
    return best;
  }
  function worldAt(sx, sy) { return { x: (sx - CX) / cam.zoom + cam.x, y: (sy - H / 2) / cam.zoom + cam.y }; }

  // ---- card + focus ----
  function setFocus(n) { neighborSet = n ? new Set(adj.get(n.id) || []) : null; }
  function showCard(n) {
    const card = $("card");
    const color = groupColorOf(n);
    const comm = activeGroups().find((c) => c.id === groupKeyOf(n)) || {};
    // each path segment URL-encoded, so a #, ? or %xx in a name opens that file, not a
    // fragment or a decoded neighbour; the line is a positive integer whatever the graph says
    const editor = "vscode://file/" + joinPath(G.meta.vault, n.path).split("/").map(encodeURIComponent).join("/") + ":" + Math.max(1, n.line | 0);
    const tags = (n.tags || []).map((x) => `<span class="chip">#${esc(x)}</span>`).join("");
    const collections = (n.collections || []).filter((id) => byId.has(id)).map((id) =>
      `<button class="btn ghost collection-link" data-id="${esc(id)}">${esc(byId.get(id).label || id)}</button>`).join("");
    card.innerHTML = `
      <h2><span class="swatch" style="background:${esc(color)}"></span>${esc(n.label || n.id)}</h2>
      <div class="path">${esc(n.path)}</div>
      <div class="meta">
        ${n.type ? `<span><span class="chip">${esc(n.type)}</span></span>` : ""}
        <span>links <b>${n.degree | 0}</b></span>
        <span>${grouping === "domain" ? "topic" : "cluster"} <b>${esc(comm.label || ("#" + groupKeyOf(n)))}</b></span>
        <span>orbit <b>${n.orbit | 0}</b></span>
      </div>
      ${tags ? `<div class="tags">${tags}</div>` : ""}
      ${collections ? `<div class="tags">Collections ${collections}</div>` : ""}
      <div class="actions">
        <button class="btn" id="read">Read note</button>
        <button class="btn ghost" id="copy">copy path</button>
        <a class="btn ghost" href="${esc(editor)}" title="Open in your editor (only works if the vault is open locally)">editor</a>
      </div>`;
    // a hover preview lets the pointer through: shown over the note under it, it would
    // swallow that note's press (and its pointerleave would hide it again). A selected
    // note's card takes the pointer, for its buttons.
    card.classList.toggle("peek", n !== selected);
    card.classList.remove("hidden");
    const rd = $("read");
    if (rd) rd.onclick = () => window.Mesh && Mesh.openNote && Mesh.openNote(n.id);
    card.querySelectorAll(".collection-link").forEach((button) => {
      button.onclick = () => { focusNodeById(button.dataset.id); if (Mesh.openNote) Mesh.openNote(button.dataset.id); };
    });
    const cp = $("copy");
    if (cp) cp.onclick = () => navigator.clipboard && navigator.clipboard.writeText(joinPath(G.meta.vault, n.path));
  }
  function hideCard() { if (!selected) $("card").classList.add("hidden"); }
  // the content moved under a still pointer (a wheel, pan, key, refit, fly, view switch
  // or regroup): the note the hover named is no longer what is under it, so let it go
  // with its peek card (a selected note's card stays), in both views: gl3d's own goes
  // through its onHover. The next move picks afresh, so a click never opens a note
  // other than the one the card names.
  function dropHover() {
    if (gl3d) gl3d.clearHover();
    if (!hover) return;
    hover = null; canvas.classList.remove("hovering");
    if (!selected) { setFocus(null); hideCard(); }
  }

  // ---- search ----
  function matches(n) { return !query || (n.label || "").toLowerCase().includes(query) || (n.path || "").toLowerCase().includes(query); }
  function runSearch(q) {
    query = q.trim().toLowerCase();
    dropHover(); // the filter changes what can be picked, and a match moves the camera
    // a match takes the camera: a lock-on or a glide back out would pull it away again
    if (query) { const f = G.nodes.find(matches); if (f) { const p = pos(f); cam.x = p.x; cam.y = p.y; cam.zoom = Math.max(cam.zoom, 1.6); autoFit = false; camFollow = null; camReturn = null; } }
  }

  // a Map, not an object literal: a key named "constructor" must not find a prototype member
  const ARROWS = new Map([["ArrowLeft", [-1, 0]], ["ArrowRight", [1, 0]], ["ArrowUp", [0, -1]], ["ArrowDown", [0, 1]]]);

  // ---- interaction (grab + fling a node, pan empty space, pinch, zoom, hover, click) ----
  // Pointer events on the canvas itself, so a mouse, a pen and fingers share one path. A
  // press is captured, so its moves and its release reach the canvas even off it, and with
  // no window listeners nothing here fires while the 3D view or a feature panel covers it.
  function wire() {
    let panning = false, lastX = 0, lastY = 0, downX = 0, downY = 0, moved = false, clickHit = null;
    let kind = "mouse";     // pointer type of the latest press: a mouse click is the click event's, a tap is handled on its pointerup
    const pts = new Map();  // pointers pressed on the canvas: id -> { x, y } in client px
    let pinch = null;       // { d, x, y }: finger spread and midpoint at the last pinch step
    let lastTap = null;     // { t, x, y, hit }: the previous tap, for a double-tap
    const local = (e) => { const r = canvas.getBoundingClientRect(); return { x: e.clientX - r.left, y: e.clientY - r.top }; };
    // what a click or a tap does: a note flies in and opens, empty space lets go of everything
    const pick = (n) => {
      if (n) { focusNodeById(n.id); if (window.Mesh && Mesh.openNote) Mesh.openNote(n.id); } // fly in + read
      else { selected = null; setFocus(null); hideCard(); clearFocus2d(); clearSpotlight(); if (window.Mesh && Mesh.closeNote) Mesh.closeNote(); }
    };
    const startPinch = () => {
      const [a, b] = pts.values();
      pinch = { d: Math.hypot(a.x - b.x, a.y - b.y) || 1, x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
    };
    canvas.addEventListener("pointerdown", (e) => {
      if (e.button > 2) return; // back/forward buttons are the browser's
      const type = e.pointerType || "mouse";
      // a primary press starts afresh: no other pointer of its kind is down, so anything
      // of its kind still held here is a release that never arrived. Another kind's press
      // (a first finger is primary too) only clears pointers the canvas no longer holds
      // captured: a mouse, pen or finger really down keeps its gesture, a lost one does
      // not leave the other kinds locked out until it presses again. A pen's press always
      // lets go of touch (palm rejection): a hand resting there first must not leave the
      // pen dead until it lifts
      if (e.isPrimary && pts.size && (type === kind || (type === "pen" && kind === "touch") ||
        [...pts.keys()].every((id) => !canvas.hasPointerCapture(id)))) { pts.clear(); pinch = null; endGesture(); }
      if (pts.size && type !== kind) return; // a pinch is two of one kind, not a pen or a mouse and a palm
      kind = type;
      pts.set(e.pointerId, { x: e.clientX, y: e.clientY });
      try { canvas.setPointerCapture(e.pointerId); } catch (_) { /* the pointer is already gone */ }
      lastInteract = now();
      if (pts.size > 1) { endGesture(); moved = true; startPinch(); return; } // a second finger: whatever the first was doing becomes a pinch
      const p = local(e);
      moved = false; lastX = downX = e.clientX; lastY = downY = e.clientY;
      // a left press right on a dot grabs it (in either view); anywhere else, and any
      // right, middle or shift+left press, pans (the same buttons as the 3D pan). A
      // fingertip is coarser than a cursor, so it gets a wider grab margin.
      const n = e.button === 0 && !e.shiftKey ? nodeAt(p.x, p.y, kind === "mouse" ? 2 : 10) : null;
      if (n) {
        // held world point: the note as drawn, so a still press moves or tugs nothing. In the
        // galaxy that includes any spring-back offset still in flight from an earlier drag.
        // ox,oy: the pointer's screen offset to that point, kept while it is held, so the
        // first step does not snap the note's centre (and its float drift) under the pointer
        const w = pos(n);
        if (view === "galaxy") { w.x += n.dispX; w.y += n.dispY; }
        const at = worldAt(p.x, p.y);
        drag = { node: n, vx: 0, vy: 0, wx: w.x, wy: w.y, ox: (w.x - at.x) * cam.zoom, oy: (w.y - at.y) * cam.zoom,
          warm: false, gx0: n.gx, gy0: n.gy };
        // graph: the first real step warms the sim (see pointermove), so a still click to
        // read a note leaves a sleeping map asleep. Galaxy: pull the local cluster elastically.
        if (view !== "graph") drag.influence = buildInfluence(n);
        canvas.classList.add("panning");
      } else { panning = true; canvas.classList.add("panning"); }
    });
    // no middle-click autoscroll (it starts from the mousedown that follows the pointerdown)
    canvas.addEventListener("mousedown", (e) => { if (e.button === 1) e.preventDefault(); });
    canvas.addEventListener("contextmenu", (e) => e.preventDefault()); // right-drag pans, so no context menu
    const endGesture = () => {
      // graph: a moved note is flung with its last step and a light wake carries it back
      // along its stretched springs. A still press only held it against the swirl, so it
      // goes back to its spot in the layout (the sim may be asleep and would strand it).
      // Galaxy: the offset springs back.
      if (drag && view === "graph") {
        const d = drag.node;
        if (drag.warm) {
          d.vx = drag.vx; d.vy = drag.vy;
          if (d === indexNode) settle = null; else settleOn(d, { x: drag.gx0, y: drag.gy0 });
          warm(d, 0.1);
        } else { d.gx = drag.gx0; d.gy = drag.gy0; }
      }
      if (drag && moved) dropHover(); // the held note springs back, or is flung, from under the still pointer
      drag = null;
      panning = false; canvas.classList.remove("panning");
    };
    canvas.addEventListener("pointermove", (e) => {
      const pt = pts.get(e.pointerId);
      if (pt) { pt.x = e.clientX; pt.y = e.clientY; }
      if (pinch) {
        if (!pt) return;
        // zoom by the change in spread about where the midpoint was, then carry that point
        // along with the midpoint: what was between the fingers stays between them
        const [a, b] = pts.values(), r = canvas.getBoundingClientRect();
        const d = Math.hypot(a.x - b.x, a.y - b.y) || 1, x = (a.x + b.x) / 2, y = (a.y + b.y) / 2;
        camFollow = null; camReturn = null; autoFit = false; lastInteract = now(); dropHover();
        zoomAt(cam.zoom * d / pinch.d, pinch.x - r.left, pinch.y - r.top);
        cam.x -= (x - pinch.x) / cam.zoom; cam.y -= (y - pinch.y) / cam.zoom;
        pinch.d = d; pinch.x = x; pinch.y = y;
        return;
      }
      // a mouse or pen mid-gesture with no button held: the release went elsewhere (a
      // menu, another window), so end it here instead of letting the view follow a bare
      // cursor or a hovering pen. Only the gesture's own pointer: a bare one passing by
      // must not end a finger's pan. (A finger is never down with no button.)
      if (pt && (panning || drag) && e.pointerType !== "touch" && !(e.buttons & 7)) {
        pts.delete(e.pointerId); endGesture();
        try { canvas.releasePointerCapture(e.pointerId); } catch (_) { /* already released */ }
      }
      if (panning || drag) {
        if (!pt) return; // some other pointer passing by mid-gesture
        // distance from the press, not per event, so a slow drag still counts as a drag; a
        // fingertip wobbles more than a cursor, so its tap is allowed more
        if (Math.abs(e.clientX - downX) + Math.abs(e.clientY - downY) > (kind === "mouse" ? 3 : TAP_SLOP)) moved = true;
        // a still press is a click: jitter inside the slop neither pans nor tugs the note.
        // lastX,lastY stay at the press until then, so the first real step carries it all.
        if (!moved) return;
        camFollow = null; camReturn = null; autoFit = false; // a real grab or pan takes manual control back
        const ddx = e.clientX - lastX, ddy = e.clientY - lastY;
        lastX = e.clientX; lastY = e.clientY; lastInteract = now();
        if (panning) { cam.x -= ddx / cam.zoom; cam.y -= ddy / cam.zoom; dropHover(); return; }
        const p = local(e), w = worldAt(p.x + drag.ox, p.y + drag.oy);
        drag.wx = w.x; drag.wy = w.y; // galaxy: the loop applies this to the held node's offset
        if (view === "graph") {
          // warm the neighbourhood (warmOn), do not reshuffle the map
          if (!drag.warm) { drag.warm = true; warm(drag.node, 0.12); }
          const g = rot(w.x, w.y, -swirl); // the sim works in the unturned frame
          drag.vx = g.x - drag.node.gx; drag.vy = g.y - drag.node.gy;
          drag.node.gx = g.x; drag.node.gy = g.y; drag.node.vx = 0; drag.node.vy = 0;
        }
        return;
      }
      // hover: on this canvas the pointer is over the canvas itself, not the rail, the
      // card or the reader, and a hidden canvas (3D view, a panel) gets no events at all
      const p = local(e), n = nodeAt(p.x, p.y);
      canvas.classList.toggle("hovering", !!n);
      if (n !== hover) { hover = n; if (!selected) { setFocus(n); n ? showCard(n) : hideCard(); } }
    });
    // off the canvas (onto the rail, the card, the reader, out of the window): no hover
    canvas.addEventListener("pointerleave", () => {
      if (panning || drag || !hover) return;
      hover = null; canvas.classList.remove("hovering");
      if (!selected) { setFocus(null); hideCard(); }
    });
    // a tap (finger or pen) is the click; a second one close by in time and place is the
    // double-click, judged like it by the first tap's hit
    const tap = (e) => {
      const t0 = now(), prev = lastTap;
      lastTap = null;
      if (prev && t0 - prev.t < DBL_TAP_MS && Math.hypot(e.clientX - prev.x, e.clientY - prev.y) < DBL_TAP_PX) { if (!prev.hit) refit(); return; }
      const p = local(e), n = nodeAt(p.x, p.y, TAP_R);
      lastTap = { t: t0, x: e.clientX, y: e.clientY, hit: n };
      pick(n);
    };
    const lift = (e) => {
      if (!pts.delete(e.pointerId)) return; // not a press of ours, or done with (a capture ends after its pointerup)
      if (pinch) {
        if (pts.size >= 2) { startPinch(); return; } // one of three fingers lifted: pinch on with two
        pinch = null;
        // one finger stays down: it carries on as a pan from where it is now
        if (pts.size === 1) { const [q] = pts.values(); lastX = q.x; lastY = q.y; panning = true; canvas.classList.add("panning"); }
        return;
      }
      if (pts.size) return;
      endGesture();
      if (e.type === "pointerup" && !moved && kind !== "mouse") tap(e); // a mouse click arrives as the click event
    };
    canvas.addEventListener("pointerup", lift);
    canvas.addEventListener("pointercancel", lift);
    canvas.addEventListener("lostpointercapture", lift);
    canvas.addEventListener("click", (e) => {
      // a tap was handled on its pointerup, and so was one dropped there because a mouse
      // is held: its click comes all the same, as this event's own type says
      if (kind !== "mouse" || (e.pointerType && e.pointerType !== "mouse")) return;
      if (e.detail <= 1) clickHit = null; // a fresh click sequence
      if (moved || e.detail > 1) return; // the second click of a double-click is the dblclick's
      if (e.shiftKey) return; // shift+left is a pan: a still one does nothing, as in 3D
      // the hovered note wins while still in reach: notes drift and turn between the hover
      // and the click, and where two notes' reach meets the nearest could change hands.
      // A camera move since has dropped the hover (dropHover)
      const p = local(e);
      pick(clickHit = nodeAt(p.x, p.y, 6, hover));
    });
    // double-click empty space: recentre on the index and refit. On a note the first
    // click already flew in and opened it, so there is nothing more to do. Judged by the
    // first click's hit: the fly-in has moved the note out from under the cursor by now.
    canvas.addEventListener("dblclick", (e) => { if (kind === "mouse" && !(e.pointerType && e.pointerType !== "mouse") && !moved && !clickHit) refit(); });
    canvas.addEventListener("wheel", (e) => {
      e.preventDefault();
      lastInteract = now();
      camFollow = null; camReturn = null; autoFit = false; // wheeling takes manual control back
      dropHover(); // the content moves under a still pointer
      const w = wheelIntent(e);
      if (w.pan) { cam.x += w.dx / cam.zoom; cam.y += w.dy / cam.zoom; return; }
      const p = local(e);
      zoomAt(cam.zoom * Math.exp(-w.dy * 0.0012), p.x, p.y);
    }, { passive: false });

    $("view-graph").onclick = () => setView("graph");
    $("view-galaxy").onclick = () => setView("galaxy");
    if ($("view-galaxy3d")) $("view-galaxy3d").onclick = () => setView("galaxy3d");
    $("q").addEventListener("input", (e) => runSearch(e.target.value));
    window.addEventListener("resize", resize);
    // capture phase, so this runs before shell.js closes the reader on the same Escape
    // and can still see there was a reader to close
    window.addEventListener("keydown", (e) => {
      // the graph's keys work only while the graph is on screen (a feature panel covers
      // both canvases), and never mid-composition in an input method
      if (document.body.classList.contains("panel-active") || e.isComposing) return;
      // camera keys never fire while typing or over a panel, and never with a modifier
      // (cmd/ctrl +/-/0 is browser zoom)
      const el = e.target, inside = (sel) => !!(el && el.closest && el.closest(sel));
      const off = e.metaKey || e.ctrlKey || e.altKey ||
        (el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName))) || inside("#explorer, #note-drawer, dialog");
      if (e.key === "Escape") {
        if (document.querySelector("dialog[open]")) return; // that Escape closes the dialog, nothing else
        // the explorer's filter box clears itself on Escape; its note list and buttons do not
        // own the key, so a note opened from the list is dismissed like one clicked on the map
        if (inside("#explorer") && /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName)) return;
        // Escape peels one layer: the reader, a selection, a filter or a spotlight. Only an
        // Escape with nothing left to dismiss hands the camera back to the auto-fit (in 3D,
        // recenters), so closing a note still returns to the framing you had before it.
        const had = selected || query || spotlight != null || camFollow || !$("note-drawer").classList.contains("hidden");
        selected = null; setFocus(null); hideCard(); clearFocus2d(); clearSpotlight(); $("q").value = ""; query = "";
        if (gl3d) gl3d.setHighlight(null); // a star selected with the reader already closed stays lit otherwise
        if (!had && !off) { if (view !== "galaxy3d") refit(); else if (gl3d) gl3d.recenter(); }
        return;
      }
      // arrows pan, +/- zoom about the visible centre, 0 resumes the auto-fit. 3D owns its
      // camera: only "0" reaches it, as a recenter.
      if (off) return;
      if (view === "galaxy3d") { if (e.key === "0" && gl3d && gl3d.recenter) { e.preventDefault(); gl3d.recenter(); } return; }
      const dir = ARROWS.get(e.key);
      if (dir) {
        if (!$("note-drawer").classList.contains("hidden")) return; // the open reader keeps the arrows for scrolling
        cam.x += dir[0] * 0.12 * (W - inset) / cam.zoom; cam.y += dir[1] * 0.12 * H / cam.zoom;
      } else if (e.key === "+" || e.key === "=") zoomAt(cam.zoom * 1.25, CX, H / 2);
      else if (e.key === "-") zoomAt(cam.zoom / 1.25, CX, H / 2);
      else if (e.key === "0") { e.preventDefault(); refit(); return; }
      else return;
      e.preventDefault();
      camFollow = null; camReturn = null; autoFit = false; lastInteract = now(); dropHover();
    }, true);
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden && !running) { running = true; requestAnimationFrame(loop); }
    });
  }

  function setViewTabs(v) {
    for (const id of ["graph", "galaxy", "galaxy3d"]) {
      const el = $("view-" + id);
      if (!el) continue;
      el.classList.toggle("active", id === v);
      el.setAttribute("aria-selected", id === v ? "true" : "false");
    }
  }
  // Create the 3D galaxy for the active grouping. The galaxy bakes node positions +
  // colors from the grouping at init, so switching grouping disposes + recalls this.
  // gl3dCam: the camera a regroup took from the galaxy it disposed (getCamera). The next
  // init opens on it, so pan, zoom and orbit survive and the fly-in does not replay.
  let gl3dCam = null;
  function initGl3d() {
    const dom = grouping === "domain";
    const saved = gl3dCam; gl3dCam = null;
    let inst = null;
    gl3d = window.Mesh3D.init(canvas3d, { ...G, edges: navigationEdges() }, {
      commColor: dom ? domainColor : commColor,
      groups: dom ? G.domains : G.communities,
      groupField: dom ? "domain" : "community",
      indexId: G.meta.index_id, still: captureStill, camera: saved,
      inset: () => inset, calm, // centre in the visible area; reduced motion stills the drift + idle orbit
      wheel: wheelIntent,       // the same wheel reading as the 2D views
      onSelect: (n) => {
        selected = n; setFocus(n);
        if (n) { showCard(n); if (gl3d) gl3d.focusNode(n.id); if (window.Mesh && Mesh.openNote) Mesh.openNote(n.id); }
        else { hideCard(); clearSpotlight(); if (gl3d) gl3d.clearFocus(); if (window.Mesh && Mesh.closeNote) Mesh.closeNote(); } // empty sky, as an empty 2D click
      },
      // a passing star previews its card only while nothing is selected: the selected
      // note's card, and the note its Read button opens, stay put (as in 2D)
      onHover: (n) => { if (selected) return; if (n) showCard(n); else hideCard(); },
      // the GPU dropped the context and has given it back: this galaxy's objects died with
      // the old one, so let it go (a lost galaxy's dispose only detaches its listeners, no
      // GL call) and rebuild on its camera, now if 3D is on screen, else when next shown
      onContextRestored: (c) => {
        if (!inst || gl3d !== inst) return; // a galaxy since replaced
        gl3dCam = c && typeof c === "object" ? c : null;
        gl3d.clearHover(); // the rebuilt galaxy starts with no hover: its card goes now (a 2D hover stays)
        gl3d.dispose(true); gl3d = null;
        if (view === "galaxy3d") { initGl3d(); if (gl3d) gl3d.resize(); else no3d(); }
      },
    });
    inst = gl3d;
    // no galaxy (a context lost, maybe yet to come back): the camera waits for the next init
    if (!gl3d) { gl3dCam = saved; return; }
    // a legend spotlight picked before this galaxy existed (first open, or since a regroup)
    gl3d.setSpotlight(spotlight);
    // the selected note: locked on again if the galaxy this one replaced was on it (its
    // star has moved to the new grouping's arm, so this flies there), else just lit
    if (selected) { if (saved && saved.focusId != null) gl3d.focusNode(selected.id); else gl3d.setHighlight(selected.id); }
  }
  function setView(v) {
    if (v === view) return;
    // a hover belongs to the view it was picked in, and a key switch leaves the pointer
    // still: its card would name a note the new view does not show there
    dropHover();
    if (v === "galaxy3d") {
      // a galaxy whose context is gone and not yet back draws nothing: let it go (on its
      // camera) and init again, which finds the context lost and falls back. Without this
      // the 3D tab stayed on a black canvas while no restore ever came.
      if (gl3d && gl3d.isLost()) { gl3dCam = gl3d.getCamera(); gl3d.dispose(true); gl3d = null; }
      // shown before the init: a relock there flies to the note's new arm, where a hidden
      // galaxy jumps (gl3d focusNode)
      canvas3d.hidden = false;
      if (!gl3d) initGl3d();
      if (!gl3d) { canvas3d.hidden = true; return no3d(); }
      view = "galaxy3d";
      canvas.hidden = true;
      gl3d.resize();
      // what the 2D views changed while this galaxy was hidden: the legend spotlight and
      // the selection. A lock on a note since replaced there moves to the one being read
      // (initGl3d already relocked a note a regroup took the lock from).
      gl3d.setSpotlight(spotlight);
      const fid = gl3d.getCamera().focusId;
      if (selected && fid != null && fid !== selected.id) gl3d.focusNode(selected.id);
      else gl3d.setHighlight(selected ? selected.id : null);
      setViewTabs(v); setHint(v);
      return;
    }
    view = v;
    canvas3d.hidden = true; canvas.hidden = false; // back to the 2D canvas
    if (v === "graph") wake(0.03); // a gentle stir on return, not a reshuffle
    setViewTabs(v); setHint(v);
    // a fresh view opens framed on the index. A note still focused flies in, and a lock on
    // a note since replaced moves to the one being read (as entering 3D does); anything
    // else left from earlier (a lock on a note closed in 3D, a glide back out) goes.
    camReturn = null; preFocus = null;
    if (camFollow && selected) camFollow = selected; else camFollow = null;
    autoFit = true; fitView();
    if (query && !camFollow) runSearch(query); // a kept lock holds the camera; a match would take it
  }
  // no WebGL2 (or a context that will not come back): disable the 3D tab and fall back to
  // the 2D galaxy, whichever path found out
  function no3d() {
    const tab = $("view-galaxy3d");
    if (tab) { tab.disabled = true; tab.title = "WebGL2 unavailable"; }
    setView("galaxy");
  }
  // The context is the canvas's, not a galaxy's. With no galaxy live (a regroup disposed
  // it, or found the context lost and fell back) nothing else hears either event: a loss
  // still asks for the context back, and its return enables the 3D tab again (it stayed
  // off until a reload). A live galaxy hears both itself and rebuilds (onContextRestored).
  canvas3d.addEventListener("webglcontextlost", (e) => e.preventDefault());
  canvas3d.addEventListener("webglcontextrestored", () => {
    const tab = $("view-galaxy3d");
    if (!gl3d && tab && tab.disabled) { tab.disabled = false; tab.removeAttribute("title"); }
  });
  // the footer hint names the gestures the active view actually has
  const HINTS = {
    flat: "drag to pan · scroll to zoom · double-click to recenter · click a note to read it",
    galaxy3d: "drag to orbit · right-drag or shift-drag to pan · scroll to zoom · double-click to recenter · click a star to read it",
  };
  function setHint(v) { const h = $("hint"); if (h) h.textContent = HINTS[v === "galaxy3d" ? "galaxy3d" : "flat"]; }

  // ---- chrome / states ----
  function setStats() { $("stats").textContent = `${G.meta.node_count} notes / ${G.meta.edge_count} links / ${G.meta.collection_edge_count || 0} collection memberships / ${activeGroups().length} ${grouping === "domain" ? "topics" : "clusters"}`; }
  function buildLegend() {
    const top = activeGroups().slice(0, 9).filter((c) => c.label);
    if (!top.length) return;
    const el = $("legend");
    el.innerHTML = "";
    // header doubles as a Clusters | Topics grouping switch
    const head = document.createElement("div");
    head.className = "legend-head";
    head.style.cssText = "display:flex;gap:6px;align-items:center;padding:0";
    [["community", "Clusters"], ["domain", "Topics"]].forEach(([g, lbl]) => {
      const t = document.createElement("button");
      t.textContent = lbl;
      t.title = g === "domain" ? "Group by topic domain (Engineering, Marketing, Sales, ...)" : "Group by the emergent link clusters";
      t.style.cssText = "flex:1;padding:3px 8px;border-radius:6px;border:1px solid rgba(255,255,255,.14);font:inherit;font-size:11px;cursor:pointer;color:inherit;background:" + (grouping === g ? "rgba(255,255,255,.16)" : "transparent");
      t.onclick = () => setGrouping(g);
      head.appendChild(t);
    });
    el.appendChild(head);
    top.forEach((c) => {
      const b = document.createElement("button");
      b.className = "row" + (spotlight === c.id ? " active" : "");
      b.title = "Spotlight this " + (grouping === "domain" ? "topic" : "cluster") + " (dim the rest). Click again to clear.";
      b.innerHTML = `<i style="background:${esc(c.color)}"></i><span>${esc(c.label)} (${c.size | 0})</span>`;
      b.onclick = () => { spotlight = (spotlight === c.id) ? null : c.id; if (gl3d) gl3d.setSpotlight(spotlight); buildLegend(); };
      el.appendChild(b);
    });
    el.classList.remove("hidden");
  }
  // Switch the active grouping (emergent clusters <-> topic domains). The 3D galaxy
  // bakes positions + colors at init, so it is disposed and recreated; the 2D views
  // re-read colors via groupColorOf on the next frame.
  function setGrouping(g) {
    if (g === grouping) return;
    dropHover(); // every note moves to its new group's place, and 3D rebuilds with no hover
    grouping = g;
    spotlight = null;
    // regroup the 2D views too: the islands flow into the new groups (each with its own
    // hubs, buildGroupIndex) and the galaxy re-deals its arms.
    buildGroupIndex(); layoutGalaxy();
    wake(0.6); cohK = REGROUP_COH;
    // the rebuilt galaxy (now, or when 3D is next shown) opens on this one's camera; keep
    // the GL context for the re-init
    if (gl3d) { gl3dCam = gl3d.getCamera(); gl3d.dispose(true); gl3d = null; }
    if (view === "galaxy3d") { initGl3d(); if (gl3d) gl3d.resize(); else no3d(); }
    buildLegend(); setStats();
    if (selected) showCard(selected); // its card names the group and its colour: the new grouping's
  }
  // ---- clusters explorer: browse communities + their notes, click to read ----
  function focusNodeById(id) {
    const n = G.nodes.find((x) => x.id === id);
    if (!n) return;
    selected = n; setFocus(n); showCard(n);
    if (view !== "galaxy3d") dropHover(); // the camera flies (gl3d's focusNode drops its own)
    if (view === "galaxy3d") { if (gl3d) gl3d.focusNode(id); } // fly the camera into the star
    else { if (!camFollow) preFocus = { x: cam.x, y: cam.y, zoom: cam.zoom, auto: autoFit }; camFollow = n; camReturn = null; autoFit = false; lastInteract = now(); }
  }
  function clearFocus2d() {
    if (!camFollow) return;
    dropHover(); // the camera glides back out
    // a canvas off screen (a feature panel over it, whose loop stands still, or the 3D
    // view) jumps, as gl3d's hidden view does: a glide would play out on return instead
    const jump = calm() || !canvas.clientWidth;
    // it was auto-fitting before the fly-in (or a view switch re-armed it): glide back
    // into the live fit rather than a stale snapshot of it
    if (autoFit || (preFocus && preFocus.auto)) { autoFit = true; fitTick = 0; if (jump) fitView(); }
    else {
      camReturn = preFocus || { x: cam.x, y: cam.y, zoom: cam.zoom };
      if (jump) { cam.x = camReturn.x; cam.y = camReturn.y; cam.zoom = camReturn.zoom; camReturn = null; }
    }
    camFollow = null;
  }
  function buildExplorer() {
    const list = $("exp-list");
    if (!list) return;
    const byComm = new Map();
    for (const n of G.nodes) { if (!byComm.has(n.community)) byComm.set(n.community, []); byComm.get(n.community).push(n); }
    const order = [];
    const seen = new Set();
    for (const c of G.communities.slice().sort((a, b) => (b.size | 0) - (a.size | 0))) { if (byComm.has(c.id)) { order.push(c); seen.add(c.id); } }
    for (const [cid, mem] of byComm) if (!seen.has(cid)) order.push({ id: cid, label: "#" + cid, color: commColor.get(cid) || "#7c766e", size: mem.length });
    // Collections are authored navigation, independent of emergent communities.
    // A note may appear in several; membership order never establishes a home.
    const byCollection = new Map();
    for (const edge of G.collection_edges) {
      const collection = byId.get(edge.target), member = byId.get(edge.source);
      if (!collection || !member) continue;
      if (!byCollection.has(collection.id)) byCollection.set(collection.id, new Map([[collection.id, collection]]));
      byCollection.get(collection.id).set(member.id, member);
    }
    const collectionGroups = [...byCollection].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0).map(([id, members]) => ({
      id, label: "Collection · " + (byId.get(id).label || id), color: groupColorOf(byId.get(id)), members: [...members.values()],
    }));

    const typeSummary = (mem) => {
      const t = new Map();
      for (const m of mem) { const k = m.type || "note"; t.set(k, (t.get(k) || 0) + 1); }
      return [...t.entries()].sort((a, b) => b[1] - a[1]).map(([k, v]) => `${v} ${esc(k)}`).join(" &middot; ");
    };
    list.innerHTML = [...collectionGroups, ...order].map((c) => {
      const mem = (c.members || byComm.get(c.id) || []).slice().sort((a, b) => (b.degree | 0) - (a.degree | 0) || ((a.label || a.id) < (b.label || b.id) ? -1 : 1));
      const notes = mem.map((n) =>
        `<button class="exp-note" data-id="${esc(n.id)}" data-name="${esc((n.label || n.id) + " " + (n.path || ""))}">` +
        `<span class="exp-type">${esc(n.type || "note")}</span>` +
        `<span class="exp-name">${esc(n.label || n.id)}</span>` +
        `<span class="exp-path">${esc(n.path || "")}</span></button>`).join("");
      return `<div class="exp-group"><button class="exp-comm"><i class="exp-sw" style="background:${esc(c.color)}"></i>` +
        `<span class="exp-clabel">${esc(c.label || ("#" + c.id))}</span><span class="exp-count">${mem.length}</span></button>` +
        `<div class="exp-sub">${typeSummary(mem)}</div><div class="exp-notes hidden">${notes}</div></div>`;
    }).join("");

    list.onclick = (e) => {
      const comm = e.target.closest(".exp-comm");
      if (comm) { const nn = comm.parentElement.querySelector(".exp-notes"); if (nn) nn.classList.toggle("hidden"); return; }
      const note = e.target.closest(".exp-note");
      if (note) { focusNodeById(note.dataset.id); if (window.Mesh && Mesh.openNote) Mesh.openNote(note.dataset.id); }
    };
    const filter = $("exp-filter");
    if (filter) filter.oninput = () => {
      const q = filter.value.trim().toLowerCase();
      list.querySelectorAll(".exp-group").forEach((g) => {
        let any = false;
        g.querySelectorAll(".exp-note").forEach((b) => { const hit = !q || (b.dataset.name || "").toLowerCase().includes(q); b.style.display = hit ? "" : "none"; if (hit) any = true; });
        g.style.display = any ? "" : "none";
        const nn = g.querySelector(".exp-notes"); if (q && nn) nn.classList.remove("hidden");
      });
    };

    const browseBtn = $("browse"), explorer = $("explorer"), expClose = $("exp-close");
    const toggleExplorer = (show) => {
      if (!explorer) return;
      const open = show == null ? explorer.classList.contains("hidden") : show;
      explorer.classList.toggle("hidden", !open);
      explorer.setAttribute("aria-hidden", String(!open));
      if (browseBtn) browseBtn.classList.toggle("active", open);
    };
    if (browseBtn) browseBtn.onclick = () => toggleExplorer();
    if (expClose) expClose.onclick = () => toggleExplorer(false);
  }

  function resize() {
    dropHover(); // the viewport, and the fit with it, changes under the pointer (gl3d.resize drops its own)
    dpr = Math.max(1, window.devicePixelRatio || 1);
    W = window.innerWidth; H = window.innerHeight;
    // centre the graph in the part of the canvas the rail does not cover (a side
    // rail only; a short top/bottom bar on mobile covers nothing sideways).
    const rr = $("rail") && $("rail").getBoundingClientRect();
    inset = rr && rr.height > H * 0.6 && rr.right < W * 0.5 ? rr.right : 0;
    CX = inset + (W - inset) / 2;
    canvas.width = W * dpr; canvas.height = H * dpr;
    canvas.style.width = W + "px"; canvas.style.height = H + "px";
    if (autoFit) fitTick = 0; // still auto-fitting: refit to the new viewport next frame
    if (dustCv) dustStale = now(); // rebaked at the new viewport + dpr once the resizing stops
    // soft, wide vignette only: a hint of depth at the far corners, NO hard frame
    const g = ctx.createRadialGradient(CX, H / 2, Math.min(W - inset, H) * 0.55, CX, H / 2, Math.max(W - inset, H) * 0.95);
    g.addColorStop(0, "rgba(6,5,10,0)"); g.addColorStop(1, "rgba(0,0,0,0.34)");
    vignette = g;
    stars = []; const n = Math.min(320, Math.round(W * H / 7000));
    for (let i = 0; i < n; i++) stars.push({ x: rand(i * 7) * W, y: rand(i * 13 + 3) * H, r: rand(i * 5) > 0.88 ? 2 : 1, a: 0.07 + rand(i * 3) * 0.2 });
    if (gl3d) gl3d.resize();
  }
  function doneOverlay() { overlay.classList.add("done", "hidden"); }
  function showEmpty() { overlay.classList.add("done"); overlayMsg.textContent = "no notes indexed yet. run: mesh index"; }
  function fail(err) { overlay.classList.add("done"); overlayMsg.textContent = "could not load the graph: " + (err && err.message ? err.message : err); }

  // ---- utils ----
  function rgb(hex) {
    let h = hex.replace("#", "");
    if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
    return { r: parseInt(h.slice(0, 2), 16) || 0, gg: parseInt(h.slice(2, 4), 16) || 0, b: parseInt(h.slice(4, 6), 16) || 0 };
  }
  function lighten(hex, amt) {
    const { r, gg, b } = rgb(hex);
    const m = (v) => Math.round(v + (255 - v) * amt);
    return `rgb(${m(r)},${m(gg)},${m(b)})`;
  }
  function rand(seed) { const x = Math.sin(seed * 99.13 + 17.7) * 43758.5453; return x - Math.floor(x); }
  function joinPath(root, rel) { return (root || "").replace(/\/$/, "") + "/" + (rel || ""); }
  function esc(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }
})();
