// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
'use strict';
// Developer-only source asset generation: pinned Electron renders the existing
// vector mark, then Apple's fixed-path image tools build the ICNS container.
const {app,BrowserWindow,session}=require('electron');
const fsSync=require('node:fs');
const fs=require('node:fs/promises'),path=require('node:path'),{execFile}=require('node:child_process'),{promisify}=require('node:util'),{createHash}=require('node:crypto');
const run=promisify(execFile),output=process.argv[2];
if(process.versions.electron!=='44.7.0'||process.platform!=='darwin'||process.argv.length!==3||!output||!path.isAbsolute(output))throw Error('Use pinned Electron44.7.0 on macOS with a new absolute output directory.');
fsSync.mkdirSync(output,{recursive:false});
app.disableHardwareAcceleration();app.enableSandbox();app.setName('Mesh icon conversion');
app.setPath('userData',path.join(output,'render-profile'));app.setPath('sessionData',path.join(output,'render-session'));app.setAppLogsPath(path.join(output,'render-logs'));
const sizes=[['icon_16x16.png',16],['icon_16x16@2x.png',32],['icon_32x32.png',32],['icon_32x32@2x.png',64],['icon_128x128.png',128],['icon_128x128@2x.png',256],['icon_256x256.png',256],['icon_256x256@2x.png',512],['icon_512x512.png',512],['icon_512x512@2x.png',1024]];
const timer=setTimeout(()=>{process.exitCode=1;app.exit(1);},30000);timer.unref();
app.whenReady().then(async()=>{
 let window,exitCode=0;
 try{
  const svg=await fs.readFile(path.join(__dirname,'../assets/mesh-icon.svg'));if(svg.length>16384)throw Error('Icon vector is too large.');
  const privateSession=session.fromPartition('mesh-icon-conversion');privateSession.webRequest.onBeforeRequest((details,callback)=>callback({cancel:!details.url.startsWith('data:')}));privateSession.setPermissionRequestHandler((_w,_p,callback)=>callback(false));
  window=new BrowserWindow({width:1024,height:1024,show:false,webPreferences:{session:privateSession,sandbox:true,contextIsolation:true,nodeIntegration:false,webSecurity:true,offscreen:true,backgroundThrottling:false}});
  window.webContents.setWindowOpenHandler(()=>({action:'deny'}));
  await window.loadURL('data:text/html;charset=utf-8,'+encodeURIComponent('<!doctype html><meta http-equiv="Content-Security-Policy" content="default-src \'none\'; img-src data:; style-src \'none\'"><title>Mesh icon conversion</title>'));
  const png=await window.webContents.executeJavaScript(`new Promise((resolve,reject)=>{const image=new Image();image.onload=()=>{const canvas=document.createElement('canvas');canvas.width=1024;canvas.height=1024;canvas.getContext('2d').drawImage(image,0,0,1024,1024);resolve(canvas.toDataURL('image/png').split(',')[1])};image.onerror=()=>reject(Error('Vector did not render'));image.src=${JSON.stringify('data:image/svg+xml;base64,'+svg.toString('base64'))};})`);
  const master=path.join(output,'mesh-icon-1024.png');await fs.writeFile(master,Buffer.from(png,'base64'));
  const iconset=path.join(output,'Mesh.iconset');await fs.mkdir(iconset);
  for(const [name,size]of sizes)await run('/usr/bin/sips',['-z',String(size),String(size),master,'--out',path.join(iconset,name)],{env:{PATH:'/usr/bin:/bin'},timeout:5000,maxBuffer:16384});
  const icns=path.join(output,'mesh.icns');await run('/usr/bin/iconutil',['--convert','icns','--output',icns,iconset],{env:{PATH:'/usr/bin:/bin'},timeout:5000,maxBuffer:16384});
  const bytes=await fs.readFile(icns);if(bytes.subarray(0,4).toString('ascii')!=='icns'||bytes.readUInt32BE(4)!==bytes.length)throw Error('Generated ICNS is malformed.');
  await fs.writeFile(path.join(output,'conversion.json'),JSON.stringify({svg_sha256:createHash('sha256').update(svg).digest('hex'),master_png_sha256:createHash('sha256').update(await fs.readFile(master)).digest('hex'),icns_sha256:createHash('sha256').update(bytes).digest('hex'),electron:process.versions.electron,chrome:process.versions.chrome,node:process.versions.node,platform:process.platform,arch:process.arch,sizes,tools:['/usr/bin/sips','/usr/bin/iconutil'],network:false,new_logo:false,source:'Existing Mesh dot and #06050a/#f5f2ec shipped web palette'},null,2)+'\n');
  console.log(JSON.stringify({output,bytes:bytes.length,sha256:createHash('sha256').update(bytes).digest('hex')}));
 }catch(error){console.error(error.stack);exitCode=1;}
 finally{clearTimeout(timer);if(window&&!window.isDestroyed())window.destroy();app.exit(exitCode);}
});
