'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs/promises'),os=require('node:os'),path=require('node:path'),{EventEmitter}=require('node:events');
const {Storage}=require('../src/storage.cjs'),{Controller,statusCard}=require('../src/controller.cjs'),{configureUpdater}=require('../src/updater.cjs');
const status={state:'ready',vault:{name:'Fixture',id:'fixture'},sync:{state:'offline',pending:2,conflicts:1,last_success:null},identity:{user:'fixture@example.invalid',role:'owner',verified:false},version:'0.0.0'};
test('managed storage and native-selected fixture persist IDs without returning paths or reading credential files',async()=>{
  const dir=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-native-fixture-'));try{
    const storage=new Storage(path.join(dir,'app'));await storage.load();const managed=await storage.create('Personal');
    assert.ok(managed.path.startsWith(path.join(dir,'app','vaults')));assert.ok(!JSON.stringify(storage.cards()).includes(dir));
    const existing=path.join(dir,'existing');await fs.mkdir(existing);await fs.writeFile(path.join(existing,'note.md'),'fixture note');
    const record=await storage.adopt(existing);assert.equal((await storage.adopt(existing)).id,record.id);
    const reload=new Storage(path.join(dir,'app'));await reload.load();assert.deepEqual(reload.cards(),storage.cards());assert.equal(await fs.readFile(path.join(existing,'note.md'),'utf8'),'fixture note');
    await assert.rejects(storage.adopt('../arbitrary'));
  }finally{await fs.rm(dir,{recursive:true,force:true});}
});
test('controller native picker initializes only new storage, exposes honest identity/offline state, and closes before switching',async()=>{
  const dir=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-native-fixture-'));const calls=[];
  class FakeEngine extends EventEmitter{start(){calls.push('start');}async request(method,params){calls.push(method);return method==='status'?structuredClone(status):{};}async close(){calls.push('close');}}
  try{const storage=new Storage(path.join(dir,'app'));await storage.load();const c=new Controller({storage,bundle:async()=>({binary:'fixed'}),picker:async()=>dir,updater:configureUpdater(null),engineFactory:()=>new FakeEngine(),appVersion:'0.1.0'});
    const overview=await c.perform('create',{name:'Personal'});assert.equal(overview.phase,'vault');assert.equal(overview.status.identity.role,'unknown');assert.equal(overview.status.sync.pending,2);assert.ok(!JSON.stringify(overview).includes(dir));assert.deepEqual(calls,['start','init','status']);
    await c.perform('pick');assert.deepEqual(calls.slice(3),['close','start','status']);await assert.rejects(c.perform('join',{invite:'synthetic'}),/JOIN_UNAVAILABLE/);
    await c.close();assert.equal(c.phase,'closed');assert.throws(()=>configureUpdater({url:'https://evil.invalid/'}));
    assert.throws(()=>statusCard({...status,vault:{name:'Fixture',id:'fixture',path:'/private/data'}}));
  }finally{await fs.rm(dir,{recursive:true,force:true});}
});
test('explicit enrollment requires native confirmation, uses a fixed hub once, preserves unknown role and never retries an uncertain redemption',async()=>{
  const dir=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-native-join-'));let confirmed=false,attempts=0;
  class FakeEngine extends EventEmitter{start(){}async request(method,params){if(method==='init')assert.equal(params.for_join,true);if(method==='join'){attempts++;assert.equal(params.hub_url,'https://mesh.brightinteraction.com/');assert.equal(params.invite,'synthetic-short-capability');throw new Error('ENROLLMENT_UNCERTAIN');}return structuredClone(status);}async close(){}}
  try{
    const storage=new Storage(path.join(dir,'app'));await storage.load();const c=new Controller({storage,bundle:async()=>({binary:'fixed'}),picker:async()=>null,updater:configureUpdater(null),engineFactory:()=>new FakeEngine(),appVersion:'0.1.0',enrollmentEnabled:true,confirmJoin:async()=>confirmed});
    await c.perform('join',{name:'Fixture team',invite:'synthetic-short-capability'});assert.equal(attempts,0);assert.equal(storage.cards().length,0);
    confirmed=true;await assert.rejects(c.perform('join',{name:'Fixture team',invite:'synthetic-short-capability'}),/ENROLLMENT_UNCERTAIN/);assert.equal(attempts,1);assert.equal(c.overview().status.identity.role,'unknown');await c.refresh();assert.equal(attempts,1);await c.close();
  }finally{await fs.rm(dir,{recursive:true,force:true});}
});
test('delayed prior-vault tool/write and status responses are refused after native home/open without overwriting the new context',async()=>{
  const dir=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-native-context-'));let resolveTool,resolveStatus;const engines=[];
  class FakeEngine extends EventEmitter{
    constructor(){super();this.ordinal=engines.length;this.delay=false;engines.push(this);}start(){}
    async request(method){if(this.delay&&method==='tool')return new Promise(resolve=>resolveTool=resolve);if(this.delay&&method==='status')return new Promise(resolve=>resolveStatus=resolve);return{...structuredClone(status),vault:{name:'Vault '+this.ordinal,id:String(this.ordinal)}};}
    async close(){}
  }
  try{
    const storage=new Storage(path.join(dir,'app'));await storage.load();const c=new Controller({storage,bundle:async()=>({binary:'fixed'}),picker:async()=>null,updater:configureUpdater(null),engineFactory:()=>new FakeEngine(),appVersion:'0.1.0'});
    await c.perform('create',{name:'First'});engines[0].delay=true;
    const oldWrite=c.perform('tool',{name:'mesh_author_note',arguments:{action:'publish',note:{title:'Prior vault'}}});const refused=assert.rejects(oldWrite,/VAULT_CHANGED/);
    const oldRefresh=c.refresh();await c.perform('home');await c.perform('create',{name:'Second'});
    resolveTool({id:'prior-write',content:'prior-vault-private-content'});resolveStatus({...structuredClone(status),vault:{name:'Old private name',id:'old'}});
    await refused;await oldRefresh;
    assert.equal(c.overview().selected.name,'Second');assert.equal(c.overview().status.vault.name,'Vault 1');assert.ok(!JSON.stringify(c.overview()).includes('prior-vault-private-content'));assert.ok(!JSON.stringify(c.overview()).includes('Old private name'));await c.close();
  }finally{await fs.rm(dir,{recursive:true,force:true});}
});
