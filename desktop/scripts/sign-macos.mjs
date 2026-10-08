// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Signing preparation only: this module never spawns tools or reads credentials.
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {createHash} from 'node:crypto';
import {createRequire} from 'node:module';
const {version,BUNDLE_ID}=createRequire(import.meta.url)('../src/release-contract.cjs');
export function signingPlan({app,arch,app_version,core_version,source,team_id,identity,inner}){
  if(!path.isAbsolute(app)||path.basename(app)!=='Mesh.app'||/[\u0000-\u001f]/.test(app)||!['arm64','x64'].includes(arch)||!/^[a-f0-9]{40}$/.test(source)||source==='0'.repeat(40)||!/^[A-Z0-9]{10}$/.test(team_id)||identity!==`Developer ID Application: Mesh (${team_id})`||!Array.isArray(inner)||!inner.length||inner.length>128)throw new Error('UNAPPROVED_SIGNING_INPUT');
  version(app_version);version(core_version);const seen=new Set();
  for(const file of inner){if(typeof file!=='string'||!file.startsWith(app+'/Contents/')||file.includes('/../')||file.includes('/./')||file.includes('\\')||seen.has(file)||/[\u0000-\u001f]/.test(file))throw new Error('UNAPPROVED_SIGNING_INPUT');seen.add(file);}
  const core=app+'/Contents/Resources/mesh-core/mesh-desktop-core';if(!seen.has(core))throw new Error('UNAPPROVED_SIGNING_INPUT');
  // A future isolated signer must derive the COMPLETE executable/nested bundle
  // inventory from independently admitted build output, never repository commands.
  const commands=[...inner.map(file=>({phase:'inner',program:'/usr/bin/codesign',argv:['--sign',identity,'--options','runtime','--timestamp',file]})),
    {phase:'bind-core',operation:'hash-signed-core-and-write-core.json',path:core},
    {phase:'outer',program:'/usr/bin/codesign',argv:['--sign',identity,'--options','runtime','--timestamp',app]},
    {phase:'verify',program:'/usr/bin/codesign',argv:['--verify','--deep','--strict',app]},
    {phase:'identity',program:'/usr/bin/codesign',argv:['--display','--verbose=4','--requirements','-',app]},
    {phase:'notarize',operation:'submit-to-separately-approved-notary-provider-and-require-Accepted'},
    {phase:'staple',program:'/usr/bin/xcrun',argv:['stapler','staple',app]},
    {phase:'staple-verify',program:'/usr/bin/xcrun',argv:['stapler','validate',app]},
    {phase:'final-verify',program:'/usr/bin/codesign',argv:['--verify','--deep','--strict',app]},
    {phase:'assess',program:'/usr/sbin/spctl',argv:['--assess','--type','execute','--verbose=4',app]},
    {phase:'zip',program:'/usr/bin/ditto',argv:['-c','-k','--sequesterRsrc','--keepParent',app,app+`.${app_version}-${arch}.zip`]},
    {phase:'freeze',operation:'hash-final-stapled-ZIP-and-admit-write-once-artifact'}];
  return Object.freeze({disabled:true,bundle_id:BUNDLE_ID,arch,app_version,core_version,source,team_id,identity,commands});
}
export function inspectSigningOutput(output,plan){
  if(typeof output!=='string'||Buffer.byteLength(output)>16384||output.includes('Signature=adhoc')||!output.includes('Identifier='+BUNDLE_ID)||!output.includes('TeamIdentifier='+plan.team_id)||!output.includes('Authority='+plan.identity)||!output.includes('Authority=Developer ID Certification Authority')||!output.includes('Authority=Apple Root CA')||!/^Timestamp=.+$/m.test(output)||!/^CodeDirectory .*flags=.*\bruntime\b/m.test(output)||!output.includes('certificate leaf[subject.OU] = "'+plan.team_id+'"'))throw new Error('INVALID_SIGNING_OUTPUT');
  const arch=plan.arch==='x64'?'x86_64':'arm64';if(!output.includes('CodeDirectory v=')||!output.includes(`Format=app bundle with Mach-O thin (${arch})`))throw new Error('INVALID_SIGNING_OUTPUT');return true;
}
// Synthetic command-output harness, deliberately no default executor and no
// keychain profile/notary credentials. This is NOT a signing implementation.
export async function exerciseSyntheticPlan(plan,{fixture,run,bindCore,finalZip}){
  if(fixture!==true||typeof run!=='function'||typeof bindCore!=='function'||typeof finalZip!=='function'||plan.disabled!==true)throw new Error('SIGNING_DISABLED');
  const completed=[];let bound=false,notarized=false,stapled=false;
  for(const step of plan.commands){
    if(step.phase==='bind-core'){const result=await bindCore(step);if(!/^[a-f0-9]{64}$/.test(result.sha256)||result.source!==plan.source||result.version!==plan.core_version)throw new Error('INVALID_SIGNING_OUTPUT');bound=true;}
    else if(step.phase==='freeze'){if(!bound||!notarized||!stapled)throw new Error('INVALID_SIGNING_ORDER');const bytes=await finalZip();if(!Buffer.isBuffer(bytes)||!bytes.length)throw new Error('INVALID_SIGNING_OUTPUT');completed.push({phase:step.phase,sha256:createHash('sha256').update(bytes).digest('hex'),size:bytes.length});continue;}
    else{const result=await run(step);if(result?.status!==0)throw new Error('SIGNING_STEP_FAILED');if(step.phase==='identity')inspectSigningOutput(result.output,plan);if(step.phase==='notarize'){if(result.notary_status!=='Accepted'||typeof result.submission_id!=='string'||!result.submission_id)throw new Error('NOTARIZATION_NOT_ACCEPTED');notarized=true;}if(step.phase==='staple-verify')stapled=true;}
    completed.push({phase:step.phase});
  }
  return{synthetic:true,distribution_signed:false,commands:completed};
}
if(process.argv[1]&&path.resolve(process.argv[1])===fileURLToPath(import.meta.url))throw new Error('SIGNING_DISABLED: no Developer ID executor or notarization account is configured.');
