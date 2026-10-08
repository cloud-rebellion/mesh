// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
(() => {
  const $=id=>document.getElementById(id), api=window.meshDesktop;
  let overview=null,catalog=null,template=null,original={},blocks=[],confirmedPublication=null,dirty=false,operation=false,viewerVault=null,viewerSync=null,viewerNeedsRefresh=false,generation=0,sessionKey=null;
  function setDirty(value){dirty=value;api.perform('editor-state',{dirty:value}).catch(()=>{});}
  function message(value) { $('message').textContent=value||'';$('message').hidden=!value; }
  async function perform(action,params={}) {
    const response=await api.perform(action,params);
    if (!response || !response.ok){const error=new Error(response?.error?.message||'Mesh could not complete this operation.');error.code=response?.error?.code;throw error;}
    return response.result;
  }
  function unwrap(value) {
    if (value && Array.isArray(value.content)) {
      const texts=value.content.filter(c=>c.type==='text'&&typeof c.text==='string');
      if(value.isError || texts.length!==1)throw new Error('Mesh could not complete the requested knowledge operation.');
      try{return JSON.parse(texts[0].text);}catch{throw new Error('The knowledge response was not structured.');}
    }
    return value;
  }
  async function tool(name,args={}) {const before=generation,value=await perform('tool',{name,arguments:args});
    if(before!==generation){const error=new Error('The vault changed during this operation. Its result was not applied here; inspect the prior vault before another attempt.');error.code='VAULT_CHANGED';throw error;}return unwrap(value);}
  function busy(value){for(const id of ['new-note','edit-note','edit-search-button','publish','save-draft','preview','add-block','drafts','sync','discard'])$(id).disabled=value;
    for(const field of $('note-form').querySelectorAll('input,textarea,select'))field.disabled=value;
    $('template').disabled=value||!!(original.update_id||original.draft_id);
    $('sync').disabled=value||['unjoined','metadata-pending','join-uncertain'].includes(overview?.status?.sync.state)||overview?.status?.state==='join-uncertain';
  }
  async function run(operationFn) {
    if(operation)return;operation=true;busy(true);message('');
    try{await operationFn();}catch(error){message(error.message);}finally{operation=false;busy(false);}
  }
  function list(value) {return value.split(',').map(v=>v.trim()).filter(Boolean);}
  function render(state) {
    overview=state;
    const next=state.phase==='vault'?state.selected?.id:null;
    if(next!==sessionKey){sessionKey=next;generation++;catalog=null;template=null;original={};blocks=[];confirmedPublication=null;viewerNeedsRefresh=false;$('edit-results').replaceChildren();$('edit-dialog').returnValue='';if($('edit-dialog').open)$('edit-dialog').close();$('editor').hidden=true;$('draft-list').hidden=true;setDirty(false);}
    const local=state.phase==='vault';$('start').hidden=local;$('workspace').hidden=!local;$('home').hidden=!local;
    $('vault-label').textContent=state.selected?.name||'Your connected knowledge';
    $('status').textContent=state.status ? `${state.status.state} · ${state.status.sync.state}` : state.phase;
    $('app-version').textContent=`Mesh ${state.app_version}`;
    $('update-state').textContent=state.updates.reason;
    $('credential-state').textContent=state.credentials.osProtected?'OS-protected credential provider':'Private local engine credentials';
    $('join').disabled=!state.enrollment.enabled;$('join-reason').textContent=state.enrollment.reason;
    $('vault-list').replaceChildren();
    for(const vault of state.vaults){const b=document.createElement('button');b.textContent=vault.name;b.addEventListener('click',()=>run(async()=>render(await perform('open',{id:vault.id}))));$('vault-list').append(b);}
    if(!state.vaults.length)$('vault-list').textContent='No vaults opened yet.';
    if(state.error)message(state.error.message);
    else if(state.status?.state==='join-uncertain')message('The invitation outcome is uncertain. Do not redeem it again; recover the existing enrollment with your administrator.');
    else if(state.status?.identity&&$('message').textContent==='Joining the team. The invitation is sent once.')message('Enrollment is saved. Access is not verified; see the displayed sync state.');
    else if(state.status?.state==='joined-preparing'&&state.status.sync.state==='metadata-pending')message('Enrollment is saved; initial sync metadata needs recovery. Do not redeem the invitation again.');
    if(local){
      const identity=state.status.identity,sync=state.status.sync;
      $('identity').textContent=identity?`${identity.user} · ${identity.verified?identity.role:'Access not verified'}`:(sync.state==='unjoined'?'Local vault · saved on this device':'Local vault · identity not established');
      $('sync').disabled=operation||['unjoined','metadata-pending'].includes(sync.state)||state.status.state==='join-uncertain';
      $('sync-detail').textContent=sync.state==='unjoined'?'Available offline on this device. No remote connection is configured.':`${sync.pending} pending changes · ${sync.conflicts} conflicts · Last successful sync: ${sync.last_success||'not recorded'}`;
      if(viewerVault!==state.selected.id){viewerVault=state.selected.id;viewerSync=sync.last_success;$('viewer').src='mesh-app://viewer/';$('editor').hidden=true;$('draft-list').hidden=true;original={};blocks=[];setDirty(false);}
      if(sync.last_success&&sync.last_success!==viewerSync){viewerSync=sync.last_success;viewerNeedsRefresh=true;}
      if($('editor').hidden&&$('draft-list').hidden)showViewer();
    }else{viewerVault=null;viewerSync=null;$('viewer').removeAttribute('src');$('viewer').hidden=true;if(state.phase==='start')setDirty(false);}
  }
  function inputField(label,description,value='') {
    const el=document.createElement('label');el.append(document.createTextNode(label));
    if(description){const p=document.createElement('span');p.className='detail';p.textContent=description;el.append(p);}
    const input=document.createElement('textarea');input.rows=4;input.value=value;input.addEventListener('input',()=>setDirty(true));el.append(input);return{el,input};
  }
  async function loadCatalog(){if(!catalog)catalog=await tool('mesh_templates');}
  async function renderTemplate(id,version,content={}) {
    template=(await tool('mesh_note_template',{template:id,version})).template;
    $('sections').replaceChildren();
    for(const section of template.sections){const field=inputField(section.heading,section.guidance,content.sections?.[section.key]||'');field.input.dataset.section=section.key;$('sections').append(field.el);}
  }
  function renderBlocks(){
    $('blocks').replaceChildren();
    blocks.forEach((block,index)=>{
      const card=document.createElement('section');card.className='block';const h=document.createElement('h2');h.textContent=block.template;card.append(h);
      for(const key of Object.keys(block.fields)){const field=inputField(key,'',block.fields[key]);field.input.addEventListener('input',()=>block.fields[key]=field.input.value);card.append(field.el);}
      const remove=document.createElement('button');remove.type='button';remove.textContent='Remove block';remove.addEventListener('click',()=>{blocks.splice(index,1);setDirty(true);renderBlocks();});card.append(remove);$('blocks').append(card);
    });
  }
  async function openEditor(note={}) {
    if(dirty&&!confirm('Discard unsaved changes?'))return false;
    await loadCatalog();original=JSON.parse(JSON.stringify(note));blocks=JSON.parse(JSON.stringify(note.blocks||[]));
    $('template').replaceChildren();for(const item of catalog.templates){const option=document.createElement('option');option.value=item.id;option.textContent=item.id;option.dataset.version=item.version;$('template').append(option);}
    const selected=note.template||catalog.templates[0].id;$('template').value=selected;$('template').disabled=true;
    await renderTemplate(selected,note.template_version||Number($('template').selectedOptions[0].dataset.version),note);
    $('note-title').value=note.title||'';$('summary').value=note.summary||'';
    for(const key of ['tags','collections','related'])$(key).value=(note[key]||[]).join(', ');
    $('block-template').replaceChildren();for(const item of catalog.blocks){const option=document.createElement('option');option.value=item.id;option.textContent=item.id;option.dataset.version=item.version;$('block-template').append(option);}
    renderBlocks();$('editor-title').textContent=note.update_id?'Edit note':note.draft_id?'Complete draft':'New note';
    $('preview-content').hidden=true;$('viewer').hidden=true;$('draft-list').hidden=true;$('editor').hidden=false;
    $('template').disabled=!!(note.update_id||note.draft_id);confirmedPublication=null;setDirty(false);return true;
  }
  function authored(){
    const note={...original,title:$('note-title').value,summary:$('summary').value,template:template.id,template_version:template.version,type:template.type,sections:{}};
    for(const field of $('sections').querySelectorAll('[data-section]'))note.sections[field.dataset.section]=field.value;
    for(const key of ['tags','collections','related'])note[key]=list($(key).value);
    note.blocks=blocks.filter(b=>Object.values(b.fields).some(v=>v.trim()));return note;
  }
  async function preview(){const result=await tool('mesh_author_note',{action:'prepare',note:authored()});$('preview-content').textContent=result.markdown||JSON.stringify(result,null,2);$('preview-content').hidden=false;return result;}
  function showViewer(){
    if(viewerNeedsRefresh){$('viewer').src='mesh-app://viewer/';viewerNeedsRefresh=false;}
    $('editor').hidden=true;$('draft-list').hidden=true;$('viewer').hidden=false;
  }
  async function save(action){
    if(confirmedPublication){
      const recovered=await tool('mesh_prepare_update',{id:confirmedPublication.id});
      if(await openEditor(recovered.note))message('The confirmed published note has been reloaded. Review it before saving another change.');
      return;
    }
    const note=authored();await preview();
    if(action==='publish'){note.status='active';const validation=await tool('mesh_author_note',{action:'validate',note});if(!validation.valid){message((validation.issues||['The note needs more substance.']).map(v=>typeof v==='string'?v:JSON.stringify(v)).join('\n'));return;}}
    const result=await tool('mesh_author_note',{action,note});
    if(result.id&&result.revision)viewerNeedsRefresh=true;
    // Update only confirmed IDs/revisions; never retry an uncertain write automatically.
    if(action==='draft'&&result.id&&result.revision){original={...note,draft_id:result.id,draft_revision:result.revision};}
    else if(action==='publish'&&result.id){
      const before=generation;confirmedPublication={id:result.id,revision:result.revision};
      try{original=(await tool('mesh_prepare_update',{id:result.id})).note||{};confirmedPublication=null;}
      catch(error){
        if(before!==generation)throw error;
        original={...note,update_id:result.id,update_revision:result.revision};delete original.draft_id;delete original.draft_revision;
        setDirty(false);
        throw new Error('The note was published, but its current revision could not be reloaded. The next save will reload this note for review before another write.');
      }
    }
    setDirty(false);message(action==='draft'?'Draft saved.':'Note published.');await perform('overview').then(render);
  }
  $('create-form').addEventListener('submit',event=>{event.preventDefault();run(async()=>render(await perform('create',{name:$('vault-name').value})));});
  $('pick').addEventListener('click',()=>run(async()=>render(await perform('pick'))));
  $('join').addEventListener('click',()=>{if(overview?.enrollment.enabled)$('join-dialog').showModal();});
  $('join-cancel').addEventListener('click',()=>{$('join-invite').value='';$('join-dialog').close();});
  $('join-form').addEventListener('submit',event=>{event.preventDefault();run(async()=>{
    try{const result=await perform('join',{name:$('join-name').value,invite:$('join-invite').value});render(result);$('join-dialog').close();if(result.phase==='start')message('Join cancelled.');else if(result.status?.state==='joining')message('Joining the team. The invitation is sent once.');else if(result.status?.identity)message('Enrollment is saved. Access is not verified; see the displayed sync state.');}
    finally{$('join-invite').value='';}
  });});
  $('home').addEventListener('click',()=>run(async()=>{if(!dirty||confirm('Discard unsaved changes?')){setDirty(false);catalog=null;render(await perform('home'));}}));
  $('sync').addEventListener('click',()=>run(async()=>render(await perform('sync'))));
  $('explore').addEventListener('click',()=>{if(dirty&&!confirm('Discard unsaved changes?'))return;setDirty(false);showViewer();});
  $('new-note').addEventListener('click',()=>run(()=>openEditor()));
  $('edit-note').addEventListener('click',()=>{$('edit-search').value='';$('edit-results').replaceChildren();$('edit-dialog').returnValue='';$('edit-dialog').showModal();});
  $('edit-search-form').addEventListener('submit',event=>{event.preventDefault();run(async()=>{
    const query=$('edit-search').value.trim();if(query.length<2||query.length>256)throw new Error('Enter a title or subject between 2 and 256 characters.');
    const result=await tool('mesh_search',{query,limit:10,budget:3000});$('edit-results').replaceChildren();
    for(const card of (Array.isArray(result.cards)?result.cards:[]).slice(0,10)){
      const id=card.NoteID||card.note_id||card.id,title=card.Title||card.title;
      if(typeof id!=='string'||!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,255}$/.test(id)||typeof title!=='string'||title.length>512)continue;
      const before=generation,row=document.createElement('button');row.type='button';row.textContent=title;row.addEventListener('click',()=>run(async()=>{if(before!==generation)throw new Error('The vault changed. Search again in the current vault.');const prepared=await tool('mesh_prepare_update',{id});$('edit-dialog').returnValue='';$('edit-dialog').close();await openEditor(prepared.note);}));$('edit-results').append(row);
    }
    if(!$('edit-results').children.length)$('edit-results').textContent='No matching notes. Try another title or subject.';
  });});
  $('edit-dialog').addEventListener('close',()=>{if($('edit-dialog').returnValue==='open')run(async()=>{const result=await tool('mesh_prepare_update',{id:$('edit-id').value.trim()});await openEditor(result.note);});});
  $('template').addEventListener('change',()=>run(async()=>{await renderTemplate($('template').value,Number($('template').selectedOptions[0].dataset.version));setDirty(true);}));
  $('add-block').addEventListener('click',()=>run(async()=>{const selected=$('block-template').selectedOptions[0];const definition=await tool('mesh_block_template',{template:selected.value,version:Number(selected.dataset.version)});
    const fields={};for(const field of definition.fields)fields[field.key]='';blocks.push({template:definition.id,version:definition.version,id:`block-${blocks.length+1}-${Date.now()}`,fields});setDirty(true);renderBlocks();}));
  $('note-form').addEventListener('input',()=>setDirty(true));$('note-form').addEventListener('submit',event=>event.preventDefault());
  $('preview').addEventListener('click',()=>run(preview));$('save-draft').addEventListener('click',()=>run(()=>save('draft')));$('publish').addEventListener('click',()=>run(()=>save('publish')));
  $('discard').addEventListener('click',()=>{if(!dirty||confirm('Discard unsaved changes?')){setDirty(false);showViewer();}});
  async function loadDrafts(offset=0){const result=await tool('mesh_drafts',{limit:20,offset});setDirty(false);$('editor').hidden=true;$('viewer').hidden=true;$('draft-list').hidden=false;$('draft-results').replaceChildren();
    for(const draft of result.drafts){const row=document.createElement('button');row.textContent=`${draft.title} · ${draft.template}`;row.addEventListener('click',()=>run(async()=>{const prepared=await tool('mesh_prepare_update',{id:draft.id,draft:true});await openEditor(prepared.note);}));$('draft-results').append(row);}
    if(offset>0){const previous=document.createElement('button');previous.textContent='Previous drafts';previous.addEventListener('click',()=>run(()=>loadDrafts(Math.max(0,offset-20))));$('draft-results').append(previous);}
    if(result.more&&Number.isSafeInteger(result.next_offset)&&result.next_offset>offset){const next=document.createElement('button');next.textContent='Next drafts';next.addEventListener('click',()=>run(()=>loadDrafts(result.next_offset)));$('draft-results').append(next);}
  }
  $('drafts').addEventListener('click',()=>run(async()=>{if(dirty&&!confirm('Discard unsaved changes?'))return;await loadDrafts();}));
  window.addEventListener('message',event=>{
    const value=event.data;
    if(event.origin!=='mesh-app://viewer'||event.source!==$('viewer').contentWindow||!value||Object.keys(value).length!==2||value.kind!=='mesh-note-selection'||typeof value.id!=='string'||!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,255}$/.test(value.id))return;
    run(async()=>{const result=await tool('mesh_prepare_update',{id:value.id});await openEditor(result.note);});
  });
  api.onChange(render);run(async()=>render(await perform('overview')));
})();
