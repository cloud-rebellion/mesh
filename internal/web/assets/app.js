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
  let settleF = 0.3, settleT = 0; // ...back to this net force, for at most this many more steps
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

  fetch("graph.json").then((r) => {
    if (!r.ok) throw new Error("graph.json " + r.status);
    return r.json();
  }).then(boot).catch(fail);

  function boot(data) {
    G = data;
    G.communities = G.communities || []; // tolerate a graph indexed before community detection
    G.edges = G.edges || [];
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
  function buildAdjacency() {
    for (const n of G.nodes) adj.set(n.id, []);
    // one link per unordered pair (key min*N+max): A->B plus B->A, or a repeated link,
    // would double that spring and the degree the springs are normalised by
    const ei = [], seen = new Set(), N = G.nodes.length;
    for (const e of G.edges) {
      if (e.source === e.target || !adj.has(e.source) || !adj.has(e.target)) continue;
      const a = nodeIndex.get(e.source), b = nodeIndex.get(e.target), k = a < b ? a * N + b : b * N + a;
      if (seen.has(k)) continue;
      seen.add(k);
      adj.get(e.source).push(e.target); adj.get(e.target).push(e.source);
      ei.push(a, b);
    }
    edgeIdx = new Int32Array(ei);
    deg = new Int32Array(G.nodes.length);
    for (const i of edgeIdx) deg[i]++;
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
    // edges inside one group draw in that group's hue; bridges between groups stay
    // a cool neutral. One path per bucket keeps it to ~K strokes a frame.
    const buckets = new Map();
    for (let j = 0; j < edgeIdx.length; j += 2) {
      const a = edgeIdx[j], b = edgeIdx[j + 1];
      const key = nodeSlot[a] === nodeSlot[b] ? nodeSlot[a] : -1;
      (buckets.get(key) || buckets.set(key, []).get(key)).push(a, b);
    }
    edgeBuckets = [...buckets].map(([k, list]) => {
      const c = k < 0 ? null : rgb(gColors[k]);
      return { slot: k, pairs: list, stroke: c ? `rgba(${c.r},${c.gg},${c.b},0.075)` : "rgba(150,165,205,0.045)" };
    });
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
    // biggest nearest the middle. The sim then blooms every knot out into an island,
    // so the opening seconds read as the map unfurling, not a random cloud collapsing.
    // The index's group goes first, seeded at the origin, so its island forms around
    // the pinned index.
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
      const ha = rand(i * 43 + 21) * TAU, hr = 0.25 + 0.8 * Math.sqrt(rand(i * 47 + 22));
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
  // The arm winds logarithmically, the cross-arm scatter widens with radius, and a
  // few notes stray into the inter-arm dark so the arms have soft edges. The whole
  // pattern turns rigidly (galaxyAngle): differential rotation would wind the arms
  // into rings within minutes. Each note's float drift is what makes it feel alive.
  let gSegA = null, gSeg0 = null, gSeg1 = null;
  // The arm count is the 3D galaxy's (Mesh3D.GAL_ARMS), so a cluster deals onto the same
  // arm in both; 3 if gl3d.js did not load. Each arm is evenly spaced plus a small
  // wobble in offset and wind so the spiral reads as grown, not stamped (the three-arm
  // tuning: offsets 0, 2.2, 4.25 and winds 3.5, 3.2, 3.8), one entry per arm.
  const ARM_WOBBLE = [0, 0.106, 0.061], ARM_WINDS = [3.5, 3.2, 3.8];
  const M3A = window.Mesh3D && window.Mesh3D.GAL_ARMS;
  const GAL_ARMS = Number.isInteger(M3A) && M3A > 0 ? M3A : ARM_WINDS.length;
  const ARM_OFF = Array.from({ length: GAL_ARMS }, (_, a) => a * TAU / GAL_ARMS + ARM_WOBBLE[a % ARM_WOBBLE.length]);
  const ARM_WIND = Array.from({ length: GAL_ARMS }, (_, a) => ARM_WINDS[a % ARM_WINDS.length]);
  const galArmAngle = (arm, r) => ARM_OFF[arm] + ARM_WIND[arm] * Math.log(1 + 3 * r) / Math.log(4);
  // Deal groups onto arms: the next biggest onto the lightest arm (ties by group id,
  // numbers first, never input order), each owning the stretch [s0, s1) of its arm.
  // groups: [{id, size}] -> Map id -> {arm, s0, s1}. The 3D galaxy's packArms and its
  // tie order byGroupId are the one copy, so a group sits on the same arm in both. If
  // gl3d.js did not load, the groups just take turns round the arms, each along all of it.
  const byGroupId = (window.Mesh3D && window.Mesh3D.byGroupId) || ((a, b) => (String(a) < String(b) ? -1 : String(a) > String(b) ? 1 : 0));
  const packArms = (window.Mesh3D && window.Mesh3D.packArms) || ((groups, arms) => new Map(groups.map((g, i) => [g.id, { arm: i % arms, s0: 0, s1: 1 }])));
  // a finger's tap follows the 3D galaxy's rules (Mesh3D: slop, double-tap time and reach,
  // hit radius), so it behaves the same in every view. A pen taps by them in 2D but by the
  // mouse's in 3D (gl3d routes a pen through its mouse handlers). These stand in if
  // gl3d.js did not load
  const M3T = window.Mesh3D || {};
  const TAP_SLOP = M3T.TAP_SLOP || 8, DBL_TAP_MS = M3T.DBL_TAP_MS || 400, DBL_TAP_PX = M3T.DBL_TAP_PX || 30, TAP_R = M3T.TAP_R || ((dot) => Math.max(20, dot + 12));
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
      const r = 0.1 + 0.9 * s;
      const gauss = (rand(i * 11 + 2) + rand(i * 13 + 3) + rand(i * 19 + 4) - 1.5) / 1.5; // ~normal in [-1,1]
      const stray = rand(i * 23 + 5) < 0.12 ? 2.8 : 1;
      const th = galArmAngle(gSegA[g], r) + gauss * (0.22 + 0.12 * r) * stray;
      const rr = r * GAL_R * (1 + (rand(i * 29 + 6) - 0.5) * 0.16);
      n.gal0x = Math.cos(th) * rr; n.gal0y = Math.sin(th) * rr;
    });
    // dust: faint unlabelled arm particles so the disc reads as a galaxy between notes.
    // The count follows the disc area (so N), with a floor so a small vault still has arms.
    dust = []; dustCv = null;
    const D = Math.min(4200, Math.max(600, Math.round(N * 0.9)));
    for (let i = 0; i < D; i++) {
      const r = 0.04 + 0.96 * Math.sqrt(rand(i * 3 + 101));
      const gauss = (rand(i * 5 + 102) + rand(i * 7 + 103) - 1) * 1.4;
      const th = galArmAngle(i % GAL_ARMS, r) + gauss * (0.16 + 0.2 * r) + (rand(i * 11 + 104) < 0.2 ? (rand(i * 13 + 105) - 0.5) * 1.6 : 0);
      const rr = r * GAL_R * 1.04;
      const roll = rand(i * 17 + 106);
      dust.push({
        x: Math.cos(th) * rr, y: Math.sin(th) * rr,
        a: (0.16 + rand(i * 19 + 107) * 0.45) * (1.15 - 0.55 * r),
        s: rand(i * 23 + 108) > 0.93 ? 1.6 : 1,
        c: roll > 0.86 ? "#f4a3c8" : roll > 0.62 ? "#ffe6c7" : "#9fe3f2",
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
    const RD = GAL_R * 1.04 + 4;
    // the galaxy fit frames its 98th percentile radius, which is about GAL_R (see computeFit)
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

  // ---- force sim (graph view): Barnes-Hut repulsion + group pull + springs ----
  // Repulsion runs on a quadtree (theta 0.9), so every note feels the whole graph at
  // O(N log N). The old cell-grid cutoff made forces jump at cell borders, and at a
  // few thousand notes those jumps froze the layout into a visible lattice. A pull
  // toward each note's group centroid, stronger than the global gravity, is what
  // opens dark space between the islands. The pools below are reused every tick.
  const THETA2 = 0.81, REP = 1.6, SOFT2 = (SPACING * 0.3) ** 2;
  const PULL = 0.004, GRAV = 0.0005, K_IN = 0.022, K_OUT = 0.0009;
  const TINY = 4; // groups this small have no island to join: they sprinkle across the disc instead
  let qCap = 0, qN = 0, qCx, qCy, qHalf, qM, qX, qY, qBody, qKid;
  const qStack = new Int32Array(8192);
  let gSize = null, deg = null;
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
  function qInsert(b, bx, by) {
    let q = 0;
    for (let depth = 0; ; depth++) {
      qM[q] += 1; qX[q] += bx; qY[q] += by;
      const s = qBody[q];
      if (s === -1) { qBody[q] = b; return; }
      if (s >= 0) {
        if (depth > 40) return; // coincident pile: the aggregate keeps its mass
        qBody[q] = -2;
        const o = G.nodes[s], c = qKidFor(q, o.gx, o.gy);
        qM[c] += 1; qX[c] += o.gx; qY[c] += o.gy; qBody[c] = s;
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
    for (let i = 0; i < N; i++) qInsert(i, nodes[i].gx, nodes[i].gy);

    gSx.fill(0); gSy.fill(0); gN.fill(0);
    let r2 = 0, rn = 0;
    for (let i = 0; i < N; i++) {
      const s = nodeSlot[i], v = nodes[i];
      gSx[s] += v.gx; gSy[s] += v.gy; gN[s]++;
      if (gSize[s] > TINY) { r2 += v.gx * v.gx + v.gy * v.gy; rn++; }
    }
    // Notes in tiny groups would otherwise be shoved out to one ring at the rim (the
    // repulsion/gravity balance point). Each gets a fixed home scattered across the
    // disc instead, scaled to the live radius, so they read as stardust between islands.
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
      const s = nodeSlot[i], n = gN[s];
      if (gSize[s] <= TINY) {
        v.fx = fx + (v.homeX * R - x) * PULL;
        v.fy = fy + (v.homeY * R - y) * PULL;
      } else {
        v.fx = fx + (gSx[s] / n - x) * PULL - x * GRAV;
        v.fy = fy + (gSy[s] / n - y) * PULL - y * GRAV;
      }
    }
    // springs, normalised by the lighter endpoint's degree (a 300-link hub would
    // otherwise haul its whole neighbourhood into a knot); bridges are weaker so
    // they tie islands together without merging them.
    for (let j = 0; j < edgeIdx.length; j += 2) {
      const ai = edgeIdx[j], bi = edgeIdx[j + 1], a = nodes[ai], b = nodes[bi];
      const dx = b.gx - a.gx, dy = b.gy - a.gy, d = Math.sqrt(dx * dx + dy * dy) || 1;
      const k = (nodeSlot[ai] === nodeSlot[bi] ? K_IN : K_OUT) / Math.sqrt(Math.min(deg[ai], deg[bi]) || 1);
      const f = (d - SPACING) * k, fx = (dx / d) * f, fy = (dy / d) * f;
      a.fx += fx; a.fy += fy; b.fx -= fx; b.fy -= fy;
    }
    // the slow swirl is not a force: it is a render-time rigid turn (swirl, in loop), so
    // a settled layout is truly still and the sim can sleep
    const damp = 0.86;
    let vSum = 0, vMax = 0;
    for (const v of nodes) {
      if (drag && v === drag.node) continue;
      if (v === indexNode) { // pinned at the origin like the galaxy sun; a drag springs back
        v.gx *= 0.8; v.gy *= 0.8; v.vx = 0; v.vy = 0;
        if (v.gx * v.gx + v.gy * v.gy < 0.01) { v.gx = 0; v.gy = 0; }
        else vMax = Infinity; // still springing home: no sleep, or it would stay off the origin
        continue;
      }
      let vx = (v.vx + v.fx * alpha) * damp;
      let vy = (v.vy + v.fy * alpha) * damp;
      let sp2 = vx * vx + vy * vy;
      if (sp2 > 3600) { const k = 60 / Math.sqrt(sp2); vx *= k; vy *= k; sp2 = 3600; }
      v.vx = vx; v.vy = vy; v.gx += vx; v.gy += vy;
      const s = Math.sqrt(sp2); vSum += s; if (s > vMax) vMax = s;
    }
    if (settle && --settleT <= 0) settle = null; // one dropped note never holds the map awake for long
    if (alpha > 0.04) alpha *= 0.993; // cools to a floor
    // cooled and still for a whole second: sleep, skipping the Barnes-Hut pass that is
    // most of an idle frame. The max check keeps it awake while a flung note is still
    // moving (one note barely moves the mean). A dropped note starts from rest and, at the
    // floor, crawls back on the weak group pull, so its speed says little: it is judged
    // by its net force instead, until that is back to about what it was when it was
    // grabbed (plenty of notes already sit above the floor's drift when the map sleeps).
    // The run of still steps catches whatever else a drag left stretched. wake() restarts it.
    // Still is judged on screen (under ~0.5 px a second on average, ~3 at most) but never
    // stricter than 0.05 / 0.2 world units a step, so a fitted map sleeps 15 to 18 s in
    // rather than after its last sub-pixel crawl home (~30 s). Zoomed in past ~0.15 that
    // floor is the gate (at zoom 1 about 3 px/s mean, 12 at most): a close-up can stop
    // mid-crawl rather than burn Barnes-Hut passes on the map's slow relaxation, mostly
    // off screen. (Sooner still leaves more of it for the next wake to release.)
    else if (drag || vSum / N >= Math.max(0.05, 0.0075 / cam.zoom) || vMax >= Math.max(0.2, 0.045 / cam.zoom) ||
      (settle && Math.hypot(settle.fx, settle.fy) > settleF)) quiet = 0;
    else if (++quiet >= 60) { simAwake = false; settle = null; }
  }
  // wake the force sim with at least this much energy (a real grab, a drop, a regroup, a
  // return to the view)
  function wake(a) { alpha = Math.max(alpha, a); simAwake = true; quiet = 0; }
  // hold the sim awake until this dropped note's net force is back near its force at the
  // grab (f0), for at most ~15 s
  function settleOn(n, f0) { settle = n; settleF = Math.max(0.3, 1.25 * f0); settleT = 900; }

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
        // at), eased when idle. Rigid: an inner-faster shear tears the islands apart.
        if (!calm()) swirl += (idle ? 0.00016 : 0.00032) * fk;
        // a held note stays under the cursor: its world point is fixed, so as the swirl
        // turns, re-pin it in the unturned frame the sim works in (the sim skips it)
        if (drag) { const g = rot(drag.wx, drag.wy, -swirl); drag.node.gx = g.x; drag.node.gy = g.y; }
        // the sim's forces, damping and cooling are per step, so it steps at 60 Hz whatever
        // the display runs at (every other frame at 120 Hz). At most one step a frame, so
        // a slow frame never piles more Barnes-Hut work onto itself.
        simAcc += fk;
        if (simAcc >= 0.5) { simAcc = Math.max(-0.5, Math.min(0.5, simAcc - 1)); if (simAwake) simStep(); }
      } else {
        if (!calm()) galaxyAngle += (idle ? 0.00025 : 0.0005) * fk; // rigid turn, ~3.5 min a revolution; eases when idle
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
        // grains shimmer in and out as the disc turns; the high one averages them
        ctx.imageSmoothingQuality = "high";
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
      ctx.strokeStyle = bk.stroke;
      ctx.globalAlpha = neighborSet ? 0.45 : (spotlight != null && gSlot.get(spotlight) !== bk.slot ? 0.25 : 1);
      ctx.beginPath();
      const L = bk.pairs;
      for (let j = 0; j < L.length; j += 2) {
        const a = sp[L[j]], b = sp[L[j + 1]];
        if (!on(a, 40) && !on(b, 40)) continue;
        if (neighborSet && (isFocus(nodes[L[j]].id) || isFocus(nodes[L[j + 1]].id))) { emph.push(a, b); continue; }
        curve(a, b);
      }
      ctx.stroke();
    }
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
      const core = nodeRadius(n) * z;
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
      const r = coreR(n, z) * (sun ? 1 : 0.8 + 0.35 * n.depth);
      const tw = drift ? 0.86 + 0.14 * Math.sin(tc * 1.3 + n.fp1 * 3) : 1;
      ctx.globalAlpha = dim ? 0.14 : (sun ? 1 : (0.5 + 0.5 * n.depth) * tw);
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

  function coreR(n, z) { return n.id === G.meta.index_id ? Math.max(2, nodeRadius(n) * z) * 1.6 : Math.max(2, nodeRadius(n) * z); }

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
  function refit() { autoFit = true; fitTick = 0; camFollow = null; camReturn = null; if (calm()) fitView(); }
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
  // dot of a given on-screen radius.
  function nodeAt(sx, sy, slop = 6) {
    let best = null, bestD = Infinity;
    for (let i = 0; i < G.nodes.length; i++) {
      const n = G.nodes[i];
      if (query && !matches(n)) continue;
      const s = sp[i]; if (!s) continue;
      const dot = nodeRadius(n) * cam.zoom, r = typeof slop === "function" ? slop(dot) : dot + slop;
      const dd = (s.x - sx) ** 2 + (s.y - sy) ** 2;
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
    const cp = $("copy");
    if (cp) cp.onclick = () => navigator.clipboard && navigator.clipboard.writeText(joinPath(G.meta.vault, n.path));
  }
  function hideCard() { if (!selected) $("card").classList.add("hidden"); }

  // ---- search ----
  function matches(n) { return !query || (n.label || "").toLowerCase().includes(query) || (n.path || "").toLowerCase().includes(query); }
  function runSearch(q) {
    query = q.trim().toLowerCase();
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
          warm: false, gx0: n.gx, gy0: n.gy, f0: Math.hypot(n.fx || 0, n.fy || 0) };
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
          if (d === indexNode) settle = null; else settleOn(d, drag.f0);
          wake(0.1);
        } else { d.gx = drag.gx0; d.gy = drag.gy0; }
      }
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
        camFollow = null; camReturn = null; autoFit = false; lastInteract = now();
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
        if (panning) { cam.x -= ddx / cam.zoom; cam.y -= ddy / cam.zoom; return; }
        const p = local(e), w = worldAt(p.x + drag.ox, p.y + drag.oy);
        drag.wx = w.x; drag.wy = w.y; // galaxy: the loop applies this to the held node's offset
        if (view === "graph") {
          if (!drag.warm) { drag.warm = true; wake(0.3); } // warm the neighbourhood, do not reshuffle the map
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
      const p = local(e);
      pick(clickHit = nodeAt(p.x, p.y));
    });
    // double-click empty space: recentre on the index and refit. On a note the first
    // click already flew in and opened it, so there is nothing more to do. Judged by the
    // first click's hit: the fly-in has moved the note out from under the cursor by now.
    canvas.addEventListener("dblclick", (e) => { if (kind === "mouse" && !(e.pointerType && e.pointerType !== "mouse") && !moved && !clickHit) refit(); });
    canvas.addEventListener("wheel", (e) => {
      e.preventDefault();
      lastInteract = now();
      camFollow = null; camReturn = null; autoFit = false; // wheeling takes manual control back
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
      camFollow = null; camReturn = null; autoFit = false; lastInteract = now();
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
    if (!window.Mesh3D) return;
    const dom = grouping === "domain";
    const saved = gl3dCam; gl3dCam = null;
    let inst = null;
    gl3d = window.Mesh3D.init(canvas3d, G, {
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
    if (v === "graph") wake(0.12); // a gentle stir on return, not a reshuffle
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
  function setStats() { $("stats").textContent = `${G.meta.node_count} notes / ${G.meta.edge_count} links / ${activeGroups().length} ${grouping === "domain" ? "topics" : "clusters"}`; }
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
    grouping = g;
    spotlight = null;
    // regroup the 2D views too: the force islands flow into the new groups and the
    // galaxy re-deals its arms.
    buildGroupIndex(); layoutGalaxy();
    wake(0.6);
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
    if (view === "galaxy3d") { if (gl3d) gl3d.focusNode(id); } // fly the camera into the star
    else { if (!camFollow) preFocus = { x: cam.x, y: cam.y, zoom: cam.zoom, auto: autoFit }; camFollow = n; camReturn = null; autoFit = false; lastInteract = now(); }
  }
  function clearFocus2d() {
    if (!camFollow) return;
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

    const typeSummary = (mem) => {
      const t = new Map();
      for (const m of mem) { const k = m.type || "note"; t.set(k, (t.get(k) || 0) + 1); }
      return [...t.entries()].sort((a, b) => b[1] - a[1]).map(([k, v]) => `${v} ${esc(k)}`).join(" &middot; ");
    };
    list.innerHTML = order.map((c) => {
      const mem = (byComm.get(c.id) || []).slice().sort((a, b) => (b.degree | 0) - (a.degree | 0) || ((a.label || a.id) < (b.label || b.id) ? -1 : 1));
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
