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


// The scheduling fixtures use the accepted main IPC and actual editor script.
// Only Electron's process/window adapters, clock and unsigned update candidate
// are synthetic: these tests confer no channel or macOS signature authority.
const {UpdateScheduler}=require('../src/update-scheduler.cjs');
const sourceTurns=async()=>{for(let i=0;i<20;i++)await new Promise(resolve=>setImmediate(resolve));};
class SchedulerClock{
 constructor(){this.time=0;this.tasks=new Map();}
 now=()=>this.time;
 setTimer=(fn,delay)=>{const token={unref(){}};this.tasks.set(token,{fn,at:this.time+delay});return token;};
 clearTimer=token=>this.tasks.delete(token);
 async advance(by){this.time+=by;for(let rounds=0;;rounds++){assert.ok(rounds<20,'No catch-up timer burst');const due=[...this.tasks].filter(([,task])=>task.at<=this.time);if(!due.length)return;for(const[token,task]of due){this.tasks.delete(token);task.fn();}await sourceTurns();}}
}
const schedulerTiming={startup:10,interval:1000,settle:5,defer:100,retry:[50,200,1000]};
class EditorElement{
 constructor(tag='div'){this.tag=tag;this.children=[];this.listeners={};this.dataset={};this.hidden=false;this.disabled=false;this.value='';this.textContent='';}
 addEventListener(name,fn){(this.listeners[name]??=[]).push(fn);}
 append(...children){this.children.push(...children);}
 replaceChildren(...children){this.children=children;}
 removeAttribute(name){delete this[name];}
 get selectedOptions(){return[this.children.find(child=>child.value===this.value)||this.children[0]].filter(Boolean);}
 querySelectorAll(){const fields=[];const visit=element=>{if(element.dataset?.section)fields.push(element);for(const child of element.children||[])visit(child);};visit(this);return fields;}
 fire(name){for(const fn of this.listeners[name]||[])fn({preventDefault(){}});}
 showModal(){}close(){this.fire('close');}
}
async function scheduledMainFixture({update=updater(),engine=new FakeEngine()}={}){
 const dir=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-main-scheduler-')),clock=new SchedulerClock();let window,handler,controller,scheduler,stateCallback,quitCount=0,templateRelease=null,holdTemplate=false;const prompts=[],errors=[],ipc=[],elements=new Map();
 const get=id=>{if(!elements.has(id))elements.set(id,new EditorElement());return elements.get(id);};get('viewer').contentWindow={};get('editor').hidden=true;get('draft-list').hidden=true;
 const respond=value=>({content:[{type:'text',text:JSON.stringify(value)}]});
 const request=engine.request.bind(engine);engine.request=async(method,params)=>{
  if(method==='tool'&&params.name==='mesh_templates')return respond({templates:[{id:'post-mortem',type:'post-mortem',version:1}],blocks:[]});
  if(method==='tool'&&params.name==='mesh_note_template')return respond({template:{id:'post-mortem',type:'post-mortem',version:1,sections:[{key:'what_happened',heading:'What happened',guidance:'Supported facts.'}]}});
  if(method==='tool'&&params.name==='mesh_author_note'&&params.arguments.action==='prepare')return respond({markdown:'Synthetic draft preview.'});
  if(method==='tool'&&params.name==='mesh_author_note'&&params.arguments.action==='draft')return receipt({status:'draft'});
  return request(method,params);
 };
 const app=new EventEmitter();Object.assign(app,{enableSandbox(){},requestSingleInstanceLock:()=>true,whenReady:()=>Promise.resolve(),getPath:()=>dir,getVersion:()=> '0.1.0',isPackaged:false,quit(){quitCount++;}});
 class Window extends EventEmitter{constructor(){super();this.webContents=new EventEmitter();this.webContents.mainFrame={url:'mesh-app://desktop/',parent:null};this.webContents.setWindowOpenHandler=()=>{};this.webContents.send=(_channel,value)=>stateCallback?.(value);window=this;}isDestroyed(){return false;}async loadURL(){}show(){}}
 const ps=new EventEmitter();Object.assign(ps,{setPermissionRequestHandler(){},setPermissionCheckHandler(){},setDevicePermissionHandler(){},webRequest:{onBeforeRequest(){}},protocol:{handle:async()=>{}}});
 const electron={app,BrowserWindow:Window,session:{fromPartition:()=>ps},protocol:{registerSchemesAsPrivileged(){}},ipcMain:{handle:(_name,h)=>handler=h},dialog:{showMessageBoxSync:(_window,prompt)=>{prompts.push(prompt);return 0;},showErrorBox:(title,message)=>errors.push({title,message}),showOpenDialog:async()=>({canceled:true})},Menu:{buildFromTemplate:t=>t,setApplicationMenu(){}},powerMonitor:new EventEmitter()};
 class ActualController extends Controller{constructor(options){super({...options,bundle:async()=>({binary:'/synthetic'}),engineFactory:()=>engine});controller=this;}}
 class ActualScheduler extends UpdateScheduler{constructor(options){super({...options,now:clock.now,setTimer:clock.setTimer,clearTimer:clock.clearTimer,timing:schedulerTiming});scheduler=this;}}
 const main=path.join(__dirname,'../src/main.cjs'),req=createRequire(main);
 try{
 vm.runInNewContext(await fs.readFile(main,'utf8'),{require:name=>name==='electron'?electron:name==='./controller.cjs'?{Controller:ActualController}:name==='./updater.cjs'?{configureUpdater:()=>update}:name==='./update-scheduler.cjs'?{UpdateScheduler:ActualScheduler}:req(name),__dirname:path.dirname(main),process,setInterval:()=>({unref(){}}),clearInterval(){},setTimeout,clearTimeout,console});
 for(let i=0;i<100&&!scheduler;i++)await new Promise(resolve=>setTimeout(resolve,1));assert.ok(scheduler,'Actual main reached scheduler creation');
 const sender=()=>({sender:window.webContents,senderFrame:window.webContents.mainFrame});assert.equal((await handler(sender(),{action:'create',params:{name:'Synthetic'}})).ok,true);
 const api={onChange:callback=>stateCallback=callback,async perform(action,params={}){ipc.push({action,params:structuredClone(params)});const result=await handler(sender(),structuredClone({action,params}));if(holdTemplate&&action==='tool'&&params.name==='mesh_note_template'){holdTemplate=false;await new Promise(resolve=>templateRelease=resolve);}return result;}};
 const all=()=>{const result=new Set();const visit=element=>{result.add(element);for(const child of element.children||[])visit(child);};for(const element of elements.values())visit(element);return[...result];};
 const document={getElementById:get,createElement:tag=>new EditorElement(tag),createTextNode:text=>({textContent:text}),querySelectorAll:()=>all()};
 vm.runInNewContext(await fs.readFile(path.join(__dirname,'../renderer/shell.js'),'utf8'),{window:{meshDesktop:api,addEventListener(){}},document,confirm:()=>{assert.fail('Quiet scheduler must not discard through renderer confirmation');},console});await sourceTurns();
 return{clock,get,engine,controller,scheduler,update,app,window,ipc,prompts,errors,handler,sender,get quitCount(){return quitCount;},holdNextTemplate(){holdTemplate=true;},releaseTemplate(){assert.ok(templateRelease);templateRelease();},get templatePending(){return!!templateRelease;},async cleanup(){templateRelease?.();await scheduler.stop();await controller.close().catch(()=>{});await fs.rm(dir,{recursive:true,force:true});}};
 }catch(error){templateRelease?.();await scheduler?.stop();await controller?.close().catch(()=>{});await fs.rm(dir,{recursive:true,force:true});throw error;}
}
test('actual main and renderer quietly defer dirty buffers, restore controls, then drain only after a new clean acknowledgement',async()=>{
 const f=await scheduledMainFixture();try{
 f.get('new-note').fire('click');await sourceTurns();assert.equal(f.get('editor').hidden,false);f.get('note-title').value='Unsaved engineering incident';f.get('note-form').fire('input');await sourceTurns();
 await f.clock.advance(10);const dirty=f.ipc.filter(call=>call.action==='editor-state'&&call.params.restart_token);assert.equal(dirty.length,1);assert.equal(dirty[0].params.dirty,true);assert.equal(dirty[0].params.operation,false);assert.match(dirty[0].params.restart_token,/^[a-f0-9]{48}$/);
 assert.equal(f.prompts.length,0);assert.equal(f.errors.length,0);assert.equal(f.update.stages,0);assert.equal(f.engine.closed,undefined);assert.equal(f.get('note-title').value,'Unsaved engineering incident');assert.equal(f.get('editor').hidden,false);assert.equal(f.get('save-draft').disabled,false);assert.equal(f.controller.restartToken,null);
 assert.equal((await f.handler(f.sender(),{action:'editor-state',params:dirty[0].params})).ok,false,'Consumed acknowledgement cannot be reused');
 // A human saves through the existing draft button; the scheduler neither
 // saves nor clears the dirty editor on its own. The shared receipt is a
 // synthetic, contract-shaped fixture rather than a real vault publication.
 f.get('save-draft').fire('click');await sourceTurns();assert.equal(f.get('message').textContent,'Draft saved.');assert.equal(f.get('note-title').value,'Unsaved engineering incident');assert.equal(f.update.stages,0);
 await f.clock.advance(100);const acknowledgements=f.ipc.filter(call=>call.action==='editor-state'&&call.params.restart_token);assert.equal(acknowledgements.length,2);assert.equal(acknowledgements[1].params.dirty,false);assert.notEqual(acknowledgements[1].params.restart_token,dirty[0].params.restart_token);assert.equal(f.update.stages,1);assert.equal(f.engine.closed,true);assert.equal(f.prompts.length,0);
 }finally{await f.cleanup();}
});
test('fresh renderer operation state defers automatic restart even after the core tool receipt settled',async()=>{
 const f=await scheduledMainFixture();try{f.holdNextTemplate();f.get('new-note').fire('click');await sourceTurns();assert.equal(f.templatePending,true);assert.equal(f.controller.tools.size,0);assert.equal(f.get('new-note').disabled,true);await f.clock.advance(10);
 const ack=f.ipc.filter(call=>call.action==='editor-state'&&call.params.restart_token).at(-1);assert.equal(ack.params.operation,true);assert.equal(f.update.stages,0);assert.equal(f.engine.closed,undefined);assert.equal(f.prompts.length,0);assert.equal(f.get('new-note').disabled,true);
 f.releaseTemplate();await sourceTurns();assert.equal(f.get('new-note').disabled,false);await f.clock.advance(100);assert.equal(f.update.stages,1);assert.equal(f.engine.closed,true);assert.equal(f.prompts.length,0);
 }finally{await f.cleanup();}
});
test('scheduled restart waits for actual synthetic NDJSON child OS exit rather than its close reply',async()=>{
 const{spawn}=require('node:child_process'),{Engine}=require('../src/engine.cjs'),dir=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-scheduler-os-exit-'));let child,closeReply;const closing=new Promise(resolve=>closeReply=resolve),clock=new SchedulerClock(),update=updater();
 const script="const newline=String.fromCharCode(10);let buffer='';process.stdin.on('data',chunk=>{buffer+=chunk;while(buffer.includes(newline)){const at=buffer.indexOf(newline),line=buffer.slice(0,at);buffer=buffer.slice(at+1);const request=JSON.parse(line);process.stdout.write(JSON.stringify({protocol:1,id:request.id,result:{closing:request.method==='close'}})+newline);if(request.method==='close')setTimeout(()=>process.exit(0),150);}});";
 const engine=new Engine({binary:'/synthetic-not-used',vault:dir,spawnProcess:(_binary,_args,options)=>{child=spawn(process.execPath,['-e',script],options);child.stdout.on('data',data=>{if(data.toString().includes('"closing":true'))closeReply();});return child;},timeout:1000});engine.start();
 const controller=new Controller({storage:{cards:()=>[]},updater:update,appVersion:'0.1.0'});controller.engine=engine;controller.selected={id:'fixture',name:'Fixture'};controller.card=card();controller.phase='vault';
 const scheduler=new UpdateScheduler({controller,confirmEditor:async()=>true,now:clock.now,setTimer:clock.setTimer,clearTimer:clock.clearTimer,timing:schedulerTiming});try{scheduler.start();await clock.advance(10);await closing;assert.equal(child.exitCode,null);assert.equal(update.stages,0);assert.equal(controller.engine,engine);await scheduler.flight;assert.equal(child.exitCode,0);assert.equal(engine.child,null);assert.equal(controller.engine,null);assert.equal(update.stages,1);}finally{await scheduler.stop();await engine.close();await fs.rm(dir,{recursive:true,force:true});}
});

async function lifecyclePendingDiscovery(){
 const{PassThrough}=require('node:stream'),{PreparedUpdater,ReleaseLedger}=require('../src/updater.cjs');const root=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-quit-discovery-'));let signalReady;const ready=new Promise(resolve=>signalReady=resolve),transport={request:null,response:null};
 const request=(_url,_options,callback)=>{const req=new EventEmitter();req.destroy=()=>req.destroyed=true;transport.request=req;setImmediate(()=>{const response=new PassThrough();response.statusCode=200;response.headers={'content-type':'application/json'};transport.response=response;callback(response);response.write('{');signalReady();});return req;};
 const native=new EventEmitter();native.setFeedURL=native.checkForUpdates=native.quitAndInstall=()=>assert.fail('No native staging authority in cancellation fixture');
 const update=new PreparedUpdater({native,installed:{platform:'darwin',arch:'arm64',bundle_id:'com.brightinteraction.mesh.desktop',app_version:'0.1.0',team_id:'FIXTURE123',distribution_signed:true,notarized:true},admit:()=>assert.fail('Incomplete discovery cannot certify a release'),ledger:new ReleaseLedger(root),temporary:root,request});return{root,ready,transport,update};
}
test('ordinary main quit cancels actual discovery and awaits both transfer cleanup and original owner drain',async()=>{
 const pending=await lifecyclePendingDiscovery(),engine=new FakeEngine();let finishClose;engine.close=()=>new Promise(resolve=>finishClose=()=>{engine.closed=true;resolve();});const f=await scheduledMainFixture({update:pending.update,engine});
 try{await f.clock.advance(10);await pending.ready;let prevented=false;f.app.emit('before-quit',{preventDefault:()=>prevented=true});assert.equal(prevented,true);await sourceTurns();assert.equal(pending.transport.request.destroyed,true);assert.equal(pending.transport.response.destroyed,true);assert.equal(pending.update.flight,null);assert.deepEqual(await fs.readdir(pending.root),[]);assert.equal(f.quitCount,0);assert.equal(f.controller.engine,engine);assert.ok(finishClose);finishClose();await sourceTurns();assert.equal(f.quitCount,1);assert.equal(f.controller.engine,null);assert.equal(f.scheduler.running,false);assert.equal(f.clock.tasks.size,0);
 }finally{finishClose?.();await f.cleanup();await fs.rm(pending.root,{recursive:true,force:true});}
});
test('refused ordinary quit resumes bounded scheduling after real cancellation without replaying an active tool',async()=>{
 const pending=await lifecyclePendingDiscovery(),f=await scheduledMainFixture({update:pending.update});let finishRead,reads=0;const original=f.engine.request.bind(f.engine);f.engine.request=(method,params)=>{if(method==='tool'&&params.name==='mesh_fetch'){reads++;return new Promise(resolve=>finishRead=()=>resolve({content:[{type:'text',text:'Inspected once.'}]}));}return original(method,params);};let read;
 try{read=f.handler(f.sender(),{action:'tool',params:{name:'mesh_fetch',arguments:{id:'fixture'}}});await f.clock.advance(10);await pending.ready;f.app.emit('before-quit',{preventDefault(){}});await sourceTurns();assert.equal(pending.transport.request.destroyed,true);assert.equal(pending.transport.response.destroyed,true);assert.equal(pending.update.flight,null);assert.equal(f.quitCount,0);assert.equal(f.scheduler.running,true);assert.equal(f.scheduler.paused,false);assert.equal(f.clock.tasks.size,1);assert.equal(f.controller.engine,f.engine);assert.equal(f.engine.closed,undefined);assert.equal(reads,1);assert.ok(f.errors.length);finishRead();await read;assert.equal(reads,1);
 }finally{finishRead?.();await read?.catch(()=>{});await f.cleanup();await fs.rm(pending.root,{recursive:true,force:true});}
});

test('quiet internal deferral preserves prior status while manual, physical drain and native failures remain visible',async()=>{
 const automatic=await fixture(),previous={code:'PRIOR_FIXTURE_STATUS',message:'Existing status retained.'};automatic.c.error=previous;await assert.rejects(automatic.c.restartForUpdate({automatic:true,confirmEditor:async()=>false}),/EDITOR_NOT_READY/);assert.equal(automatic.c.error,previous);assert.equal(automatic.engine.closed,undefined);assert.equal(automatic.update.stages,0);
 const manual=await fixture();await assert.rejects(manual.c.restartForUpdate({confirmEditor:async()=>false}),/EDITOR_NOT_READY/);assert.equal(manual.c.error.code,'UPDATE_EDITOR_NOT_READY');assert.equal(manual.engine.closed,undefined);
 const close=await fixture();close.engine.close=async()=>{throw new Error('UPDATE_EDITOR_NOT_READY');};await assert.rejects(close.c.restartForUpdate({automatic:true,confirmEditor:async()=>true}),/EDITOR_NOT_READY/);assert.equal(close.c.error.code,'UPDATE_EDITOR_NOT_READY','Error whitelist cannot hide a physical drain failure');assert.equal(close.c.engine,close.engine);assert.equal(close.update.stages,0);
 const native=await fixture();native.update.install=async({drain})=>{await drain();native.update.state='uncertain';throw new Error('UPDATE_EDITOR_NOT_READY');};await assert.rejects(native.c.restartForUpdate({automatic:true,confirmEditor:async()=>true}),/EDITOR_NOT_READY/);assert.equal(native.c.error.code,'UPDATE_EDITOR_NOT_READY');assert.equal(native.c.phase,'update-recovery');assert.equal(native.c.engine,null);assert.equal(native.c.updateLock,true);assert.equal(native.update.state,'uncertain');
});
