// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const fs = require('node:fs/promises');
const path = require('node:path');
const { privateURL,engineWebPath,CSP,VIEWER_CSP,exact } = require('./boundaries.cjs');
const STATIC = new Map([['/','index.html'],['/shell.js','shell.js'],['/shell.css','shell.css']]);
const VIEWER_STATIC = new Map([['/assets/desktop-viewer.css','viewer.css'],['/assets/desktop-viewer.js','viewer-bridge.js']]);
const MIME = {'.html':'text/html; charset=utf-8','.js':'text/javascript; charset=utf-8','.css':'text/css; charset=utf-8'};
const HEADERS = {'Content-Security-Policy':CSP,'X-Content-Type-Options':'nosniff','Cache-Control':'no-store'};
function engineResponse(value,webPath) {
  exact(value,['status','headers','body_base64']);
  if (!Number.isSafeInteger(value.status) || value.status < 200 || value.status > 599 || typeof value.body_base64 !== 'string' || value.body_base64.length > 24 * 1024 * 1024 || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value.body_base64)) throw new Error('INVALID_ENGINE_RESPONSE');
  if (!value.headers || typeof value.headers !== 'object' || Array.isArray(value.headers)) throw new Error('INVALID_ENGINE_RESPONSE');
  // Ignore redirects/cookies/location/disposition/engine CSP. A private origin never inherits remote authority.
  const type=value.headers['Content-Type'];
  if (typeof type !== 'string' || !/^(?:text\/(?:html|css|plain)|(?:application|text)\/javascript|application\/json|font\/woff2|image\/(?:png|svg\+xml|jpeg|webp))(?:; charset=utf-8)?$/i.test(type)) throw new Error('INVALID_ENGINE_RESPONSE');
  let body=Buffer.from(value.body_base64,'base64'); if (body.length > 16 * 1024 * 1024) throw new Error('INVALID_ENGINE_RESPONSE');
  if (webPath === '/' && value.status === 200) {
    const html=body.toString('utf8'), known='<base href="/">';
    if (html.split(known).length !== 2 || html.split('</body>').length !== 2) throw new Error('INVALID_ENGINE_RESPONSE');
    body=Buffer.from(html.replace(known,'<base href="mesh-app://viewer/"><link rel="stylesheet" href="assets/desktop-viewer.css">')
      .replace('</body>','<script src="assets/desktop-viewer.js" defer></script></body>'));
  }
  return {body,status:value.status,headers:{...HEADERS,'Content-Security-Policy':VIEWER_CSP,'Content-Type':type}};
}
function makeProtocol({renderer,controller}) {
  return async request=>{
    const denied=()=>new Response('Unavailable',{status:403,headers:{...HEADERS,'Content-Type':'text/plain'}});
    const u=privateURL(request.url); if (!u || request.method !== 'GET') return denied();
    try {
      if (u.hostname==='desktop' && STATIC.has(u.pathname) && !u.search) {
        const file=path.join(renderer,STATIC.get(u.pathname)); const st=await fs.lstat(file);
        if (!st.isFile() || st.isSymbolicLink() || st.size > 1024 * 1024) return denied();
        return new Response(await fs.readFile(file),{headers:{...HEADERS,'Content-Type':MIME[path.extname(file)]}});
      }
      if(u.hostname==='viewer'&&VIEWER_STATIC.has(u.pathname)&&!u.search&&controller.phase==='vault'){
        const file=path.join(renderer,VIEWER_STATIC.get(u.pathname)),st=await fs.lstat(file);
        if(!st.isFile()||st.isSymbolicLink()||st.size>65536)return denied();
        return new Response(await fs.readFile(file),{headers:{...HEADERS,'Content-Security-Policy':VIEWER_CSP,'Content-Type':MIME[path.extname(file)]}});
      }
      if (u.hostname==='viewer' && controller.phase === 'vault' && controller.engine) {
        const engine=controller.engine,selected=controller.selected;
        const webPath=engineWebPath(u.pathname+u.search);
        const value=await engine.request('web',{method:'GET',path:webPath});
        if (controller.engine !== engine || controller.selected !== selected || controller.phase !== 'vault') return denied();
        const result=engineResponse(value,webPath);
        return new Response(result.body,{status:result.status,headers:result.headers});
      }
    } catch { return denied(); }
    return denied();
  };
}
module.exports = { makeProtocol,engineResponse,HEADERS };
