// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const ORIGIN = 'mesh-app://desktop';
const VIEWER_ORIGIN = 'mesh-app://viewer';
const REQUEST_BYTES = 1024 * 1024;
const RESPONSE_BYTES = 16 * 1024 * 1024;
const TOOLS = new Set(['mesh_templates', 'mesh_note_template', 'mesh_block_template',
  'mesh_author_note', 'mesh_prepare_update', 'mesh_drafts', 'mesh_search', 'mesh_fetch']);
const CSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-src mesh-app://viewer; object-src 'none'; base-uri 'self'; form-action 'none'; frame-ancestors 'none'";
const VIEWER_CSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-src 'none'; object-src 'none'; base-uri 'self'; form-action 'none'; frame-ancestors mesh-app://desktop";
function object(value) { return value !== null && typeof value === 'object' && !Array.isArray(value) && [Object.prototype,null].includes(Object.getPrototypeOf(value)); }
function exact(value, allowed, required = allowed) {
  if (!object(value) || Object.keys(value).some(k => !allowed.includes(k)) || required.some(k => !(k in value))) throw new Error('INVALID_REQUEST');
}
function text(value, max) { return typeof value === 'string' && value.length > 0 && Buffer.byteLength(value) <= max && !/[\u0000-\u001f\u007f]/.test(value); }
function privateURL(value) {
  if (typeof value !== 'string' || /[\\\u0000-\u0020]/.test(value)) return null;
  let u; try { u = new URL(value); } catch { return null; }
  if (u.protocol !== 'mesh-app:' || !['desktop','viewer'].includes(u.hostname) || u.port || u.username || u.password || u.hash) return null;
  // URL parsing normalizes traversal. Reject before parsing rather than accepting an alias.
  const origin=u.hostname==='desktop'?ORIGIN:VIEWER_ORIGIN;
  const raw = value.slice(origin.length).split('?')[0];
  if (!value.startsWith(origin + '/') || /%(?:2e|2f|5c|00)/i.test(raw) || raw.split('/').some(p => p === '.' || p === '..')) return null;
  return u;
}
function appURL(value){const u=privateURL(value);return u?.hostname==='desktop'?u:null;}
function viewerURL(value){const u=privateURL(value);return u?.hostname==='viewer'?u:null;}
function engineWebPath(value) {
  if (!text(value, 4096) || !value.startsWith('/') || value.startsWith('//') || /[\\#]/.test(value) || /%(?:2e|2f|5c|00)/i.test(value)) throw new Error('FORBIDDEN_ROUTE');
  const u = new URL(value, 'https://route.invalid');
  if (u.pathname !== value.split('?')[0] || u.pathname.split('/').some(p => p === '.' || p === '..')) throw new Error('FORBIDDEN_ROUTE');
  const fixed = ['/', '/graph.json', '/api/status', '/api/docs'];
  if (fixed.includes(u.pathname) && !u.search) return value;
  if (/^\/assets\/[a-zA-Z0-9_-]+(?:\/[a-zA-Z0-9_-]+)*\.[a-z0-9]+$/.test(u.pathname) && !u.search) return value;
  if (/^\/api\/(?:note|docs)\/[a-zA-Z0-9_-]+$/.test(u.pathname) && !u.search) return value;
  if (u.pathname === '/api/search') {
    const keys = [...u.searchParams.keys()];
    if (keys.length > 4 || new Set(keys).size !== keys.length || keys.some(k => !['q','limit','budget','offset'].includes(k))) throw new Error('FORBIDDEN_ROUTE');
    for (const k of ['limit','budget','offset']) if (u.searchParams.has(k) && !/^\d{1,6}$/.test(u.searchParams.get(k))) throw new Error('FORBIDDEN_ROUTE');
    if (Buffer.byteLength(u.searchParams.get('q') || '') > 1024) throw new Error('FORBIDDEN_ROUTE');
    return value;
  }
  throw new Error('FORBIDDEN_ROUTE');
}
function validateMethod(method, params) {
  if (!object(params)) throw new Error('INVALID_REQUEST');
  if (['status', 'sync', 'close'].includes(method)) exact(params, []);
  else if (method === 'web') { exact(params, ['method','path']); if (params.method !== 'GET') throw new Error('FORBIDDEN_ROUTE'); engineWebPath(params.path); }
  else if (method === 'tool') { exact(params, ['name','arguments']); if (!TOOLS.has(params.name) || !object(params.arguments)) throw new Error('FORBIDDEN_TOOL'); }
  else if (method === 'init') { exact(params, ['name','for_join'],['name']); if (!text(params.name, 128) || 'for_join' in params && typeof params.for_join!=='boolean') throw new Error('INVALID_REQUEST'); }
  else if (method === 'join') { exact(params, ['hub_url','invite']); if (!text(params.invite, 4096) || !validHub(params.hub_url)) throw new Error('INVALID_REQUEST'); }
  else throw new Error('FORBIDDEN_METHOD');
  if (Buffer.byteLength(JSON.stringify(params)) > REQUEST_BYTES - 256) throw new Error('REQUEST_TOO_LARGE');
}
function validHub(value) {
  if (!text(value, 2048)) return false;
  let u; try { u = new URL(value); } catch { return false; }
  // Native enrollment permits only the configured Mesh service, never renderer-selected network destinations.
  return u.origin === 'https://mesh.brightinteraction.com' && u.pathname === '/' && !u.search && !u.hash && !u.username && !u.password;
}
function assertSender(event, window) {
  if (!window || window.isDestroyed() || event.sender !== window.webContents ||
      !event.senderFrame || event.senderFrame !== window.webContents.mainFrame ||
      event.senderFrame.parent || event.senderFrame.url !== ORIGIN + '/') throw new Error('FORBIDDEN_SENDER');
}
function safeError(error) {
  const code = typeof error?.message === 'string' && /^[A-Z_]{2,48}$/.test(error.message) ? error.message : 'OPERATION_FAILED';
  const messages = { ENGINE_UNAVAILABLE:'The local Mesh engine is unavailable. Reopen the vault to recover.',
    ENGINE_TIMEOUT:'The local operation timed out. Check the vault state before retrying.',
    STALE_REVISION:'This note changed. Reopen it before saving.',
    VAULT_CHANGED:'The vault changed during this operation. Its result was not applied here. A write may have completed in the prior vault; inspect that vault before another attempt.',
    ENGINE_EXIT_UNCERTAIN:'Mesh could not confirm that the prior engine stopped. A new vault owner was not started.',
    JOIN_UNAVAILABLE:'Team enrollment is unavailable in this build.',
    UPDATES_DISABLED:'Signed application updates are not configured in this build.' };
  return { code, message:messages[code] || 'Mesh could not complete this operation. No success was recorded.' };
}
module.exports = { ORIGIN, VIEWER_ORIGIN, REQUEST_BYTES, RESPONSE_BYTES, TOOLS, CSP, VIEWER_CSP, object, exact, text, appURL,viewerURL,privateURL,engineWebPath, validateMethod, validHub, assertSender, safeError };
