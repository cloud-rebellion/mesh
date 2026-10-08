// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const { contextBridge,ipcRenderer }=require('electron');
// One named API; never expose ipcRenderer, invoke, paths, process, shell or credential handles.
if (process.isMainFrame) contextBridge.exposeInMainWorld('meshDesktop',Object.freeze({
  perform:(action,params={})=>ipcRenderer.invoke('mesh-desktop:action',{action,params}),
  onChange:callback=>{ if (typeof callback !== 'function') throw new TypeError('Callback required');
    const handler=(_event,value)=>callback(value);ipcRenderer.on('mesh-desktop:state',handler);
    return ()=>ipcRenderer.removeListener('mesh-desktop:state',handler);
  }
}));
