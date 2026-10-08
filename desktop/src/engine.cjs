// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const { EventEmitter } = require('node:events');
const { spawn } = require('node:child_process');
const { REQUEST_BYTES, RESPONSE_BYTES, exact, object, validateMethod, safeError } = require('./boundaries.cjs');
class Engine extends EventEmitter {
  constructor({ binary, vault, spawnProcess = spawn, timeout = 30000 }) {
    super(); this.binary = binary; this.vault = vault; this.spawnProcess = spawnProcess; this.timeout = timeout;
    this.pending = new Map(); this.sequence = 0; this.state = 'stopped'; this.buffer = Buffer.alloc(0); this.closing = false;
  }
  start() {
    if (this.child || this.closing) throw new Error('ENGINE_UNAVAILABLE');
    this.state = 'starting';
    this.child = this.spawnProcess(this.binary, ['--stdio','--vault',this.vault], {
      shell:false, windowsHide:true, stdio:['pipe','pipe','ignore'], cwd:this.vault,
      env:{ PATH:'/usr/bin:/bin', LANG:'en_US.UTF-8' }
    });
    this.child.stdout.on('data', data => this.receive(data));
    this.child.on('error', () => this.fail('ENGINE_UNAVAILABLE'));
    this.child.on('exit', () => { const expected = this.closing; this.child = null; this.fail(expected ? 'ENGINE_CLOSED' : 'ENGINE_UNAVAILABLE'); });
  }
  receive(data) {
    if (!Buffer.isBuffer(data)) data = Buffer.from(data);
    // Check each incomplete NDJSON line before allocating a combined buffer.
    let at = 0;
    while (at < data.length) {
      const end = data.indexOf(10, at); const part = data.subarray(at, end < 0 ? data.length : end);
      if (this.buffer.length + part.length > RESPONSE_BYTES) return this.fail('INVALID_ENGINE_RESPONSE', true);
      this.buffer = Buffer.concat([this.buffer, part]);
      if (end < 0) return;
      const line = this.buffer; this.buffer = Buffer.alloc(0); at = end + 1;
      if (!line.length) return this.fail('INVALID_ENGINE_RESPONSE', true);
      let value; try { value = JSON.parse(new TextDecoder('utf-8',{fatal:true}).decode(line)); } catch { return this.fail('INVALID_ENGINE_RESPONSE', true); }
      try {
        exact(value, ['protocol','id','result','error'], ['protocol','id']);
        if (value.protocol !== 1 || !Number.isSafeInteger(value.id) || value.id <= 0 || !this.pending.has(value.id) || ('result' in value) === ('error' in value)) throw new Error();
        if ('error' in value) { exact(value.error, ['code','message']); if (typeof value.error.code !== 'string' || !/^[A-Z_]{2,48}$/.test(value.error.code) || typeof value.error.message !== 'string') throw new Error(); }
      } catch { return this.fail('INVALID_ENGINE_RESPONSE', true); }
      const pending = this.pending.get(value.id); this.pending.delete(value.id); clearTimeout(pending.timer);
      if ('error' in value) pending.reject(new Error(value.error.code)); else pending.resolve(value.result);
    }
  }
  request(method, params = {}, timeout = this.timeout) {
    try { validateMethod(method, params); } catch (e) { return Promise.reject(e); }
    if (!this.child || this.closing && method !== 'close' || this.pending.size >= 32 || this.sequence >= Number.MAX_SAFE_INTEGER) return Promise.reject(new Error('ENGINE_UNAVAILABLE'));
    const id = ++this.sequence;
    const wire = Buffer.from(JSON.stringify({ protocol:1,id,method,params }) + '\n');
    if (wire.length > REQUEST_BYTES) return Promise.reject(new Error('REQUEST_TOO_LARGE'));
    return new Promise((resolve,reject) => {
      const timer = setTimeout(() => { this.pending.delete(id); reject(new Error('ENGINE_TIMEOUT')); this.fail('ENGINE_TIMEOUT',true); }, timeout);
      this.pending.set(id,{ resolve,reject,timer });
      this.child.stdin.write(wire, e => { if (e) this.fail('ENGINE_UNAVAILABLE',true); });
    });
  }
  fail(code, terminate = false) {
    for (const p of this.pending.values()) { clearTimeout(p.timer); p.reject(new Error(code)); }
    this.pending.clear(); this.buffer = Buffer.alloc(0); this.state = this.closing ? 'stopped' : 'failed';
    if (terminate && this.child) this.child.kill('SIGKILL');
    this.emit('state', { state:this.state, error:safeError(new Error(code)) });
  }
  async close() {
    if(this.closePromise)return this.closePromise;
    this.closePromise=this.stop();try{return await this.closePromise;}finally{this.closePromise=null;}
  }
  async stop() {
    const child = this.child;
    if (!child) return;
    const wasClosing=this.closing;this.closing=true;
    const exited=timeout=>new Promise(resolve=>{
      if(this.child!==child)return resolve(true);
      const done=()=>{clearTimeout(timer);resolve(true);};
      const timer=setTimeout(()=>{child.removeListener('exit',done);resolve(false);},timeout);child.once('exit',done);
    });
    if(!wasClosing){try { await this.request('close',{},3000); } catch { /* Never replay an uncertain write. */ }}
    if(!await exited(this.closeWait??3000)){child.kill('SIGKILL');if(!await exited(this.killWait??2000))throw new Error('ENGINE_EXIT_UNCERTAIN');}
    this.state = 'stopped';
  }
}
module.exports = { Engine };
