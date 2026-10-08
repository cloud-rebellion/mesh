// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const { EventEmitter } = require('node:events');
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
class Controller extends EventEmitter {
  constructor({storage,bundle,picker,updater,engineFactory = options=>new Engine(options),appVersion,enrollmentEnabled=false,confirmJoin=async()=>false}) {
    super(); Object.assign(this,{storage,bundle,picker,updater,engineFactory,appVersion});
    this.engine = null; this.selected = null; this.card = null; this.phase = 'start'; this.error = null; this.busy = false;
    this.credentials = new PrivateFileCredentialProvider();
    this.enrollmentEnabled=enrollmentEnabled;this.confirmJoin=confirmJoin;
  }
  overview() { return {phase:this.phase,vaults:this.storage.cards(),selected:this.selected ? {id:this.selected.id,name:this.selected.name}:null,
    status:this.card,error:this.error,app_version:this.appVersion,updates:this.updater.status(),credentials:this.credentials.capabilities(),enrollment:{enabled:this.enrollmentEnabled,reason:this.enrollmentEnabled?'Use an invitation to join the configured Mesh service.':'Native team enrollment is pending verification.'}}; }
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
    try {
      if (action === 'overview') { exact(params,[]); return this.overview(); }
      if (action === 'tool') {
        exact(params,['name','arguments']); if (this.phase !== 'vault' || !this.engine) throw new Error('ENGINE_UNAVAILABLE');
        const engine=this.engine,selected=this.selected;
        try{const result=await engine.request('tool',params);
          if(this.engine!==engine||this.selected!==selected||this.phase!=='vault')throw new Error('VAULT_CHANGED');return result;
        }catch(error){if(this.engine!==engine||this.selected!==selected||this.phase!=='vault')throw new Error('VAULT_CHANGED');throw error;}
      }
      if (this.busy) throw new Error('OPERATION_BUSY');
      this.busy=true;
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
      if (action === 'updates') { exact(params,[]); await this.updater.check(); }
      else throw new Error('FORBIDDEN_ACTION');
    } catch (error) {
      this.error=safeError(error); if (['opening','recovery'].includes(this.phase)) this.phase='recovery';this.changed();throw error;
    } finally { if (action !== 'tool' && action !== 'overview') this.busy=false; }
  }
  async refresh() {
    if (!this.engine || this.busy || this.phase !== 'vault') return this.overview();
    const engine=this.engine,selected=this.selected;
    try { const value=await engine.request('status',{});if(this.engine!==engine||this.selected!==selected||this.phase!=='vault')return this.overview();
      this.card=statusCard(value);this.changed();
    } catch (e) {if(this.engine!==engine||this.selected!==selected||this.phase!=='vault')return this.overview();this.error=safeError(e);this.changed(); }
    return this.overview();
  }
  async close() { this.phase='closing';this.changed();try{if (this.engine) await this.engine.close();this.engine=null;this.phase='closed';}
    catch(error){this.phase='recovery';this.error=safeError(error);this.changed();throw error;} }
}
module.exports = { Controller, statusCard };
