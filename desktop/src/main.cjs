// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const path=require('node:path');
const { app,BrowserWindow,session,protocol,ipcMain,dialog,Menu,powerMonitor }=require('electron');
const { ORIGIN,viewerURL,privateURL,assertSender,exact,safeError }=require('./boundaries.cjs');
const { Storage }=require('./storage.cjs');
const { Controller }=require('./controller.cjs');
const { verifyBundle }=require('./identity.cjs');
const { configureUpdater }=require('./updater.cjs');
const { UpdateScheduler }=require('./update-scheduler.cjs');
const { makeProtocol }=require('./protocol.cjs');
app.enableSandbox();
protocol.registerSchemesAsPrivileged([{scheme:'mesh-app',privileges:{standard:true,secure:true,supportFetchAPI:true,corsEnabled:true,stream:true}}]);
let window=null,controller=null,updateScheduler=null,quitting=false,refreshTimer=null,editorDirty=false,restartAcknowledgement=null,quitRequest=null;
function confirmDiscard({record=true}={}){
  if(!editorDirty)return true;
  const choice=dialog.showMessageBoxSync(window,{type:'question',buttons:['Keep editing','Discard changes'],defaultId:0,cancelId:0,
    message:'Discard unsaved changes?',detail:'Save a draft or publish the note to preserve your work.'});
  if(choice!==1)return false;if(record)editorDirty=false;return true;
}
function confirmEditorForUpdate(token,{automatic=false}={}){
  return new Promise((resolve,reject)=>{
    const frame=window?.webContents.mainFrame,selected=controller.selected,engine=controller.engine;
    const finish=(error,value)=>{clearTimeout(timer);restartAcknowledgement=null;error?reject(error):resolve(value);};
    const timer=setTimeout(()=>finish(new Error('UPDATE_EDITOR_NOT_READY')),5000);
    restartAcknowledgement={token,frame,selected,engine,finish,automatic};
    // Resend after registering the acknowledgement; an earlier state event may
    // already be queued. One token, current window/frame and vault only.
    controller.changed();
  });
}
async function restartUpdate(){
  try{await controller.restartForUpdate({confirmEditor:confirmEditorForUpdate});}
  catch(error){dialog.showErrorBox('Mesh update deferred',safeError(error).message);}
}
async function requestGracefulQuit(){
  if(quitting||!controller||quitRequest)return;
  // A ready native install has already drained the editor and original owner.
  // Staging/uncertain native states may install on quit and cannot use this path.
  if(controller.updateLock){if(controller.updater.status().state==='ready'&&!controller.engine){updateScheduler?.stop().catch(()=>{});quitting=true;clearInterval(refreshTimer);app.quit();}return;}
  if(!confirmDiscard({record:false}))return;
  const discoveryPaused=updateScheduler?.pause()||Promise.resolve();
  quitRequest=controller.quitGracefully({confirmUnknown:async({write,join})=>{
    const detail=[write?'A note write has an unknown outcome. Mesh will not clear it or repeat it. Inspect the note after reopening.':'',join?'Team enrollment or its recovery is unsettled. Mesh will stop the local worker without redeeming the invitation again.':''].filter(Boolean).join(' ');
    return dialog.showMessageBoxSync(window,{type:'warning',buttons:['Keep Mesh open','Quit Mesh'],defaultId:0,cancelId:0,message:'Quit with an unresolved operation?',detail})===1;
  }});
  try{await quitRequest;await discoveryPaused;await updateScheduler?.stop();quitting=true;clearInterval(refreshTimer);app.quit();}
  catch(error){await discoveryPaused;updateScheduler?.resume();if(error.message!=='QUIT_CANCELLED')dialog.showErrorBox('Mesh could not quit safely',safeError(error).message);}
  finally{quitRequest=null;}
}
if (!app.requestSingleInstanceLock()) app.quit();
else {
  app.on('second-instance',()=>{ if (window) { if (window.isMinimized()) window.restore();window.focus(); } });
  // Incoming deep links are not enrollment. No callback/invite is read from process arguments.
  app.on('open-url',event=>event.preventDefault());
  app.whenReady().then(async()=>{
    const storage=new Storage(path.join(app.getPath('userData'),'knowledge'));await storage.load();
    const resources=app.isPackaged ? path.join(process.resourcesPath,'mesh-core') : path.join(__dirname,'../bundle/mesh-core');
    controller=new Controller({storage,bundle:()=>verifyBundle(resources),appVersion:app.getVersion(),updater:configureUpdater(null),enrollmentEnabled:true,
      confirmJoin:async({name,hub})=>{const result=await dialog.showMessageBox(window,{type:'question',buttons:['Cancel','Join team'],defaultId:0,cancelId:0,
        message:`Join a Mesh team in ${name}?`,detail:`The invitation will be sent once to ${hub}. Your identity and access will be shown only after the server establishes them.`});return result.response===1;},
      picker:async()=>{const result=await dialog.showOpenDialog(window,{title:'Open a Mesh vault',properties:['openDirectory']});return result.canceled ? null : result.filePaths[0];}});
    const privateSession=session.fromPartition('mesh-desktop');
    privateSession.setPermissionRequestHandler((_contents,_permission,callback)=>callback(false));
    privateSession.setPermissionCheckHandler(()=>false);
    privateSession.setDevicePermissionHandler(()=>false);
    privateSession.on('will-download',(event)=>event.preventDefault());
    privateSession.webRequest.onBeforeRequest((details,callback)=>callback({cancel:!privateURL(details.url)}));
    await privateSession.protocol.handle('mesh-app',makeProtocol({renderer:path.join(__dirname,'../renderer'),controller}));
    window=new BrowserWindow({width:1250,height:850,minWidth:800,minHeight:600,title:'Mesh',show:false,
      webPreferences:{session:privateSession,preload:path.join(__dirname,'preload.cjs'),sandbox:true,contextIsolation:true,
        nodeIntegration:false,nodeIntegrationInWorker:false,nodeIntegrationInSubFrames:false,webviewTag:false,webSecurity:true,
        allowRunningInsecureContent:false,experimentalFeatures:false,devTools:!app.isPackaged,spellcheck:false}});
    window.webContents.setWindowOpenHandler(()=>({action:'deny'}));
    window.webContents.on('will-navigate',event=>{if (event.url !== ORIGIN + '/') event.preventDefault();});
    window.webContents.on('will-frame-navigate',event=>{if (event.isMainFrame ? event.url !== ORIGIN + '/' : !viewerURL(event.url)) event.preventDefault();});
    window.webContents.on('will-attach-webview',event=>event.preventDefault());
    window.webContents.on('render-process-gone',()=>{updateScheduler?.pause().catch(()=>{});if (controller) controller.close().catch(()=>{}); });
    controller.on('change',value=>{if (window&&!window.isDestroyed()) window.webContents.send('mesh-desktop:state',value);});
    ipcMain.handle('mesh-desktop:action',async(event,input)=>{
      try { assertSender(event,window);exact(input,['action','params']);if(typeof input.action!=='string')throw new Error('INVALID_REQUEST');
        if(input.action==='editor-state'){
          exact(input.params,['dirty','restart_token','operation'],['dirty']);if(typeof input.params.dirty!=='boolean')throw new Error('INVALID_REQUEST');
          if('restart_token'in input.params){
            const pending=restartAcknowledgement;
            if(!pending||input.params.restart_token!==pending.token||event.senderFrame!==pending.frame||controller.selected!==pending.selected||controller.engine!==pending.engine||typeof input.params.operation!=='boolean')throw new Error('UPDATE_EDITOR_NOT_READY');
            editorDirty=input.params.dirty;
            if(input.params.operation)pending.finish(new Error('UPDATE_OPERATION_ACTIVE'));
            else pending.finish(null,pending.automatic?!input.params.dirty:confirmDiscard());
          }else{if('operation'in input.params)throw new Error('INVALID_REQUEST');editorDirty=input.params.dirty;if(!editorDirty)updateScheduler?.wake();}
          return{ok:true,result:{recorded:true}};
        }
        if(['create','open','pick','home','join'].includes(input.action)&&!confirmDiscard())return{ok:true,result:controller.overview()};
        return {ok:true,result:await controller.perform(input.action,input.params)};
      } catch(error) {return {ok:false,error:safeError(error)};}
    });
    Menu.setApplicationMenu(Menu.buildFromTemplate([{label:'Mesh',submenu:[{role:'about'},{type:'separator'},
      {label:'Vaults',click:()=>{if(confirmDiscard())controller.perform('home',{}).catch(()=>{});}},{label:'Check for updates',click:()=>controller.perform('updates',{}).catch(()=>{})},
      {label:'Restart to update',click:()=>restartUpdate()},
      {type:'separator'},{role:'quit'}]},{label:'Edit',submenu:[{role:'undo'},{role:'redo'},{type:'separator'},{role:'cut'},{role:'copy'},{role:'paste'},{role:'selectAll'}]},
      {label:'View',submenu:[{role:'togglefullscreen'}]}]));
    window.once('ready-to-show',()=>window.show());await window.loadURL(ORIGIN+'/');
    window.on('close',event=>{
      if(quitting)return;
      // Keep the usable window until confirmation and physical engine drain.
      event.preventDefault();requestGracefulQuit().catch(()=>{});
    });
    updateScheduler=new UpdateScheduler({controller,confirmEditor:token=>confirmEditorForUpdate(token,{automatic:true})});updateScheduler.start();
    refreshTimer=setInterval(()=>controller.refresh().catch(()=>{}),15000);refreshTimer.unref();
    powerMonitor.on('resume',()=>{controller.refresh().catch(()=>{});updateScheduler.wake();});
    app.on('activate',()=>{if(window&&!window.isDestroyed())window.show();});
  }).catch(()=>{dialog.showErrorBox('Mesh could not start','Mesh could not open its private application storage. No vault was changed.');app.quit();});
  app.on('before-quit',event=>{
    if(quitting||!controller)return;event.preventDefault();requestGracefulQuit().catch(()=>{});
  });
  app.on('window-all-closed',()=>app.quit());
}
