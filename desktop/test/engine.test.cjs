'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),path=require('node:path');
const {spawn}=require('node:child_process'),{EventEmitter}=require('node:events'),{PassThrough}=require('node:stream');
const {Engine}=require('../src/engine.cjs');
test('actual subprocess protocol handles fragmented replies, offline status, whitelisted calls and drained close',async()=>{
  const fixture=path.join(__dirname,'fixtures/core.cjs');let args;
  const engine=new Engine({binary:process.execPath,vault:'/private/tmp',spawnProcess:(binary,argv,options)=>{args=argv;return spawn(binary,[fixture,...argv],options);}});
  engine.start();try{
    assert.equal((await engine.request('status')).sync.state,'offline');
    assert.equal((await engine.request('tool',{name:'mesh_fetch',arguments:{id:'fixture'}})).content.length,1);
    assert.deepEqual(await engine.request('tool',{name:'mesh_search',arguments:{query:'fixture'}}),{cards:[]});
    await assert.rejects(engine.request('tool',{name:'mesh_invite',arguments:{}}));
    assert.deepEqual(args,['--stdio','--vault','/private/tmp']);
  }finally{await engine.close();}assert.equal(engine.child,null);
});
function fake(){const child=new EventEmitter();child.stdout=new PassThrough();child.stdin=new PassThrough();child.kill=()=>{child.emit('exit',1);};return child;}
test('oversized/unsolicited/double-outcome engine responses fail closed and reject pending operations',async()=>{
  for(const value of [{protocol:1,id:999,result:{}},{protocol:1,id:1,result:{},error:{code:'X',message:'bad'}},{protocol:1,id:1,result:{},credentials:'forbidden'}]){
    const child=fake(),engine=new Engine({binary:'fixed',vault:'/tmp',spawnProcess:()=>child});engine.start();const pending=engine.request('status');child.stdout.write(JSON.stringify(value)+'\n');await assert.rejects(pending,/INVALID_ENGINE_RESPONSE/);
  }
  const child=fake(),engine=new Engine({binary:'fixed',vault:'/tmp',spawnProcess:()=>child});engine.start();const pending=engine.request('status');child.stdout.write(Buffer.alloc(16*1024*1024+1,65));await assert.rejects(pending,/INVALID_ENGINE_RESPONSE/);
});
test('timeout terminates the engine instead of automatically replaying an uncertain operation',async()=>{
  const child=fake(),engine=new Engine({binary:'fixed',vault:'/tmp',spawnProcess:()=>child,timeout:10});engine.start();await assert.rejects(engine.request('tool',{name:'mesh_author_note',arguments:{action:'publish',note:{title:'Fixture'}}}),/ENGINE_TIMEOUT/);assert.equal(engine.pending.size,0);assert.equal(engine.child,null);
});
test('unconfirmed OS child exit refuses closure and retains the owner instead of admitting a new engine',async()=>{
  const child=fake();child.kill=()=>false;
  child.stdin.on('data',data=>{const req=JSON.parse(data.toString());if(req.method==='close')child.stdout.write(JSON.stringify({protocol:1,id:req.id,result:{closed:true}})+'\n');});
  const engine=new Engine({binary:'fixed',vault:'/tmp',spawnProcess:()=>child});engine.closeWait=5;engine.killWait=5;engine.start();
  await assert.rejects(engine.close(),/ENGINE_EXIT_UNCERTAIN/);assert.equal(engine.child,child);assert.throws(()=>engine.start(),/ENGINE_UNAVAILABLE/);
  child.emit('exit',0);await engine.close();assert.equal(engine.child,null);
});
