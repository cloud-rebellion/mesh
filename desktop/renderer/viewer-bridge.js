// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
(() => {
  // Reader content remains unprivileged. This message requests selection only.
  // The parent validates origin/source/id and applies normal Mesh access/CAS checks.
  const mesh=window.Mesh,bar=document.querySelector('.nd-bar');
  if(!mesh||typeof mesh.openNote!=='function'||!bar)return;
  let selected=null;const original=mesh.openNote;
  const button=document.createElement('button');button.id='desktop-edit-note';button.textContent='Edit this note';button.type='button';button.hidden=true;bar.append(button);
  mesh.openNote=function(id,...rest){selected=typeof id==='string'&&/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,255}$/.test(id)?id:null;button.hidden=!selected;return original.call(this,id,...rest);};
  button.addEventListener('click',()=>{if(selected)window.parent.postMessage({kind:'mesh-note-selection',id:selected},'mesh-app://desktop');});
})();
