// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
// mesh ui 3D galaxy: a raw WebGL2 renderer (no three.js, no CDN, no deps). A real
// spiral galaxy: community clusters strung along spiral arms on a thin disc that
// slowly turns (differential rotation, inner faster), a blazing multi-layer core
// bulge with dust lanes cutting the arms, a dense twinkling field-star disc, a real
// bloom post-process (HDR-ish bright-pass + separable blur + tonemapped composite),
// and a cinematic fly-in on open. Falls back to direct rendering if bloom FBOs are
// unavailable. Exposed as window.Mesh3D.init(canvas, G, opts) -> api | null.
(() => {
  "use strict";

  // --- tiny mat4 (column-major) ---
  function perspective(fovy, aspect, near, far) {
    const f = 1 / Math.tan(fovy / 2), nf = 1 / (near - far);
    return [f / aspect, 0, 0, 0, 0, f, 0, 0, 0, 0, (far + near) * nf, -1, 0, 0, 2 * far * near * nf, 0];
  }
  function mul(a, b) {
    const o = new Array(16);
    for (let c = 0; c < 4; c++) for (let r = 0; r < 4; r++) {
      o[c * 4 + r] = a[r] * b[c * 4] + a[4 + r] * b[c * 4 + 1] + a[8 + r] * b[c * 4 + 2] + a[12 + r] * b[c * 4 + 3];
    }
    return o;
  }
  // orbit view about a center point (cx,cy,cz): translate the world so center is at
  // the origin, rotate (yaw,pitch), then pull back by dist. The center lets the
  // camera fly into and orbit a single note instead of always the galaxy origin.
  function viewMatrix(yaw, pitch, dist, cx, cy, cz) {
    const cyw = Math.cos(yaw), syw = Math.sin(yaw), cp = Math.cos(pitch), sp = Math.sin(pitch);
    const ry = [cyw, 0, -syw, 0, 0, 1, 0, 0, syw, 0, cyw, 0, 0, 0, 0, 1];
    const rx = [1, 0, 0, 0, 0, cp, sp, 0, 0, -sp, cp, 0, 0, 0, 0, 1];
    const tr = [1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, -dist, 1];
    const tc = [1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, -(cx || 0), -(cy || 0), -(cz || 0), 1];
    return mul(tr, mul(rx, mul(ry, tc)));
  }

  function compile(gl, type, src) {
    const s = gl.createShader(type);
    gl.shaderSource(s, src); gl.compileShader(s);
    if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) { console.error("mesh3d shader:", gl.getShaderInfoLog(s)); gl.deleteShader(s); return null; }
    return s;
  }
  function program(gl, vs, fs) {
    const v = compile(gl, gl.VERTEX_SHADER, vs), f = compile(gl, gl.FRAGMENT_SHADER, fs);
    if (!v || !f) { gl.deleteShader(v); gl.deleteShader(f); return null; }
    const p = gl.createProgram();
    gl.attachShader(p, v); gl.attachShader(p, f); gl.linkProgram(p);
    gl.deleteShader(v); gl.deleteShader(f); // only flagged while attached: they go with the program
    if (!gl.getProgramParameter(p, gl.LINK_STATUS)) { console.error("mesh3d link:", gl.getProgramInfoLog(p)); gl.deleteProgram(p); return null; }
    return p;
  }

  function rand(seed) {
    let x = (seed * 2654435761) >>> 0;
    x ^= x >>> 15; x = (x * 2246822519) >>> 0; x ^= x >>> 13;
    return (x >>> 0) / 4294967296;
  }
  function hexToRGB(hex) {
    let h = (hex || "#7c766e").replace("#", "");
    if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
    return [(parseInt(h.slice(0, 2), 16) || 0) / 255, (parseInt(h.slice(2, 4), 16) || 0) / 255, (parseInt(h.slice(4, 6), 16) || 0) / 255];
  }

  // differential rotation about Y: inner radii turn faster, like a real galaxy. The
  // SAME formula is used in the shaders and in JS picking, so clicks stay accurate.
  const SPIN_GLSL = `
  vec3 spin(vec3 p, float t, float omega){
    float r = length(p.xz);
    if (r < 0.001 || omega == 0.0) return p;
    float a = t * omega * (0.4 + 30.0 / (r + 18.0));
    float c = cos(a), s = sin(a);
    return vec3(p.x * c - p.z * s, p.y, p.x * s + p.z * c);
  }`;
  function spinJS(x, y, z, t, omega) {
    const r = Math.hypot(x, z);
    if (r < 0.001 || omega === 0) return [x, y, z];
    const a = t * omega * (0.4 + 30.0 / (r + 18.0));
    const c = Math.cos(a), s = Math.sin(a);
    return [x * c - z * s, y, x * s + z * c];
  }

  // Deal groups onto spiral arms. Shared with the 2D galaxy (app.js loads after this
  // file), so a cluster lands on the same arm in both views: biggest first, each onto
  // the lightest arm so far (ties to the lower arm), equal sizes in group id order
  // (numbers before strings), never input order. Each group owns [s0,s1) of its arm,
  // sized by its note count. groups: [{id, size}] -> Map id -> {arm, s0, s1}.
  const GAL_ARMS = 3;
  function byGroupId(a, b) {
    const na = typeof a === "number", nb = typeof b === "number";
    if (na && nb) return a - b;
    if (na !== nb) return na ? -1 : 1;
    const sa = String(a), sb = String(b);
    return sa < sb ? -1 : sa > sb ? 1 : 0;
  }
  function packArms(groups, arms) {
    const A = Math.max(1, Math.floor(arms) || GAL_ARMS);
    const order = (groups || []).slice().sort((a, b) => (b.size || 0) - (a.size || 0) || byGroupId(a.id, b.id));
    const tot = new Array(A).fill(0), list = Array.from({ length: A }, () => []);
    for (const g of order) {
      let a = 0;
      for (let k = 1; k < A; k++) if (tot[k] < tot[a]) a = k;
      list[a].push(g); tot[a] += g.size || 0;
    }
    const out = new Map();
    list.forEach((l, a) => {
      let c = 0;
      for (const g of l) { const s0 = c / (tot[a] || 1); c += g.size || 0; out.set(g.id, { arm: a, s0, s1: c / (tot[a] || 1) }); }
    });
    return out;
  }

  // A finger's tap, the same in every view (app.js reads these): a press that travels at
  // most TAP_SLOP px (x + y) is a tap, a second one within DBL_TAP_MS and DBL_TAP_PX px
  // of it is a double-tap, and a tap hits a dot within TAP_R(dot) px of its centre (dot:
  // its radius on screen): 12 px past its edge, never under 20, as a fingertip is wider
  // than a cursor.
  const TAP_SLOP = 8, DBL_TAP_MS = 400, DBL_TAP_PX = 30;
  const TAP_R = (dot) => Math.max(20, dot + 12);

  // float drift: each note loops slowly around its home on three frequencies (an
  // ~8-14 s wander per axis), so the disc breathes while the layout holds. They are all
  // multiples of 0.01, so the motion repeats exactly every 200*PI of time: the clock
  // wraps there (DRIFT_PERIOD) and stays precise in a float32 uniform. The phase is a
  // per-note attribute and the SAME math runs in JS picking, so a drifting star is
  // still exactly where you click. Edge endpoints carry their node's phase, so edges
  // stay attached. A negative phase pins a note: the sun sits dead on its corona.
  const DRIFT_GLSL = `
  vec3 drift(float ph, float t){
    if (ph < 0.0) return vec3(0.0);
    return vec3(sin(t * 0.30 + ph * 6.2832), 0.55 * sin(t * 0.43 + ph * 12.566), cos(t * 0.25 + ph * 9.4248));
  }`;
  function driftJS(ph, t, amp) {
    if (ph < 0) return [0, 0, 0];
    return [Math.sin(t * 0.30 + ph * 6.2832) * amp, 0.55 * Math.sin(t * 0.43 + ph * 12.566) * amp, Math.cos(t * 0.25 + ph * 9.4248) * amp];
  }
  const DRIFT_PERIOD = 200 * Math.PI; // also a whole number of turns for the twinkle, sun pulse and pitch sway

  const SPRITE_VS = `#version 300 es
  layout(location=0) in vec2 corner;
  layout(location=1) in vec3 iPos;
  layout(location=2) in float iSize;
  layout(location=3) in vec3 iColor;
  layout(location=4) in float iFlag;
  layout(location=5) in float iComm;
  layout(location=6) in float iPhase;
  uniform mat4 uProj, uView;
  uniform float uHi, uTime, uSpinTime, uSizeMul, uTwinkle, uOmega, uSpotComm, uFloat;
  out vec2 vUV; out vec3 vColor; out float vGlow; out float vViewZ;
  ${SPIN_GLSL}
  ${DRIFT_GLSL}
  void main(){
    vec3 wp = spin(iPos + drift(iPhase, uTime) * uFloat, uSpinTime, uOmega);
    vec4 vp = uView * vec4(wp, 1.0);
    float size = iSize * uSizeMul;
    bool sun = iFlag > 0.5;
    if (sun) size *= 1.7 + 0.10 * sin(uTime * 0.8);
    bool hi = abs(float(gl_InstanceID) - uHi) < 0.5;
    if (hi) size *= 1.5;
    float tw = 1.0 - uTwinkle * 0.5 + uTwinkle * 0.5 * sin(uTime * 1.6 + float(gl_InstanceID) * 0.7);
    vp.xy += corner * size;
    gl_Position = uProj * vp;
    vUV = corner;
    vColor = sun ? mix(iColor, vec3(1.0, 0.96, 0.86), 0.78) : iColor;
    vGlow = (sun ? 1.9 : (hi ? 1.7 : 1.0)) * tw;
    if (uSpotComm >= 0.0 && abs(iComm - uSpotComm) > 0.5) vGlow *= 0.10; // legend spotlight dims the rest
    vViewZ = -vp.z;
  }`;

  const SPRITE_FS = `#version 300 es
  precision highp float;
  in vec2 vUV; in vec3 vColor; in float vGlow; in float vViewZ;
  out vec4 frag;
  uniform float uSoft, uHalo, uIntensity, uFog, uCamDist, uSpike;
  void main(){
    float d = length(vUV);
    float c = max(0.0, 1.0 - d);
    float g = pow(c, uSoft) + pow(c, 1.7) * uHalo;
    if (uSpike > 0.0) {
      // 4-point diffraction spikes: a thin bright cross along the sprite axes.
      float sx = pow(max(0.0, 1.0 - abs(vUV.y) * 9.0), 2.0) * max(0.0, 1.0 - abs(vUV.x));
      float sy = pow(max(0.0, 1.0 - abs(vUV.x) * 9.0), 2.0) * max(0.0, 1.0 - abs(vUV.y));
      g += (sx + sy) * uSpike;
    } else if (d > 1.0) discard;
    float fade = mix(1.0, clamp(0.5 + (uCamDist - vViewZ) / 200.0, 0.18, 1.0), uFog);
    float glow = g * vGlow * uIntensity * fade;
    frag = vec4(vColor * glow, glow);
  }`;

  const LINE_VS = `#version 300 es
  layout(location=0) in vec3 aPos;
  layout(location=1) in vec3 aColor;
  layout(location=2) in float aPhase;
  uniform mat4 uProj, uView;
  uniform float uSpinTime, uOmega, uTime, uFloat;
  out vec3 vC; out float vZ;
  ${SPIN_GLSL}
  ${DRIFT_GLSL}
  void main(){ vec3 wp = spin(aPos + drift(aPhase, uTime) * uFloat, uSpinTime, uOmega); vec4 vp = uView * vec4(wp, 1.0); vZ = -vp.z; gl_Position = uProj * vp; vC = aColor; }`;
  const LINE_FS = `#version 300 es
  precision highp float;
  in vec3 vC; in float vZ; out vec4 frag;
  uniform float uCamDist;
  void main(){
    float fade = clamp(0.5 + (uCamDist - vZ) / 200.0, 0.12, 1.0);
    float a = 0.055 * fade;
    frag = vec4((vC * 0.6 + vec3(0.3, 0.34, 0.5)) * a, a);
  }`;

  // fullscreen post-process programs (bloom).
  const FS_VS = `#version 300 es
  layout(location=0) in vec2 corner;
  out vec2 vUv;
  void main(){ vUv = corner * 0.5 + 0.5; gl_Position = vec4(corner, 0.0, 1.0); }`;
  const BRIGHT_FS = `#version 300 es
  precision highp float; in vec2 vUv; out vec4 frag;
  uniform sampler2D uTex; uniform float uThresh;
  void main(){ vec3 c = texture(uTex, vUv).rgb; frag = vec4(max(c - uThresh, 0.0) * 1.4, 1.0); }`;
  const BLUR_FS = `#version 300 es
  precision highp float; in vec2 vUv; out vec4 frag;
  uniform sampler2D uTex; uniform vec2 uDir;
  void main(){
    vec3 s = texture(uTex, vUv).rgb * 0.227027;
    s += texture(uTex, vUv + uDir * 1.384615).rgb * 0.316216;
    s += texture(uTex, vUv - uDir * 1.384615).rgb * 0.316216;
    s += texture(uTex, vUv + uDir * 3.230769).rgb * 0.070270;
    s += texture(uTex, vUv - uDir * 3.230769).rgb * 0.070270;
    frag = vec4(s, 1.0);
  }`;
  const COMPOSITE_FS = `#version 300 es
  precision highp float; in vec2 vUv; out vec4 frag;
  uniform sampler2D uScene; uniform sampler2D uBloom; uniform float uBloomStr, uCx, uCy;
  void main(){
    float r = length(vUv - vec2(uCx, uCy));
    vec3 c = texture(uScene, vUv).rgb + texture(uBloom, vUv).rgb * uBloomStr;
    c += vec3(0.012, 0.026, 0.060) * (1.0 - smoothstep(0.0, 0.85, r)); // deep-blue ambient haze
    c = (c * (2.51 * c + 0.03)) / (c * (2.43 * c + 0.59) + 0.14);      // ACES-ish tonemap
    c *= 1.0 - 0.34 * smoothstep(0.42, 1.08, r);                       // cinematic vignette
    frag = vec4(c, 1.0);
  }`;

  const SPIN = 0.04; // disc angular-speed base (inner clusters turn faster)
  // the image sits up a touch (the near rim projects larger than the far one): clip y
  // gains LENS_UP, so the galaxy centre, and the haze + vignette with it, sit LENS_UP/2
  // above the middle in texture space
  const LENS_UP = 0.12;

  function init(canvas, G, opts) {
    const gl = canvas.getContext("webgl2", { antialias: true, alpha: false, premultipliedAlpha: false });
    // a lost context hands back its old object, on which every shader fails to compile
    // (ten console errors, then null anyway)
    if (!gl || gl.isContextLost()) return null;
    opts = opts || {};
    const commColor = opts.commColor || new Map();
    const indexId = opts.indexId || (G.meta && G.meta.index_id) || "";
    // grouping: which per-node field drives arm placement + color (the emergent
    // 'community' by default, or 'domain' for the topic view). grp() reads it.
    const groupField = opts.groupField || "community";
    const grp = (n) => n[groupField] || 0;

    const sprite = program(gl, SPRITE_VS, SPRITE_FS);
    const line = program(gl, LINE_VS, LINE_FS);
    const bright = program(gl, FS_VS, BRIGHT_FS);
    const blur = program(gl, FS_VS, BLUR_FS);
    const composite = program(gl, FS_VS, COMPOSITE_FS);
    const progs = [sprite, line, bright, blur, composite];
    if (!sprite || !line) { progs.forEach((p) => p && gl.deleteProgram(p)); return null; }
    // every buffer + VAO this galaxy makes, so dispose can free them: the Clusters/Topics
    // toggle re-inits on the same context, where each init used to strand its buffers,
    // programs and bloom targets (~31 MB at 1440x900 on a 2x display)
    const glBufs = [], glVAOs = [];
    const newBuf = (data) => { const b = gl.createBuffer(); glBufs.push(b); gl.bindBuffer(gl.ARRAY_BUFFER, b); gl.bufferData(gl.ARRAY_BUFFER, data, gl.STATIC_DRAW); return b; };
    const newVAO = () => { const v = gl.createVertexArray(); glVAOs.push(v); gl.bindVertexArray(v); return v; };

    const su = {};
    ["uProj", "uView", "uHi", "uTime", "uSpinTime", "uSizeMul", "uTwinkle", "uSoft", "uHalo", "uIntensity", "uFog", "uCamDist", "uOmega", "uSpotComm", "uSpike", "uFloat"].forEach((n) => (su[n] = gl.getUniformLocation(sprite, n)));
    const lu = {};
    ["uProj", "uView", "uCamDist", "uSpinTime", "uOmega", "uTime", "uFloat"].forEach((n) => (lu[n] = gl.getUniformLocation(line, n)));

    const nodes = G.nodes || [];
    const N = nodes.length;
    const idIndex = new Map();
    nodes.forEach((n, i) => idIndex.set(n.id, i));

    const groupList = opts.groups || G.communities || [];
    const comms = groupList.map((c) => c.id);
    const commRank = new Map(comms.map((id, i) => [id, i]));
    const C = Math.max(1, comms.length);
    const DISC_R = 165, DISC_THICK = 13, ARM_WIND = 2.2; // ARM_WIND matches the field-star spiral
    const ARMS = GAL_ARMS; // the 2D galaxy's count, so a cluster sits on the same arm in both
    const TWO_PI = Math.PI * 2;
    // Each group owns a segment of one arm, sized by its note count (packArms). A
    // 700-note cluster streams along a long stretch of arm; with equal slots it used to
    // pile into one bright clump.
    const gCount = new Map();
    for (const n of nodes) gCount.set(grp(n), (gCount.get(grp(n)) || 0) + 1);
    // a group present on the nodes but missing from the list (a stale list, a note with
    // no group) still gets its own stretch of arm; they all used to pile into one rim band
    const segIds = comms.concat([...gCount.keys()].filter((id) => !commRank.has(id)));
    for (const id of segIds) if (!commRank.has(id)) commRank.set(id, commRank.size);
    const seg = packArms(segIds.map((id) => ({ id, size: gCount.get(id) || 0 })), ARMS);
    // A cluster owns an arm and a radius band along it. armAngle() winds the angle with
    // radius on the SAME spiral as the field stars, so nodes spread along the band trace
    // the arm (the gravitational flow) instead of forming an isolated ball. The inner
    // ring is pushed out of the core so the disc reads spread, not piled at the center.
    function clusterGeom(commId) {
      const k = commRank.has(commId) ? commRank.get(commId) : C - 1;
      const sg = seg.get(commId) || { arm: k % ARMS, s0: 0.92, s1: 1 };
      const rf = (sg.s0 + sg.s1) / 2;
      const rCenter = DISC_R * (0.26 + 0.74 * rf);
      const armBase = sg.arm * (TWO_PI / ARMS) + (rand(k * 13 + 1) - 0.5) * 0.14;
      return { k, arm: sg.arm, rf, rCenter, armBase, s0: sg.s0, s1: sg.s1 };
    }
    // the field stars' ridge (radius = DISC_R * (0.06 + 0.98 t), angle + t * ARM_WIND),
    // solved for t: notes wound on plain r/DISC_R sat ~0.1 rad ahead, in the dust lane
    function armAngle(armBase, r) { return armBase + ((r / DISC_R - 0.06) / 0.98) * ARM_WIND; }
    function centroid(commId) {
      const g = clusterGeom(commId);
      const a = armAngle(g.armBase, g.rCenter);
      const cy = (rand(g.k * 7 + 3) - 0.5) * DISC_THICK * (0.35 + 0.65 * (1 - g.rf));
      return [Math.cos(a) * g.rCenter, cy, Math.sin(a) * g.rCenter];
    }

    const pos = new Float32Array(N * 3), size = new Float32Array(N), color = new Float32Array(N * 3), flag = new Float32Array(N), comm = new Float32Array(N);
    for (let i = 0; i < N; i++) {
      const n = nodes[i];
      const g = grp(n);
      comm[i] = g;
      const isSun = n.id === indexId;
      let x, y, z;
      if (isSun) { x = 0; y = 0; z = 0; }
      else {
        // Each cluster is a TANGENTIAL arc: its notes spread along the direction of
        // rotation (an arc at near-constant radius) with a little radial thickness, so
        // the cluster lies along the orbital flow instead of pointing at the center like
        // a spoke. Its center still sits on the spiral arm, so the arms still wind.
        // Spread each note WIDELY along its spiral arm so the dots scatter through the
        // galaxy and float among the field stars instead of forming a tight clump. The
        // angle winds with the note's own radius (armAngle), so the scatter follows the
        // arm's curve (aligned with rotation, not a spoke), with gentle cross-arm jitter
        // for body. Same spiral formula as the field stars, so the notes intermix.
        // The note sits somewhere along its group's arm segment, with a soft gaussian
        // scatter across the arm and out of the plane that widens toward the rim; one in
        // ten strays into the inter-arm dark so the arms have feathered edges.
        const cg = clusterGeom(g);
        const s = cg.s0 + (cg.s1 - cg.s0) * rand(i * 3 + 1);
        const gss = (a, b, c) => (rand(a) + rand(b) + rand(c) - 1.5) / 1.5;
        const stray = rand(i * 23 + 7) < 0.1 ? 2.6 : 1;
        const rr = Math.max(14, DISC_R * (0.26 + 0.74 * s) + gss(i * 13 + 4, i * 17 + 6, i * 19 + 8) * (5 + 5 * s) * stray);
        const rad = Math.min(1, rr / DISC_R);
        const a = armAngle(cg.armBase, rr) + gss(i * 5 + 9, i * 7 + 10, i * 29 + 11) * (0.12 + 0.12 * rad) * stray;
        x = Math.cos(a) * rr;
        y = gss(i * 11 + 5, i * 31 + 12, i * 37 + 13) * DISC_THICK * 0.62 * (0.55 + 0.6 * (1 - rad)) * (stray > 1 ? 1.6 : 1);
        z = Math.sin(a) * rr;
      }
      pos[i * 3] = x; pos[i * 3 + 1] = y; pos[i * 3 + 2] = z;
      size[i] = isSun ? 4.4 : Math.min(2.8, Math.max(1.1, (n.size || 1) * 0.74)); // small + crisp, like the field stars
      const rgb = hexToRGB(commColor.get(g));
      color[i * 3] = rgb[0]; color[i * 3 + 1] = rgb[1]; color[i * 3 + 2] = rgb[2];
      flag[i] = isSun ? 1 : 0;
    }

    const quad = new Float32Array([-1, -1, 1, -1, -1, 1, 1, 1]);
    function makeSprites(p, s, c, f) {
      const vao = newVAO();
      newBuf(quad);
      gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
      const mk = (loc, data, n) => {
        newBuf(data);
        gl.enableVertexAttribArray(loc); gl.vertexAttribPointer(loc, n, gl.FLOAT, false, 0, 0); gl.vertexAttribDivisor(loc, 1);
      };
      mk(1, p, 3); mk(2, s, 1); mk(3, c, 3); mk(4, f, 1);
      gl.bindVertexArray(null);
      return vao;
    }
    const nodeVAO = makeSprites(pos, size, color, flag);
    // node-only per-instance community (location 5) so the legend can spotlight a cluster.
    let spotComm = -1;
    gl.bindVertexArray(nodeVAO);
    newBuf(comm);
    gl.enableVertexAttribArray(5); gl.vertexAttribPointer(5, 1, gl.FLOAT, false, 0, 0); gl.vertexAttribDivisor(5, 1);
    const phase = new Float32Array(N);
    for (let i = 0; i < N; i++) phase[i] = flag[i] ? -1 : rand(i * 41 + 17); // -1: the sun never drifts off its corona
    newBuf(phase);
    gl.enableVertexAttribArray(6); gl.vertexAttribPointer(6, 1, gl.FLOAT, false, 0, 0); gl.vertexAttribDivisor(6, 1);
    gl.bindVertexArray(null);
    const calm = () => !!(opts.calm && opts.calm());
    const FLOAT_AMP = 1.7; // world units; notes sit ~4 apart, so neighbours visibly wander past each other

    // fullscreen quad VAO (for the bloom passes)
    const fsVAO = newVAO();
    newBuf(quad);
    gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    gl.bindVertexArray(null);

    // nebula gas, only around the 24 biggest groups of five or more notes, as in 2D: a
    // singleton has no body to glow around, and an orphan-heavy vault (every orphan is
    // its own group) drew thousands of sprites. An empty list used to put one grey cloud
    // at the rim. k (the group's rank) keeps each cloud's seed as it was.
    const nebIds = segIds.filter((id) => (gCount.get(id) || 0) >= 5)
      .sort((a, b) => gCount.get(b) - gCount.get(a) || byGroupId(a, b)).slice(0, 24);
    const neb = [];
    for (const id of nebIds) {
      const k = commRank.get(id), c = centroid(id), rgb = hexToRGB(commColor.get(id));
      for (let j = 0; j < 3; j++) {
        const ox = (rand(k * 31 + j * 7 + 1) - 0.5) * 20, oy = (rand(k * 17 + j * 5 + 2) - 0.5) * 7, oz = (rand(k * 43 + j * 3 + 3) - 0.5) * 20;
        neb.push({ p: [c[0] + ox, c[1] + oy, c[2] + oz], s: 9 + rand(k * 9 + j) * 13, c: rgb });
      }
    }
    for (let j = 0; j < 8; j++) {
      const a = rand(j * 13 + 1) * TWO_PI, r = rand(j * 7 + 2) * 18;
      neb.push({ p: [Math.cos(a) * r, (rand(j * 5 + 3) - 0.5) * 6, Math.sin(a) * r], s: 14 + rand(j * 3) * 14, c: [0.36, 0.8, 0.96] });
    }
    // pink HII / star-forming regions scattered along the arms (real galaxies glow
    // pink where new stars ignite the hydrogen) - a big realism + beauty lever.
    for (const id of nebIds) {
      const k = commRank.get(id);
      if (rand(k * 53 + 7) < 0.55) continue;
      const c = centroid(id);
      const ox = (rand(k * 29 + 2) - 0.5) * 16, oz = (rand(k * 37 + 4) - 0.5) * 16;
      neb.push({ p: [c[0] + ox, c[1] + (rand(k * 5 + 1) - 0.5) * 5, c[2] + oz], s: 6 + rand(k * 9) * 9, c: [0.95, 0.42, 0.6] });
    }
    // continuous teal disc glow: soft cyan gas spread around the mid-disc so the
    // arms read as a luminous sheet (the dominant Andromeda hue), not dark gaps.
    for (let k = 0; k < 24; k++) {
      const a = (k / 24) * TWO_PI + (rand(k * 61 + 1) - 0.5) * 0.55;
      const rr = DISC_R * (0.32 + 0.52 * rand(k * 23 + 3));
      neb.push({ p: [Math.cos(a) * rr, (rand(k * 7 + 2) - 0.5) * 5, Math.sin(a) * rr], s: 15 + rand(k * 13) * 16, c: [0.3, 0.78, 0.97] });
    }
    // pink dust-lane zone hugging the bulge: the warm magenta the reference shows
    // between the white core and the teal arms.
    for (let k = 0; k < 14; k++) {
      const a = (k / 14) * TWO_PI + rand(k * 71 + 2) * 0.6;
      const rr = DISC_R * (0.15 + 0.17 * rand(k * 19 + 5));
      neb.push({ p: [Math.cos(a) * rr, (rand(k * 9 + 1) - 0.5) * 4, Math.sin(a) * rr], s: 9 + rand(k * 11) * 12, c: [0.96, 0.4, 0.66] });
    }
    // glowing gas at each arm's tip. The rim glows the approved look shows were every
    // singleton group's clouds piled where the smallest groups pack (the ends of the
    // arms); with those gone this keeps them, at 8 bright clouds an arm instead of ~48.
    const tipCols = [[0.95, 0.5, 0.75], [0.45, 0.82, 0.97], [0.72, 0.6, 0.95], [1.0, 0.78, 0.6]];
    for (let a = 0; a < ARMS; a++) {
      for (let j = 0; j < 8; j++) {
        const rr = DISC_R * (0.95 + 0.06 * rand(a * 17 + j * 5 + 3));
        const an = armAngle(a * (TWO_PI / ARMS), rr) + (rand(a * 29 + j * 11 + 1) - 0.5) * 0.14;
        const c = tipCols[j % 4]; // x4: the brightness of the piled clouds, baked into the colour
        neb.push({ p: [Math.cos(an) * rr + (rand(a * 31 + j * 7 + 1) - 0.5) * 20, (rand(a * 3 + j) - 0.5) * 7, Math.sin(an) * rr + (rand(a * 43 + j * 3 + 3) - 0.5) * 20], s: 9 + rand(a * 19 + j * 3) * 13, c: [c[0] * 4, c[1] * 4, c[2] * 4] });
      }
    }
    const NEB = neb.length;
    const npos = new Float32Array(NEB * 3), nsize = new Float32Array(NEB), ncol = new Float32Array(NEB * 3), nflag = new Float32Array(NEB);
    neb.forEach((d, i) => { npos[i * 3] = d.p[0]; npos[i * 3 + 1] = d.p[1]; npos[i * 3 + 2] = d.p[2]; nsize[i] = d.s; ncol[i * 3] = d.c[0]; ncol[i * 3 + 1] = d.c[1]; ncol[i * 3 + 2] = d.c[2]; });
    const nebVAO = makeSprites(npos, nsize, ncol, nflag);

    // core bulge (denser + brighter than before, for a blazing core)
    const BULGE = 150;
    const bpos = new Float32Array(BULGE * 3), bsize = new Float32Array(BULGE), bcol = new Float32Array(BULGE * 3), bflag = new Float32Array(BULGE);
    for (let i = 0; i < BULGE; i++) {
      const a = rand(i * 3 + 1) * TWO_PI, u = rand(i * 7 + 2), r = Math.pow(u, 0.6) * 17;
      bpos[i * 3] = Math.cos(a) * r * 1.18; bpos[i * 3 + 1] = (rand(i * 11 + 3) - 0.5) * 3.2 * (1 - u); bpos[i * 3 + 2] = Math.sin(a) * r;
      bsize[i] = 1.1 + rand(i * 5) * 1.8;
      const warm = 0.9 + rand(i * 9) * 0.1;
      bcol[i * 3] = warm; bcol[i * 3 + 1] = warm * 0.94; bcol[i * 3 + 2] = warm * 0.84;
    }
    const bulgeVAO = makeSprites(bpos, bsize, bcol, bflag);

    // blazing core corona: concentric warm halos + a near-white centre (intensity
    // baked into the colour, since all are one instanced draw).
    const corona = [[6, 1.2, [1.0, 0.99, 0.97]], [19, 0.8, [1.0, 0.96, 0.88]], [40, 0.38, [1.0, 0.93, 0.82]], [72, 0.15, [0.95, 0.92, 0.86]], [115, 0.06, [0.78, 0.84, 0.95]]];
    const CORO = corona.length;
    const cpos = new Float32Array(CORO * 3), csize = new Float32Array(CORO), ccol = new Float32Array(CORO * 3), cflag = new Float32Array(CORO);
    corona.forEach((c, i) => { csize[i] = c[0]; const g = c[1]; ccol[i * 3] = c[2][0] * g; ccol[i * 3 + 1] = c[2][1] * g; ccol[i * 3 + 2] = c[2][2] * g; });
    const coronaVAO = makeSprites(cpos, csize, ccol, cflag);

    // edges -> tinted line buffer. One line per unordered pair (key min*N+max), as in the
    // 2D views: lines blend additively, so A->B plus B->A, or a repeated link, would draw
    // that pair twice as bright. Self-loops have no length to draw.
    const edges = G.edges || [];
    const lp = [], lc = [], lph = [], seenPair = new Set();
    for (const e of edges) {
      const a = idIndex.get(e.source), b = idIndex.get(e.target);
      if (a === undefined || b === undefined || a === b) continue;
      const key = a < b ? a * N + b : b * N + a;
      if (seenPair.has(key)) continue;
      seenPair.add(key);
      lp.push(pos[a * 3], pos[a * 3 + 1], pos[a * 3 + 2], pos[b * 3], pos[b * 3 + 1], pos[b * 3 + 2]);
      lc.push(color[a * 3], color[a * 3 + 1], color[a * 3 + 2], color[b * 3], color[b * 3 + 1], color[b * 3 + 2]);
      lph.push(phase[a], phase[b]); // each endpoint rides its own node's drift
    }
    const lineVerts = new Float32Array(lp), lineCols = new Float32Array(lc);
    const lvao = newVAO();
    newBuf(lineVerts);
    gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 3, gl.FLOAT, false, 0, 0);
    newBuf(lineCols);
    gl.enableVertexAttribArray(1); gl.vertexAttribPointer(1, 3, gl.FLOAT, false, 0, 0);
    newBuf(new Float32Array(lph));
    gl.enableVertexAttribArray(2); gl.vertexAttribPointer(2, 1, gl.FLOAT, false, 0, 0);
    gl.bindVertexArray(null);

    // background starfield (does NOT spin: the far sky)
    const STAR = 1200;
    const spos = new Float32Array(STAR * 3), ssize = new Float32Array(STAR), scol = new Float32Array(STAR * 3), sflag = new Float32Array(STAR);
    for (let i = 0; i < STAR; i++) {
      const y = 1 - (2 * i + 1) / STAR, r = Math.sqrt(Math.max(0, 1 - y * y)), phi = i * 2.399963229728653, R = 240 + rand(i * 9) * 120;
      spos[i * 3] = Math.cos(phi) * r * R; spos[i * 3 + 1] = y * R; spos[i * 3 + 2] = Math.sin(phi) * r * R;
      const bs = rand(i * 13) > 0.97;
      ssize[i] = (bs ? 1.6 : 0.45) + rand(i * 3) * 0.7;
      if (rand(i * 5) > 0.6) { scol[i * 3] = 1.0; scol[i * 3 + 1] = 0.88; scol[i * 3 + 2] = 0.72; }
      else { const w = 0.7 + rand(i * 17) * 0.3; scol[i * 3] = w * 0.86; scol[i * 3 + 1] = w * 0.92; scol[i * 3 + 2] = w * 1.1; }
    }
    const starVAO = makeSprites(spos, ssize, scol, sflag);

    // dense disc field stars tracing the arms, with DUST LANES (a dim band offset
    // from each arm ridge so the arms read as dusty, not uniform).
    const FIELD = 11000;
    const fpos = new Float32Array(FIELD * 3), fsize = new Float32Array(FIELD), fcol = new Float32Array(FIELD * 3), fflag = new Float32Array(FIELD);
    for (let i = 0; i < FIELD; i++) {
      const arm = i % ARMS;
      const t = Math.sqrt(rand(i * 3 + 1));
      const radius = DISC_R * (0.06 + 0.98 * t);
      const s1 = rand(i * 7 + 2) - 0.5, s2 = rand(i * 11 + 3) - 0.5;
      const scatter = (s1 + s2) * 0.42;
      const angle = arm * (TWO_PI / ARMS) + t * ARM_WIND + scatter;
      const rr = radius + (rand(i * 13 + 4) - 0.5) * (6 + 12 * t);
      fpos[i * 3] = Math.cos(angle) * rr;
      fpos[i * 3 + 1] = (rand(i * 5 + 6) - 0.5) * DISC_THICK * (0.45 + 0.55 * (1 - t)) + (s1 + s2) * 2.0;
      fpos[i * 3 + 2] = Math.sin(angle) * rr;
      // dust lane: a darker band a touch ahead of the arm ridge
      const lane = Math.abs(scatter - 0.16) < 0.055 ? 0.22 : 1.0;
      fsize[i] = (0.55 + rand(i * 17) * 0.9) * (lane < 1 ? 0.7 : 1);
      const w = (0.5 + rand(i * 23) * 0.42) * lane;
      const rad = Math.min(1, rr / DISC_R);            // 0 core .. 1 rim
      const roll = rand(i * 19);
      if (roll > 0.88) { // pink HII / dust-edge stars threaded through the arms
        fcol[i * 3] = w * 1.0; fcol[i * 3 + 1] = w * 0.46; fcol[i * 3 + 2] = w * 0.72;
      } else if (roll > 0.74) { // warm-white, denser toward the core
        const ww = w * (0.86 + 0.14 * (1 - rad));
        fcol[i * 3] = ww; fcol[i * 3 + 1] = ww * 0.95; fcol[i * 3 + 2] = ww * 0.86;
      } else { // teal/cyan disc, cyan toward the rim (the dominant Andromeda hue)
        fcol[i * 3] = w * (0.26 + 0.12 * (1 - rad));
        fcol[i * 3 + 1] = w * (0.84 + 0.06 * rad);
        fcol[i * 3 + 2] = w * (0.94 + 0.06 * rad);
      }
    }
    const fieldVAO = makeSprites(fpos, fsize, fcol, fflag);

    // bright foreground stars with 4-point diffraction spikes (the photographic
    // sparkle the reference shows). A handful, large, white/blue, some lifted out of
    // the disc plane so they read as nearby stars in front of the galaxy.
    const SPK = 14;
    const kpos = new Float32Array(SPK * 3), ksize = new Float32Array(SPK), kcol = new Float32Array(SPK * 3), kflag = new Float32Array(SPK);
    for (let i = 0; i < SPK; i++) {
      kpos[i * 3] = (rand(i * 91 + 1) - 0.5) * 230;       // scattered across the frame as
      kpos[i * 3 + 1] = (rand(i * 7 + 3) - 0.5) * 140;    // foreground sky stars, not tied
      kpos[i * 3 + 2] = (rand(i * 53 + 5) - 0.5) * 160;   // to the disc
      ksize[i] = rand(i * 29) > 0.85 ? 6 + rand(i * 5) * 4 : 3 + rand(i * 5) * 2.5; // mostly small, a couple slightly bigger
      if (rand(i * 13) > 0.5) { kcol[i * 3] = 0.82; kcol[i * 3 + 1] = 0.92; kcol[i * 3 + 2] = 1.0; }
      else { kcol[i * 3] = 1.0; kcol[i * 3 + 1] = 0.97; kcol[i * 3 + 2] = 0.9; }
    }
    const spikeVAO = makeSprites(kpos, ksize, kcol, kflag);

    // --- bloom FBOs (guarded; falls back to direct render) ---
    const bu = { uTex: gl.getUniformLocation(bright, "uTex"), uThresh: gl.getUniformLocation(bright, "uThresh") };
    const blu = { uTex: gl.getUniformLocation(blur, "uTex"), uDir: gl.getUniformLocation(blur, "uDir") };
    const cu = { uScene: gl.getUniformLocation(composite, "uScene"), uBloom: gl.getUniformLocation(composite, "uBloom"), uBloomStr: gl.getUniformLocation(composite, "uBloomStr"), uCx: gl.getUniformLocation(composite, "uCx"), uCy: gl.getUniformLocation(composite, "uCy") };
    // bloomSupported: the programs built. bloomOK: this SIZE has complete targets, so a
    // collapsed 0-height view renders direct and the next real resize brings bloom back
    // (one incomplete FBO used to switch it off for good)
    const bloomSupported = !!(bright && blur && composite);
    let bloomOK = false, bloomWarned = false;
    let sceneFBO = null, sceneTex = null, bFBO = [null, null], bTex = [null, null], bw = 1, bh = 1;
    function mkTex(w, h) {
      const t = gl.createTexture();
      gl.bindTexture(gl.TEXTURE_2D, t);
      gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, w, h, 0, gl.RGBA, gl.UNSIGNED_BYTE, null);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
      return t;
    }
    function mkFBO(t) {
      const f = gl.createFramebuffer();
      gl.bindFramebuffer(gl.FRAMEBUFFER, f);
      gl.framebufferTexture2D(gl.FRAMEBUFFER, gl.COLOR_ATTACHMENT0, gl.TEXTURE_2D, t, 0);
      const ok = gl.checkFramebufferStatus(gl.FRAMEBUFFER) === gl.FRAMEBUFFER_COMPLETE;
      gl.bindFramebuffer(gl.FRAMEBUFFER, null);
      if (!ok) gl.deleteFramebuffer(f);
      return ok ? f : null;
    }
    function freeBloom() {
      [sceneTex, bTex[0], bTex[1]].forEach((t) => t && gl.deleteTexture(t));
      [sceneFBO, bFBO[0], bFBO[1]].forEach((f) => f && gl.deleteFramebuffer(f));
      sceneTex = sceneFBO = null; bTex = [null, null]; bFBO = [null, null];
    }
    function setupBloom(fw, fh) {
      freeBloom(); bloomOK = false;
      if (!bloomSupported || fw < 1 || fh < 1) return; // nothing to bloom at 0 px
      bw = Math.max(1, fw >> 1); bh = Math.max(1, fh >> 1);
      sceneTex = mkTex(fw, fh); sceneFBO = mkFBO(sceneTex);
      bTex[0] = mkTex(bw, bh); bFBO[0] = mkFBO(bTex[0]);
      bTex[1] = mkTex(bw, bh); bFBO[1] = mkFBO(bTex[1]);
      bloomOK = !!(sceneFBO && bFBO[0] && bFBO[1]);
      if (!bloomOK) { freeBloom(); if (!bloomWarned) { bloomWarned = true; console.warn("mesh3d: bloom unavailable, direct render"); } }
    }

    // --- camera (cinematic fly-in, then flick inertia) ---
    // Rest framing: far enough back to hold the whole spiral (the old 175 sat on the
    // disc rim, so the near arm smeared into foreground blobs), tilted enough to read
    // the arms as a spiral rather than a lens.
    const REST_DIST = 272;
    // REST_DIST was tuned at 1440x900 with a 204px rail; a narrower visible area pulls
    // back in proportion so the whole disc still fits across (height-bound when wider).
    const REF_ASPECT = 1.37, FOVY = 1.05;
    const still = !!opts.still || calm(); // reduced motion opens at rest too, no cinematic fly-in
    // saved: a camera handed over by a re-init (the Clusters/Topics toggle, getCamera), so
    // the new galaxy opens where the old one was and never replays the fly-in
    const saved = opts.camera && typeof opts.camera === "object" ? opts.camera : null;
    // heldFocus: the note the old galaxy was locked on after the user zoomed or panned off
    // it (focusMoved). The caller's relock (focusNode) keeps that zoom and that close lets
    // go; the relock used to fly back to 42 and a close then flew all the way home.
    let heldFocus = saved && saved.focusMoved && saved.focusId != null ? saved.focusId : null;
    const fin = (v, d) => (Number.isFinite(v) ? v : d);
    const cam = saved
      ? { yaw: fin(saved.yaw, 0.6), pitch: Math.max(-1.45, Math.min(1.45, fin(saved.pitch, 0.38))), dist: Math.max(20, Math.min(1800, fin(saved.dist, REST_DIST))), cx: fin(saved.cx, 0), cy: fin(saved.cy, 0), cz: fin(saved.cz, 0) }
      : { yaw: still ? 0.6 : 2.0, pitch: 0.38, dist: still ? REST_DIST : 820, cx: 0, cy: 0, cz: 0 };
    const vel = { yaw: 0, pitch: 0 };
    let W = 1, H = 1, ins = 0, dpr = 1, proj = perspective(FOVY, 1, 1, 4000), vp = viewMatrix(cam.yaw, cam.pitch, cam.dist, 0, 0, 0);
    // firstHit: the pick of a click sequence's first click, which a double-click is judged by.
    // hoverIdx is the star under the cursor, selIdx the selected one; the selection
    // keeps the highlight, so moving the mouse off a focused star no longer drops it.
    let drag = false, panning = false, lx = 0, ly = 0, downX = 0, downY = 0, moved = false, firstHit = -1, hoverIdx = -1, selIdx = -1, renderPitch = cam.pitch;
    // the clocks carry over too, so the disc, the field stars and the drift do not jump
    let time = saved ? fin(saved.time, 0) % DRIFT_PERIOD : 0, spinTime = saved ? fin(saved.spinTime, 0) : 0;
    // userCam: zoomed or panned since the last recenter, so a resize must not refit.
    let introT = 0, introDone = still || !!saved, focusIdx = -1, anim = null, userCam = !!(saved && saved.userCam);
    // focusMoved: panned or zoomed (or cut the fly-in short) while a note is focused, so
    // closing it lets go where the user is instead of flying home. lastP: the focused
    // star's last position, so the camera can ride its drift.
    let focusMoved = false, lastP = null;
    // idle orbit: after a few quiet seconds the camera drifts slowly around the disc
    // (~95 s a lap), easing in; any input stops it dead.
    let idleT = 0, autoYaw = 0, lensX = 0, lastNow = 0;
    const touch = () => { idleT = 0; autoYaw = 0; };
    // only a real gesture cuts the fly-in short (a moved drag, a pan, a pinch, a fly to a
    // note), never a still click or wheel momentum, which froze it mid-way. The camera
    // it leaves is then the user's, so a resize must not snap it to the rest framing.
    const endIntro = () => { if (introDone) return; introDone = true; userCam = true; };
    // capped at 4x: the old 2.4 left a phone (390x844 wants 2.96) 6 px of margin at the
    // rim, so the stray notes out past it were clipped
    const restDist = () => REST_DIST * Math.max(1, Math.min(4, REF_ASPECT / Math.max(0.1, (W - ins) / Math.max(1, H))));
    // animate the orbit center + distance, used to fly into a note and back out. kind
    // "focus" homes on the star as it drifts; "rest" follows a resize to the new framing.
    function startAnim(toC, toD, after, kind) { anim = { fromC: [cam.cx, cam.cy, cam.cz], toC: toC, fromD: cam.dist, toD: toD, t: 0, after: after || null, kind: kind || "" }; }
    // the user took the camera: a resize must not refit it, and a focused note that is
    // closed now keeps it
    function userMoved() { userCam = true; if (focusIdx >= 0) focusMoved = true; }
    // a gesture mid-tween stops it where it is but still runs its completion, so an
    // interrupted fly-out still unlocks the spin instead of leaving the disc frozen.
    // The camera is then off its rest framing, so it counts as a user move.
    function stopAnim() { if (!anim) return; const cb = anim.after; anim = null; userMoved(); if (cb) cb(); }

    // lost: the GPU dropped the context (a driver reset, a sleep, too many contexts). It
    // stays set: every object this galaxy made belongs to the dead context, and after a
    // restore a delete of one is an INVALID_OPERATION, so this galaxy never draws or
    // frees again. The restore is the caller's to rebuild on (opts.onContextRestored).
    let lost = false;
    // without preventDefault the browser never restores the context (restoreContext is
    // refused), so the 3D view stayed black until a reload
    function onLost(e) { e.preventDefault(); lost = true; }
    function onRestored() { if (lost && opts.onContextRestored) opts.onContextRestored(getCamera()); }
    canvas.addEventListener("webglcontextlost", onLost);
    canvas.addEventListener("webglcontextrestored", onRestored);

    function resize() {
      dpr = Math.min(2, Math.max(1, window.devicePixelRatio || 1));
      W = window.innerWidth; H = window.innerHeight;
      canvas.width = Math.round(W * dpr); canvas.height = Math.round(H * dpr);
      canvas.style.width = W + "px"; canvas.style.height = H + "px";
      proj = perspective(FOVY, W / Math.max(1, H), 1, 4000);
      // lens shift: slide the image right by half the rail width so the galaxy centres
      // in the visible area. Picking multiplies the same proj, so it stays exact.
      ins = opts.inset ? opts.inset() || 0 : 0;
      lensX = ins / Math.max(1, W);
      proj[8] -= lensX;
      proj[9] -= LENS_UP; // and up a touch: the near rim projects larger than the far one
      if (!lost) setupBloom(canvas.width, canvas.height); // a lost context makes no targets: no false "bloom unavailable"
      // a camera still at rest refits, so a narrow window keeps the whole disc in view
      if (introDone && focusIdx < 0 && !anim && !userCam) cam.dist = restDist();
      if (anim && anim.kind === "rest") anim.toD = restDist(); // and a recenter in flight lands on the new one
    }
    resize();

    // hitR: the hit radius in px for a dot of a given on-screen radius (a tap passes
    // TAP_R); the cursor's is the dot itself, never under 9
    function pickAt(mx, my, hitR) {
      const v = viewMatrix(cam.yaw, renderPitch, cam.dist, cam.cx, cam.cy, cam.cz);
      const m = mul(proj, v);
      let best = -1, bestD = Infinity;
      for (let i = 0; i < N; i++) {
        const sp = worldPos(i);
        const cx = m[0] * sp[0] + m[4] * sp[1] + m[8] * sp[2] + m[12];
        const cy = m[1] * sp[0] + m[5] * sp[1] + m[9] * sp[2] + m[13];
        const cw = m[3] * sp[0] + m[7] * sp[1] + m[11] * sp[2] + m[15];
        if (cw <= 0) continue;
        const sx = (cx / cw * 0.5 + 0.5) * W, sy = (1 - (cy / cw * 0.5 + 0.5)) * H;
        const dx = sx - mx, dy = sy - my, d2 = dx * dx + dy * dy;
        const dot = size[i] * 90 / cw, rad = hitR ? hitR(dot) : Math.max(9, dot);
        if (d2 <= rad * rad && d2 < bestD) { bestD = d2; best = i; }
      }
      return best;
    }

    function worldPos(i) {
      const f = calm() ? [0, 0, 0] : driftJS(phase[i], time, FLOAT_AMP);
      return spinJS(pos[i * 3] + f[0], pos[i * 3 + 1] + f[1], pos[i * 3 + 2] + f[2], spinTime, SPIN);
    }
    // pan: slide the orbit target across the screen plane. right and up are rows 0
    // and 1 of Rx(pitch)*Ry(yaw) from viewMatrix; k is world units per px at the
    // target depth, so the scene there tracks the cursor 1:1. dx,dy = px the content moves.
    function pan(dx, dy) {
      const k = 2 * cam.dist * Math.tan(FOVY / 2) / Math.max(1, H);
      const cyw = Math.cos(cam.yaw), syw = Math.sin(cam.yaw), cp = Math.cos(renderPitch), sp = Math.sin(renderPitch);
      // target -= right*dx*k, target += up*dy*k (right = [cyw,0,syw], up = [sp*syw,cp,-sp*cyw])
      cam.cx += (-dx * cyw + dy * sp * syw) * k;
      cam.cy += dy * cp * k;
      cam.cz += (-dx * syw - dy * sp * cyw) * k;
      endIntro(); userMoved(); stopAnim(); touch();
    }
    // a mouse event within a moment of a touch is the browser's echo of that touch (the
    // cancelled pointerdown should stop them; this is the belt to that), never a pick
    let lastTouchT = -1e9;
    const fromTouch = () => performance.now() - lastTouchT < 800;
    function onDown(e) {
      if (e.button > 2 || fromTouch()) return; // back/forward buttons are the browser's
      // right, middle or shift+left drag pans; plain left drag orbits
      panning = e.button === 1 || e.button === 2 || (e.button === 0 && e.shiftKey);
      if (e.button === 1) e.preventDefault(); // no middle-click autoscroll
      if (panning) canvas.style.cursor = "move";
      if (e.detail <= 1) firstHit = -1; // a fresh click sequence
      // no endIntro here: a still click must not freeze the fly-in (the first real drag step does)
      drag = true; moved = false; lx = downX = e.clientX; ly = downY = e.clientY; vel.yaw = 0; vel.pitch = 0; touch();
    }
    function onUp(e) {
      if (!drag) return; // a mouseup that began off this canvas (the rail, the 2D view) is not a pick
      drag = false;
      const wasPan = panning; panning = false;
      if (canvas.hidden) return; // the view switched mid-press: nothing under the cursor is ours
      // the second click of a double-click (detail > 1) is onDbl's: the first click's
      // fly-in has moved the star out from under the cursor, so a re-pick would miss and
      // close the note, or land on a neighbour and open that instead
      if (!moved && !wasPan && e.detail <= 1 && opts.onSelect) {
        const rect = canvas.getBoundingClientRect();
        const i = pickAt(e.clientX - rect.left, e.clientY - rect.top);
        selIdx = i; firstHit = i; // before onSelect, so the app's focusNode or clearFocus has the last word
        opts.onSelect(i >= 0 ? nodes[i] : null);
      }
    }
    function onMove(e) {
      if (drag) {
        // the view switched mid-press: let go, or the hidden camera orbits on unseen and
        // the 3D view comes back turned (onUp then has no press to pick with)
        if (canvas.hidden) { drag = false; panning = false; return; }
        // distance from the press, not per event, so a slow orbit still counts as a drag
        if (Math.abs(e.clientX - downX) + Math.abs(e.clientY - downY) > 3) moved = true;
        // moved with no button held: the mouseup went elsewhere (another window), so end
        // the gesture without a pick. Never on a still press, whose mouseup may yet pick.
        if (moved && !(e.buttons & 7)) { drag = false; panning = false; return; }
        // a still press is a click, and jitter must not cut a fly-in short (as in 2D).
        // lx,ly stay at the press until then, so the first real step carries it all.
        if (!moved) return;
        endIntro();
        const dx = e.clientX - lx, dy = e.clientY - ly; lx = e.clientX; ly = e.clientY;
        if (panning) { pan(dx, dy); return; }
        const dyaw = dx * 0.005, dpitch = dy * 0.005;
        cam.yaw += dyaw; cam.pitch = Math.max(-1.45, Math.min(1.45, cam.pitch + dpitch));
        vel.yaw = dyaw; vel.pitch = dpitch; stopAnim(); touch(); // dragging cancels a fly-in tween
      } else if (!canvas.hidden && !fromTouch()) {
        // the listener is on window, so a hidden 3D view must not pick under the 2D one
        touch(); // any input stops the idle orbit, a hover too
        if (!opts.onHover) return;
        // over the rail, the card or the reader it is empty sky: no picking through them
        let i = -1;
        if (e.target === canvas) {
          const rect = canvas.getBoundingClientRect();
          i = pickAt(e.clientX - rect.left, e.clientY - rect.top);
          canvas.style.cursor = i >= 0 ? "pointer" : "grab";
        }
        if (i !== hoverIdx) { hoverIdx = i; opts.onHover(i >= 0 ? nodes[i] : null); }
      }
    }
    // app.js passes its wheel reading (px units, shift+wheel as sideways, pan or zoom) so
    // every view reads a wheel the same way; alone, the wheel only zooms
    const wheelOf = opts.wheel || ((e) => ({ pan: false, dx: 0, dy: e.deltaY }));
    function onWheel(e) {
      e.preventDefault(); touch();
      // the intro and a tween own the camera; trackpad momentum must not cut either short
      if (!introDone || anim) return;
      const w = wheelOf(e);
      if (w.pan) { pan(-w.dx, -w.dy); return; } // content follows the fingers
      userMoved();
      cam.dist *= Math.exp(w.dy * 0.001); cam.dist = Math.max(20, Math.min(1800, cam.dist));
    }
    // double-click on empty sky recenters. On a star the first click already flew in and
    // opened it; judged by that click's pick, since the fly-in has moved the star by now.
    function onDbl() { if (!moved && firstHit < 0 && !fromTouch()) recenter(); }
    const onCtx = (e) => e.preventDefault(); // right-drag pans, so no context menu

    // touch: one finger orbits; two pan (their midpoint, content follows the fingers)
    // and pinch (their spread scales cam.dist) at once; a tap picks and a second tap on
    // empty sky recenters, as the mouse's click and double-click. Mouse and pen keep the
    // mouse handlers above. Pointer capture keeps a finger that slides off the canvas
    // ours, touch-action none keeps the page from scrolling or zooming under it, and the
    // cancelled pointerdown stops the browser echoing the touch as mouse events.
    const tps = new Map(); // pointerId -> {x, y}: at most two fingers, extras are ignored
    let tMoved = false, tMulti = false, tDownX = 0, tDownY = 0, pinch = null, lastTap = null;
    const touchActionWas = canvas.style.touchAction;
    canvas.style.touchAction = "none";
    function twoFingers() {
      const [a, b] = [...tps.values()];
      return { mx: (a.x + b.x) / 2, my: (a.y + b.y) / 2, d: Math.hypot(a.x - b.x, a.y - b.y) };
    }
    // A pen's press beats touch (palm rejection), as in 2D: a palm resting first lets go,
    // so its moves neither orbit nor mark the pen's mousedown, which follows, as a touch's
    // echo; and while that pen is down no touch is taken.
    let penDrag = false;
    function onPDown(e) {
      if (e.pointerType !== "touch") {
        penDrag = e.pointerType === "pen"; // the press whose mousedown comes next
        if (penDrag) { tps.clear(); pinch = null; lastTouchT = -1e9; }
        return;
      }
      if (drag && penDrag) return;
      // a primary touch starts afresh (as in 2D): no other finger is down, so one still
      // held here is a release that never arrived. Kept, it held the camera still, made
      // every swipe a pinch, swallowed every tap, and two of them refused all fingers.
      if (e.isPrimary && tps.size) { tps.clear(); pinch = null; }
      if (tps.size >= 2) return;
      e.preventDefault();
      try { canvas.setPointerCapture(e.pointerId); } catch (_) { /* the pointer is already gone */ }
      tps.set(e.pointerId, { x: e.clientX, y: e.clientY });
      lastTouchT = performance.now(); vel.yaw = 0; vel.pitch = 0; touch();
      if (tps.size === 1) { tMoved = false; tMulti = false; tDownX = e.clientX; tDownY = e.clientY; }
      else { tMulti = true; pinch = twoFingers(); } // a two-finger touch is never a tap
    }
    function onPMove(e) {
      const p = tps.get(e.pointerId);
      if (!p) return;
      lastTouchT = performance.now(); touch();
      if (tps.size === 2) {
        p.x = e.clientX; p.y = e.clientY;
        const c = twoFingers();
        if (pinch) {
          pan(c.mx - pinch.mx, c.my - pinch.my); // ends the intro, stops a tween, counts as the user's camera
          if (pinch.d > 0 && c.d > 0) cam.dist = Math.max(20, Math.min(1800, cam.dist * pinch.d / c.d));
        }
        pinch = c;
        return;
      }
      // one finger: a still press is a tap, so, as with the mouse, nothing moves until
      // the finger has really travelled (a fingertip jitters more than a cursor), and the
      // first real step then carries the whole way from the press
      if (!tMoved && Math.abs(e.clientX - tDownX) + Math.abs(e.clientY - tDownY) <= TAP_SLOP) return;
      tMoved = true; endIntro();
      const dyaw = (e.clientX - p.x) * 0.005, dpitch = (e.clientY - p.y) * 0.005;
      p.x = e.clientX; p.y = e.clientY;
      cam.yaw += dyaw; cam.pitch = Math.max(-1.45, Math.min(1.45, cam.pitch + dpitch));
      vel.yaw = dyaw; vel.pitch = dpitch; stopAnim(); // a flick coasts on after the lift, as a mouse flick does
    }
    // pointerup, and pointercancel / lostpointercapture (the browser took the touch, or
    // the view was hidden mid-gesture), which only let go
    function onPUp(e) {
      if (!tps.has(e.pointerId)) return;
      tps.delete(e.pointerId);
      lastTouchT = performance.now(); pinch = null;
      if (tps.size) { tMoved = true; return; } // the finger left behind orbits on from where it is
      if (e.type !== "pointerup" || tMoved || tMulti || canvas.hidden) return;
      const rect = canvas.getBoundingClientRect();
      const x = e.clientX - rect.left, y = e.clientY - rect.top, t = lastTouchT;
      // the second tap of a double-tap is judged by the first tap's pick (as onDbl): the
      // first tap on a star already flew in and opened it, one on empty sky recenters
      if (lastTap && t - lastTap.t < DBL_TAP_MS && Math.hypot(x - lastTap.x, y - lastTap.y) < DBL_TAP_PX) {
        const first = lastTap.hit; lastTap = null;
        if (first < 0) recenter();
        return;
      }
      const i = pickAt(x, y, TAP_R); // a fingertip is wider than a cursor
      lastTap = { t, x, y, hit: i };
      selIdx = i;
      if (opts.onSelect) opts.onSelect(i >= 0 ? nodes[i] : null);
    }

    canvas.addEventListener("mousedown", onDown);
    window.addEventListener("mouseup", onUp);
    window.addEventListener("mousemove", onMove);
    canvas.addEventListener("wheel", onWheel, { passive: false });
    canvas.addEventListener("dblclick", onDbl);
    canvas.addEventListener("contextmenu", onCtx);
    canvas.addEventListener("pointerdown", onPDown);
    canvas.addEventListener("pointermove", onPMove);
    canvas.addEventListener("pointerup", onPUp);
    canvas.addEventListener("pointercancel", onPUp);
    canvas.addEventListener("lostpointercapture", onPUp);

    function setHighlight(id) { selIdx = (id != null && idIndex.has(id)) ? idIndex.get(id) : -1; }
    function setSpotlight(commId) { spotComm = (commId == null) ? -1 : commId; } // legend cluster spotlight

    // Fly the camera into a note and lock onto it (the disc spin freezes and the camera
    // rides the note's drift, so it stays put while you read). Reduced motion jumps, and
    // so does a view off screen: a tween started there would play out on return instead.
    // The relock of a note the user had zoomed off (heldFocus) keeps their distance.
    // Off screen is hidden (the 2D view) or not laid out: a feature panel hides the canvas
    // by CSS (display none, so no width), and app.js steps no frame behind it.
    const unseen = () => canvas.hidden || !canvas.clientWidth;
    function focusNode(id) {
      const i = (id != null && idIndex.has(id)) ? idIndex.get(id) : -1;
      if (i < 0) return;
      const held = heldFocus != null && heldFocus === id; heldFocus = null;
      selIdx = i; focusIdx = i; focusMoved = held; lastP = null; endIntro(); touch();
      const sp = worldPos(i), d = held ? cam.dist : 42;
      if (calm() || unseen()) { anim = null; cam.cx = sp[0]; cam.cy = sp[1]; cam.cz = sp[2]; cam.dist = d; return; }
      startAnim(sp, d, null, "focus");
    }
    // Closing a note eases back out and resumes the spin, unless the user panned or
    // zoomed while reading: then it only lets go (the spin resumes) and keeps their camera.
    function clearFocus() {
      selIdx = -1; heldFocus = null; // the note is closed: its star lets go of the highlight
      if (focusIdx < 0 && !anim) return; // nothing to leave: a user pan stays put
      if (focusIdx >= 0 && focusMoved) { focusIdx = -1; return; }
      recenter();
    }
    // back to the index (the sun, at the origin) at the fitted rest framing. Reduced
    // motion jumps instead of flying, as does a view off screen (a note closed from 2D or
    // behind a panel): the fly-out used to replay on return, the disc frozen for its ~0.7 s.
    function recenter() {
      const done = () => { focusIdx = -1; };
      endIntro(); userCam = false; focusMoved = false; heldFocus = null; touch(); // back at rest: a resize refits again
      if (!calm() && !unseen()) { startAnim([0, 0, 0], restDist(), done, "rest"); return; }
      anim = null; cam.cx = 0; cam.cy = 0; cam.cz = 0; cam.dist = restDist(); done();
    }

    function setLayer(o) {
      gl.uniform1f(su.uSizeMul, o.sizeMul == null ? 1 : o.sizeMul);
      gl.uniform1f(su.uSoft, o.soft); gl.uniform1f(su.uHalo, o.halo || 0);
      gl.uniform1f(su.uIntensity, o.intensity); gl.uniform1f(su.uTwinkle, calm() ? 0 : o.twinkle || 0); // reduced motion: no twinkle
      gl.uniform1f(su.uFog, o.fog || 0); gl.uniform1f(su.uHi, o.hi == null ? -1 : o.hi);
      gl.uniform1f(su.uOmega, o.omega || 0); gl.uniform1f(su.uSpotComm, o.spot == null ? -1 : o.spot);
      gl.uniform1f(su.uSpike, o.spk || 0); gl.uniform1f(su.uFloat, o.float || 0);
    }
    function drawLayer(vao, count, o) { setLayer(o); gl.bindVertexArray(vao); gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, 4, count); }

    function drawScene() {
      gl.clearColor(0.010, 0.014, 0.034, 1);
      gl.clear(gl.COLOR_BUFFER_BIT);
      gl.enable(gl.BLEND); gl.blendFunc(gl.SRC_ALPHA, gl.ONE); gl.disable(gl.DEPTH_TEST);

      gl.useProgram(sprite);
      gl.uniformMatrix4fv(su.uProj, false, proj); gl.uniformMatrix4fv(su.uView, false, vp);
      gl.uniform1f(su.uTime, time); gl.uniform1f(su.uSpinTime, spinTime); gl.uniform1f(su.uCamDist, cam.dist);

      drawLayer(nebVAO, NEB, { soft: 1.0, intensity: 0.19, fog: 0.8, omega: SPIN });
      drawLayer(starVAO, STAR, { soft: 6.0, intensity: 1.0, twinkle: 1.0, fog: 0.0, omega: 0 });
      drawLayer(fieldVAO, FIELD, { soft: 6.0, halo: 0.16, intensity: 1.52, twinkle: 0.8, fog: 0.55, omega: SPIN });

      if (lineVerts.length) {
        gl.useProgram(line);
        gl.uniformMatrix4fv(lu.uProj, false, proj); gl.uniformMatrix4fv(lu.uView, false, vp);
        gl.uniform1f(lu.uCamDist, cam.dist); gl.uniform1f(lu.uSpinTime, spinTime); gl.uniform1f(lu.uOmega, SPIN); // edges spin with their endpoints
        gl.uniform1f(lu.uTime, time); gl.uniform1f(lu.uFloat, calm() ? 0 : FLOAT_AMP);
        gl.bindVertexArray(lvao); gl.drawArrays(gl.LINES, 0, lineVerts.length / 3);
        gl.useProgram(sprite);
        gl.uniformMatrix4fv(su.uProj, false, proj); gl.uniformMatrix4fv(su.uView, false, vp);
        gl.uniform1f(su.uTime, time); gl.uniform1f(su.uSpinTime, spinTime); gl.uniform1f(su.uCamDist, cam.dist);
      }

      drawLayer(coronaVAO, CORO, { soft: 1.2, intensity: 1.0, fog: 0.0, omega: 0 });
      drawLayer(bulgeVAO, BULGE, { soft: 4.0, halo: 0.1, intensity: 0.8, twinkle: 0.6, fog: 0.3, omega: SPIN });
      drawLayer(nodeVAO, N, { soft: 6.2, halo: 0.18, intensity: 1.55, twinkle: 0.85, fog: 0.6, sizeMul: 1.0, hi: selIdx >= 0 ? selIdx : hoverIdx, omega: SPIN, spot: spotComm, float: calm() ? 0 : FLOAT_AMP }); // crisp star profile, same spin as the field stars
      drawLayer(spikeVAO, SPK, { soft: 6.0, intensity: 1.3, twinkle: 0.7, fog: 0.0, omega: 0, spk: 0.6 });
      gl.bindVertexArray(null);
    }

    function frame() {
      if (lost) return; // nothing to draw onto until the caller rebuilds on the restore
      // step by real time in 60 Hz frames (k = 1 at 60 Hz, as everything below was tuned),
      // so a 120 Hz display does not run the galaxy at double speed; clamped to 0.1 s so a
      // stall or a return from the 2D view does not leap
      const now = performance.now();
      const k = lastNow ? Math.min(6, Math.max(0, (now - lastNow) * 0.06)) : 1;
      lastNow = now;
      if (!calm()) time = (time + 0.03 * k) % DRIFT_PERIOD; // reduced motion also stills the sun's pulse
      if (focusIdx < 0 && !calm()) { // the disc turns only when not locked on a note (and not under reduced motion)
        spinTime += 0.03 * k;
        // a panned target rides the disc at its own radius (the same per-frame turn as the
        // stars there), so the region you panned to stays in view instead of turning away
        if (!anim && (cam.cx || cam.cz)) { const c = spinJS(cam.cx, cam.cy, cam.cz, 0.03 * k, SPIN); cam.cx = c[0]; cam.cz = c[2]; }
      }
      if (focusIdx >= 0) {
        // locked on a note: the fly-in homes on where the star is now, then the target
        // moves by the star's own drift step, so the note holds still while you read and
        // a pan made while reading keeps its offset
        const p = worldPos(focusIdx);
        if (anim && anim.kind === "focus") anim.toC = p;
        else if (!anim && lastP) { cam.cx += p[0] - lastP[0]; cam.cy += p[1] - lastP[1]; cam.cz += p[2] - lastP[2]; }
        lastP = p;
      }
      if (!introDone) {
        introT += 0.016 * k;
        const p = Math.min(1, introT / 2.8), e = 1 - Math.pow(1 - p, 3);
        cam.dist = 820 - (820 - restDist()) * e;
        cam.yaw = 2.0 - 1.4 * e;
        if (p >= 1) introDone = true;
      } else if (anim) {
        anim.t += 0.016 / 0.8 * k;
        const p = Math.min(1, anim.t), e = 1 - Math.pow(1 - p, 3);
        cam.cx = anim.fromC[0] + (anim.toC[0] - anim.fromC[0]) * e;
        cam.cy = anim.fromC[1] + (anim.toC[1] - anim.fromC[1]) * e;
        cam.cz = anim.fromC[2] + (anim.toC[2] - anim.fromC[2]) * e;
        cam.dist = anim.fromD + (anim.toD - anim.fromD) * e;
        if (p >= 1) { const cb = anim.after; anim = null; if (cb) cb(); }
      } else if (!drag && !tps.size) { // a finger on the glass holds the camera as a held button does
        cam.yaw += vel.yaw * k; cam.pitch = Math.max(-1.45, Math.min(1.45, cam.pitch + vel.pitch * k));
        const fr = Math.pow(0.94, k); vel.yaw *= fr; vel.pitch *= fr;
        idleT += k;
        if (idleT > 240 && focusIdx < 0 && !calm()) { autoYaw += (0.0011 - autoYaw) * (1 - Math.pow(0.994, k)); cam.yaw += autoYaw * k; }
      }
      renderPitch = cam.pitch + (calm() ? 0 : Math.sin(time * 0.18) * 0.03);
      vp = viewMatrix(cam.yaw, renderPitch, cam.dist, cam.cx, cam.cy, cam.cz);

      if (bloomOK) {
        gl.bindFramebuffer(gl.FRAMEBUFFER, sceneFBO);
        gl.viewport(0, 0, canvas.width, canvas.height);
        drawScene();
        // bright-pass downsample
        gl.disable(gl.BLEND);
        gl.bindFramebuffer(gl.FRAMEBUFFER, bFBO[0]); gl.viewport(0, 0, bw, bh);
        gl.useProgram(bright); gl.activeTexture(gl.TEXTURE0); gl.bindTexture(gl.TEXTURE_2D, sceneTex);
        gl.uniform1i(bu.uTex, 0); gl.uniform1f(bu.uThresh, 0.32);
        gl.bindVertexArray(fsVAO); gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
        // separable blur, ping-pong
        gl.useProgram(blur); gl.uniform1i(blu.uTex, 0);
        let src = 0, dst = 1;
        for (let k = 0; k < 4; k++) {
          gl.bindFramebuffer(gl.FRAMEBUFFER, bFBO[dst]); gl.viewport(0, 0, bw, bh);
          gl.bindTexture(gl.TEXTURE_2D, bTex[src]);
          if (k % 2 === 0) gl.uniform2f(blu.uDir, 1 / bw, 0); else gl.uniform2f(blu.uDir, 0, 1 / bh);
          gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
          const t = src; src = dst; dst = t;
        }
        // composite to screen
        gl.bindFramebuffer(gl.FRAMEBUFFER, null); gl.viewport(0, 0, canvas.width, canvas.height);
        gl.useProgram(composite);
        gl.activeTexture(gl.TEXTURE0); gl.bindTexture(gl.TEXTURE_2D, sceneTex); gl.uniform1i(cu.uScene, 0);
        gl.activeTexture(gl.TEXTURE1); gl.bindTexture(gl.TEXTURE_2D, bTex[src]); gl.uniform1i(cu.uBloom, 1);
        gl.uniform1f(cu.uBloomStr, 1.34); gl.uniform1f(cu.uCx, 0.5 + lensX / 2); gl.uniform1f(cu.uCy, 0.5 + LENS_UP / 2);
        gl.bindVertexArray(fsVAO); gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
        gl.bindVertexArray(null); gl.activeTexture(gl.TEXTURE0);
      } else {
        gl.bindFramebuffer(gl.FRAMEBUFFER, null);
        gl.viewport(0, 0, canvas.width, canvas.height);
        drawScene();
      }
    }

    // keepContext: detach listeners but KEEP the live WebGL context, so the caller can
    // immediately re-init on the same canvas (used by the Clusters/Topics toggle). Losing
    // the context here would leave the next init rendering onto a dead context (white).
    function dispose(keepContext) {
      canvas.removeEventListener("mousedown", onDown);
      window.removeEventListener("mouseup", onUp);
      window.removeEventListener("mousemove", onMove);
      canvas.removeEventListener("wheel", onWheel);
      canvas.removeEventListener("dblclick", onDbl);
      canvas.removeEventListener("contextmenu", onCtx);
      canvas.removeEventListener("pointerdown", onPDown);
      canvas.removeEventListener("pointermove", onPMove);
      canvas.removeEventListener("pointerup", onPUp);
      canvas.removeEventListener("pointercancel", onPUp);
      canvas.removeEventListener("lostpointercapture", onPUp);
      canvas.removeEventListener("webglcontextlost", onLost);
      canvas.removeEventListener("webglcontextrestored", onRestored);
      tps.clear(); canvas.style.touchAction = touchActionWas;
      // a context lost under this galaxy took its objects with it: no GL call at all, so
      // the caller can drop it from onContextRestored and re-init on the restored context
      if (lost || gl.isContextLost()) { lost = true; return; }
      // free this galaxy's GL objects on either path: a kept context lives on into the
      // next init, and a lost one may not let go until it is collected
      gl.bindVertexArray(null); gl.useProgram(null); gl.bindBuffer(gl.ARRAY_BUFFER, null); gl.bindFramebuffer(gl.FRAMEBUFFER, null);
      freeBloom();
      glVAOs.forEach((v) => gl.deleteVertexArray(v)); glBufs.forEach((b) => gl.deleteBuffer(b));
      progs.forEach((p) => p && gl.deleteProgram(p));
      glVAOs.length = 0; glBufs.length = 0; progs.length = 0;
      if (!keepContext) { const ext = gl.getExtension("WEBGL_lose_context"); if (ext) ext.loseContext(); }
    }

    // The camera to hand to the next init (opts.camera) when the galaxy is rebuilt, so the
    // Clusters/Topics toggle keeps the view instead of replaying the fly-in. A fly-in or
    // a recenter in flight reports where it lands. userCam is true whenever the camera is
    // off its rest framing (moved, or on a note), so the new galaxy's first resize does
    // not snap it home; a focus is the caller's to restore with focusNode (focusId: the
    // note it is locked on or flying into, else null). focusMoved: the user zoomed or
    // panned off that note, so the relock keeps dist and a close lets go where it is.
    function getCamera() {
      const clocks = { spinTime, time };
      if (!introDone || (anim && anim.kind === "rest")) return { yaw: introDone ? cam.yaw : 0.6, pitch: cam.pitch, dist: restDist(), cx: 0, cy: 0, cz: 0, userCam: false, focusId: null, focusMoved: false, ...clocks };
      const f = focusIdx >= 0;
      return { yaw: cam.yaw, pitch: cam.pitch, dist: cam.dist, cx: cam.cx, cy: cam.cy, cz: cam.cz, userCam: userCam || f || !!anim, focusId: f ? nodes[focusIdx].id : null, focusMoved: f && focusMoved, ...clocks };
    }

    // the caller's check for a context that went and has not come back (onContextRestored
    // is the other half): a lost galaxy draws nothing, so it is let go and re-initialised
    const isLost = () => lost;

    return { frame, resize, dispose, setHighlight, setSpotlight, focusNode, clearFocus, recenter, getCamera, isLost, setData() {} };
  }

  // packArms and byGroupId are the one copy: the 2D galaxy deals (and ties) with these,
  // and its taps follow the same TAP_ rules
  window.Mesh3D = { init, packArms, byGroupId, GAL_ARMS, TAP_SLOP, DBL_TAP_MS, DBL_TAP_PX, TAP_R };
})();
