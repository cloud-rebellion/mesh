'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs/promises'),os=require('node:os'),path=require('node:path'),{createHash}=require('node:crypto');
const {verifyBundle}=require('../src/identity.cjs');
test('actual fixture identity is admitted only after immutable binary hash/platform/source fence',async()=>{
  const dir=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-native-identity-'));
  try{
    const binary=path.join(dir,'mesh-desktop-core'),fixture=path.join(__dirname,'fixtures/core.cjs');
    await fs.writeFile(binary,`#!/bin/sh\nexec '${process.execPath}' '${fixture}' "$@"\n`,{mode:0o700});
    const manifest={protocol:1,version:'0.0.0',source:'a'.repeat(40),platform:'darwin',arch:'arm64',sha256:createHash('sha256').update(await fs.readFile(binary)).digest('hex')};
    await fs.writeFile(path.join(dir,'core.json'),JSON.stringify(manifest));
    assert.equal((await verifyBundle(dir,{platform:'darwin',arch:'arm64'})).identity.source,manifest.source);
    await assert.rejects(verifyBundle(dir,{platform:'darwin',arch:'x64'}),/INVALID_CORE_BUNDLE/);
    await fs.appendFile(binary,'# changed');await assert.rejects(verifyBundle(dir,{platform:'darwin',arch:'arm64'}),/INVALID_CORE_BUNDLE/);
    await fs.unlink(binary);await fs.symlink(fixture,binary);await assert.rejects(verifyBundle(dir,{platform:'darwin',arch:'arm64'}),/INVALID_CORE_BUNDLE/);
  }finally{await fs.rm(dir,{recursive:true,force:true});}
});
