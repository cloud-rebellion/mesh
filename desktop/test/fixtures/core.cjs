'use strict';
// Disposable stdio fixture. It reads/writes only the main-selected temporary fixture vault.
// This is not the Go engine, production authoring or a native enrollment authority.
const readline=require('node:readline'),fs=require('node:fs'),path=require('node:path'),{createHash}=require('node:crypto');
const identity={protocol:1,version:'0.0.0',source:'a'.repeat(40),platform:'darwin',arch:'arm64'};
if(process.argv.includes('--version')){process.stdout.write(JSON.stringify(identity));process.exit(0);}
const vault=process.argv[process.argv.indexOf('--vault')+1],store=path.join(vault,'fixture-state.json');
let state={name:'Fixture',notes:{}};try{state=JSON.parse(fs.readFileSync(store,'utf8'));}catch{}
const definitions={
  'post-mortem':{type:'post-mortem',keys:['what_happened','impact','timeline','root_cause','resolution','follow_up']},
  plan:{type:'note',keys:['objective','context','approach','milestones','success_measures','dependencies']},
  procedure:{type:'concept',keys:['prerequisites','steps','expected_result','verification','recovery']}
};
const result=value=>({content:[{type:'text',text:JSON.stringify(value)}]});
function tool(name,args){
  if(name==='mesh_templates')return result({templates:Object.entries(definitions).map(([id,d])=>({id,type:d.type,version:1})),blocks:[{id:'table',version:1}]});
  if(name==='mesh_note_template'){const d=definitions[args.template];return result({template:{id:args.template,version:1,type:d.type,sections:d.keys.map(key=>({key,heading:key,guidance:'Illustrative fixture. State unknowns.'}))}});}
  if(name==='mesh_block_template')return result({id:'table',version:1,fields:[{key:'purpose'},{key:'table'},{key:'source'},{key:'limitations'}]});
  if(name==='mesh_drafts')return result({drafts:Object.values(state.notes).filter(n=>n.status==='draft').map(n=>({id:n.id,title:n.note.title,template:n.note.template,revision:n.revision})),more:false});
  if(name==='mesh_prepare_update'){
    const saved=state.notes[args.id];if(!saved||saved.status==='draft'&&!args.draft)throw new Error('NOTE_NOT_FOUND');
    const note={...saved.note};delete note.draft_id;delete note.draft_revision;delete note.update_id;delete note.update_revision;
    if(saved.status==='draft'){note.draft_id=saved.id;note.draft_revision=saved.revision;}else{note.update_id=saved.id;note.update_revision=saved.revision;}
    return result({id:saved.id,revision:saved.revision,note,created:saved.created});
  }
  if(name==='mesh_author_note'){
    if(args.action==='prepare')return result({saved:false,markdown:'# '+args.note.title+'\n\n'+args.note.summary});
    const valid=!!args.note.summary&&Object.values(args.note.sections||{}).every(v=>v.trim());
    if(args.action==='validate')return result({valid,issues:valid?[]:['Missing supported fixture content']});
    if(args.action==='publish'&&!valid)throw new Error('INVALID_NOTE');
    const id=args.note.update_id||args.note.draft_id||args.note.title.toLowerCase().replace(/[^a-z0-9]+/g,'-').replace(/^-|-$/g,'');
    const previous=state.notes[id],revision=args.note.update_revision||args.note.draft_revision;
    if(previous&&revision!==previous.revision)throw new Error('STALE_REVISION');
    const created=previous?.created||new Date().toISOString(),nextRevision=createHash('sha256').update(JSON.stringify(args.note)+Date.now()).digest('hex');
    state.notes[id]={id,note:args.note,revision:nextRevision,created,status:args.action==='draft'?'draft':'active'};fs.writeFileSync(store,JSON.stringify(state),{mode:0o600});
    return result({id,revision:nextRevision,created});
  }
  if(name==='mesh_fetch')return result({id:args.id||'fixture',summary:'Local fixture'});
  if(name==='mesh_search')return{cards:[]};
  throw new Error('FORBIDDEN_TOOL');
}
readline.createInterface({input:process.stdin}).on('line',line=>{
  const request=JSON.parse(line);const response={protocol:1,id:request.id};
  if(request.method==='close'){response.result={closed:true};process.stdout.write(JSON.stringify(response)+'\n',()=>process.exit(0));return;}
  try{
    if(request.method==='status')response.result={state:'ready',vault:{name:state.name,id:path.basename(vault)},sync:{state:'offline',pending:Object.keys(state.notes).length,conflicts:0,last_success:null},identity:null,version:'0.0.0'};
    else if(request.method==='init'){state.name=request.params.name;fs.writeFileSync(store,JSON.stringify(state),{mode:0o600});response.result={initialized:true};}
    else if(request.method==='web'){
      const body=request.params.path==='/'?'<html><head><base href="/"></head><body><div class="nd-bar"></div><h1>Fixture knowledge reader</h1><script src="assets/fixture.js" defer></script></body></html>':'window.Mesh={openNote: function(id){return id;}};';
      response.result={status:200,headers:{'Content-Type':request.params.path==='/'?'text/html; charset=utf-8':'text/javascript; charset=utf-8'},body_base64:Buffer.from(body).toString('base64')};
    }else if(request.method==='tool')response.result=tool(request.params.name,request.params.arguments);
    else response.result={accepted:true};
  }catch(error){response.error={code:error.message,message:'Fixture refusal'};}
  if(request.method==='tool'&&request.params.name==='mesh_search'){process.stdout.write(JSON.stringify(response).slice(0,8));setTimeout(()=>process.stdout.write(JSON.stringify(response).slice(8)+'\n'),5);return;}
  process.stdout.write(JSON.stringify(response)+'\n');
});
