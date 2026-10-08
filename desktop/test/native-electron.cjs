'use strict';
// Run only inside the exact pinned Electron runtime with an ephemeral review profile.
// This executes actual main/preload/page/native Chromium; engine remains a disposable fixture.
const {app,BrowserWindow,dialog}=require('electron');
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const temp=process.env.MESH_RENDERED_FIXTURE_ROOT;
if(!temp||!path.isAbsolute(temp)||!temp.startsWith('/private/tmp/mesh-desktop-rendered-fixture-'))throw new Error('Only a dedicated temporary fixture profile is permitted.');
app.disableHardwareAcceleration();app.setPath('userData',path.join(temp,'profile'));app.setPath('sessionData',path.join(temp,'session'));app.setAppLogsPath(path.join(temp,'logs'));app.setName('Mesh isolated rendering fixture');
const assertions=[];
let discard=0;
// Human dialogs are fixture choices only. No picker/profile/invitation is used.
dialog.showMessageBoxSync=()=>discard;
require('../src/main.cjs');
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
async function until(check,label){for(let i=0;i<200;i++){if(await check())return;await sleep(25);}throw new Error('Timed out: '+label);}
function passed(label){assertions.push(label);}
app.whenReady().then(async()=>{
  try{
    await until(()=>BrowserWindow.getAllWindows().length===1,'window');const win=BrowserWindow.getAllWindows()[0];
    await until(()=>win.webContents.getURL()==='mesh-app://desktop/'&&!win.webContents.isLoading(),'shell');
    const preferences=win.webContents.getLastWebPreferences();assert.equal(preferences.sandbox,true);assert.equal(preferences.contextIsolation,true);assert.equal(preferences.nodeIntegration,false);passed('actual native window sandbox/contextIsolation/noNode');
    assert.equal(await win.webContents.executeJavaScript("typeof require + ':' + typeof process"),'undefined:undefined');passed('actual shell renderer has no Node primitives');
    await win.webContents.executeJavaScript("document.getElementById('vault-name').value='Rendered fixture'; document.getElementById('create-form').requestSubmit();");
    await until(()=>win.webContents.executeJavaScript("!document.getElementById('workspace').hidden"),'vault initialization');
    await until(()=>win.webContents.mainFrame.frames.some(f=>f.url==='mesh-app://viewer/'),'viewer');
    const frame=win.webContents.mainFrame.frames.find(f=>f.url==='mesh-app://viewer/');
    await until(()=>frame.executeJavaScript("typeof window.Mesh?.openNote==='function'"),'viewer scripts');
    assert.equal(await frame.executeJavaScript("typeof window.meshDesktop"),'undefined');
    const denied=await frame.executeJavaScript("(()=>{try{return typeof parent.meshDesktop}catch(e){return e.name}})()");assert.equal(denied,'SecurityError');
    const documentDenied=await frame.executeJavaScript("(()=>{try{return parent.document.URL}catch(e){return e.name}})()");assert.equal(documentDenied,'SecurityError');passed('actual cross-origin viewer cannot read parent bridge/document');
    assert.equal(await frame.executeJavaScript("typeof require + ':' + typeof process"),'undefined:undefined');passed('actual iframe renderer has no Node primitives');
    for(const [template,title] of [['post-mortem','Engineering fixture'],['plan','Marketing fixture'],['procedure','Sales fixture']]){
      await until(()=>win.webContents.executeJavaScript("!document.getElementById('new-note').disabled"),'prior operation drained');
      await win.webContents.executeJavaScript("document.getElementById('new-note').click()");await until(()=>win.webContents.executeJavaScript("!document.getElementById('editor').hidden"),'editor');
      await until(()=>win.webContents.executeJavaScript("!document.getElementById('publish').disabled"),'new editor ready');
      await win.webContents.executeJavaScript(`document.getElementById('template').value=${JSON.stringify(template)};document.getElementById('template').dispatchEvent(new Event('change'));`);
      await until(()=>win.webContents.executeJavaScript(`document.getElementById('sections').querySelector('[data-section="${template==='post-mortem'?'root_cause':template==='plan'?'milestones':'expected_result'}"]')!==null`),'template');
      await until(()=>win.webContents.executeJavaScript("!document.getElementById('publish').disabled"),'template operation ready');
      await win.webContents.executeJavaScript(`document.getElementById('note-title').value=${JSON.stringify(title)};document.getElementById('summary').value='Illustrative rendered fixture; no live facts.';for(const field of document.querySelectorAll('[data-section]'))field.value='Unknown; not verified against a real system.';document.getElementById('tags').value='fixture, example';document.getElementById('publish').click();`);
      await until(()=>win.webContents.executeJavaScript("document.getElementById('message').textContent==='Note published.'"),'confirmed publication');
      passed('actual rendered '+template+' template/shared-tool publication fixture');
    }
    await win.webContents.executeJavaScript("document.getElementById('explore').click()");
    await frame.executeJavaScript("Mesh.openNote('engineering-fixture');document.getElementById('desktop-edit-note').click();");
    await until(()=>win.webContents.executeJavaScript("!document.getElementById('editor').hidden&&document.getElementById('note-title').value==='Engineering fixture'"),'reader edit selection');passed('actual cross-origin reader selection opens native editor');
    await win.webContents.executeJavaScript("document.getElementById('summary').value='Unsaved';document.getElementById('summary').dispatchEvent(new Event('input',{bubbles:true}));");await sleep(25);
    const prior=await win.webContents.executeJavaScript("meshDesktop.perform('overview').then(r=>r.result.selected.id)");
    const canceled=await win.webContents.executeJavaScript("meshDesktop.perform('create',{name:'Should remain unopened'})");assert.equal(canceled.result.selected.id,prior);passed('actual native dirty switch canceled');
    discard=1;
    const switched=await win.webContents.executeJavaScript("meshDesktop.perform('create',{name:'Second fixture'})");assert.notEqual(switched.result.selected.id,prior);
    await until(()=>win.webContents.executeJavaScript("document.getElementById('editor').hidden"),'prior editor reset');passed('actual accepted dirty switch resets prior-vault editor');
    assert.equal(await win.webContents.executeJavaScript("document.getElementById('join').disabled"),false);assert.ok((await win.webContents.executeJavaScript("document.getElementById('update-state').textContent")).includes('not configured'));passed('actual native enrollment is enabled while unconfigured updates remain visibly disabled');
    fs.writeFileSync(path.join(temp,'receipt.json'),JSON.stringify({electron:process.versions.electron,chrome:process.versions.chrome,node:process.versions.node,platform:process.platform,arch:process.arch,assertions,engine:'disposable Node stdio fixture; not Go acceptance',signed:false,installed:false},null,2));
    console.log(JSON.stringify({ok:true,assertions:assertions.length,receipt:path.join(temp,'receipt.json')}));app.quit();
  }catch(error){fs.writeFileSync(path.join(temp,'failure.txt'),error.stack);fs.writeFileSync(path.join(temp,'failure-receipt.json'),JSON.stringify({electron:process.versions.electron,assertions,error:error.message}));
    const win=BrowserWindow.getAllWindows()[0];if(win){try{discard=1;await win.webContents.executeJavaScript("meshDesktop.perform('home')");}catch{}}
    console.error(error.stack);app.exit(1);}
});
