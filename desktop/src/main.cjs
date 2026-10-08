// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const path=require('node:path');
const { app,BrowserWindow,session,protocol,ipcMain,dialog,Menu,powerMonitor }=require('electron');
const { ORIGIN,viewerURL,privateURL,assertSender,exact,safeError }=require('./boundaries.cjs');
const { Storage }=require('./storage.cjs');
const { Controller }=require('./controller.cjs');
const { verifyBundle }=require('./identity.cjs');
const { configureUpdater }=require('./updater.cjs');
const { makeProtocol }=require('./protocol.cjs');
app.enableSandbox();
protocol.registerSchemesAsPrivileged([{scheme:'mesh-app',privileges:{standard:true,secure:true,supportFetchAPI:true,corsEnabled:true,stream:true}}]);
let window=null,controller=null,quitting=false,refreshTimer=null,editorDirty=false;
function confirmDiscard(){
  if(!editorDirty)return true;
  const choice=dialog.showMessageBoxSync(window,{type:'question',buttons:['Keep editing','Discard changes'],defaultId:0,cancelId:0,
    message:'Discard unsaved changes?',detail:'Save a draft or publish the note to preserve your work.'});
  if(choice===0)return false;editorDirty=false;return true;
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
    window.webContents.on('render-process-gone',()=>{ if (controller) controller.close().catch(()=>{}); });
    controller.on('change',value=>{if (window&&!window.isDestroyed()) window.webContents.send('mesh-desktop:state',value);});
    ipcMain.handle('mesh-desktop:action',async(event,input)=>{
      try { assertSender(event,window);exact(input,['action','params']);if(typeof input.action!=='string')throw new Error('INVALID_REQUEST');
        if(input.action==='editor-state'){exact(input.params,['dirty']);if(typeof input.params.dirty!=='boolean')throw new Error('INVALID_REQUEST');editorDirty=input.params.dirty;return{ok:true,result:{recorded:true}};}
        if(['create','open','pick','home','join'].includes(input.action)&&!confirmDiscard())return{ok:true,result:controller.overview()};
        return {ok:true,result:await controller.perform(input.action,input.params)};
      } catch(error) {return {ok:false,error:safeError(error)};}
    });
    Menu.setApplicationMenu(Menu.buildFromTemplate([{label:'Mesh',submenu:[{role:'about'},{type:'separator'},
      {label:'Vaults',click:()=>{if(confirmDiscard())controller.perform('home',{}).catch(()=>{});}},{label:'Check for updates',click:()=>controller.perform('updates',{}).catch(()=>{})},
      {type:'separator'},{role:'quit'}]},{label:'Edit',submenu:[{role:'undo'},{role:'redo'},{type:'separator'},{role:'cut'},{role:'copy'},{role:'paste'},{role:'selectAll'}]},
      {label:'View',submenu:[{role:'togglefullscreen'}]}]));
    window.once('ready-to-show',()=>window.show());await window.loadURL(ORIGIN+'/');
    window.on('close',event=>{if(!quitting&&!confirmDiscard())event.preventDefault();});
    refreshTimer=setInterval(()=>controller.refresh().catch(()=>{}),15000);refreshTimer.unref();
    powerMonitor.on('resume',()=>controller.refresh().catch(()=>{}));
    app.on('activate',()=>{if(window&&!window.isDestroyed())window.show();});
  }).catch(()=>{dialog.showErrorBox('Mesh could not start','Mesh could not open its private application storage. No vault was changed.');app.quit();});
  app.on('before-quit',event=>{
    if(quitting||!controller)return;event.preventDefault();if(!confirmDiscard())return;quitting=true;clearInterval(refreshTimer);
    controller.close().then(()=>app.quit()).catch(()=>{quitting=false;dialog.showErrorBox('Mesh engine has not stopped','Mesh could not confirm engine termination. This vault owner is retained; inspect the local engine before closing or opening another vault.');});
  });
  app.on('window-all-closed',()=>app.quit());
}
