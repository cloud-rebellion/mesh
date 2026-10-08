'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),{EventEmitter}=require('node:events'),fs=require('node:fs/promises'),os=require('node:os'),path=require('node:path'),vm=require('node:vm'),{createRequire}=require('node:module');
const {Controller}=require('../src/controller.cjs');
const card=()=>({state:'ready',vault:{name:'Fixture',id:'fixture'},sync:{state:'unjoined',pending:0,conflicts:0,last_success:null},identity:null,version:'0.42.15'});
class FakeEngine extends EventEmitter{start(){}async request(method){if(method==='status')return card();if(method==='tool')return{content:[{type:'text',text:'{}'}]};return{};}async close(){this.closed=true;}}
function updater(){return{state:'available',stages:0,status(){return{state:this.state,reason:this.state};},async install({drain}){await drain();this.stages++;this.state='ready';}};}
async function fixture(){const engine=new FakeEngine(),update=updater(),record={id:'fixture',name:'Fixture',path:'/synthetic'},c=new Controller({storage:{cards:()=>[],resolve:()=>record},bundle:async()=>({binary:'/synthetic'}),picker:async()=>null,updater:update,appVersion:'0.1.0',engineFactory:()=>engine});await c.open(record);return{c,engine,update};}
const note=()=>({title:'Fixture',template:'post-mortem',template_version:1,status:'active'});
const receipt=(overrides={})=>({content:[{type:'text',text:JSON.stringify({id:'fixture',path:'post-mortems/fixture.md',when:'2026-10-08',todo:null,status:'active',template:'post-mortem',template_version:1,revision:'b'.repeat(64),...overrides})}]});
test('actual controller refuses pending shared author writes and freezes new tools while confirming a safe restart',async()=>{
 const{c,engine,update}=await fixture();let finish;engine.request=()=>new Promise(r=>finish=r);const pending=c.perform('tool',{name:'mesh_author_note',arguments:{action:'publish',note:note()}});
 await assert.rejects(c.restartForUpdate({confirmEditor:async()=>true}),/OPERATION_ACTIVE/);assert.equal(update.stages,0);finish(receipt());await pending;
 let release;const restart=c.restartForUpdate({confirmEditor:()=>new Promise(r=>release=r)});await assert.rejects(c.perform('tool',{name:'mesh_search',arguments:{query:'fixture'}}),/RESTART_LOCKED/);assert.equal(engine.closed,undefined);release(true);await restart;assert.equal(engine.closed,true);assert.equal(update.stages,1);
});
test('dirty/active editor refusal retains selected vault, buffers context and owner; close failure cannot stage',async()=>{
 const{c,engine,update}=await fixture();const selected=c.selected;await assert.rejects(c.restartForUpdate({confirmEditor:async()=>false}),/EDITOR_NOT_READY/);assert.equal(c.phase,'vault');assert.equal(c.selected,selected);assert.equal(engine.closed,undefined);assert.equal(c.updateLock,false);assert.equal(c.restartToken,null);
 engine.close=async()=>{throw new Error('ENGINE_EXIT_UNCERTAIN');};await assert.rejects(c.restartForUpdate({confirmEditor:async()=>true}),/EXIT_UNCERTAIN/);assert.equal(update.stages,0);assert.equal(c.engine,engine);assert.equal(c.phase,'vault');assert.equal(c.updateLock,false);
});
test('uncertain write and durable uncertain/accepted-but-incomplete enrollment block update without replay',async()=>{
 const f=await fixture();let writes=0;f.engine.request=async()=>{writes++;throw new Error('ENGINE_TIMEOUT');};await assert.rejects(f.c.perform('tool',{name:'mesh_author_note',arguments:{action:'draft',note:{}}}),/TIMEOUT/);await assert.rejects(f.c.restartForUpdate({confirmEditor:async()=>true}),/WRITE_UNCERTAIN/);assert.equal(writes,1);assert.equal(f.update.stages,0);
 for(const state of ['joining','join-uncertain','joined-preparing']){const{c,update}=await fixture();c.card.state=state;await assert.rejects(c.restartForUpdate({confirmEditor:async()=>true}),/JOIN_UNSETTLED/);assert.equal(update.stages,0);}
});
test('main real authenticated editor-state IPC binds restart to current frame, token and vault; cancellation defers without discard',async()=>{
 const dir=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-main-update-'));let window,handler,controller,menu,choice=0,token,oldFrame;const update=updater(),engine=new FakeEngine(),app=new EventEmitter();Object.assign(app,{enableSandbox(){},requestSingleInstanceLock:()=>true,whenReady:()=>Promise.resolve(),getPath:()=>dir,getVersion:()=> '0.1.0',isPackaged:false,quit(){}});
 class Window extends EventEmitter{constructor(){super();this.webContents=new EventEmitter();this.webContents.mainFrame={url:'mesh-app://desktop/',parent:null};this.webContents.setWindowOpenHandler=()=>{};this.webContents.send=(_channel,value)=>{if(value.updates.restart_token)token=value.updates.restart_token;};window=this;}isDestroyed(){return false;}async loadURL(){}show(){}}
 const ps=new EventEmitter();Object.assign(ps,{setPermissionRequestHandler(){},setPermissionCheckHandler(){},setDevicePermissionHandler(){},webRequest:{onBeforeRequest(){}},protocol:{handle:async()=>{}}});
 const electron={app,BrowserWindow:Window,session:{fromPartition:()=>ps},protocol:{registerSchemesAsPrivileged(){}},ipcMain:{handle:(_name,h)=>handler=h},dialog:{showMessageBoxSync:()=>choice,showErrorBox(){},showOpenDialog:async()=>({canceled:true})},Menu:{buildFromTemplate:t=>t,setApplicationMenu:t=>menu=t},powerMonitor:new EventEmitter()};
 class ActualController extends Controller{constructor(options){super({...options,bundle:async()=>({binary:'/synthetic'}),engineFactory:()=>engine});controller=this;}}
 const main=path.join(__dirname,'../src/main.cjs'),req=createRequire(main);
 try{vm.runInNewContext(await fs.readFile(main,'utf8'),{require:name=>name==='electron'?electron:name==='./controller.cjs'?{Controller:ActualController}:name==='./updater.cjs'?{configureUpdater:()=>update}:req(name),__dirname:path.dirname(main),process,setInterval:()=>({unref(){}}),clearInterval(){},setTimeout,clearTimeout,console});
 for(let i=0;i<20&&!handler;i++)await new Promise(r=>setTimeout(r,2));assert.ok(handler);const sender=()=>({sender:window.webContents,senderFrame:window.webContents.mainFrame});
 const created=await handler(sender(),{action:'create',params:{name:'Synthetic'}});assert.equal(created.ok,true,JSON.stringify(created));assert.equal(controller.phase,'vault');await handler(sender(),{action:'editor-state',params:{dirty:true}});
 const restart=menu[0].submenu.find(v=>v.label==='Restart to update');restart.click();for(let i=0;i<20&&!token;i++)await new Promise(r=>setTimeout(r,1));assert.match(token,/^[a-f0-9]{48}$/);
 assert.equal((await handler({...sender(),senderFrame:{url:'mesh-app://viewer/',parent:window.webContents.mainFrame}},{action:'editor-state',params:{dirty:false,operation:false,restart_token:token}})).ok,false);
 assert.equal((await handler(sender(),{action:'editor-state',params:{dirty:false,operation:false,restart_token:'0'.repeat(48)}})).ok,false);
 await handler(sender(),{action:'editor-state',params:{dirty:true,operation:false,restart_token:token}});await new Promise(r=>setImmediate(r));assert.equal(update.stages,0);assert.equal(controller.phase,'vault');assert.equal(engine.closed,undefined);assert.equal(controller.restartToken,null);
 const consumed=token;assert.equal((await handler(sender(),{action:'editor-state',params:{dirty:false,operation:false,restart_token:consumed}})).ok,false);
 token=null;restart.click();for(let i=0;i<20&&!token;i++)await new Promise(r=>setTimeout(r,1));assert.notEqual(token,consumed);await handler(sender(),{action:'editor-state',params:{dirty:false,operation:true,restart_token:token}});await new Promise(r=>setImmediate(r));assert.equal(update.stages,0);
 token=null;choice=1;restart.click();for(let i=0;i<20&&!token;i++)await new Promise(r=>setTimeout(r,1));await handler(sender(),{action:'editor-state',params:{dirty:true,operation:false,restart_token:token}});await new Promise(r=>setImmediate(r));assert.equal(update.stages,1);assert.equal(engine.closed,true);
 }finally{await fs.rm(dir,{recursive:true,force:true});}
});

test('cancelled native join cannot be bypassed by a refused competing action clearing its busy ownership',async()=>{
 const{c,update}=await fixture();let cancel;c.enrollmentEnabled=true;c.confirmJoin=()=>new Promise(r=>cancel=r);const joining=c.perform('join',{name:'Synthetic',invite:'synthetic invite'});
 await assert.rejects(c.perform('sync',{}),/BUSY/);assert.equal(c.busy,true);await assert.rejects(c.restartForUpdate({confirmEditor:async()=>true}),/OPERATION_ACTIVE/);assert.equal(update.stages,0);cancel(false);await joining;assert.equal(c.busy,false);await c.restartForUpdate({confirmEditor:async()=>true});assert.equal(update.stages,1);
});

test('confirmed OS-exit failure from the actual Engine prevents native stage and retains the prior owner',async()=>{
 const{Engine}=require('../src/engine.cjs'),child=new EventEmitter();child.stdout=new EventEmitter();child.stdin={write(){}};let kills=0;child.kill=()=>kills++;
 const engine=new Engine({binary:'/synthetic',vault:'/synthetic',spawnProcess:()=>child});engine.start();engine.request=async()=>({});engine.closeWait=1;engine.killWait=1;
 const update=updater(),record={id:'fixture',name:'Fixture'},c=new Controller({storage:{cards:()=>[]},updater:update,appVersion:'0.1.0'});c.engine=engine;c.selected=record;c.card=card();c.phase='vault';
 await assert.rejects(c.restartForUpdate({confirmEditor:async()=>true}),/EXIT_UNCERTAIN/);assert.equal(kills,1);assert.equal(update.stages,0);assert.equal(c.engine,engine);assert.equal(c.phase,'vault');assert.equal(c.updateLock,false);child.emit('exit',0);
});

test('shared MCP error or malformed write receipt is uncertain even when transport returned a response',async()=>{
 for(const response of [{isError:true,content:[{type:'text',text:'write outcome unavailable'}]},{content:[{type:'text',text:'{"id":"saved"}'}]},{id:'saved',revision:'not a revision'}]){const{c,engine,update}=await fixture();let count=0;engine.request=async()=>{count++;return response;};await c.perform('tool',{name:'mesh_author_note',arguments:{action:'publish',note:{}}});await assert.rejects(c.restartForUpdate({confirmEditor:async()=>true}),/WRITE_UNCERTAIN/);assert.equal(count,1);assert.equal(update.stages,0);}
});

test('durable write confirmation matches actual shared Go receipt and requested action/template/update identity',async()=>{
 const cases=[
  [receipt(),{action:'publish',note:note()},true],
  [receipt({index_stale:true,index_error:'bounded fixture',owner_down:true,warning:'Already saved; do not retry.'}),{action:'publish',note:note()},true],
  [receipt({id:'知识-é',path:'notes/知识-é.md'}),{action:'publish',note:note()},false],
  [receipt({id:'知识-δ',path:'notes/知识-δ.md'}),{action:'publish',note:note()},true],
  [receipt({status:'draft'}),{action:'draft',note:note()},true],
  [receipt({when:'',updated:true,previous_revision:'a'.repeat(64)}),{action:'publish',note:{...note(),update_id:'fixture',update_revision:'a'.repeat(64)}},true],
  [receipt({when:'historical date not established',updated:true,previous_revision:'a'.repeat(64)}),{action:'publish',note:{...note(),update_id:'fixture',update_revision:'a'.repeat(64)}},true],
  [receipt({updated:true,previous_revision:'a'.repeat(64)}),{action:'publish',note:{...note(),update_id:'fixture',update_revision:'a'.repeat(64)}},true],
  [receipt({saved:false}),{action:'publish',note:note()},false],
  [receipt({error:'write failed'}),{action:'publish',note:note()},false],
  [receipt({valid:false}),{action:'publish',note:note()},false],
  [receipt({status:'draft'}),{action:'publish',note:note()},false],
  [receipt(),{action:'draft',note:note()},false],
  [receipt({template:'decision'}),{action:'publish',note:note()},false],
  [receipt({template_version:2}),{action:'publish',note:note()},false],
  [receipt({updated:true}),{action:'publish',note:{...note(),update_id:'fixture'}},false],
  [receipt({status:'draft'}),{action:'draft',note:{...note(),draft_id:'fixture'}},false],
  [receipt({id:'other',updated:true,previous_revision:'a'.repeat(64)}),{action:'publish',note:{...note(),update_id:'fixture',update_revision:'a'.repeat(64)}},false],
  [receipt({updated:true,previous_revision:'c'.repeat(64)}),{action:'publish',note:{...note(),update_id:'fixture',update_revision:'a'.repeat(64)}},false],
  [receipt(),{action:'publish',note:{...note(),draft_id:'other'}},false],
  [receipt({id:'Fixture'}),{action:'publish',note:note()},false],
  [receipt({path:'../outside.md'}),{action:'publish',note:note()},false],
  [{content:[{type:'text',text:JSON.stringify({id:'fixture',revision:'b'.repeat(64),saved:false,markdown:'prepared'})}]},{action:'publish',note:note()},false]
 ];
 for(const [result,args,confirmed]of cases){const{c,engine}=await fixture();engine.request=async()=>result;await c.perform('tool',{name:'mesh_author_note',arguments:args});assert.equal(c.uncertainWrite,!confirmed,JSON.stringify({result,args}));}
});

async function mainQuitFixture({writeResponse=receipt({error:'valid-looking failure'})}={}){
 const dir=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-main-safe-quit-'));let window,handler,controller,choices=[],prompts=[],errors=[],quitCount=0;const update=updater();update.state='disabled';
 const{Engine}=require('../src/engine.cjs'),child=new EventEmitter();child.stdout=new EventEmitter();child.kills=[];child.kill=signal=>child.kills.push(signal);let writes=0,reads=0,closes=0;
 child.stdin={write(wire,callback){const request=JSON.parse(wire);let result={};if(request.method==='status')result=card();if(request.method==='tool'){if(request.params.name==='mesh_author_note'){writes++;result=writeResponse;}else{reads++;result={content:[{type:'text',text:'Existing note inspected.'}]};}}if(request.method==='close')closes++;
  setImmediate(()=>{callback?.();child.stdout.emit('data',Buffer.from(JSON.stringify({protocol:1,id:request.id,result})+'\n'));});}};
 const engine=new Engine({binary:'/synthetic',vault:dir,spawnProcess:()=>child,timeout:1000});
 const app=new EventEmitter();Object.assign(app,{enableSandbox(){},requestSingleInstanceLock:()=>true,whenReady:()=>Promise.resolve(),getPath:()=>dir,getVersion:()=> '0.1.0',isPackaged:false,quit(){quitCount++;}});
 class Window extends EventEmitter{constructor(){super();this.webContents=new EventEmitter();this.webContents.mainFrame={url:'mesh-app://desktop/',parent:null};this.webContents.setWindowOpenHandler=()=>{};this.webContents.send=()=>{};window=this;}isDestroyed(){return false;}async loadURL(){}show(){}}
 const ps=new EventEmitter();Object.assign(ps,{setPermissionRequestHandler(){},setPermissionCheckHandler(){},setDevicePermissionHandler(){},webRequest:{onBeforeRequest(){}},protocol:{handle:async()=>{}}});
 const electron={app,BrowserWindow:Window,session:{fromPartition:()=>ps},protocol:{registerSchemesAsPrivileged(){}},ipcMain:{handle:(_name,h)=>handler=h},dialog:{showMessageBoxSync:(_window,prompt)=>{prompts.push(prompt);return choices.shift()??0;},showErrorBox:(title,message)=>errors.push({title,message}),showOpenDialog:async()=>({canceled:true})},Menu:{buildFromTemplate:t=>t,setApplicationMenu(){}},powerMonitor:new EventEmitter()};
 class ActualController extends Controller{constructor(options){super({...options,bundle:async()=>({binary:'/synthetic'}),engineFactory:()=>engine});controller=this;}}
 const main=path.join(__dirname,'../src/main.cjs'),req=createRequire(main);
 vm.runInNewContext(await fs.readFile(main,'utf8'),{require:name=>name==='electron'?electron:name==='./controller.cjs'?{Controller:ActualController}:name==='./updater.cjs'?{configureUpdater:()=>update}:req(name),__dirname:path.dirname(main),process,setInterval:()=>({unref(){}}),clearInterval(){},setTimeout,clearTimeout,console});
 for(let i=0;i<30&&!window?.listenerCount('close');i++)await new Promise(r=>setTimeout(r,2));assert.ok(handler);
 const sender=()=>({sender:window.webContents,senderFrame:window.webContents.mainFrame});await handler(sender(),{action:'create',params:{name:'Synthetic'}});
 const turn=()=>new Promise(r=>setImmediate(r)),closeWindow=()=>{let prevented=false;window.emit('close',{preventDefault:()=>prevented=true});return prevented;};
 return {app,window,controller,engine,child,update,choices,prompts,errors,handler,sender,turn,closeWindow,get quitCount(){return quitCount;},get writes(){return writes;},get reads(){return reads;},get closes(){return closes;},async cleanup(){if(engine.child)child.emit('exit',0);await turn();await fs.rm(dir,{recursive:true,force:true});}};
}
test('actual main failed author receipt then inspection can quit only after explicit consent and confirmed engine exit, without update or replay',async()=>{
 const f=await mainQuitFixture();try{
  const saved=await f.handler(f.sender(),{action:'tool',params:{name:'mesh_author_note',arguments:{action:'publish',note:note()}}});assert.equal(saved.ok,true);assert.equal(f.controller.uncertainWrite,true);
  await f.handler(f.sender(),{action:'tool',params:{name:'mesh_fetch',arguments:{id:'fixture'}}});assert.equal(f.reads,1);assert.equal(f.controller.uncertainWrite,true);
  f.update.state='available';await assert.rejects(f.controller.restartForUpdate({confirmEditor:async()=>true}),/WRITE_UNCERTAIN/);f.update.state='disabled';assert.equal(f.update.stages,0);
  f.choices.push(0);assert.equal(f.closeWindow(),true);await f.turn();assert.equal(f.quitCount,0);assert.equal(f.controller.engine,f.engine);assert.equal(f.controller.phase,'vault');assert.match(f.prompts.at(-1).detail,/unknown outcome/);
  f.choices.push(1);assert.equal(f.closeWindow(),true);for(let i=0;i<30&&!f.closes;i++)await f.turn();assert.equal(f.closes,1);assert.equal(f.quitCount,0);assert.equal(f.controller.engine,f.engine);
  assert.equal((await f.handler(f.sender(),{action:'tool',params:{name:'mesh_search',arguments:{query:'fixture'}}})).ok,false);
  f.child.emit('exit',0);await f.turn();await f.turn();assert.equal(f.quitCount,1);assert.equal(f.controller.engine,null);assert.equal(f.controller.uncertainWrite,true);assert.equal(f.writes,1);assert.equal(f.update.stages,0);assert.equal(f.closeWindow(),false);
 }finally{await f.cleanup();}
});
test('main close deferral preserves dirty acknowledgement and usable window after refused consent or unconfirmed OS exit',async()=>{
 const f=await mainQuitFixture();try{
  await f.handler(f.sender(),{action:'tool',params:{name:'mesh_author_note',arguments:{action:'publish',note:note()}}});await f.handler(f.sender(),{action:'editor-state',params:{dirty:true}});
  f.choices.push(1,0);assert.equal(f.closeWindow(),true);await f.turn();assert.equal(f.closes,0);assert.equal(f.quitCount,0);assert.equal(f.controller.phase,'vault');
  f.engine.closeWait=1;f.engine.killWait=1;f.choices.push(1,1);assert.equal(f.closeWindow(),true);for(let i=0;i<30&&!f.errors.length;i++)await new Promise(r=>setTimeout(r,2));assert.equal(f.prompts.at(-2).message,'Discard unsaved changes?');assert.equal(f.quitCount,0);assert.equal(f.controller.engine,f.engine);assert.equal(f.controller.phase,'vault');assert.deepEqual(f.child.kills,['SIGKILL']);assert.equal(f.controller.quitLock,false);assert.equal(f.controller.uncertainWrite,true);assert.equal(f.update.stages,0);
  f.choices.push(0);assert.equal(f.closeWindow(),true);await f.turn();assert.equal(f.prompts.at(-1).message,'Discard unsaved changes?');assert.equal(f.quitCount,0);
 }finally{await f.cleanup();}
});
test('ordinary quit refuses active author requests and native staging/uncertain update locks',async()=>{
 const{c,engine,update}=await fixture();let finish;engine.request=()=>new Promise(r=>finish=r);const write=c.perform('tool',{name:'mesh_author_note',arguments:{action:'publish',note:note()}});
 await assert.rejects(c.quitGracefully({confirmUnknown:async()=>true}),/OPERATION_ACTIVE/);assert.equal(engine.closed,undefined);finish(receipt());await write;
 for(const state of ['staging','uncertain','available']){c.updateLock=true;update.state=state;await assert.rejects(c.quitGracefully({confirmUnknown:async()=>true}),/RESTART_LOCKED/);assert.equal(engine.closed,undefined);}c.updateLock=false;
 c.card.state='join-uncertain';await assert.rejects(c.quitGracefully({confirmUnknown:async()=>false}),/QUIT_CANCELLED/);assert.equal(engine.closed,undefined);await c.quitGracefully({confirmUnknown:async value=>{assert.equal(value.join,true);return true;}});assert.equal(engine.closed,true);assert.equal(update.stages,0);
});

test('disabled-updater ordinary before-quit drains after a shared MCP error without clearing persistent write uncertainty',async()=>{
 const f=await mainQuitFixture({writeResponse:{isError:true,content:[{type:'text',text:'Unknown write outcome.'}]}});try{
  await f.handler(f.sender(),{action:'tool',params:{name:'mesh_author_note',arguments:{action:'publish',note:note()}}});assert.equal(f.controller.uncertainWrite,true);assert.equal(f.update.state,'disabled');
  f.choices.push(1);let prevented=false;f.app.emit('before-quit',{preventDefault:()=>prevented=true});assert.equal(prevented,true);
  for(let i=0;i<30&&!f.closes;i++)await f.turn();assert.equal(f.closes,1);assert.equal(f.quitCount,0);assert.equal(f.controller.phase,'vault');
  f.child.emit('exit',0);await f.turn();await f.turn();assert.equal(f.quitCount,1);assert.equal(f.controller.uncertainWrite,true);assert.equal(f.writes,1);assert.equal(f.update.stages,0);
 }finally{await f.cleanup();}
});
