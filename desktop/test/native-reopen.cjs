'use strict';
// Cold native restart of an already populated disposable Go-engine fixture.
const {app,BrowserWindow,dialog}=require('electron');
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const temp=process.env.MESH_RENDERED_FIXTURE_ROOT;
if(!temp||!temp.startsWith('/private/tmp/mesh-desktop-rendered-fixture-go-')||!path.isAbsolute(temp))throw new Error('Dedicated Go fixture directory required');
const prior=JSON.parse(fs.readFileSync(path.join(temp,'receipt.json'),'utf8'));
assert.equal(prior.version,'0.0.0-fixture');assert.equal(prior.ids.length,3);
const storage=JSON.parse(fs.readFileSync(path.join(temp,'profile/knowledge/vaults.json'),'utf8'));
assert.equal(storage.vaults.length,1);const selected=storage.vaults[0];
assert.ok(selected.path.startsWith(path.join(temp,'profile/knowledge/vaults/')));
app.disableHardwareAcceleration();app.setPath('userData',path.join(temp,'profile'));app.setPath('sessionData',path.join(temp,'session'));app.setAppLogsPath(path.join(temp,'logs'));app.setName('Mesh cold-reopen fixture');
dialog.showMessageBoxSync=()=>1;
dialog.showOpenDialog=async()=>({canceled:false,filePaths:[selected.path]});
require('../src/main.cjs');
const assertions=[],sleep=ms=>new Promise(r=>setTimeout(r,ms));let win;
async function until(check,label){const deadline=Date.now()+12000;while(Date.now()<deadline){if(await check())return;await sleep(25);}throw new Error('Timed out: '+label);}
function bounded(promise){let timer;return Promise.race([promise,new Promise((_,reject)=>timer=setTimeout(()=>reject(new Error('Native evaluation timed out')),10000))]).finally(()=>clearTimeout(timer));}
const ui=code=>bounded(win.webContents.executeJavaScript(code));
setTimeout(()=>{fs.writeFileSync(path.join(temp,'cold-watchdog-failure.json'),JSON.stringify({assertions,error:'Native fixture exceeded its90second boundary'}));app.quit();setTimeout(()=>app.exit(1),6000).unref();},90000).unref();
async function ready(){await until(()=>ui("!document.getElementById('new-note').disabled"),'operation ready');}
async function tool(name,args={}){const response=await ui(`meshDesktop.perform('tool',${JSON.stringify({name,arguments:args})})`);assert.equal(response.ok,true,JSON.stringify(response.error));return JSON.parse(response.result.content[0].text);}
app.whenReady().then(async()=>{
  try{
    await until(()=>BrowserWindow.getAllWindows().length===1,'window');win=BrowserWindow.getAllWindows()[0];
    await until(()=>win.webContents.getURL()==='mesh-app://desktop/'&&!win.webContents.isLoading(),'shell');await ready();
    const start=(await ui("meshDesktop.perform('overview')")).result;
    assert.equal(start.phase,'start');assert.equal(start.vaults.length,1);assert.equal(start.vaults[0].id,selected.id);
    assert.equal(start.vaults[0].path,undefined);assertions.push('cold application restart retains opaque recent vault identity without exposing paths');
    await ui("document.getElementById('vault-list').firstElementChild.click()");await ready();
    await until(()=>ui("!document.getElementById('workspace').hidden"),'recent vault opened');
    const before=await tool('mesh_prepare_update',{id:prior.ids[0]});assert.ok(before.note.summary.includes('Clarified after'));assert.equal(before.note.verified_at,undefined);
    assertions.push('real cold native restart reopens saved revision and historical uncertainty offline');
    await ui(`document.getElementById('edit-id').value=${JSON.stringify(prior.ids[0])};document.getElementById('edit-dialog').returnValue='open';document.getElementById('edit-dialog').dispatchEvent(new Event('close'));`);await ready();
    await ui("document.getElementById('summary').value+=' Cold restart revision remains local.';document.getElementById('summary').dispatchEvent(new Event('input',{bubbles:true}));document.getElementById('publish').click()");
    await until(()=>ui("document.getElementById('message').textContent==='Note published.'"),'cold edit');await ready();
    const revised=await tool('mesh_prepare_update',{id:prior.ids[0]});assert.notEqual(revised.revision,before.revision);assert.ok(revised.note.summary.includes('Cold restart'));assert.equal(revised.note.verified_at,undefined);
    assertions.push('real in-app edit after process restart uses shared revision check and no invented verification');
    await ui("document.getElementById('home').click()");await ready();await ui("document.getElementById('pick').click()");await ready();
    await until(()=>ui("!document.getElementById('workspace').hidden"),'controlled fixture picker');
    const reopened=(await ui("meshDesktop.perform('overview')")).result;assert.equal(reopened.selected.id,selected.id);assert.equal(reopened.vaults.length,1);assert.equal(reopened.status.identity,null);
    assert.equal((await tool('mesh_prepare_update',{id:prior.ids[0]})).revision,revised.revision);
    assertions.push('controlled native picker reopens existing fixture vault with the same identity and content');
    assert.equal(reopened.enrollment.enabled,true);assert.equal(reopened.updates.state,'disabled');
    await until(async()=>{const frame=win.webContents.mainFrame.frames.find(f=>f.url==='mesh-app://viewer/');if(!frame)return false;try{return await bounded(frame.executeJavaScript("document.readyState==='complete'&&document.getElementById('stats')?.textContent.includes('5 notes')"));}catch{return false;}},'complete reopened graph viewer');
    await ui("document.getElementById('edit-note').click();document.getElementById('edit-search').value='Desktop fixture marketing strategy';document.getElementById('edit-search-form').requestSubmit()");await ready();
    await until(()=>ui("Array.from(document.getElementById('edit-results').children).some(b=>b.textContent==='Desktop fixture marketing strategy')"),'human title selection');
    await ui("Array.from(document.getElementById('edit-results').children).find(b=>b.textContent==='Desktop fixture marketing strategy').click()");await ready();
    assert.equal(await ui("document.getElementById('note-title').value"),'Desktop fixture marketing strategy');assert.equal(await ui("document.getElementById('template').disabled"),true);assert.equal(await ui("document.getElementById('edit-dialog').open"),false);
    assertions.push('human title search and card selection open a saved note in the native editor without requiring a raw ID');
    assert.equal(await ui("document.getElementById('identity').textContent"),'Local vault · saved on this device');assert.equal(await ui("document.getElementById('sync').disabled"),true);
    assertions.push('unjoined local saving and offline availability are clear without suggesting failed identity or pending remote sync');
    await sleep(200);
    fs.writeFileSync(path.join(temp,'cold-reopened-vault.png'),(await win.webContents.capturePage()).toPNG());
    fs.writeFileSync(path.join(temp,'cold-reopen-receipt.json'),JSON.stringify({electron:process.versions.electron,assertions,revision:revised.revision,version:reopened.status.version,signed:false,installed:false,actual_human_picker:false,network:'unconfigured personal vault; no team credential'},null,2));
    console.log(JSON.stringify({ok:true,assertions:assertions.length,receipt:path.join(temp,'cold-reopen-receipt.json')}));app.quit();
  }catch(error){if(win){try{await ui("meshDesktop.perform('home')");}catch{}}
    fs.writeFileSync(path.join(temp,'cold-reopen-failure.json'),JSON.stringify({assertions,error:error.stack},null,2));console.error(error.stack);app.exit(1);}
});
