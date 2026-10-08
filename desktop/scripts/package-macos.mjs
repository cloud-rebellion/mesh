// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
import fs from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import {fileURLToPath} from 'node:url';
import {createHash} from 'node:crypto';
import childProcess from 'node:child_process';
import {syncBuiltinESMExports} from 'node:module';
const {spawnSync}=childProcess;
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
if(process.platform!=='darwin')throw new Error('macOS packaging requires macOS.');
const arch=process.argv[2];if(!['arm64','x64'].includes(arch))throw new Error('Choose arm64 or x64 explicitly.');
const electronZipDir=process.argv[3];
if(process.argv.length!==4||!electronZipDir||!path.isAbsolute(electronZipDir))throw new Error('Supply an absolute directory containing the pinned official Electron ZIP.');
const [nodeMajor,nodeMinor]=process.versions.node.split('.').map(Number);
if(nodeMajor<22||nodeMajor===22&&nodeMinor<12)throw new Error('Use the pinned dependency-supported Node toolchain (>=22.12).');
// Packaging invokes Apple's keyless codesign tooling, never a user PATH replacement.
process.env.PATH='/usr/bin:/bin';
const keylessCommands=[],originalSpawn=childProcess.spawn,originalSpawnSync=childProcess.spawnSync;
function recordKeyless(program,args){
  if(program!=='codesign'&&program!=='/usr/bin/codesign')return;
  if(!Array.isArray(args)||args[args.indexOf('--sign')+1]!=='-')throw new Error('Only keyless temporary ad-hoc sealing is permitted.');
  keylessCommands.push({program:'/usr/bin/codesign',argv:[...args]});
}
childProcess.spawn=function(program,args,...rest){recordKeyless(program,args);return originalSpawn.call(this,program,args,...rest);};
childProcess.spawnSync=function(program,args,...rest){recordKeyless(program,args);return originalSpawnSync.call(this,program,args,...rest);};
syncBuiltinESMExports();
// Import after installing the process-local observation/identity refusal boundary.
const {packager}=await import('@electron/packager');
if(typeof packager!=='function')throw new Error('Pinned packager does not expose its reviewed API.');
const {flipFuses,FuseVersion,FuseV1Options}=await import('@electron/fuses');
const {verifyBundle}=await import('../src/identity.cjs');
const binary=path.join(root,'bundle/mesh-core/mesh-desktop-core');
const st=await fs.lstat(binary);if(!st.isFile()||st.isSymbolicLink()||st.size===0||st.size>128*1024*1024)throw new Error('Invalid bundled core.');
// Cross-architecture packaging still requires runnable --version identity on the packaging host.
const identityRun=spawnSync(binary,['--version','--json'],{encoding:'utf8',timeout:5000,maxBuffer:16384,env:{PATH:'/usr/bin:/bin'},shell:false});
if(identityRun.status!==0)throw new Error('Bundled core identity did not run successfully.');
const identity=JSON.parse(identityRun.stdout);
if(JSON.stringify(Object.keys(identity).sort())!==JSON.stringify(['arch','platform','protocol','source','version'])||identity.protocol!==1||identity.platform!=='darwin'||identity.arch!==({x64:'amd64',arm64:'arm64'})[arch]||!/^[a-f0-9]{40}$/.test(identity.source)||!/^[a-zA-Z0-9][a-zA-Z0-9.+_-]{0,63}$/.test(identity.version))throw new Error('Core identity does not match the chosen platform/source.');
const packageJSON=JSON.parse(await fs.readFile(path.join(root,'package.json'),'utf8'));
if(packageJSON.devDependencies.electron!=='44.7.0')throw new Error('Review the pinned official runtime hashes before changing Electron.');
const runtimeHashes={arm64:'e04e411b58a0a14375dd21b0ab4a378fd38930a702e4e20e322fee4849404c0b',x64:'4fe494a7159b10a81e2098dab388719582065e99e604c4510bb117e282213566'};
const zipName=`electron-v44.7.0-darwin-${arch}.zip`,zipSource=path.join(electronZipDir,zipName);
const zipStat=await fs.lstat(zipSource);
if(!zipStat.isFile()||zipStat.isSymbolicLink()||zipStat.size===0||zipStat.size>256*1024*1024)throw new Error('Invalid pinned Electron archive.');
const output=path.join(root,'out',`${packageJSON.version}-${arch}`);
try{await fs.lstat(output);throw new Error('Output already exists; immutable review bundles are never replaced.');}catch(e){if(e.code!=='ENOENT')throw e;}
const staging=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-desktop-package-'));
try{
  const application=path.join(staging,'application'),runtime=path.join(staging,'runtime');await fs.mkdir(application);await fs.mkdir(runtime);
  const pinnedArchive=path.join(runtime,zipName);await fs.copyFile(zipSource,pinnedArchive);
  if(createHash('sha256').update(await fs.readFile(pinnedArchive)).digest('hex')!==runtimeHashes[arch])throw new Error('Pinned Electron archive checksum mismatch.');
  for(const dir of ['src','renderer'])await fs.cp(path.join(root,dir),path.join(application,dir),{recursive:true,dereference:false});
  await fs.writeFile(path.join(application,'package.json'),JSON.stringify({name:packageJSON.name,productName:'Mesh',version:packageJSON.version,private:true,license:'UNLICENSED',main:'src/main.cjs'}));
  const manifest={...identity,sha256:createHash('sha256').update(await fs.readFile(binary)).digest('hex')};
  const core=path.join(staging,'mesh-core');await fs.mkdir(core);await fs.copyFile(binary,path.join(core,'mesh-desktop-core'));await fs.chmod(path.join(core,'mesh-desktop-core'),0o755);await fs.writeFile(path.join(core,'core.json'),JSON.stringify(manifest));
  const packageDirectories=await packager({dir:application,out:output,name:'Mesh',platform:'darwin',arch,electronVersion:packageJSON.devDependencies.electron,electronZipDir:runtime,
    appBundleId:'com.brightinteraction.mesh.desktop',appVersion:packageJSON.version,buildVersion:packageJSON.version,
    asar:true,overwrite:false,prune:true,ignore:[/^\/mesh-core(?:\/|$)/],extraResource:[core],osxSign:false,osxNotarize:false});
  const apps=[];
  for(const directory of packageDirectories){
    const app=path.join(directory,'Mesh.app');const appStat=await fs.lstat(app);if(!appStat.isDirectory()||appStat.isSymbolicLink())throw new Error('Pinned packager returned an invalid application bundle.');apps.push(app);
    await flipFuses(app,{version:FuseVersion.V1,resetAdHocDarwinSignature:true,[FuseV1Options.RunAsNode]:false,
      [FuseV1Options.EnableNodeOptionsEnvironmentVariable]:false,[FuseV1Options.EnableNodeCliInspectArguments]:false,
      [FuseV1Options.OnlyLoadAppFromAsar]:true,[FuseV1Options.EnableEmbeddedAsarIntegrityValidation]:true,
      [FuseV1Options.GrantFileProtocolExtraPrivileges]:false});
    // A deep ad-hoc pass can alter the core's Mach-O seal. Bind the actual packaged
    // bytes, then repair only the outer resource seal before its final verification.
    const packagedCore=path.join(app,'Contents/Resources/mesh-core/mesh-desktop-core');
    const packagedManifest={...identity,sha256:createHash('sha256').update(await fs.readFile(packagedCore)).digest('hex')};
    if(packagedManifest.sha256!==manifest.sha256){
      await fs.writeFile(path.join(app,'Contents/Resources/mesh-core/core.json'),JSON.stringify(packagedManifest));
      const repaired=childProcess.spawnSync('/usr/bin/codesign',['--sign','-','--force','--preserve-metadata=entitlements,requirements,flags,runtime',app],{encoding:'utf8',timeout:30000,maxBuffer:16384});
      if(repaired.status!==0)throw new Error('Temporary outer ad-hoc resource seal failed.');
    }
    const verification=spawnSync('/usr/bin/codesign',['--verify','--deep','--strict',app],{encoding:'utf8',timeout:30000,maxBuffer:16384});
    if(verification.status!==0)throw new Error('Temporary final bundle seal verification failed.');
    const packagedIdentity=await verifyBundle(path.dirname(packagedCore),{platform:'darwin',arch});
    // No app bytes are changed after the final seal; this receipt lives beside it.
    await fs.writeFile(path.join(path.dirname(app),'UNSIGNED-REVIEW-ONLY.json'),JSON.stringify({source:identity.source,core:packagedManifest,input_core_sha256:manifest.sha256,verified_packaged_identity:packagedIdentity.identity,electron:packageJSON.devDependencies.electron,electron_zip_sha256:runtimeHashes[arch],app_version:packageJSON.version,distribution_signed:false,ad_hoc_runtime_seal:true,signer:'-',developer_id:false,key_used:false,notarized:false,updates:'disabled',observed_keyless_commands:keylessCommands,verification:{program:'/usr/bin/codesign',argv:['--verify','--deep','--strict',app],exit:verification.status}}));
  }
  console.log(JSON.stringify({apps,distribution_signed:false,ad_hoc_runtime_seal:true,signer:'-',notarized:false,updates:'disabled'}));
}finally{await fs.rm(staging,{recursive:true,force:true});}
