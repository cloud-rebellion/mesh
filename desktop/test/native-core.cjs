'use strict';
// Actual native shell + real Go proposal. Dedicated temporary app-data only.
const {app,BrowserWindow,dialog}=require('electron');
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const temp=process.env.MESH_RENDERED_FIXTURE_ROOT;
if(!temp||!temp.startsWith('/private/tmp/mesh-desktop-rendered-fixture-go-')||!path.isAbsolute(temp))throw new Error('Dedicated Go fixture directory required');
app.disableHardwareAcceleration();app.setPath('userData',path.join(temp,'profile'));app.setPath('sessionData',path.join(temp,'session'));app.setAppLogsPath(path.join(temp,'logs'));app.setName('Mesh real-core fixture');
dialog.showMessageBoxSync=()=>1;
require('../src/main.cjs');
const assertions=[],sleep=ms=>new Promise(r=>setTimeout(r,ms));let win,collectionID;
function bounded(promise,label){let timer;return Promise.race([promise,new Promise((_,reject)=>timer=setTimeout(()=>reject(new Error('Native evaluation timed out: '+label)),10000))]).finally(()=>clearTimeout(timer));}
async function until(check,label){fs.writeFileSync(path.join(temp,'current-step.json'),JSON.stringify({label}));const deadline=Date.now()+12000;while(Date.now()<deadline){if(await check())return;await sleep(25);}throw new Error('Timed out: '+label);}
function passed(label){assertions.push(label);fs.writeFileSync(path.join(temp,'progress.json'),JSON.stringify({assertions}));}
const evalUI=code=>bounded(win.webContents.executeJavaScript(code),'shell');
setTimeout(()=>{fs.writeFileSync(path.join(temp,'watchdog-failure.json'),JSON.stringify({assertions,error:'Native fixture exceeded its90second boundary'}));app.quit();setTimeout(()=>app.exit(1),6000).unref();},90000).unref();
async function tool(name,args={}){
  const response=await evalUI(`meshDesktop.perform('tool',${JSON.stringify({name,arguments:args})})`);
  assert.equal(response.ok,true,JSON.stringify(response.error));const content=response.result.content;assert.equal(content.length,1);return JSON.parse(content[0].text);
}
async function ready(){await until(()=>evalUI("!document.getElementById('new-note').disabled"),'operation ready');}
async function fill(template,title,{complete=true}={}){
  await ready();await evalUI("document.getElementById('new-note').click()");await ready();
  assert.equal(await evalUI("document.getElementById('template').disabled"),false,'Native new-note template selector is enabled');
  await evalUI(`document.getElementById('template').value=${JSON.stringify(template)};document.getElementById('template').dispatchEvent(new Event('change'));`);await ready();
  await evalUI(`document.getElementById('note-title').value=${JSON.stringify(title)};document.getElementById('summary').value='Illustrative native integration fixture in a disposable vault. No production facts or verification are asserted.';for(const field of document.querySelectorAll('[data-section]'))field.value=${JSON.stringify(complete?'Illustrative fixture context. Actual system cause, owner and production evidence are unknown; this passage tests the shared authoring flow.':'')};document.getElementById('tags').value='fixture, desktop';document.getElementById('collections').value=${JSON.stringify(collectionID)};`);
}
app.whenReady().then(async()=>{
  try{
    await until(()=>BrowserWindow.getAllWindows().length===1,'window');win=BrowserWindow.getAllWindows()[0];await until(()=>win.webContents.getURL()==='mesh-app://desktop/'&&!win.webContents.isLoading(),'shell');
    fs.writeFileSync(path.join(temp,'start-screen.png'),(await win.webContents.capturePage()).toPNG());
    await evalUI("document.getElementById('vault-name').value='Real core disposable fixture';document.getElementById('create-form').requestSubmit();");await until(()=>evalUI("!document.getElementById('workspace').hidden"),'personal vault');await ready();
    const overview=(await evalUI("meshDesktop.perform('overview')")).result;assert.equal(overview.phase,'vault');assert.equal(overview.status.identity,null);assert.equal(overview.enrollment.enabled,true);passed('real core personal vault initialized with conservative local identity');
    await until(()=>win.webContents.mainFrame.frames.some(f=>f.url==='mesh-app://viewer/'),'real viewer');const frame=win.webContents.mainFrame.frames.find(f=>f.url==='mesh-app://viewer/');
    assert.equal(await frame.executeJavaScript("(()=>{try{return typeof parent.meshDesktop}catch(e){return e.name}})()"),'SecurityError');passed('real core viewer cross-origin native bridge denied');
    const catalog=await tool('mesh_templates');assert.ok(catalog.templates.some(t=>t.id==='post-mortem'));assert.ok(catalog.templates.some(t=>t.id==='plan'));assert.ok(catalog.templates.some(t=>t.id==='procedure'));passed('real approved catalog fetched');
    const index=(await tool('mesh_search',{query:'Real core disposable fixture index',limit:5,budget:2000})).cards.find(c=>(c.Title||c.title||'').startsWith('Real core disposable fixture index '));
    assert.ok(index);collectionID=index.NoteID||index.note_id||index.id;assert.ok(collectionID);passed('real personal collection starter retrieved with local search');
    const ids=[];
    for(const [template,title] of [['post-mortem','Desktop fixture engineering incident'],['plan','Desktop fixture marketing strategy'],['procedure','Desktop fixture sales procedure']]){
      await fill(template,title);await evalUI("document.getElementById('publish').click()");await until(()=>evalUI("document.getElementById('message').textContent==='Note published.'"),'real '+template+' publication');await ready();
      const found=await tool('mesh_search',{query:title,limit:5,budget:2000});const card=found.cards.find(c=>(c.Title||c.title)===title);assert.ok(card,'Published note searchable');const id=card.NoteID||card.note_id||card.id;assert.ok(id);ids.push(id);
      const prepared=await tool('mesh_prepare_update',{id});assert.equal(prepared.note.title,title);assert.equal(prepared.note.template,template);assert.ok(prepared.created);assert.ok(prepared.note.update_revision);assert.deepEqual(prepared.note.collections,[collectionID]);assert.equal(prepared.note.verified_at,undefined);passed('real rendered '+template+' publication/search/automatic created/revision/valid collection link');
    }
    await fill('finding','Desktop fixture incomplete draft',{complete:false});await evalUI("document.getElementById('save-draft').click()");await until(()=>evalUI("document.getElementById('message').textContent==='Draft saved.'"),'incomplete draft');await ready();
    const drafts=await tool('mesh_drafts',{limit:20}),draft=drafts.drafts.find(d=>d.title==='Desktop fixture incomplete draft');assert.ok(draft);passed('real incomplete content saved only in draft inbox');
    const selected=overview.selected.id;await evalUI("meshDesktop.perform('home')");await evalUI(`meshDesktop.perform('open',{id:${JSON.stringify(selected)}})`);await ready();passed('real engine drained and same vault reopened without credentials/network');
    const recovered=await tool('mesh_prepare_update',{id:draft.id,draft:true});assert.equal(recovered.note.draft_id,draft.id);assert.equal(recovered.note.draft_revision,draft.revision);assert.equal(recovered.note.verified_at,undefined);
    await evalUI("document.getElementById('drafts').click()");await ready();await evalUI("Array.from(document.getElementById('draft-results').children).find(b=>b.textContent.includes('Desktop fixture incomplete draft')).click()");await ready();
    assert.equal(await evalUI("document.getElementById('template').disabled"),true,'Native resumed draft fixes its template/version');
    await evalUI("for(const field of document.querySelectorAll('[data-section]'))field.value='Illustrative recovered fixture. Relevant findings, sources and production outcomes remain unknown.';document.getElementById('publish').click()");await until(()=>evalUI("document.getElementById('message').textContent==='Note published.'"),'draft completion');await ready();
    assert.ok(!(await tool('mesh_drafts',{limit:20})).drafts.some(d=>d.id===draft.id));passed('real reopened lossless draft completed through shared CAS authoring');
    await evalUI(`document.getElementById('edit-id').value=${JSON.stringify(ids[0])};document.getElementById('edit-dialog').returnValue='open';document.getElementById('edit-dialog').dispatchEvent(new Event('close'));`);await ready();
    assert.equal(await evalUI("document.getElementById('template').disabled"),true,'Native published edit fixes its template/version');
    await evalUI("document.getElementById('summary').value+=' Clarified after local reopening; historical uncertainty is preserved.';document.getElementById('summary').dispatchEvent(new Event('input',{bubbles:true}));document.getElementById('publish').click()");await until(()=>evalUI("document.getElementById('message').textContent==='Note published.'"),'real edit');await ready();
    const edited=await tool('mesh_prepare_update',{id:ids[0]});assert.ok(edited.note.summary.includes('Clarified after'));assert.equal(edited.note.verified_at,undefined);passed('real note revised in application after reopen with no fabricated verification');
    await evalUI("document.getElementById('explore').click()");await sleep(300);
    await until(async()=>{const current=win.webContents.mainFrame.frames.find(f=>f.url==='mesh-app://viewer/');if(!current)return false;try{const diagnostic=await bounded(current.executeJavaScript("({ready:document.readyState,open:typeof window.Mesh?.openNote,stats:document.getElementById('stats')?.textContent,overlay:document.getElementById('overlay-msg')?.textContent})"),'graph readiness');fs.writeFileSync(path.join(temp,'graph-diagnostic.json'),JSON.stringify(diagnostic));return diagnostic.ready==='complete'&&diagnostic.open==='function'&&diagnostic.stats?.includes('5 notes');}catch(error){fs.writeFileSync(path.join(temp,'graph-diagnostic.json'),JSON.stringify({error:error.message}));return false;}},'complete real graph with newly published notes');
    const reader=win.webContents.mainFrame.frames.find(f=>f.url==='mesh-app://viewer/');await bounded(reader.executeJavaScript(`Mesh.openNote(${JSON.stringify(ids[0])})`),'note reader');
    assert.equal(await reader.executeJavaScript("document.getElementById('nd-body').textContent.includes('Clarified after')"),true);passed('real graph refresh and rendered reader show the current published revision');
    await reader.executeJavaScript("document.getElementById('desktop-edit-note').click()");await ready();await until(()=>evalUI("!document.getElementById('editor').hidden&&document.getElementById('note-title').value==='Desktop fixture engineering incident'"),'real viewer selection editor');passed('real reader selection opens revision-fenced native editor without publishing');
    const final=(await evalUI("meshDesktop.perform('overview')")).result;assert.equal(final.enrollment.enabled,true);assert.equal(final.updates.state,'disabled');
    // A background/occluded Chromium window may suspend requestAnimationFrame.
    // Never let screenshot timing turn the bounded fixture into an idle owner.
    await sleep(200);
    fs.writeFileSync(path.join(temp,'shared-editor.png'),(await win.webContents.capturePage()).toPNG());
    assert.equal(final.status.version,'0.0.0-fixture');
    fs.writeFileSync(path.join(temp,'receipt.json'),JSON.stringify({electron:process.versions.electron,chrome:process.versions.chrome,assertions,ids,draft_id:draft.id,version:final.status.version,core:'real uncommitted Go proposal with synthetic version and all-zero source; no accepted release identity',signed:false,installed:false,network:'personal vault unconfigured; no team credential'},null,2));console.log(JSON.stringify({ok:true,assertions:assertions.length,receipt:path.join(temp,'receipt.json')}));app.quit();
  }catch(error){let diagnostics={};if(win){try{diagnostics=await evalUI("({message:document.getElementById('message').textContent,status:document.getElementById('status').textContent,editor_hidden:document.getElementById('editor').hidden})");await evalUI("meshDesktop.perform('home')");}catch{}}
    fs.writeFileSync(path.join(temp,'failure-receipt.json'),JSON.stringify({electron:process.versions.electron,assertions,diagnostics,error:error.stack},null,2));console.error(error.stack);app.exit(1);}
});
