// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const fs = require('node:fs/promises');
const path = require('node:path');
const { createHash } = require('node:crypto');
const { spawn } = require('node:child_process');
const { exact } = require('./boundaries.cjs');
async function verifyBundle(resources, {platform = process.platform,arch = process.arch,spawnProcess = spawn} = {}) {
  arch = ({x64:'amd64',ia32:'386'})[arch] || arch;
  const manifestPath = path.join(resources,'core.json'), binary = path.join(resources,'mesh-desktop-core');
  const mstat = await fs.lstat(manifestPath), bstat = await fs.lstat(binary);
  if (!mstat.isFile() || mstat.isSymbolicLink() || mstat.size > 16384 || !bstat.isFile() || bstat.isSymbolicLink() || bstat.size > 128 * 1024 * 1024 || bstat.size === 0) throw new Error('INVALID_CORE_BUNDLE');
  const manifest = JSON.parse(await fs.readFile(manifestPath,'utf8'));
  exact(manifest,['protocol','version','source','platform','arch','sha256']);
  if (manifest.protocol !== 1 || !/^[a-zA-Z0-9][a-zA-Z0-9.+_-]{0,63}$/.test(manifest.version) || !/^[a-f0-9]{40}$/.test(manifest.source) || !/^[a-f0-9]{64}$/.test(manifest.sha256) || manifest.platform !== platform || manifest.arch !== arch) throw new Error('INVALID_CORE_BUNDLE');
  const hash = createHash('sha256').update(await fs.readFile(binary)).digest('hex');
  if (hash !== manifest.sha256) throw new Error('INVALID_CORE_BUNDLE');
  // Identity runs only after the packaged hash fence. No vault or credential is opened.
  const reported = await new Promise((resolve,reject) => {
    const child = spawnProcess(binary,['--version','--json'],{shell:false,stdio:['ignore','pipe','ignore'],env:{PATH:'/usr/bin:/bin',LANG:'en_US.UTF-8'}});
    let data = Buffer.alloc(0), settled = false;
    const fail = () => { if (!settled) { settled=true; clearTimeout(timer); child.kill('SIGKILL'); reject(new Error('INVALID_CORE_BUNDLE')); } };
    const timer = setTimeout(fail,5000);
    child.stdout.on('data',chunk=>{ if (data.length + chunk.length > 16384) return fail(); data = Buffer.concat([data,chunk]); });
    child.once('error',fail);
    child.once('exit',code=>{ if (settled) return; settled=true; clearTimeout(timer); if (code !== 0) return reject(new Error('INVALID_CORE_BUNDLE')); try { resolve(JSON.parse(data.toString('utf8'))); } catch { reject(new Error('INVALID_CORE_BUNDLE')); } });
  });
  exact(reported,['protocol','version','source','platform','arch']);
  for (const k of Object.keys(reported)) if (reported[k] !== manifest[k]) throw new Error('INVALID_CORE_BUNDLE');
  return Object.freeze({binary,identity:reported});
}
module.exports = { verifyBundle };
