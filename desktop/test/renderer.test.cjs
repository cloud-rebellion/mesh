'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs/promises'),path=require('node:path'),vm=require('node:vm');
class Element {
  constructor(tag='div'){this.tag=tag;this.children=[];this.listeners={};this.dataset={};this.hidden=false;this.value='';this.textContent='';this.sourceChanges=[];this.disabled=false;}
  set src(value){this.sourceChanges.push(value);this._src=value;}get src(){return this._src;}
  addEventListener(name,fn){(this.listeners[name]??=[]).push(fn);}
  append(...children){this.children.push(...children);}
  replaceChildren(...children){this.children=children;}
  removeAttribute(name){delete this[name];}
  get selectedOptions(){return[this.children.find(c=>c.value===this.value)||this.children[0]].filter(Boolean);}
  querySelectorAll(selector){const all=[];const visit=el=>{if(el.dataset?.section)all.push(el);for(const child of el.children||[])visit(child);};visit(this);return all;}
  fire(name){for(const fn of this.listeners[name]||[])fn({preventDefault(){}});}
  showModal(){}close(){this.fire('close');}
}
const definitions={
  'post-mortem':{type:'post-mortem',keys:['what_happened','impact','timeline','root_cause','resolution','follow_up']},
  plan:{type:'note',keys:['objective','context','approach','milestones','success_measures','dependencies']},
  procedure:{type:'concept',keys:['prerequisites','steps','expected_result','verification','recovery']}
};
const flush=async()=>{for(let i=0;i<20;i++)await new Promise(r=>setImmediate(r));};
async function fixture({valid=true,draftError=false,update=null,delayedTool=null,prepareFailures=0,joinState=null}={}){
  const elements=new Map();const get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
  get('editor').hidden=true;get('draft-list').hidden=true;
  const calls=[],state={phase:'vault',vaults:[{id:'fixture',name:'Fixture',managed:true}],selected:{id:'fixture',name:'Fixture'},status:{state:'ready',vault:{name:'Fixture',id:'fixture'},sync:{state:'offline',pending:0,conflicts:0,last_success:null},identity:null},error:null,app_version:'0.1.0',updates:{state:'disabled',reason:'Unconfigured'},credentials:{osProtected:false},enrollment:{enabled:true,reason:'Use an invitation'}};
  const respond=value=>({ok:true,result:{content:[{type:'text',text:JSON.stringify(value)}]}});let stateCallback;
  const api={onChange:callback=>stateCallback=callback,async perform(action,params){calls.push(JSON.parse(JSON.stringify({action,params})));
    if(action==='editor-state')return{ok:true,result:{recorded:true}};
    if(action==='overview')return{ok:true,result:state};
    if(action==='join')return{ok:true,result:joinState||state};
    const {name,arguments:args}=params;
    if(delayedTool&&delayedTool.name===name&&(!delayedTool.action||delayedTool.action===args.action))return await delayedTool.wait;
    if(name==='mesh_templates')return respond({templates:Object.entries(definitions).map(([id,d])=>({id,type:d.type,version:1})),blocks:[{id:'table',version:1}]});
    if(name==='mesh_note_template'){const definition=definitions[args.template];return respond({template:{id:args.template,version:1,type:definition.type,sections:definition.keys.map(key=>({key,heading:key,guidance:'Use supported facts.'}))}});}
    if(name==='mesh_block_template')return respond({id:'table',version:1,fields:[{key:'purpose'},{key:'table'},{key:'source'},{key:'limitations'}]});
    if(name==='mesh_prepare_update'){if(prepareFailures-->0)return{ok:false,error:{code:'TOOL_FAILED',message:'Unavailable fixture reader'}};return respond({note:update||{title:'Saved',template:'post-mortem',template_version:1,update_id:'saved',update_revision:'b'.repeat(64),summary:'Saved',sections:{}}});}
    if(name==='mesh_search')return respond({cards:[{NoteID:'saved',Title:'Human title selection'},{NoteID:'../../escape',Title:'Refused alias'}]});
    if(name==='mesh_drafts')return respond({drafts:[{id:'historical-draft',title:'Historical draft',template:'procedure'}],more:false});
    if(name==='mesh_author_note'){
      if(args.action==='prepare')return respond({saved:false,markdown:'# Prepared fixture\n'+args.note.summary});
      if(args.action==='validate')return respond({valid,issues:valid?[]:['Missing supported verification']});
      if(args.action==='draft'&&draftError)throw new Error('Uncertain write fixture');
      return respond({id:'saved',revision:'b'.repeat(64)});
    }
    throw new Error('Unexpected fixture operation');
  }};
  const document={getElementById:get,createElement:tag=>new Element(tag),createTextNode:text=>({textContent:text}),querySelectorAll:()=>{
    const found=new Set();function visit(element){if(!element||found.has(element))return;found.add(element);for(const child of element.children||[])visit(child);}
    for(const element of elements.values())visit(element);return[...found].filter(element=>'disabled'in element||['button','input','textarea','select'].includes(element.tag));
  }};
  const events={},window={meshDesktop:api,addEventListener:(name,fn)=>events[name]=fn};get('viewer').contentWindow={};
  vm.runInNewContext(await fs.readFile(path.join(__dirname,'../renderer/shell.js'),'utf8'),{window,document,confirm:()=>true,console});await flush();
  return{get,calls,events,state,stateCallback};
}
test('actual page script prepares and validates engineering, marketing and sales notes using selected fixed template sections',async()=>{
  for(const [template,title] of [['post-mortem','Engineering fixture incident'],['plan','Marketing fixture strategy'],['procedure','Sales fixture procedure']]){
    const {get,calls}=await fixture();get('new-note').fire('click');await flush();
    assert.equal(get('template').disabled,false,'A human new-note author can choose an approved template');
    get('template').value=template;get('template').fire('change');await flush();
    get('note-title').value=title;get('summary').value='A bounded illustrative fixture, not live evidence.';
    for(const field of get('sections').querySelectorAll('[data-section]'))field.value='Unknown; this fixture does not establish a factual result.';
    get('tags').value='fixture, example';get('related').value='existing-note';get('publish').fire('click');await flush();
    const published=calls.find(c=>c.params?.arguments?.action==='publish').params.arguments.note;
    assert.equal(published.title,title);assert.equal(published.template,template);assert.equal(published.template_version,1);assert.deepEqual(Object.keys(published.sections),definitions[template].keys);assert.deepEqual(published.blocks,[]);
    assert.equal('created' in published,false);assert.equal('verified_at' in published,false);assert.deepEqual(published.related,['existing-note']);
    assert.ok(calls.findIndex(c=>c.params?.arguments?.action==='validate')<calls.findIndex(c=>c.params?.arguments?.action==='publish'));
    assert.equal(get('join').disabled,false);
  }
});
test('actual page script denies invalid publication, omits untouched optional blocks and never auto-retries an uncertain draft',async()=>{
  const {get,calls}=await fixture({valid:false,draftError:true});get('new-note').fire('click');await flush();get('summary').value='Fixture';
  get('add-block').fire('click');await flush();get('publish').fire('click');await flush();assert.equal(calls.filter(c=>c.params?.arguments?.action==='publish').length,0);
  get('save-draft').fire('click');get('save-draft').fire('click');await flush();
  const drafts=calls.filter(c=>c.params?.arguments?.action==='draft');assert.equal(drafts.length,1);assert.deepEqual(drafts[0].params.arguments.note.blocks,[]);assert.equal(get('message').textContent,'Uncertain write fixture');
});
test('actual edit page retains original update revision, historical content and supported metadata without forging verification',async()=>{
  const update={title:'Historical fixture',summary:'Preserved caveat',template:'procedure',template_version:1,type:'concept',update_id:'historical',update_revision:'a'.repeat(64),sections:{prerequisites:'Original',steps:'Original ordered steps',expected:'Unknown',verification:'Historical evidence',recovery:'Original'},tags:['legacy'],related:['method'],supersedes:['older'],status:'active'};
  const {get,calls}=await fixture({update});get('edit-id').value='historical';get('edit-dialog').returnValue='open';get('edit-dialog').fire('close');await flush();
  assert.equal(get('template').disabled,true,'Published note retains its approved template/version');
  get('summary').value='Preserved caveat, clarified';get('publish').fire('click');await flush();
  const note=calls.find(c=>c.params?.arguments?.action==='publish').params.arguments.note;
  assert.equal(note.update_revision,update.update_revision);assert.equal(note.update_id,'historical');assert.equal(note.sections.verification,'Historical evidence');assert.deepEqual(note.supersedes,['older']);assert.equal('verified_at' in note,false);
});
test('actual draft page requests lossless shared preparation and preserves the draft CAS while validating active publication',async()=>{
  const update={title:'Historical draft',summary:'Preserved caveat',template:'procedure',template_version:1,type:'concept',draft_id:'historical-draft',draft_revision:'c'.repeat(64),sections:{prerequisites:'Original',steps:'Original ordered steps',expected:'Unknown',verification:'Historical evidence',recovery:'Original'},tags:['legacy'],related:['method'],status:'draft'};
  const {get,calls}=await fixture({update});get('drafts').fire('click');await flush();get('draft-results').children[0].fire('click');await flush();
  assert.equal(get('template').disabled,true,'Resumed draft retains its approved template/version');
  const prepared=calls.find(c=>c.params?.name==='mesh_prepare_update');assert.equal(prepared.params.arguments.draft,true);
  get('publish').fire('click');await flush();const validated=calls.find(c=>c.params?.arguments?.action==='validate').params.arguments.note;
  assert.equal(validated.draft_revision,update.draft_revision);assert.equal(validated.draft_id,'historical-draft');assert.equal('update_id' in validated,false);assert.equal(validated.status,'active');assert.equal(validated.sections.verification,'Historical evidence');assert.equal('verified_at' in validated,false);
});
test('viewer selection checks exact origin, expected iframe window and bounded closed payload; it cannot publish',async()=>{
  const {get,calls,events}=await fixture();
  const valid={origin:'mesh-app://viewer',source:get('viewer').contentWindow,data:{kind:'mesh-note-selection',id:'stable-note'}};
  for(const event of [{...valid,origin:'https://evil.invalid'},{...valid,source:{}},{...valid,data:{...valid.data,action:'publish'}},{...valid,data:{kind:'mesh-note-selection',id:'../path'}}])events.message(event);
  await flush();assert.equal(calls.filter(c=>c.params?.name==='mesh_prepare_update').length,0);
  events.message(valid);await flush();assert.equal(calls.filter(c=>c.params?.name==='mesh_prepare_update').length,1);assert.equal(calls.filter(c=>c.params?.arguments?.action==='publish').length,0);
});
test('delayed prior-session editable content cannot enter the new vault editor',async()=>{
  let resolve;const wait=new Promise(r=>resolve=r);const {get,state,stateCallback}=await fixture({delayedTool:{name:'mesh_prepare_update',wait}});
  get('edit-id').value='prior-note';get('edit-dialog').returnValue='open';get('edit-dialog').fire('close');await flush();
  stateCallback({...state,selected:{id:'second',name:'Second'},status:{...state.status,vault:{name:'Second',id:'second'}}});
  resolve({ok:true,result:{content:[{type:'text',text:JSON.stringify({note:{title:'Prior private title',template:'procedure',template_version:1,summary:'Prior private prose',sections:{}}})}]}});await flush();
  assert.equal(get('editor').hidden,true);assert.notEqual(get('note-title').value,'Prior private title');assert.ok(get('message').textContent.includes('vault changed'));
});
test('delayed successful prior-vault save is treated as context uncertainty and is never followed by an update call on the new vault',async()=>{
  let resolve;const wait=new Promise(r=>resolve=r),delayedTool={name:'mesh_author_note',action:'publish',wait};const {get,calls,state,stateCallback}=await fixture({delayedTool});
  get('new-note').fire('click');await flush();get('note-title').value='Prior';get('summary').value='Fixture';for(const field of get('sections').querySelectorAll('[data-section]'))field.value='Fixture unknown';get('publish').fire('click');await flush();
  stateCallback({...state,selected:{id:'second',name:'Second'},status:{...state.status,vault:{name:'Second',id:'second'}}});resolve({ok:true,result:{content:[{type:'text',text:JSON.stringify({id:'prior',revision:'d'.repeat(64)})}]}});await flush();
  assert.equal(get('editor').hidden,true);assert.equal(calls.filter(c=>c.params?.name==='mesh_prepare_update').length,0);assert.equal(calls.filter(c=>c.params?.arguments?.action==='publish').length,1);assert.ok(get('message').textContent.includes('vault changed'));
});
test('confirmed publication followed by reader failure reloads for review before another write and cannot create a duplicate',async()=>{
  const {get,calls}=await fixture({prepareFailures:2});get('new-note').fire('click');await flush();get('note-title').value='Confirmed';get('summary').value='Fixture';
  get('publish').fire('click');await flush();assert.equal(calls.filter(c=>c.params?.arguments?.action==='publish').length,1);assert.ok(get('message').textContent.includes('note was published'));
  get('publish').fire('click');await flush();assert.equal(calls.filter(c=>c.params?.arguments?.action==='publish').length,1);assert.equal(calls.filter(c=>c.params?.name==='mesh_prepare_update').length,2);
  get('save-draft').fire('click');await flush();assert.equal(calls.filter(c=>c.params?.arguments?.action==='draft').length,0);assert.equal(calls.filter(c=>c.params?.arguments?.action==='publish').length,1);assert.equal(get('note-title').value,'Saved');assert.ok(get('message').textContent.includes('reloaded'));
  get('summary').value='Reviewed subsequent change';get('publish').fire('click');await flush();const writes=calls.filter(c=>c.params?.arguments?.action==='publish');assert.equal(writes.length,2);assert.equal(writes[1].params.arguments.note.update_id,'saved');assert.equal(writes[1].params.arguments.note.update_revision,'b'.repeat(64));
});
test('confirmed publication reload arriving after a vault switch cannot retain its receipt or editable content in the new vault',async()=>{
  let resolve;const wait=new Promise(r=>resolve=r);const {get,calls,state,stateCallback}=await fixture({delayedTool:{name:'mesh_prepare_update',wait}});
  get('new-note').fire('click');await flush();get('note-title').value='Prior confirmed';get('summary').value='Prior fixture';get('publish').fire('click');await flush();
  assert.equal(calls.filter(c=>c.params?.arguments?.action==='publish').length,1);
  stateCallback({...state,selected:{id:'second',name:'Second'},status:{...state.status,vault:{name:'Second',id:'second'}}});
  resolve({ok:true,result:{content:[{type:'text',text:JSON.stringify({note:{title:'Prior private note',template:'procedure',template_version:1,update_id:'prior',update_revision:'a'.repeat(64),summary:'Prior private prose',sections:{}}})}]}});await flush();
  assert.equal(get('editor').hidden,true);assert.ok(get('message').textContent.includes('vault changed'));
  get('new-note').fire('click');await flush();get('note-title').value='Second vault';get('summary').value='Second fixture';get('save-draft').fire('click');await flush();
  const draft=calls.find(c=>c.params?.arguments?.action==='draft').params.arguments.note;assert.equal(draft.title,'Second vault');assert.equal('update_id' in draft,false);assert.equal(calls.filter(c=>c.params?.name==='mesh_prepare_update').length,1);
});
test('returning to the graph after confirmed authoring refreshes it once and never after an uncertain failed write',async()=>{
  const {get}=await fixture();get('new-note').fire('click');await flush();get('summary').value='Fixture';get('publish').fire('click');await flush();const before=get('viewer').sourceChanges.length;
  get('explore').fire('click');assert.equal(get('viewer').sourceChanges.length,before+1);assert.equal(get('viewer').hidden,false);get('explore').fire('click');assert.equal(get('viewer').sourceChanges.length,before+1);
  const failed=await fixture({draftError:true});failed.get('new-note').fire('click');await flush();failed.get('save-draft').fire('click');await flush();const original=failed.get('viewer').sourceChanges.length;failed.get('discard').fire('click');assert.equal(failed.get('viewer').sourceChanges.length,original);
});

test('local-only saving is presented plainly and sync remains disabled after other authoring work',async()=>{
  const {get,state,stateCallback}=await fixture();state.status.sync={state:'unjoined',pending:5,conflicts:0,last_success:null};stateCallback(state);
  assert.equal(get('identity').textContent,'Local vault · saved on this device');assert.ok(get('sync-detail').textContent.includes('Available offline'));assert.equal(get('sync').disabled,true);
  get('new-note').fire('click');await flush();assert.equal(get('sync').disabled,true);
  stateCallback({...state,status:{...state.status,state:'join-uncertain',sync:{...state.status.sync,state:'join-uncertain'}}});assert.ok(get('message').textContent.includes('Do not redeem'));assert.equal(get('sync').disabled,true);
});
test('confirmed remote synchronization refreshes a visible graph, defers while editing and preserves the editor',async()=>{
  const {get,state,stateCallback}=await fixture();const initial=get('viewer').sourceChanges.length;
  stateCallback({...state,status:{...state.status,sync:{...state.status.sync,last_success:'2026-10-08T00:00:00Z'}}});assert.equal(get('viewer').sourceChanges.length,initial+1);
  get('new-note').fire('click');await flush();get('summary').value='Unsaved current editor';const count=get('viewer').sourceChanges.length;
  stateCallback({...state,status:{...state.status,sync:{...state.status.sync,last_success:'2026-10-08T00:00:01Z'}}});assert.equal(get('viewer').sourceChanges.length,count);assert.equal(get('summary').value,'Unsaved current editor');
  get('discard').fire('click');assert.equal(get('viewer').sourceChanges.length,count+1);
});

test('human title search exposes bounded cards and opens shared revision preparation without requiring or publishing an ID',async()=>{
  const {get,calls}=await fixture();get('edit-note').fire('click');get('edit-search').value='Human title';get('edit-search-form').fire('submit');await flush();
  const search=calls.find(c=>c.params?.name==='mesh_search');assert.equal(search.params.arguments.query,'Human title');assert.equal(search.params.arguments.limit,10);assert.equal(search.params.arguments.budget,3000);assert.equal(get('edit-results').children.length,1);assert.equal(get('edit-results').children[0].textContent,'Human title selection');
  get('edit-results').children[0].fire('click');await flush();assert.equal(calls.find(c=>c.params?.name==='mesh_prepare_update').params.arguments.id,'saved');assert.equal(get('editor').hidden,false);assert.equal(calls.filter(c=>c.params?.arguments?.action==='publish').length,0);
});
test('delayed title search cannot display private cards from an earlier vault',async()=>{
  let resolve;const wait=new Promise(r=>resolve=r);const {get,state,stateCallback}=await fixture({delayedTool:{name:'mesh_search',wait}});get('edit-search').value='Private title';get('edit-search-form').fire('submit');await flush();
  stateCallback({...state,selected:{id:'second',name:'Second'},status:{...state.status,vault:{name:'Second',id:'second'}}});resolve({ok:true,result:{content:[{type:'text',text:JSON.stringify({cards:[{NoteID:'private',Title:'Earlier private title'}]})}]}});await flush();assert.equal(get('edit-results').children.length,0);assert.ok(get('message').textContent.includes('vault changed'));
});

test('a queued detached title card from the prior vault cannot start a new-vault preparation',async()=>{
  const {get,calls,state,stateCallback}=await fixture();get('edit-search').value='Human title';get('edit-search-form').fire('submit');await flush();const old=get('edit-results').children[0];
  stateCallback({...state,selected:{id:'second',name:'Second'},status:{...state.status,vault:{name:'Second',id:'second'}}});old.fire('click');await flush();assert.equal(calls.filter(c=>c.params?.name==='mesh_prepare_update').length,0);assert.ok(get('message').textContent.includes('Search again'));
});

test('native join cancellation and pending acceptance are described truthfully while invitation text is cleared',async()=>{
  const cancel={phase:'start',vaults:[],selected:null,status:null,error:null,app_version:'0.1.0',updates:{state:'disabled',reason:'Unconfigured'},credentials:{osProtected:false},enrollment:{enabled:true,reason:'Use an invitation'}};
  const c=await fixture({joinState:cancel});c.get('join-invite').value='Synthetic transient invitation';c.get('join-name').value='Fixture';c.get('join-form').fire('submit');await flush();assert.equal(c.get('message').textContent,'Join cancelled.');assert.equal(c.get('join-invite').value,'');
  const j=await fixture();j.state.status.state='joining';j.state.status.sync.state='syncing';j.get('join-invite').value='Synthetic transient invitation';j.get('join-name').value='Fixture';j.get('join-form').fire('submit');await flush();assert.equal(j.get('message').textContent,'Joining the team. The invitation is sent once.');assert.equal(j.get('join-invite').value,'');
  j.stateCallback({...j.state,status:{...j.state.status,sync:{...j.state.status.sync,state:'idle'},identity:{user:'Synthetic teammate',role:'unknown',verified:false}}});assert.ok(j.get('message').textContent.includes('Access is not verified'));assert.ok(j.get('identity').textContent.includes('Access not verified'));
});

test('actual renderer freezes editor controls then acknowledges its dirty buffer, and deferral restores the unchanged note without authoring',async()=>{
 const{get,calls,state,stateCallback}=await fixture();get('new-note').fire('click');await flush();get('summary').value='Unsaved human buffer';get('summary').fire('input');get('note-form').fire('input');await flush();
 const writes=calls.filter(c=>['draft','publish'].includes(c.params?.arguments?.action)).length,token='d'.repeat(48);
 stateCallback({...state,updates:{state:'quiescing',reason:'Preparing a safe restart.',restart_token:token}});await flush();
 const ack=calls.find(c=>c.action==='editor-state'&&c.params.restart_token===token);assert.equal(ack.params.dirty,true);assert.equal(ack.params.operation,false);assert.equal(get('publish').disabled,true);assert.equal(get('summary').disabled,true);
 get('save-draft').fire('click');await flush();assert.equal(calls.filter(c=>['draft','publish'].includes(c.params?.arguments?.action)).length,writes);
 stateCallback({...state,updates:{state:'available',reason:'Deferred',restart_token:null}});await flush();assert.equal(get('summary').value,'Unsaved human buffer');assert.equal(get('publish').disabled,false);assert.equal(get('summary').disabled,false);assert.equal(get('editor').hidden,false);
});
test('actual renderer acknowledges active shared publication and never reports it idle merely because controls are frozen',async()=>{
 let resolve;const wait=new Promise(r=>resolve=r),{get,calls,state,stateCallback}=await fixture({delayedTool:{name:'mesh_author_note',action:'publish',wait}});get('new-note').fire('click');await flush();get('summary').value='Human authoring';get('note-form').fire('input');get('publish').fire('click');await flush();
 const token='e'.repeat(48);stateCallback({...state,updates:{state:'quiescing',reason:'Restart requested',restart_token:token}});await flush();const ack=calls.find(c=>c.params?.restart_token===token);assert.equal(ack.params.operation,true);assert.equal(ack.params.dirty,true);assert.equal(get('publish').disabled,true);
 stateCallback({...state,updates:{state:'available',reason:'Deferred',restart_token:null}});resolve({ok:true,result:{content:[{type:'text',text:JSON.stringify({id:'saved',revision:'b'.repeat(64)})}]}});await flush();assert.equal(calls.filter(c=>c.params?.arguments?.action==='publish').length,1);assert.equal(get('publish').disabled,false);
});
