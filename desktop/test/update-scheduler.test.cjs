'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),{EventEmitter,getEventListeners}=require('node:events'),{PassThrough}=require('node:stream'),fs=require('node:fs/promises'),os=require('node:os'),path=require('node:path');
const {Controller}=require('../src/controller.cjs'),{UpdateScheduler}=require('../src/update-scheduler.cjs'),{PreparedUpdater,ReleaseLedger,configureUpdater,transfer}=require('../src/updater.cjs');
const flush=async()=>{for(let i=0;i<8;i++)await new Promise(resolve=>setImmediate(resolve));};
class Clock{
 constructor(){this.time=0;this.tasks=new Map();this.next=0;}
 now=()=>this.time;
 setTimer=(fn,delay)=>{const token={id:++this.next,unref(){}};this.tasks.set(token,{fn,at:this.time+delay});return token;};
 clearTimer=token=>this.tasks.delete(token);
 async advance(by){this.time+=by;for(let rounds=0;;rounds++){assert.ok(rounds<20,'No timer catch-up burst');const due=[...this.tasks].filter(([,value])=>value.at<=this.time);if(!due.length)break;for(const[token,value]of due){this.tasks.delete(token);value.fn();}await flush();}}
}
const timing={startup:10,interval:1000,settle:5,defer:100,retry:[50,200,1000]};
const card=()=>({state:'ready',vault:{name:'Fixture',id:'fixture'},sync:{state:'unjoined',pending:0,conflicts:0,last_success:null},identity:null,version:'0.42.17'});
function fixture({state='idle',confirm=async()=>true}={}){
 const clock=new Clock(),engine=new EventEmitter();engine.close=async()=>{engine.closed=true;};engine.request=async()=>({});
 const updater={state,checks:0,stages:0,disposed:0,status(){return{state:this.state,reason:'Synthetic mechanism fixture; no signing proof.'};},async check(){this.checks++;this.state='available';},async install({drain}){await drain();this.stages++;this.state='ready';},async dispose(){this.disposed++;}};
 const controller=new Controller({storage:{cards:()=>[]},updater,appVersion:'0.1.0'});controller.engine=engine;controller.selected={id:'fixture',name:'Fixture'};controller.card=card();controller.phase='vault';
 const scheduler=new UpdateScheduler({controller,confirmEditor:confirm,now:clock.now,setTimer:clock.setTimer,clearTimer:clock.clearTimer,timing});return{clock,engine,updater,controller,scheduler};
}
test('production configuration cannot schedule a channel or accept renderer-style configuration',async()=>{
 const updater=configureUpdater(null),controller=new Controller({storage:{cards:()=>[]},updater,appVersion:'0.1.0'}),clock=new Clock(),scheduler=new UpdateScheduler({controller,confirmEditor:()=>assert.fail('No renderer restart'),now:clock.now,setTimer:clock.setTimer,clearTimer:clock.clearTimer,timing});
 assert.equal(scheduler.start(),false);scheduler.wake();await clock.advance(100000);assert.equal(clock.tasks.size,0);assert.throws(()=>configureUpdater({signed:true}),/UNAPPROVED/);await scheduler.stop();
});
test('startup discovery is serialized and an available release uses the actual controller drain once',async()=>{
 const f=fixture();try{assert.equal(f.scheduler.start(),true);assert.equal(f.scheduler.start(),false);await f.clock.advance(9);assert.equal(f.updater.checks,0);await f.clock.advance(1);assert.equal(f.updater.checks,1);assert.equal(f.updater.stages,0);await f.clock.advance(5);assert.equal(f.engine.closed,true);assert.equal(f.controller.engine,null);assert.equal(f.updater.stages,1);for(let i=0;i<20;i++)f.scheduler.wake();await f.clock.advance(100000);assert.equal(f.updater.stages,1);}finally{await f.scheduler.stop();}
});
test('resume bursts do not enqueue duplicate checks or bypass capped failure backoff',async()=>{
 const f=fixture();f.updater.check=async()=>{f.updater.checks++;throw new Error('UPDATE_TRANSFER_FAILED');};try{
 f.scheduler.start();await f.clock.advance(10);assert.equal(f.updater.checks,1);for(let i=0;i<50;i++)f.scheduler.wake();await f.clock.advance(49);assert.equal(f.updater.checks,1);await f.clock.advance(1);assert.equal(f.updater.checks,2);
 for(let i=0;i<50;i++)f.scheduler.wake();await f.clock.advance(199);assert.equal(f.updater.checks,2);await f.clock.advance(1);assert.equal(f.updater.checks,3);await f.clock.advance(100000);assert.equal(f.updater.checks,4,'Missed intervals yield one current attempt');assert.equal(f.clock.tasks.size,1);
 }finally{await f.scheduler.stop();}
});
test('an in-flight discovery has one owner; pause awaits cancellation and no stale completion stages',async()=>{
 const f=fixture();let settle,aborted=false;f.updater.check=({signal})=>{f.updater.checks++;return new Promise(resolve=>{settle=()=>{f.updater.state='available';resolve();};signal.addEventListener('abort',()=>{aborted=true;},{once:true});});};
 f.scheduler.start();await f.clock.advance(10);for(let i=0;i<50;i++){f.scheduler.wake();f.scheduler.tick();}assert.equal(f.updater.checks,1);let stopped=false;const pause=f.scheduler.pause().then(()=>stopped=true);await flush();assert.equal(aborted,true);assert.equal(stopped,false,'Ignored cancellation is not completion');settle();await pause;assert.equal(f.updater.stages,0);assert.equal(f.clock.tasks.size,0);await f.scheduler.stop();
});
test('dirty editor refusal preserves the owner and quietly defers until a later clean acknowledgement',async()=>{
 let clean=false;const f=fixture({state:'available',confirm:async()=>clean});try{f.scheduler.start();await f.clock.advance(10);assert.equal(f.updater.stages,0);assert.equal(f.engine.closed,undefined);assert.equal(f.controller.updateLock,false);assert.equal(f.controller.restartToken,null);clean=true;for(let i=0;i<20;i++)f.scheduler.wake();await f.clock.advance(99);assert.equal(f.updater.stages,0);await f.clock.advance(1);assert.equal(f.updater.stages,1);}finally{await f.scheduler.stop();}
});
test('actual author requests, write uncertainty and unsettled enrollment forbid unattended drain without replay',async()=>{
 for(const mode of ['active','uncertain','joining','join-uncertain','joined-preparing']){
 const f=fixture({state:'available',confirm:()=>assert.fail('Blocked before editor confirmation')});let complete,pending,writes=0;
 if(mode==='active'){f.engine.request=()=>{writes++;return new Promise(resolve=>complete=resolve);};pending=f.controller.perform('tool',{name:'mesh_author_note',arguments:{action:'publish',note:{}}});}
 else if(mode==='uncertain'){f.engine.request=async()=>{writes++;throw new Error('ENGINE_TIMEOUT');};await assert.rejects(f.controller.perform('tool',{name:'mesh_author_note',arguments:{action:'draft',note:{}}}));}
 else f.controller.card.state=mode;
 try{f.scheduler.start();await f.clock.advance(10);assert.equal(f.updater.stages,0);assert.equal(f.engine.closed,undefined);if(pending){complete({isError:true,content:[{type:'text',text:'Unknown fixture outcome'}]});await pending;}assert.ok(writes<=1);assert.equal(f.controller.engine,f.engine);}finally{await f.scheduler.stop();}
 }
});
test('unconfirmed owner exit or native uncertainty halts automatic attempts and cannot be cleared by resume/stop',async()=>{
 for(const mode of ['exit','native']){const f=fixture({state:'available'});let closes=0;
 if(mode==='exit')f.engine.close=async()=>{closes++;throw new Error('ENGINE_EXIT_UNCERTAIN');};
 else f.updater.install=async({drain})=>{await drain();f.updater.stages++;f.updater.state='uncertain';throw new Error('NATIVE_UPDATE_UNCERTAIN');};
 f.scheduler.start();await f.clock.advance(10);assert.equal(f.scheduler.halted,true);for(let i=0;i<20;i++){f.scheduler.wake();f.scheduler.resume();}await f.clock.advance(100000);assert.equal(closes,mode==='exit'?1:0);assert.equal(f.updater.stages,mode==='native'?1:0);await f.scheduler.stop();if(mode==='native'){assert.equal(f.controller.updateLock,true);assert.equal(f.updater.state,'uncertain');assert.equal(f.updater.disposed,0);}else assert.equal(f.controller.engine,f.engine);
 }
});
function pendingTransport(){let ready;const state={requests:[],response:null,ready:new Promise(resolve=>ready=resolve)};state.request=(_url,_options,callback)=>{const request=new EventEmitter();request.destroy=()=>request.destroyed=true;state.requests.push(request);setImmediate(()=>{const response=new PassThrough();response.statusCode=200;response.headers={'content-type':'application/json'};state.response=response;callback(response);response.write('{');ready();});return request;};return state;}
async function preparedPending(){const root=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-scheduler-cancel-'));const transport=pendingTransport(),native=new EventEmitter();native.setFeedURL=native.checkForUpdates=native.quitAndInstall=()=>assert.fail('No native update authority in cancellation fixture');
 const updater=new PreparedUpdater({native,installed:{platform:'darwin',arch:'arm64',bundle_id:'com.brightinteraction.mesh.desktop',app_version:'0.1.0',team_id:'FIXTURE123',distribution_signed:true,notarized:true},admit:()=>assert.fail('Incomplete bytes cannot certify admission'),ledger:new ReleaseLedger(root),temporary:root,request:transport.request});return{root,updater,transport};}
test('actual PreparedUpdater cancellation destroys transfer, removes listeners and cleans private held files before completion',async()=>{
 const f=await preparedPending(),abort=new AbortController(),check=f.updater.check({signal:abort.signal});check.catch(()=>{});try{
 await f.transport.ready;assert.equal(f.transport.requests.length,1);assert.equal(f.updater.status().state,'checking');abort.abort();await assert.rejects(check,/CHECK_CANCELLED/);assert.equal(f.transport.requests[0].destroyed,true);assert.equal(f.transport.response.destroyed,true);assert.equal(getEventListeners(abort.signal,'abort').length,0);assert.deepEqual(await fs.readdir(f.root),[]);assert.equal(f.updater.flight,null);assert.equal(f.updater.blobs.length,0);
 }finally{abort.abort();await check.catch(()=>{});await f.updater.dispose();await fs.rm(f.root,{recursive:true,force:true});}
});
test('already-cancelled discovery does not make any request or allocate a held blob',async()=>{
 const f=await preparedPending(),abort=new AbortController();abort.abort();try{await assert.rejects(f.updater.check({signal:abort.signal}),/CHECK_CANCELLED/);assert.equal(f.transport.requests.length,0);assert.deepEqual(await fs.readdir(f.root),[]);assert.equal(getEventListeners(abort.signal,'abort').length,0);}finally{await fs.rm(f.root,{recursive:true,force:true});}
});
test('transfer cancellation awaits an actual pending body writer before releasing its owner',async()=>{
 const abort=new AbortController(),transport=pendingTransport();let finishWrite,settled=false;const writing=new Promise(resolve=>finishWrite=()=>resolve({bytesWritten:1}));
 const result=transfer('https://mesh.brightinteraction.com/desktop/releases/stable/darwin-arm64.json',{limit:10,kind:'metadata',writer:{write:()=>writing},request:transport.request,signal:abort.signal}).then(()=>{settled=true;},error=>{settled=true;throw error;});
 await flush();abort.abort();await flush();assert.equal(settled,false);assert.equal(transport.response.destroyed,true);assert.equal(transport.requests[0].destroyed,true);finishWrite();await assert.rejects(result,/CHECK_CANCELLED/);assert.equal(getEventListeners(abort.signal,'abort').length,0);
});
test('scheduler pause binds actual cancellable discovery and stop drains before discarding held bytes',async()=>{
 const f=await preparedPending(),clock=new Clock(),controller=new Controller({storage:{cards:()=>[]},updater:f.updater,appVersion:'0.1.0'}),scheduler=new UpdateScheduler({controller,confirmEditor:()=>assert.fail('No restart from cancellation'),now:clock.now,setTimer:clock.setTimer,clearTimer:clock.clearTimer,timing});
 try{scheduler.start();await clock.advance(10);await f.transport.ready;await scheduler.pause();assert.equal(f.transport.requests[0].destroyed,true);assert.deepEqual(await fs.readdir(f.root),[]);assert.equal(clock.tasks.size,0);await scheduler.stop();assert.equal(f.updater.flight,null);assert.equal(f.updater.blobs.length,0);}finally{await scheduler.stop();await fs.rm(f.root,{recursive:true,force:true});}
});
