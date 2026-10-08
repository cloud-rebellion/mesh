// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const https=require('node:https'),fs=require('node:fs/promises'),os=require('node:os'),path=require('node:path');
const {constants}=require('node:fs'),{createHash,randomUUID}=require('node:crypto');
const {ORIGIN,LIMITS,parseRelease,validateArchive,hashFile,readAt}=require('./release-contract.cjs');
// No channel, signing identity or desktop publisher authority exists yet.
// Keep the future provider behind a capability boundary instead of accepting a renderer URL.
class DisabledUpdater {
  status() { return Object.freeze({state:'disabled',reason:'Signed application updates are not configured in this build.'}); }
  async check() { throw new Error('UPDATES_DISABLED'); }
  async install() { throw new Error('UPDATES_DISABLED'); }
}
async function heldBlob(root,write){
  const directory=await fs.mkdtemp(path.join(root,'mesh-update-'));await fs.chmod(directory,0o700);
  const filename=path.join(directory,'bytes');let writer,reader;
  try{
    writer=await fs.open(filename,constants.O_WRONLY|constants.O_CREAT|constants.O_EXCL|constants.O_NOFOLLOW,0o600);
    await write(writer);await writer.sync();const written=await writer.stat();await writer.close();writer=null;
    await fs.chmod(filename,0o400);reader=await fs.open(filename,constants.O_RDONLY|constants.O_NOFOLLOW);
    const read=await reader.stat();if(!read.isFile()||read.dev!==written.dev||read.ino!==written.ino||read.size!==written.size||read.nlink!==1||(read.mode&0o777)!==0o400||(typeof process.getuid==='function'&&read.uid!==process.getuid()))throw new Error('UPDATE_INTEGRITY_FAILED');
    await fs.unlink(filename);await fs.rmdir(directory);
    const digest=await hashFile(reader,read.size);let disposed=false;
    return {handle:reader,size:read.size,sha256:digest,url:`file:///dev/fd/${reader.fd}`,async dispose(){if(!disposed){disposed=true;await reader.close();}}};
  }catch(e){await writer?.close().catch(()=>{});await reader?.close().catch(()=>{});await fs.rm(directory,{recursive:true,force:true});throw e;}
}
// No ambient cookie/auth/proxy/Range headers. Node HTTPS does not follow redirects.
function transfer(url,{limit,size,sha256,kind,writer,request=https.get,timeout=120000}){
  if(typeof url!=='string'||!/^https:\/\/mesh\.brightinteraction\.com\/desktop\/releases\/(?:stable\/darwin-(?:arm64|x64)\.json|\d+\.\d+\.\d+\/Mesh-\d+\.\d+\.\d+-(?:arm64|x64)\.zip)$/.test(url))return Promise.reject(new Error('FORBIDDEN_UPDATE_URL'));
  return new Promise((resolve,reject)=>{
    let settled=false,response,req,total=0,hash=createHash('sha256');
    const finish=error=>{if(settled)return;settled=true;clearTimeout(timer);if(error){response?.destroy();req?.destroy();reject(error);}else resolve({size:total,sha256:hash.digest('hex')});};
    const timer=setTimeout(()=>finish(new Error('UPDATE_TRANSFER_FAILED')),timeout);
    try{req=request(url,{agent:false,minVersion:'TLSv1.2',headers:{Accept:kind==='metadata'?'application/json':'application/zip, application/octet-stream'},timeout:30000},async res=>{
      response=res;const length=res.headers['content-length'],type=(res.headers['content-type']||'').split(';')[0].trim().toLowerCase();
      if(res.statusCode!==200||res.headers.location||res.headers['content-range']||res.headers['content-encoding']&&res.headers['content-encoding']!=='identity'||(length!==undefined&&(!/^(0|[1-9]\d*)$/.test(length)||!Number.isSafeInteger(Number(length))||Number(length)>limit||size!==undefined&&Number(length)!==size))||!(kind==='metadata'?type==='application/json':['application/zip','application/octet-stream'].includes(type))){finish(new Error('UPDATE_TRANSFER_FAILED'));return;}
      try{for await(const part of res){if(settled)return;const bytes=Buffer.from(part);total+=bytes.length;if(total>limit||size!==undefined&&total>size)throw new Error('UPDATE_INTEGRITY_FAILED');hash.update(bytes);let at=0;while(at<bytes.length){const result=await writer.write(bytes,at,bytes.length-at,null);if(!result.bytesWritten)throw new Error('UPDATE_TRANSFER_FAILED');at+=result.bytesWritten;}}
        if(size!==undefined&&total!==size||sha256&&hash.copy().digest('hex')!==sha256)throw new Error('UPDATE_INTEGRITY_FAILED');finish();
      }catch(error){finish(error);}
    });req.on('error',()=>finish(new Error('UPDATE_TRANSFER_FAILED')));req.on('timeout',()=>finish(new Error('UPDATE_TRANSFER_FAILED')));
    }catch{finish(new Error('UPDATE_TRANSFER_FAILED'));}
  });
}
class ReleaseLedger {
  constructor(directory){if(!path.isAbsolute(directory))throw new Error('INVALID_UPDATE_LEDGER');this.directory=directory;this.filename=path.join(directory,'release-slots.json');this.slots={};}
  async load(){
    const st=await fs.lstat(this.directory);if(!st.isDirectory()||st.isSymbolicLink()||(st.mode&0o777)!==0o700||typeof process.getuid==='function'&&st.uid!==process.getuid())throw new Error('INVALID_UPDATE_LEDGER');
    let handle;try{handle=await fs.open(this.filename,constants.O_RDONLY|constants.O_NOFOLLOW);const stat=await handle.stat();if(!stat.isFile()||stat.size>65536||stat.nlink!==1||(stat.mode&0o777)!==0o600)throw new Error('INVALID_UPDATE_LEDGER');
      const value=JSON.parse(new TextDecoder('utf-8',{fatal:true}).decode(await readAt(handle,stat.size,0)));if(!value||Array.isArray(value)||Object.keys(value).length>128)throw new Error('INVALID_UPDATE_LEDGER');
      for(const [version,slot]of Object.entries(value)){if(!/^\d+\.\d+\.\d+$/.test(version)||!slot||Object.keys(slot).sort().join(',')!=='arch,fingerprint,sha256,size,source'||!['arm64','x64'].includes(slot.arch)||!Number.isSafeInteger(slot.size)||slot.size<=0||slot.size>LIMITS.archive||!/^[a-f0-9]{64}$/.test(slot.sha256)||!/^[a-f0-9]{64}$/.test(slot.fingerprint)||!/^[a-f0-9]{40}$/.test(slot.source))throw new Error('INVALID_UPDATE_LEDGER');}this.slots=value;
    }catch(error){if(error.code!=='ENOENT')throw new Error('INVALID_UPDATE_LEDGER');this.slots={};}finally{await handle?.close();}return structuredClone(this.slots);
  }
  async remember(release){
    // Do not treat a timeout/crash as permission to break another writer's lock.
    // A residual lock refuses publication until its owner is inspected.
    let lock;try{lock=await fs.open(path.join(this.directory,'release-slots.lock'),constants.O_WRONLY|constants.O_CREAT|constants.O_EXCL|constants.O_NOFOLLOW,0o600);}catch{throw new Error('UPDATE_LEDGER_LOCKED');}
    try{return await this.rememberLocked(release);}finally{await lock.close();await fs.unlink(path.join(this.directory,'release-slots.lock'));}
  }
  async rememberLocked(release){
    await this.load();const fingerprint=createHash('sha256').update(JSON.stringify([release.app_version,release.core.protocol,release.core.version,release.core.source,release.core.arch,release.artifact.url,release.artifact.size,release.artifact.sha256,release.signing.team_id,release.signing.notarized,release.provenance.source,release.provenance.checks_sha256,release.provenance.approval_sha256])).digest('hex'),old=this.slots[release.app_version];
    if(old&&old.fingerprint!==fingerprint)throw new Error('RELEASE_VERSION_COLLISION');
    if(!old&&Object.keys(this.slots).length>=128)throw new Error('INVALID_UPDATE_LEDGER');
    this.slots[release.app_version]={fingerprint,sha256:release.artifact.sha256,size:release.artifact.size,source:release.core.source,arch:release.arch};
    const temporary=path.join(this.directory,randomUUID()+'.pending');let writer;
    try{writer=await fs.open(temporary,constants.O_WRONLY|constants.O_CREAT|constants.O_EXCL|constants.O_NOFOLLOW,0o600);await writer.writeFile(JSON.stringify(this.slots));await writer.sync();await writer.close();writer=null;await fs.rename(temporary,this.filename);const parent=await fs.open(this.directory,constants.O_RDONLY);try{await parent.sync();}finally{await parent.close();}}
    finally{await writer?.close().catch(()=>{});await fs.rm(temporary,{force:true});}
  }
}
class PreparedUpdater {
  // Native-main preparation only. configureUpdater never selects this class.
  // Admission must be supplied by a separately accepted desktop publisher;
  // an untrusted descriptor cannot certify its own source/CI/signing claims.
  constructor({native,installed,admit,ledger,request=https.get,temporary=os.tmpdir(),stageTimeout=60000}){
    if(!native||typeof admit!=='function'||!(ledger instanceof ReleaseLedger)||!installed||installed.platform!=='darwin'||installed.bundle_id!=='com.brightinteraction.mesh.desktop'||!['arm64','x64'].includes(installed.arch)||!/^[A-Z0-9]{10}$/.test(installed.team_id)||installed.distribution_signed!==true||installed.notarized!==true)throw new Error('UNAPPROVED_UPDATE_CHANNEL');
    Object.assign(this,{native,installed:structuredClone(installed),admit,ledger,request,temporary,stageTimeout});this.state='idle';this.blobs=[];this.release=null;this.flight=null;
  }
  status(){const reasons={idle:'Update preparation is configured.',checking:'Checking the approved release channel.',available:'A verified update is downloaded. Restart after saving your work.',quiescing:'Preparing a safe restart.',staging:'The vault is closed while the native updater stages the admitted release.',ready:'The native update is ready to restart.',uncertain:'The native update outcome is uncertain. Keep this application open and inspect its state before another attempt.',failed:'The update was refused. No native staging was started.'};return Object.freeze({state:this.state,reason:reasons[this.state]||reasons.failed});}
  async check(){
    if(this.flight||['quiescing','staging','ready','uncertain'].includes(this.state))throw new Error('UPDATE_BUSY');
    const work=this.download();this.flight=work;try{return await work;}finally{this.flight=null;}
  }
  async download(){
    this.state='checking';let metadata,archive;
    try{
      metadata=await heldBlob(this.temporary,w=>transfer(ORIGIN+`/desktop/releases/stable/darwin-${this.installed.arch}.json`,{kind:'metadata',limit:LIMITS.metadata,writer:w,request:this.request}));
      this.installed.slots=await this.ledger.load();const release=parseRelease(await readAt(metadata.handle,metadata.size,0),this.installed);
      if(await this.admit(release)!==true)throw new Error('UNAPPROVED_RELEASE');
      archive=await heldBlob(this.temporary,w=>transfer(release.artifact.url,{kind:'archive',limit:LIMITS.archive,size:release.artifact.size,sha256:release.artifact.sha256,writer:w,request:this.request}));
      await validateArchive(archive.handle,release);
      const normalized=Buffer.from(JSON.stringify({currentRelease:release.app_version,releases:[{version:release.app_version,updateTo:{version:release.app_version,url:archive.url,sha256:archive.sha256,size:archive.size,notes:'Verified Mesh application update.'}}]}));
      const feed=await heldBlob(this.temporary,w=>w.writeFile(normalized));await metadata.dispose();metadata=null;
      try{await this.ledger.remember(release);}catch(error){await feed.dispose();throw error;}
      for(const previous of this.blobs)await previous.dispose();this.blobs=[archive,feed];archive=null;this.release=release;
      this.installed.slots||={};this.installed.slots[release.app_version]={sha256:release.artifact.sha256,size:release.artifact.size,source:release.core.source,arch:release.arch};this.state='available';return this.status();
    }catch(error){await metadata?.dispose();await archive?.dispose();this.state='failed';throw error;}
  }
  async install({drain}={}){
    if(this.flight||this.state!=='available'||typeof drain!=='function')throw new Error('UPDATE_NOT_READY');
    this.state='quiescing';try{await drain();}catch(e){this.state='available';throw e;}
    // Squirrel can install a staged release on any later termination. Start its
    // native check only after the actual editor/engine drain has been confirmed.
    // Download admission may be old by the time a human can safely restart.
    // Revalidate authority immediately before any native staging transition.
    try{if(await this.admit(this.release)!==true)throw new Error('UNAPPROVED_RELEASE');}
    catch(e){this.state='uncertain';throw e;}
    this.state='staging';const work=this.stage();this.flight=work;
    try{await work;this.state='ready';await this.dispose();this.native.quitAndInstall();}
    catch(e){this.state='uncertain';throw e;}finally{this.flight=null;}
  }
  stage(){return new Promise((resolve,reject)=>{
    let finished=false;const done=error=>{if(finished)return;finished=true;clearTimeout(timer);this.native.removeListener('error',failed);this.native.removeListener('update-not-available',missing);this.native.removeListener('update-downloaded',downloaded);error?reject(error):resolve();};
    const failed=()=>done(new Error('NATIVE_UPDATE_UNCERTAIN')),missing=()=>done(new Error('NATIVE_UPDATE_UNCERTAIN')),downloaded=()=>done();
    const timer=setTimeout(()=>done(new Error('NATIVE_UPDATE_UNCERTAIN')),this.stageTimeout);
    this.native.once('error',failed);this.native.once('update-not-available',missing);this.native.once('update-downloaded',downloaded);
    try{this.native.setFeedURL({url:this.blobs[1].url,serverType:'json'});this.native.checkForUpdates();}catch{failed();}
  });}
  async dispose(){for(const blob of this.blobs)await blob.dispose();this.blobs=[];}
}
function configureUpdater(configuration) {
  if (configuration !== null && configuration !== undefined) throw new Error('UNAPPROVED_UPDATE_CHANNEL');
  return new DisabledUpdater();
}
module.exports = { DisabledUpdater, configureUpdater,PreparedUpdater,ReleaseLedger,heldBlob,transfer };
