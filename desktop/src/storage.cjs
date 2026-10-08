// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const fs = require('node:fs/promises');
const path = require('node:path');
const { randomUUID } = require('node:crypto');
const { exact, text } = require('./boundaries.cjs');
class Storage {
  constructor(root) { if (!path.isAbsolute(root)) throw new Error('INVALID_STORAGE'); this.root = root; this.records = []; }
  async load() {
    await fs.mkdir(this.root,{ recursive:true,mode:0o700 });
    const stat = await fs.lstat(this.root); if (!stat.isDirectory() || stat.isSymbolicLink() || (stat.mode & 0o077)) throw new Error('INVALID_STORAGE');
    const file = path.join(this.root,'vaults.json');
    try {
      const st = await fs.lstat(file); if (!st.isFile() || st.isSymbolicLink() || st.size > 65536 || (st.mode & 0o077)) throw new Error('INVALID_STORAGE');
      const data = JSON.parse(await fs.readFile(file,'utf8')); exact(data,['version','vaults']);
      if (data.version !== 1 || !Array.isArray(data.vaults) || data.vaults.length > 64) throw new Error('INVALID_STORAGE');
      const ids = new Set();
      for (const record of data.vaults) {
        exact(record,['id','name','path','managed']);
        if (!/^[0-9a-f-]{36}$/.test(record.id) || ids.has(record.id) || !text(record.name,128) || !path.isAbsolute(record.path) || typeof record.managed !== 'boolean') throw new Error('INVALID_STORAGE');
        if (record.managed && record.path !== path.join(this.root,'vaults',record.id)) throw new Error('INVALID_STORAGE');
        ids.add(record.id);
      }
      this.records = data.vaults;
    } catch (e) { if (e.code !== 'ENOENT') throw new Error('INVALID_STORAGE'); }
    return this.cards();
  }
  cards() { return this.records.map(({ id,name,managed })=>({ id,name,managed })); }
  resolve(id) { const r = this.records.find(r=>r.id===id); if (!r) throw new Error('VAULT_NOT_FOUND'); return r; }
  async save() {
    const tmp = path.join(this.root,`vaults-${randomUUID()}.tmp`);
    const h = await fs.open(tmp,'wx',0o600);
    try { await h.writeFile(JSON.stringify({version:1,vaults:this.records})); await h.sync(); } finally { await h.close(); }
    await fs.rename(tmp,path.join(this.root,'vaults.json'));
    const directory=await fs.open(this.root,'r');try{await directory.sync();}finally{await directory.close();}
  }
  async create(name) {
    if (!text(name,128) || this.records.length >= 64) throw new Error('INVALID_REQUEST');
    const id = randomUUID(), location = path.join(this.root,'vaults',id);
    await fs.mkdir(location,{ recursive:true,mode:0o700 });
    const record = {id,name,path:location,managed:true}; this.records.push(record); await this.save(); return record;
  }
  async adopt(selected) {
    // Only the native picker calls this method. No renderer path is accepted.
    if (!path.isAbsolute(selected)) throw new Error('INVALID_STORAGE');
    const location = await fs.realpath(selected); const st = await fs.stat(location);
    if (!st.isDirectory()) throw new Error('INVALID_STORAGE');
    const prior = this.records.find(r=>r.path===location); if (prior) return prior;
    if (this.records.length >= 64) throw new Error('INVALID_STORAGE');
    const record = { id:randomUUID(),name:path.basename(location).slice(0,80) || 'Vault',path:location,managed:false };
    if (!text(record.name,128)) throw new Error('INVALID_STORAGE');
    this.records.push(record); await this.save(); return record;
  }
}
module.exports = { Storage };
