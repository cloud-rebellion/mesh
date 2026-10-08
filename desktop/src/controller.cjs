// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const { EventEmitter } = require('node:events');
const {randomBytes}=require('node:crypto');
const { Engine } = require('./engine.cjs');
const { exact, text, safeError } = require('./boundaries.cjs');
const { PrivateFileCredentialProvider } = require('./credentials.cjs');
function statusCard(value) {
  exact(value,['state','vault','sync','identity','version']);
  exact(value.vault,['name','id']); exact(value.sync,['state','pending','conflicts','last_success']);
  if (!text(value.vault.name,128) || typeof value.vault.id !== 'string' || value.vault.id.length > 128 ||
      !text(value.state,48) || !text(value.sync.state,48) || !Number.isSafeInteger(value.sync.pending) || value.sync.pending < 0 ||
      !Number.isSafeInteger(value.sync.conflicts) || value.sync.conflicts < 0 ||
      !(value.sync.last_success === null || typeof value.sync.last_success === 'string' && value.sync.last_success.length <= 64) || !text(value.version,64)) throw new Error('INVALID_ENGINE_RESPONSE');
  let identity = null;
  if (value.identity !== null) {
    exact(value.identity,['user','role','verified']);
    if (!text(value.identity.user,256) || typeof value.identity.verified !== 'boolean' || !(value.identity.role === null || ['owner','member','viewer','unknown'].includes(value.identity.role))) throw new Error('INVALID_ENGINE_RESPONSE');
    identity = { user:value.identity.user,role:value.identity.verified && value.identity.role || 'unknown',verified:value.identity.verified };
  }
  return {state:value.state,vault:value.vault,sync:value.sync,identity,version:value.version};
}
// Match the durable toolWrite receipt, not a prepare/validate or valid-looking
// error result. The shared Go writer may confirm saving with index_stale=true.
function confirmedWrite(result,{action,note}={}){
  try{
    if(result?.isError===true||!Array.isArray(result?.content)||result.content.length!==1)return false;
    const item=result.content[0];if(item.type!=='text'||typeof item.text!=='string'||Buffer.byteLength(item.text)>16384)return false;
    const r=JSON.parse(item.text);exact(r,['id','path','when','todo','status','template','template_version','revision','updated','previous_revision','index_stale','index_error','owner_down','warning'],['id','path','when','todo','status','template','template_version','revision']);
    if(!['draft','publish'].includes(action)||!note||typeof note!=='object')return false;
    const id=r.id;
    if(typeof id!=='string'||!id||Buffer.byteLength(id)>252||id!==id.toLowerCase().normalize('NFC')||!/^[\p{L}\p{Nd}]/u.test(id)||!/^[\p{L}\p{Nd}\p{M}-]+$/u.test(id)||id.endsWith('-')||id.includes('--')||/[àáâãäåæçèéêëìíîïðñòóôõöøùúûüýÿþßœ]/u.test(id))return false;
    if(typeof r.revision!=='string'||!/^[a-f0-9]{64}$/.test(r.revision)||!text(r.path,4096)||r.path.startsWith('/')||r.path.includes('\\')||!r.path.endsWith('.md')||r.path.split('/').some(part=>!part||part==='.'||part==='..')||typeof r.when!=='string'||Buffer.byteLength(r.when)>64||/[\u0000-\u001f\u007f]/.test(r.when)||!(r.todo===null||Array.isArray(r.todo)&&r.todo.every(item=>typeof item==='string')))return false;
    const expected=action==='draft'?'draft':typeof note.status==='string'&&note.status.trim().toLowerCase()!=='draft'&&note.status.trim()?note.status.trim().toLowerCase():'active';
    if(r.status!==expected||typeof r.template!=='string'||!/^[a-z][a-z0-9_-]{0,63}$/.test(r.template)||!Number.isSafeInteger(r.template_version)||r.template_version<1||note.template&&r.template!==note.template||note.template_version&&r.template_version!==note.template_version)return false;
    if(note.update_id){if(note.draft_id||typeof note.update_revision!=='string'||!/^[a-f0-9]{64}$/.test(note.update_revision)||r.id!==note.update_id||r.updated!==true||r.previous_revision!==note.update_revision)return false;}
    else if('updated'in r||'previous_revision'in r)return false;
    if(note.draft_id&&(typeof note.draft_revision!=='string'||!/^[a-f0-9]{64}$/.test(note.draft_revision)||r.id!==note.draft_id))return false;
    if('index_stale'in r&&r.index_stale!==true||'index_error'in r&&typeof r.index_error!=='string'||'owner_down'in r&&r.owner_down!==true||'warning'in r&&typeof r.warning!=='string')return false;
    return true;
  }catch{return false;}
}
class Controller extends EventEmitter {
  constructor({storage,bundle,picker,updater,engineFactory = options=>new Engine(options),appVersion,enrollmentEnabled=false,confirmJoin=async()=>false}) {
    super(); Object.assign(this,{storage,bundle,picker,updater,engineFactory,appVersion});
    this.engine = null; this.selected = null; this.card = null; this.phase = 'start'; this.error = null; this.busy = false;
    this.credentials = new PrivateFileCredentialProvider();
    this.enrollmentEnabled=enrollmentEnabled;this.confirmJoin=confirmJoin;
    this.tools=new Set();this.uncertainWrite=false;this.updateLock=false;this.quitLock=false;this.restartToken=null;
  }
  overview() { return {phase:this.phase,vaults:this.storage.cards(),selected:this.selected ? {id:this.selected.id,name:this.selected.name}:null,
    status:this.card,error:this.error,app_version:this.appVersion,updates:{...this.updater.status(),restart_token:this.restartToken},credentials:this.credentials.capabilities(),enrollment:{enabled:this.enrollmentEnabled,reason:this.enrollmentEnabled?'Use an invitation to join the configured Mesh service.':'Native team enrollment is pending verification.'}}; }
  changed() { this.emit('change',this.overview()); }
  async open(record,{initialize=false,forJoin=false}={}) {
    this.phase='opening'; this.error=null; this.changed();
    if (this.engine) await this.engine.close();
    this.card=null; this.selected=record;
    const verified = await this.bundle();
    const engine=this.engineFactory({binary:verified.binary,vault:record.path}); this.engine=engine;
    engine.on('state',state=>{ if (this.engine === engine && state.state === 'failed') { this.phase='recovery';this.error=state.error;this.changed(); } });
    engine.start();
    if (initialize) await engine.request('init',forJoin?{name:record.name,for_join:true}:{name:record.name});
    this.card=statusCard(await engine.request('status',{})); this.phase='vault'; this.changed();
    return this.overview();
  }
  async perform(action,params={}) {
    let ownsBusy=false;
    try {
      if (action === 'overview') { exact(params,[]); return this.overview(); }
      if(this.updateLock)throw new Error('UPDATE_RESTART_LOCKED');
      if(this.quitLock)throw new Error('QUIT_DRAIN_LOCKED');
      if (action === 'tool') {
        exact(params,['name','arguments']); if (this.phase !== 'vault' || !this.engine) throw new Error('ENGINE_UNAVAILABLE');
        const engine=this.engine,selected=this.selected,write=params.name==='mesh_author_note'&&['draft','publish'].includes(params.arguments?.action),pending={write};this.tools.add(pending);
        try{const result=await engine.request('tool',params);
          if(write&&!confirmedWrite(result,params.arguments))this.uncertainWrite=true;
          if(this.engine!==engine||this.selected!==selected||this.phase!=='vault')throw new Error('VAULT_CHANGED');return result;
        }catch(error){if(write)this.uncertainWrite=true;if(this.engine!==engine||this.selected!==selected||this.phase!=='vault')throw new Error('VAULT_CHANGED');throw error;}
        finally{this.tools.delete(pending);}
      }
      if (this.busy) throw new Error('OPERATION_BUSY');
      this.busy=true;ownsBusy=true;
      if (action === 'create') { exact(params,['name']); return await this.open(await this.storage.create(params.name),{initialize:true}); }
      if (action === 'open') { exact(params,['id']); return await this.open(this.storage.resolve(params.id)); }
      if (action === 'pick') { exact(params,[]); const selected=await this.picker(); if (!selected) return this.overview(); return await this.open(await this.storage.adopt(selected)); }
      if (action === 'home') { exact(params,[]); if (this.engine) await this.engine.close(); this.engine=null;this.selected=null;this.card=null;this.phase='start';this.error=null;this.changed();return this.overview(); }
      if (action === 'sync') { exact(params,[]); if (!this.engine || this.phase !== 'vault') throw new Error('ENGINE_UNAVAILABLE'); await this.engine.request('sync',{}); this.card=statusCard(await this.engine.request('status',{}));this.changed();return this.overview(); }
      if (action === 'join') {
        if(!this.enrollmentEnabled)throw new Error('JOIN_UNAVAILABLE');
        exact(params,['name','invite']);if(!text(params.name,128)||!text(params.invite,4096))throw new Error('INVALID_REQUEST');
        if(!await this.confirmJoin({name:params.name,hub:'https://mesh.brightinteraction.com/'}))return this.overview();
        const record=await this.storage.create(params.name);await this.open(record,{initialize:true,forJoin:true});
        // A single explicit attempt. The core owns durable accepted/uncertain enrollment receipts.
        await this.engine.request('join',{hub_url:'https://mesh.brightinteraction.com/',invite:params.invite});
        this.card=statusCard(await this.engine.request('status',{}));this.changed();return this.overview();
      }
      if (action === 'updates') { exact(params,[]); await this.updater.check();this.changed();return this.overview(); }
      else throw new Error('FORBIDDEN_ACTION');
    } catch (error) {
      this.error=safeError(error); if (['opening','recovery'].includes(this.phase)) this.phase='recovery';this.changed();throw error;
    } finally { if (ownsBusy) this.busy=false; }
  }
  async refresh() {
    if (!this.engine || this.busy || this.updateLock || this.quitLock || this.phase !== 'vault') return this.overview();
    const engine=this.engine,selected=this.selected;
    try { const value=await engine.request('status',{});if(this.engine!==engine||this.selected!==selected||this.phase!=='vault')return this.overview();
      this.card=statusCard(value);this.changed();
    } catch (e) {if(this.engine!==engine||this.selected!==selected||this.phase!=='vault')return this.overview();this.error=safeError(e);this.changed(); }
    return this.overview();
  }
  assertRestartable(){
    if(this.busy||this.tools.size||this.quitLock)throw new Error('UPDATE_OPERATION_ACTIVE');
    if(this.uncertainWrite)throw new Error('UPDATE_WRITE_UNCERTAIN');
    if(this.engine&&this.phase!=='vault'||['joining','join-uncertain','joined-preparing'].includes(this.card?.state)||['metadata-pending','join-uncertain'].includes(this.card?.sync.state))throw new Error('UPDATE_JOIN_UNSETTLED');
  }
  async restartForUpdate({confirmEditor}){
    if(this.updateLock)throw new Error('UPDATE_RESTART_LOCKED');
    if(this.updater.status().state!=='available')throw new Error('UPDATE_NOT_READY');
    this.assertRestartable();const engine=this.engine,selected=this.selected;
    this.updateLock=true;this.restartToken=randomBytes(24).toString('hex');
    let drained=false;
    try{
      await this.updater.install({drain:async()=>{
        if(await confirmEditor(this.restartToken)!==true)throw new Error('UPDATE_EDITOR_NOT_READY');
        this.assertRestartable();if(this.engine!==engine||this.selected!==selected)throw new Error('VAULT_CHANGED');
        // Retain the selected owner if the OS does not confirm its exit.
        await this.close({forUpdate:true});drained=true;
      }});
    }catch(error){
      if(drained){this.phase='update-recovery';this.error=safeError(error);}
      else{this.updateLock=false;this.restartToken=null;this.error=safeError(error);}
      this.changed();throw error;
    }
  }
  async quitGracefully({confirmUnknown=async()=>false}={}){
    if(this.updateLock)throw new Error('UPDATE_RESTART_LOCKED');
    if(this.quitLock||this.busy||this.tools.size)throw new Error('QUIT_OPERATION_ACTIVE');
    const engine=this.engine,selected=this.selected;
    this.quitLock=true;
    try{
      const joinUnsettled=['joining','join-uncertain','joined-preparing'].includes(this.card?.state)||['metadata-pending','join-uncertain'].includes(this.card?.sync.state);
      if((this.uncertainWrite||joinUnsettled)&&await confirmUnknown({write:this.uncertainWrite,join:joinUnsettled})!==true)throw new Error('QUIT_CANCELLED');
      if(this.engine!==engine||this.selected!==selected||this.busy||this.tools.size)throw new Error('QUIT_OPERATION_ACTIVE');
      // Quitting does not resolve or replay an unknown write/enrollment. Drain
      // the original process and require its OS exit before the window closes.
      await this.close({forQuit:true});
    }catch(error){this.quitLock=false;throw error;}
  }
  async close({forUpdate=false,forQuit=false}={}) { const before=this.phase,preserve=forUpdate||forQuit;if(!preserve){this.phase='closing';this.changed();}try{if (this.engine) await this.engine.close();this.engine=null;this.phase='closed';this.changed();}
    catch(error){this.phase=preserve?before:'recovery';this.error=safeError(error);this.changed();throw error;} }
}
module.exports = { Controller, statusCard };
