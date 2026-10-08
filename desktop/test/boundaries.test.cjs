'use strict';
const test=require('node:test'),assert=require('node:assert/strict');
const {appURL,engineWebPath,validateMethod,assertSender,safeError}=require('../src/boundaries.cjs');
test('private origin and fixed routes reject authority/path aliases and mutating/foreign routes',()=>{
  for(const url of ['https://mesh.brightinteraction.com/','mesh-app://desktop.evil/','mesh-app://user@desktop/','mesh-app://desktop:55/','file:///tmp/a','mesh-app://desktop/../secret','mesh-app://desktop/%2e%2e/secret','mesh-app://desktop/viewer/%2fetc','mesh-app://desktop/\\a'])assert.equal(appURL(url),null,url);
  assert.ok(appURL('mesh-app://desktop/viewer/api/search?q=OAuth&limit=15'));
  for(const path of ['/api/config','/api/login','/api/pending','/openapi.json','//evil/path','/api/note/../status','/api/note/%2e%2e','/assets/../../etc/passwd','/api/search?q=x&q=y','/api/search?url=https://evil','/api/search?limit=-1'])assert.throws(()=>engineWebPath(path),undefined,path);
  for(const path of ['/','/assets/fonts/geist.woff2','/graph.json','/api/status','/api/note/stable-id','/api/docs/02-retrieval','/api/search?q=OAuth&limit=15'])assert.equal(engineWebPath(path),path);
  assert.throws(()=>validateMethod('web',{method:'POST',path:'/api/status'}));
  assert.throws(()=>validateMethod('tool',{name:'mesh_invite',arguments:{}}));
  assert.throws(()=>validateMethod('status',{vault:'/private/user'}));
  assert.throws(()=>validateMethod('join',{hub_url:'https://evil.invalid/',invite:'synthetic'}));
});
test('IPC only admits the exact live top frame of the current native window',()=>{
  const frame={url:'mesh-app://desktop/',parent:null},wc={mainFrame:frame},window={webContents:wc,isDestroyed:()=>false};
  assert.doesNotThrow(()=>assertSender({sender:wc,senderFrame:frame},window));
  for(const event of [{sender:{},senderFrame:frame},{sender:wc,senderFrame:{url:frame.url,parent:frame}},{sender:wc,senderFrame:{url:frame.url,parent:null}}])assert.throws(()=>assertSender(event,window));
  frame.url='mesh-app://desktop/viewer/';assert.throws(()=>assertSender({sender:wc,senderFrame:frame},window));
  frame.url='mesh-app://desktop/?fixture=1';assert.throws(()=>assertSender({sender:wc,senderFrame:frame},window));
  frame.url='mesh-app://viewer/';assert.throws(()=>assertSender({sender:wc,senderFrame:frame},window));
  frame.url='https://evil.invalid';assert.throws(()=>assertSender({sender:wc,senderFrame:frame},window));
});
test('raw engine diagnostics and secret-shaped text never become user-facing errors',()=>{
  assert.equal(safeError(new Error('credential=synthetic-private-value')).code,'OPERATION_FAILED');
  assert.ok(!JSON.stringify(safeError(new Error('credential=synthetic-private-value'))).includes('synthetic-private-value'));
});
