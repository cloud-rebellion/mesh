'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),vm=require('node:vm'),fs=require('node:fs/promises'),os=require('node:os'),path=require('node:path'),{EventEmitter}=require('node:events'),{createRequire}=require('node:module');
test('actual main source uses Electron44 event-details and enforces sandbox, current frame, permission/network and dirty-quit boundaries',async()=>{
  const dir=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-native-main-'));let window,handler,requestGuard,permissionRequest,discardChoice=0,quitCount=0,sandbox=false;const intervals=[];
  const app=new EventEmitter();Object.assign(app,{enableSandbox:()=>sandbox=true,requestSingleInstanceLock:()=>true,whenReady:()=>Promise.resolve(),getPath:()=>dir,getVersion:()=> '0.1.0',isPackaged:false,quit:()=>quitCount++});
  class Window extends EventEmitter{constructor(options){super();this.options=options;this.webContents=new EventEmitter();this.webContents.mainFrame={url:'mesh-app://desktop/',parent:null};this.webContents.setWindowOpenHandler=h=>this.open=h;this.webContents.send=()=>{};window=this;}isDestroyed(){return false;}async loadURL(url){assert.equal(url,'mesh-app://desktop/');}show(){} }
  const privateSession=new EventEmitter();Object.assign(privateSession,{setPermissionRequestHandler:h=>permissionRequest=h,setPermissionCheckHandler:h=>assert.equal(h(),false),setDevicePermissionHandler:h=>assert.equal(h(),false),webRequest:{onBeforeRequest:h=>requestGuard=h},protocol:{handle:async(scheme,h)=>assert.equal(scheme,'mesh-app')}});
  const electron={app,BrowserWindow:Window,session:{fromPartition:p=>{assert.equal(p,'mesh-desktop');return privateSession;}},protocol:{registerSchemesAsPrivileged:schemes=>assert.equal(schemes[0].privileges.secure,true)},ipcMain:{handle:(_name,h)=>handler=h},dialog:{showOpenDialog:async()=>({canceled:true}),showMessageBoxSync:()=>discardChoice,showErrorBox:()=>assert.fail('Unexpected startup failure')},Menu:{buildFromTemplate:t=>t,setApplicationMenu:()=>{}},powerMonitor:new EventEmitter()};
  const main=path.join(__dirname,'../src/main.cjs'),localRequire=createRequire(main);
  try{
    vm.runInNewContext(await fs.readFile(main,'utf8'),{require:name=>name==='electron'?electron:localRequire(name),__dirname:path.dirname(main),process,
      setInterval:fn=>{const timer={unref(){},fn};intervals.push(timer);return timer;},clearInterval:()=>{},console});
    for(let i=0;i<10&&!handler;i++)await new Promise(r=>setTimeout(r,5));assert.ok(handler);assert.equal(sandbox,true);
    assert.equal(window.options.webPreferences.sandbox,true);assert.equal(window.options.webPreferences.contextIsolation,true);assert.equal(window.options.webPreferences.nodeIntegration,false);
    assert.equal(window.open({url:'https://evil.invalid'}).action,'deny');
    let allowed;permissionRequest(null,'filesystem',v=>allowed=v);assert.equal(allowed,false);
    requestGuard({url:'https://evil.invalid'},v=>assert.equal(v.cancel,true));requestGuard({url:'mesh-app://desktop/'},v=>assert.equal(v.cancel,false));
    let prevented=false;window.webContents.emit('will-frame-navigate',{url:'https://evil.invalid',isMainFrame:false,preventDefault:()=>prevented=true});assert.equal(prevented,true);
    prevented=false;window.webContents.emit('will-frame-navigate',{url:'mesh-app://desktop/',isMainFrame:false,preventDefault:()=>prevented=true});assert.equal(prevented,true);
    prevented=false;window.webContents.emit('will-frame-navigate',{url:'mesh-app://desktop/?fixture=1',isMainFrame:true,preventDefault:()=>prevented=true});assert.equal(prevented,true);
    const sender={sender:window.webContents,senderFrame:window.webContents.mainFrame};
    const overview=await handler(sender,{action:'overview',params:{}});assert.equal(overview.ok,true);assert.equal(overview.result.enrollment.enabled,true);
    assert.equal((await handler({...sender,senderFrame:{url:'mesh-app://desktop/',parent:sender.senderFrame}},{action:'overview',params:{}})).ok,false);
    assert.equal((await handler(sender,{action:'editor-state',params:{dirty:true}})).ok,true);
    let stopped=false;app.emit('before-quit',{preventDefault:()=>stopped=true});assert.equal(stopped,true);assert.equal(quitCount,0);
    discardChoice=1;app.emit('before-quit',{preventDefault:()=>{}});await new Promise(r=>setTimeout(r,1));assert.equal(quitCount,1);
  }finally{await fs.rm(dir,{recursive:true,force:true});}
});
