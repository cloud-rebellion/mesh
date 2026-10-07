// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const { createHash } = require('node:crypto');
const REPO = 'cloud-rebellion/mesh';
const RELEASES = `https://api.github.com/repos/${REPO}/releases`;
const MAX_VSIX = 16 * 1024 * 1024;
const stable = value => typeof value === 'string' && value === value.trim() && /^(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})$/.test(value);
function compare(a, b) {
  if (!stable(a) || !stable(b)) throw new Error('Invalid stable IDE version');
  const x = a.split('.').map(Number), y = b.split('.').map(Number);
  for (let i = 0; i < 3; i++) if (x[i] !== y[i]) return x[i] - y[i];
  return 0;
}
function assetURL(tag, name) { return `https://github.com/${REPO}/releases/download/${tag}/${name}`; }
function manifest(value, version) {
  if (!stable(version) || !value || value.schema !== 1 || value.extension !== 'bright-interaction.mesh-workspace' ||
      value.version !== version || value.dirty !== false || typeof value.source_commit !== 'string' || value.source_commit.length !== 40 || !/^[a-f0-9]{40}$/.test(value.source_commit) ||
      value.file !== `mesh-workspace-${version}.vsix` || !Number.isSafeInteger(value.bytes) || value.bytes < 1 || value.bytes > MAX_VSIX ||
      typeof value.sha256 !== 'string' || value.sha256.length !== 64 || !/^[a-f0-9]{64}$/.test(value.sha256)) throw new Error('Invalid IDE release manifest');
  // Schema 1 releases predating coordinated updates omit both fields. New
  // releases bind their bundled viewer to a core release and supported API.
  if (value.mesh_release !== undefined || value.viewer_api !== undefined) {
    if (typeof value.mesh_release !== 'string' || !value.mesh_release.startsWith('v') ||
        !stable(value.mesh_release.slice(1)) || value.viewer_api !== 1) throw new Error('Invalid Mesh release pairing');
  }
  return value;
}
function verifyBytes(bytes, info) {
  manifest(info, info.version);
  if (bytes.length !== info.bytes || createHash('sha256').update(bytes).digest('hex') !== info.sha256) throw new Error('IDE download checksum or size mismatch');
  return bytes;
}
// Only our fixed public release endpoints and GitHub's asset CDN. No credentials,
// cookies, vault paths, workspace configuration or server-provided next URLs.
function allowedURL(raw, initial) {
  const url = new URL(raw);
  if (url.protocol !== 'https:' || url.username || url.password || url.port || url.hash) throw new Error('Untrusted release URL');
  if (raw === initial || url.hostname === 'release-assets.githubusercontent.com') return url;
  throw new Error('Untrusted release redirect');
}
async function readBounded(url, limit, { signal, fetchImpl = globalThis.fetch, timeout = 20000 } = {}) {
  if (!(url.startsWith(RELEASES + '?per_page=100&page=') || /^https:\/\/github\.com\/cloud-rebellion\/mesh\/releases\/download\/ide-v[\d.]+\/(manifest\.json|SHA256SUMS|mesh-workspace-[\d.]+\.vsix)$/.test(url))) throw new Error('Untrusted release endpoint');
  const controller = new AbortController();
  const abort = () => controller.abort();
  if (signal?.aborted) abort();
  signal?.addEventListener('abort', abort, { once: true });
  const timer = setTimeout(abort, timeout);
  let response;
  try {
    let current = url;
    for (let i = 0; i <= 3; i++) {
      if (controller.signal.aborted) throw new Error('Release request cancelled');
      allowedURL(current, url);
      response = await fetchImpl(current, { redirect: 'manual', credentials: 'omit', signal: controller.signal, headers: { 'User-Agent': 'Mesh-IDE', Accept: current.startsWith(RELEASES) ? 'application/vnd.github+json' : 'application/octet-stream' } });
      if ([301, 302, 303, 307, 308].includes(response.status)) {
        await response.body?.cancel();
        const location = response.headers.get('location');
        if (!location || i === 3) throw new Error('Invalid release redirect');
        current = new URL(location, current).href;
        continue;
      }
      if (!response.ok) throw new Error('Release service unavailable');
      const length = response.headers.get('content-length');
      if (length !== null && (!/^\d+$/.test(length) || Number(length) > limit)) throw new Error('Release response too large');
      const reader = response.body?.getReader();
      if (!reader) throw new Error('Empty release response');
      const chunks = []; let size = 0;
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          size += value.byteLength;
          if (size > limit) throw new Error('Release response too large');
          chunks.push(Buffer.from(value));
        }
      } finally { await reader.cancel(); }
      return Buffer.concat(chunks, size);
    }
  } finally {
    controller.abort(); clearTimeout(timer); signal?.removeEventListener('abort', abort);
    if (response?.body && !response.body.locked) await response.body.cancel().catch(() => {});
  }
}
async function checkUpdate(installed, options = {}) {
  compare(installed, installed);
  let best;
  // Bound discovery even if the repository accumulates many core releases.
  for (let page = 1; page <= 3; page++) {
    const rows = JSON.parse((await readBounded(`${RELEASES}?per_page=100&page=${page}`, 2 * 1024 * 1024, options)).toString());
    if (!Array.isArray(rows) || rows.length > 100) throw new Error('Invalid release listing');
    for (const row of rows) {
      if (!row || row.draft !== false || row.prerelease !== false || typeof row.tag_name !== 'string' || !row.tag_name.startsWith('ide-v')) continue;
      const version = row.tag_name.slice(5);
      if (stable(version) && compare(version, installed) > 0 && (!best || compare(version, best.version) > 0)) best = { version, tag: row.tag_name, assets: row.assets };
    }
    if (rows.length < 100) break;
  }
  if (!best) return null;
  const info = manifest(JSON.parse((await readBounded(assetURL(best.tag, 'manifest.json'), 16384, options)).toString()), best.version);
  for (const name of [info.file, 'manifest.json', 'SHA256SUMS']) {
    const assets = Array.isArray(best.assets) ? best.assets.filter(a => a.name === name) : [];
    if (assets.length !== 1 || assets[0].state !== 'uploaded' || assets[0].browser_download_url !== assetURL(best.tag, name)) throw new Error('Incomplete or untrusted IDE release');
  }
  const sums = (await readBounded(assetURL(best.tag, 'SHA256SUMS'), 4096, options)).toString();
  if (sums !== `${info.sha256}  ${info.file}\n`) throw new Error('Release checksums disagree');
  return info;
}
async function downloadUpdate(info, options = {}) {
  manifest(info, info.version);
  return verifyBytes(await readBounded(assetURL('ide-v' + info.version, info.file), info.bytes, options), info);
}
module.exports = { REPO, stable, compare, manifest, verifyBytes, readBounded, checkUpdate, downloadUpdate };
