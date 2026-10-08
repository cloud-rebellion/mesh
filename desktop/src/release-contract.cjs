// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const zlib=require('node:zlib');
const {createHash}=require('node:crypto');
const {exact}=require('./boundaries.cjs');
const ORIGIN='https://mesh.brightinteraction.com',BUNDLE_ID='com.brightinteraction.mesh.desktop';
const LIMITS=Object.freeze({metadata:65536,archive:256*1024*1024,expanded:512*1024*1024,entry:256*1024*1024,entries:4096,pathBytes:1024,pathComponents:64,namespaceNodes:16384});
const sha=value=>typeof value==='string'&&/^[a-f0-9]{64}$/.test(value);
const source=value=>typeof value==='string'&&/^[a-f0-9]{40}$/.test(value)&&value!=='0'.repeat(40);
function parsePlist(bytes){
  let text;try{text=new TextDecoder('utf-8',{fatal:true}).decode(bytes);}catch{throw new Error('INVALID_UPDATE_ARCHIVE');}
  if(text.length>65536||/<!ENTITY/.test(text))throw new Error('INVALID_UPDATE_ARCHIVE');
  text=text.trim().replace(/^<\?xml version="1\.0" encoding="UTF-8"\?>\s*/,'').replace(/^<!DOCTYPE plist PUBLIC "-\/\/Apple\/\/DTD PLIST 1\.0\/\/EN" "http:\/\/www\.apple\.com\/DTDs\/PropertyList-1\.0\.dtd">\s*/,'');
  const tokens=text.match(/<[^>]*>|[^<]+/g)||[];if(tokens.join('')!==text)throw new Error('INVALID_UPDATE_ARCHIVE');let at=0,nodes=0;
  const skip=()=>{while(at<tokens.length&&/^\s+$/.test(tokens[at]))at++;};
  const take=value=>{skip();if(tokens[at++]!==value)throw new Error('INVALID_UPDATE_ARCHIVE');};
  const decode=value=>{if(/&(?!amp;|lt;|gt;|quot;|apos;)/.test(value))throw new Error('INVALID_UPDATE_ARCHIVE');return value.replace(/&(amp|lt|gt|quot|apos);/g,(_match,key)=>({amp:'&',lt:'<',gt:'>',quot:'"',apos:"'"})[key]);};
  const scalar=tag=>{take('<'+tag+'>');let value='';if(at<tokens.length&&!tokens[at].startsWith('<'))value=decode(tokens[at++]);take('</'+tag+'>');return value;};
  function value(depth){skip();if(depth>24||++nodes>4096)throw new Error('INVALID_UPDATE_ARCHIVE');const token=tokens[at];
    if(token==='<dict>'){at++;const result=Object.create(null);while(true){skip();if(tokens[at]==='</dict>'){at++;return result;}const key=scalar('key');if(!key||Object.hasOwn(result,key))throw new Error('INVALID_UPDATE_ARCHIVE');result[key]=value(depth+1);}}
    if(token==='<array>'){at++;const result=[];while(true){skip();if(tokens[at]==='</array>'){at++;return result;}result.push(value(depth+1));}}
    if(/^<(true|false)\s*\/>$/.test(token||'')){at++;return token.startsWith('<true');}
    if(token==='<string>')return scalar('string');if(token==='<string/>'){at++;return '';}
    for(const tag of ['integer','real','date','data'])if(token==='<'+tag+'>'){const result=scalar(tag);if(tag==='integer'&&!/^-?\d+$/.test(result)||tag==='real'&&!/^-?\d+(?:\.\d+)?$/.test(result)||tag==='date'&&!/^\d{4}-\d{2}-\d{2}T[0-9:.]+Z$/.test(result)||tag==='data'&&!/^[A-Za-z0-9+/=\s]*$/.test(result))throw new Error('INVALID_UPDATE_ARCHIVE');return result;}
    throw new Error('INVALID_UPDATE_ARCHIVE');}
  take('<plist version="1.0">');const result=value(0);take('</plist>');skip();if(at!==tokens.length||!result||Array.isArray(result)||typeof result!=='object')throw new Error('INVALID_UPDATE_ARCHIVE');return result;
}
function version(value){if(typeof value!=='string'||!/^\d{1,6}\.\d{1,6}\.\d{1,6}$/.test(value)||value.split('.').some(v=>String(Number(v))!==v))throw new Error('INVALID_RELEASE_VERSION');return value.split('.').map(Number);}
function compareVersions(a,b){const x=version(a),y=version(b);for(let i=0;i<3;i++)if(x[i]!==y[i])return x[i]<y[i]?-1:1;return 0;}
function releaseURL(value,pathname){if(typeof value!=='string'||value!==ORIGIN+pathname)throw new Error('FORBIDDEN_UPDATE_URL');return new URL(value);}
function parseRelease(bytes,installed){
  if(!Buffer.isBuffer(bytes)||!bytes.length||bytes.length>LIMITS.metadata)throw new Error('INVALID_RELEASE_METADATA');
  let r;try{r=JSON.parse(new TextDecoder('utf-8',{fatal:true}).decode(bytes));}catch{throw new Error('INVALID_RELEASE_METADATA');}
  try{
    exact(r,['schema','product','channel','bundle_id','platform','arch','app_version','core','artifact','signing','provenance']);
    exact(r.core,['protocol','version','source','arch']);exact(r.artifact,['url','size','sha256']);
    exact(r.signing,['team_id','notarized']);exact(r.provenance,['source','checks_sha256','approval_sha256']);
    if(r.schema!==1||r.product!=='mesh-desktop'||r.channel!=='stable'||r.bundle_id!==BUNDLE_ID||r.platform!=='darwin'||!['arm64','x64'].includes(r.arch)||r.arch!==installed.arch||r.core.protocol!==1||r.core.arch!==({arm64:'arm64',x64:'amd64'})[r.arch]||!source(r.core.source)||r.provenance.source!==r.core.source||!sha(r.provenance.checks_sha256)||!sha(r.provenance.approval_sha256)||!Number.isSafeInteger(r.artifact.size)||r.artifact.size<=0||r.artifact.size>LIMITS.archive||!sha(r.artifact.sha256)||r.signing.team_id!==installed.team_id||!/^[A-Z0-9]{10}$/.test(r.signing.team_id)||r.signing.notarized!==true)throw new Error();
    version(r.app_version);version(r.core.version);releaseURL(r.artifact.url,`/desktop/releases/${r.app_version}/Mesh-${r.app_version}-${r.arch}.zip`);
  }catch(e){if(['INVALID_RELEASE_VERSION','FORBIDDEN_UPDATE_URL'].includes(e.message))throw e;throw new Error('INVALID_RELEASE_METADATA');}
  if(compareVersions(r.app_version,installed.app_version)<=0)throw new Error('UPDATE_VERSION_NOT_NEW');
  // Same-version slots are immutable even when the installed version has not advanced.
  const slot=installed.slots?.[r.app_version];if(slot&&(slot.sha256!==r.artifact.sha256||slot.source!==r.core.source||slot.size!==r.artifact.size||slot.arch!==r.arch))throw new Error('RELEASE_VERSION_COLLISION');
  return Object.freeze({...r,core:Object.freeze(r.core),artifact:Object.freeze(r.artifact),signing:Object.freeze(r.signing),provenance:Object.freeze(r.provenance)});
}
async function readAt(handle,length,position){const b=Buffer.alloc(length);let n=0;while(n<length){const r=await handle.read(b,n,length-n,position+n);if(!r.bytesRead)throw new Error('INVALID_UPDATE_ARCHIVE');n+=r.bytesRead;}return b;}
async function hashFile(handle,size){const hash=createHash('sha256');for(let at=0;at<size;at+=65536)hash.update(await readAt(handle,Math.min(65536,size-at),at));return hash.digest('hex');}
function crc32(data){if(typeof zlib.crc32==='function')return zlib.crc32(data);let crc=0xffffffff;for(const n of data){crc^=n;for(let b=0;b<8;b++)crc=(crc>>>1)^((crc&1)?0xedb88320:0);}return (crc^0xffffffff)>>>0;}
function canonicalName(bytes,limits){if(bytes.length>(limits.pathBytes??LIMITS.pathBytes))throw new Error('INVALID_UPDATE_ARCHIVE');let name;try{name=new TextDecoder('utf-8',{fatal:true}).decode(bytes);}catch{throw new Error('INVALID_UPDATE_ARCHIVE');}
  if(name.split('/').length-(name.endsWith('/')?1:0)>(limits.pathComponents??LIMITS.pathComponents)||name.normalize('NFC')!==name||!name.startsWith('Mesh.app/')||/[^a-zA-Z0-9_ .()\/@+,\-]/.test(name)||name.split('/').slice(0,name.endsWith('/')?-1:undefined).some(p=>!p||p==='.'||p==='..'))throw new Error('INVALID_UPDATE_ARCHIVE');return name;}
function zipExtra(bytes,local){
  // The shipped ditto contract uses only Info-ZIP Unix timestamps/uid/gid.
  // Unicode-path, ZIP64 and other interpretation-changing extensions refuse.
  let at=0;while(at<bytes.length){if(at+4>bytes.length)throw new Error('INVALID_UPDATE_ARCHIVE');const id=bytes.readUInt16LE(at),length=bytes.readUInt16LE(at+2);if(id!==0x5855||length!==(local?12:8)||at+4+length>bytes.length||at!==0)throw new Error('INVALID_UPDATE_ARCHIVE');at+=4+length;}
}
// ZIP admission is independent of Squirrel's extractor. Framework symlinks are
// permitted only when their complete chain stays inside the single Mesh.app root.
async function validateArchive(handle,release,limits=LIMITS){
  const stat=await handle.stat();if(!stat.isFile()||stat.size!==release.artifact.size||stat.size>limits.archive||stat.size<22||await hashFile(handle,stat.size)!==release.artifact.sha256)throw new Error('UPDATE_INTEGRITY_FAILED');
  const tail=await readAt(handle,Math.min(stat.size,65557),stat.size-Math.min(stat.size,65557));let end=-1;
  for(let n=tail.length-22;n>=0;n--)if(tail.readUInt32LE(n)===0x06054b50&&n+22+tail.readUInt16LE(n+20)===tail.length){end=n;break;}
  if(end<0||tail.readUInt16LE(end+4)||tail.readUInt16LE(end+6)||tail.readUInt16LE(end+8)!==tail.readUInt16LE(end+10))throw new Error('INVALID_UPDATE_ARCHIVE');
  const count=tail.readUInt16LE(end+10),length=tail.readUInt32LE(end+12),offset=tail.readUInt32LE(end+16),endOffset=stat.size-tail.length+end;
  if(!count||count>limits.entries||count===65535||length>4*1024*1024||offset+length!==endOffset)throw new Error('INVALID_UPDATE_ARCHIVE');
  const central=await readAt(handle,length,offset),entries=new Map(),aliases=new Set(),ranges=[],content=new Map();let at=0,total=0;
  for(let n=0;n<count;n++){
    if(at+46>length||central.readUInt32LE(at)!==0x02014b50||central.readUInt16LE(at+4)>>>8!==3)throw new Error('INVALID_UPDATE_ARCHIVE');
    const flags=central.readUInt16LE(at+8),method=central.readUInt16LE(at+10),crc=central.readUInt32LE(at+16),compressed=central.readUInt32LE(at+20),expanded=central.readUInt32LE(at+24),nl=central.readUInt16LE(at+28),el=central.readUInt16LE(at+30),cl=central.readUInt16LE(at+32),disk=central.readUInt16LE(at+34),mode=central.readUInt32LE(at+38)>>>16,start=central.readUInt32LE(at+42);
    if(at+46+nl+el+cl>length||disk||flags&~0x0808||![0,8].includes(method)||compressed===0xffffffff||expanded===0xffffffff||start===0xffffffff||expanded>limits.entry||(total+=expanded)>limits.expanded)throw new Error('INVALID_UPDATE_ARCHIVE');
    const nameBytes=central.subarray(at+46,at+46+nl),name=canonicalName(nameBytes,limits),kind=mode&0xf000;
    zipExtra(central.subarray(at+46+nl,at+46+nl+el),false);
    const alias=name.replace(/\/$/,'').toLowerCase();if(aliases.has(alias)||mode&0o7000||![0x8000,0x4000,0xa000].includes(kind)||name.endsWith('/')&&kind!==0x4000||kind===0x4000&&!name.endsWith('/'))throw new Error('INVALID_UPDATE_ARCHIVE');aliases.add(alias);
    const local=await readAt(handle,30,start);if(local.readUInt32LE(0)!==0x04034b50||local.readUInt16LE(6)!==flags||local.readUInt16LE(8)!==method||local.readUInt16LE(26)!==nl)throw new Error('INVALID_UPDATE_ARCHIVE');
    const extra=local.readUInt16LE(28),dataStart=start+30+nl+extra,dataEnd=dataStart+compressed;if(dataEnd>offset)throw new Error('INVALID_UPDATE_ARCHIVE');
    zipExtra(await readAt(handle,extra,start+30+nl),true);
    if(!(flags&8)&&(local.readUInt32LE(14)!==crc||local.readUInt32LE(18)!==compressed||local.readUInt32LE(22)!==expanded))throw new Error('INVALID_UPDATE_ARCHIVE');
    if(!(await readAt(handle,nl,start+30)).equals(nameBytes))throw new Error('INVALID_UPDATE_ARCHIVE');
    let rangeEnd=dataEnd;
    if(flags&8){const d=await readAt(handle,Math.min(16,offset-dataEnd),dataEnd),skip=d.length>=4&&d.readUInt32LE(0)===0x08074b50?4:0;
      if(d.length<skip+12||d.readUInt32LE(skip)!==crc||d.readUInt32LE(skip+4)!==compressed||d.readUInt32LE(skip+8)!==expanded)throw new Error('INVALID_UPDATE_ARCHIVE');rangeEnd+=skip+12;}
    const raw=await readAt(handle,compressed,dataStart);let data;try{if(method===0)data=raw;else{const inflated=zlib.inflateRawSync(raw,{maxOutputLength:Math.max(1,expanded),info:true});if(inflated.engine.bytesWritten!==raw.length)throw new Error();data=inflated.buffer;}}catch{throw new Error('INVALID_UPDATE_ARCHIVE');}
    if(data.length!==expanded||crc32(data)!==crc||name.endsWith('/')&&data.length)throw new Error('INVALID_UPDATE_ARCHIVE');
    entries.set(name,{kind,mode,data:kind===0xa000?data:null});ranges.push([start,rangeEnd]);
    if(['Mesh.app/Contents/Info.plist','Mesh.app/Contents/Resources/mesh-core/core.json'].includes(name)){if(kind===0xa000||expanded>65536)throw new Error('INVALID_UPDATE_ARCHIVE');content.set(name,data);}
    if(name==='Mesh.app/Contents/MacOS/Mesh'){if(kind!==0x8000||!(mode&0o111)||expanded>16*1024*1024)throw new Error('INVALID_UPDATE_ARCHIVE');content.set(name,data);}
    at+=46+nl+el+cl;
  }
  if(at!==length)throw new Error('INVALID_UPDATE_ARCHIVE');ranges.sort((a,b)=>a[0]-b[0]);let next=0;for(const [start,end]of ranges){if(start!==next)throw new Error('INVALID_UPDATE_ARCHIVE');next=end;}if(next!==offset)throw new Error('INVALID_UPDATE_ARCHIVE');
  // Register every component, including implicit directories. macOS commonly
  // compares names without case; a full-path alias check alone misses ancestors.
  // Count a component trie first: no growing full-path prefix is allocated
  // before the complete implicit namespace fits its bound.
  const tree={component:'Mesh.app',kind:0x4000,explicit:false,children:new Map()};let nodeCount=1;
  for(const [name,e]of entries){
    const parts=name.replace(/\/$/,'').split('/');let node=tree;
    for(let i=0;i<parts.length;i++){
      const leaf=i===parts.length-1;
      if(i){const component=parts[i],key=component.toLowerCase();let child=node.children.get(key);
        if(child&&child.component!==component)throw new Error('INVALID_UPDATE_ARCHIVE');
        if(!child){if(++nodeCount>(limits.namespaceNodes??LIMITS.namespaceNodes))throw new Error('INVALID_UPDATE_ARCHIVE');child={component,kind:leaf?e.kind:0x4000,explicit:false,children:new Map()};node.children.set(key,child);}node=child;}
      if(!leaf&&node.kind!==0x4000||leaf&&node.explicit||leaf&&node.kind===0x4000&&e.kind!==0x4000)throw new Error('INVALID_UPDATE_ARCHIVE');
      if(leaf){node.kind=e.kind;node.explicit=true;}
    }
  }
  const namespace=new Map();
  function record(node,parent){const name=parent?parent+'/'+node.component:node.component;namespace.set(name.toLowerCase(),{name,kind:node.kind});for(const child of node.children.values())record(child,name);}
  record(tree,'');
  function linkTarget(entry){
    if(entry.data.length>4096)throw new Error('INVALID_UPDATE_ARCHIVE');
    let target;try{target=new TextDecoder('utf-8',{fatal:true}).decode(entry.data);}catch{throw new Error('INVALID_UPDATE_ARCHIVE');}
    if(!target||target.startsWith('/')||/[^a-zA-Z0-9_ .()\/@+,\-]/.test(target)||target.split('/').some(part=>!part))throw new Error('INVALID_UPDATE_ARCHIVE');
    return target.split('/');
  }
  for(const [name,e]of entries){
    if(e.kind!==0xa000)continue;
    // Resolve components in filesystem order. A parent step applies AFTER a
    // preceding symlink, and may never leave Mesh.app even if a later step returns.
    const stack=name.split('/').slice(0,-1),pending=linkTarget(e);let links=0,steps=0;
    while(pending.length){
      if(++steps>4096)throw new Error('INVALID_UPDATE_ARCHIVE');const part=pending.shift();
      if(part==='.')continue;
      if(part==='..'){if(stack.length<=1)throw new Error('INVALID_UPDATE_ARCHIVE');stack.pop();continue;}
      const current=[...stack,part].join('/'),node=namespace.get(current.toLowerCase());
      // Wrong-case link targets are refused rather than relying on volume rules.
      if(!node||node.name!==current)throw new Error('INVALID_UPDATE_ARCHIVE');
      if(node.kind===0xa000){if(++links>32)throw new Error('INVALID_UPDATE_ARCHIVE');pending.unshift(...linkTarget(entries.get(current)));}
      else{if(pending.length&&node.kind!==0x4000)throw new Error('INVALID_UPDATE_ARCHIVE');stack.push(part);}
    }
    if(stack[0]!=='Mesh.app')throw new Error('INVALID_UPDATE_ARCHIVE');
  }
  const coreBytes=content.get('Mesh.app/Contents/Resources/mesh-core/core.json'),plistBytes=content.get('Mesh.app/Contents/Info.plist');if(!coreBytes||!plistBytes)throw new Error('INVALID_UPDATE_ARCHIVE');
  let core;try{core=JSON.parse(new TextDecoder('utf-8',{fatal:true}).decode(coreBytes));exact(core,['protocol','version','source','platform','arch','sha256']);}catch{throw new Error('INVALID_UPDATE_ARCHIVE');}
  if(core.protocol!==1||core.version!==release.core.version||core.source!==release.core.source||core.platform!=='darwin'||core.arch!==release.core.arch||!sha(core.sha256))throw new Error('UPDATE_SOURCE_MISMATCH');
  const plist=parsePlist(plistBytes);
  if(plist.CFBundleIdentifier!==BUNDLE_ID||plist.CFBundleShortVersionString!==release.app_version||plist.CFBundleVersion!==release.app_version||plist.ElectronSquirrelPreventDowngrades!==true)throw new Error('UPDATE_SOURCE_MISMATCH');
  // Hash the actual core bytes, rather than accepting only its self-reported manifest.
  const corePath='Mesh.app/Contents/Resources/mesh-core/mesh-desktop-core';if(entries.get(corePath)?.kind!==0x8000||!(entries.get(corePath).mode&0o111))throw new Error('INVALID_UPDATE_ARCHIVE');
  // Full ZIP bytes have already been CRC-read. Re-read the bounded core from its
  // central record to avoid retaining every executable in memory at once.
  let position=0,actualCore;
  while(position<central.length){const nl=central.readUInt16LE(position+28),el=central.readUInt16LE(position+30),cl=central.readUInt16LE(position+32);if(central.subarray(position+46,position+46+nl).toString('utf8')===corePath){const localAt=central.readUInt32LE(position+42),local=await readAt(handle,30,localAt),raw=await readAt(handle,central.readUInt32LE(position+20),localAt+30+nl+local.readUInt16LE(28));actualCore=central.readUInt16LE(position+10)===0?raw:zlib.inflateRawSync(raw,{maxOutputLength:limits.entry});break;}position+=46+nl+el+cl;}
  if(!actualCore||createHash('sha256').update(actualCore).digest('hex')!==core.sha256)throw new Error('UPDATE_SOURCE_MISMATCH');
  const cpu=release.arch==='arm64'?0x0100000c:0x01000007;
  for(const executable of [actualCore,content.get('Mesh.app/Contents/MacOS/Mesh')])if(!executable||executable.length<32||executable.readUInt32LE(0)!==0xfeedfacf||executable.readUInt32LE(4)!==cpu)throw new Error('UPDATE_ARCH_MISMATCH');
  return Object.freeze({entries:count,expanded:total,core:Object.freeze(core)});
}
module.exports={ORIGIN,BUNDLE_ID,LIMITS,version,compareVersions,releaseURL,parseRelease,parsePlist,readAt,hashFile,validateArchive,crc32};
