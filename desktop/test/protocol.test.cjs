'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),path=require('node:path');
const {makeProtocol,engineResponse}=require('../src/protocol.cjs');
const encoded=(body,headers={'Content-Type':'text/html; charset=utf-8'})=>({status:200,headers,body_base64:Buffer.from(body).toString('base64')});
test('protocol serves fixed local files and exact base rewrite; foreign routes, POST and cookie/redirect authority are refused',async()=>{
  let calls=0;const controller={phase:'vault',selected:{id:'fixture'},engine:{request:async(method,p)=>{calls++;assert.equal(p.path,'/');return encoded('<base href="/"><h1>Fixture</h1></body>',{'Content-Type':'text/html; charset=utf-8','Set-Cookie':'synthetic','Location':'https://evil.invalid','Content-Security-Policy':'*'});}}};
  const handler=makeProtocol({renderer:path.join(__dirname,'../renderer'),controller});
  const shell=await handler({url:'mesh-app://desktop/',method:'GET'});assert.equal(shell.status,200);assert.ok((await shell.text()).includes('Create a personal vault'));
  const viewer=await handler({url:'mesh-app://viewer/',method:'GET'});assert.equal(viewer.status,200);assert.ok((await viewer.text()).includes('href="mesh-app://viewer/"'));assert.equal(viewer.headers.get('Set-Cookie'),null);assert.equal(viewer.headers.get('Location'),null);assert.ok(viewer.headers.get('Content-Security-Policy').includes("default-src 'none'"));
  for(const request of [{url:'https://evil.invalid/',method:'GET'},{url:'mesh-app://viewer/api/config',method:'GET'},{url:'mesh-app://viewer/',method:'POST'},{url:'mesh-app://desktop/test/../index.html',method:'GET'},{url:'mesh-app://desktop/viewer/',method:'GET'},{url:'mesh-app://viewer/shell.js',method:'GET'}])assert.equal((await handler(request)).status,403);
  assert.equal(calls,1);assert.throws(()=>engineResponse(encoded('<base href="/arbitrary/">'),'/'));
});
test('an in-flight response from the prior vault cannot be delivered after native vault switch',async()=>{
  let reply;const controller={phase:'vault',selected:{id:'first'},engine:{request:()=>new Promise(resolve=>reply=resolve)}};
  const handler=makeProtocol({renderer:path.join(__dirname,'../renderer'),controller});const pending=handler({url:'mesh-app://viewer/',method:'GET'});
  controller.selected={id:'second'};reply(encoded('<base href="/"></body>'));assert.equal((await pending).status,403);
});
