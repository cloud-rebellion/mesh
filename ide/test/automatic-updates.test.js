import {test,expect} from 'bun:test';
import {createRequire} from 'node:module';
import {createHash} from 'node:crypto';
const require=createRequire(import.meta.url);
const {automaticUpdates}=require('../src/automatic-updates');
const {updateCommand}=require('../src/update-command');
const bytes=Buffer.from('fixture');
const info={schema:1,extension:'bright-interaction.mesh-workspace',version:'0.3.5',source_commit:'a'.repeat(40),dirty:false,file:'mesh-workspace-0.3.5.vsix',bytes:bytes.length,sha256:createHash('sha256').update(bytes).digest('hex')};
function setup(patch={}) {
 const calls=[],messages=[];
 const vscode={workspace:{isTrusted:true},Uri:{file:p=>p},commands:{executeCommand:async(...args)=>{calls.push(args);}},window:{showInformationMessage:async(...args)=>{messages.push(args);},showWarningMessage:async()=>{}}};
 const deps={enabled:()=>true,check:async()=>info,download:async()=>bytes,stage:async()=>({file:'/fixture.vsix',cleanup:async()=>calls.push(['cleanup'])}),...patch};
 return {vscode,deps,calls,messages,command:updateCommand(vscode,'0.3.4',deps)};
}
test('automatic update installs verified bytes without a pre-install prompt and leaves activation to the user',async()=>{
 const f=setup();await f.command.runAutomatic();
 expect(f.calls).toEqual([['workbench.extensions.installExtension','/fixture.vsix'],['cleanup']]);
 expect(f.messages).toHaveLength(1);expect(f.messages[0][0]).toContain('installed automatically');
 await f.command.runAutomatic();expect(f.calls).toHaveLength(2);
});
test('disabled, untrusted and revoked automatic settings prevent installation',async()=>{
 const disabled=setup({enabled:()=>false});await disabled.command.runAutomatic();expect(disabled.calls).toEqual([]);
 const untrusted=setup();untrusted.vscode.workspace.isTrusted=false;await untrusted.command.runAutomatic();expect(untrusted.calls).toEqual([]);
 let enabled=true;const changed=setup({enabled:()=>enabled,download:async()=>{enabled=false;return bytes;}});await changed.command.runAutomatic();expect(changed.calls).toEqual([]);
});
test('offline checks stay quiet and the bounded scheduler retries with overlap prevention and disposal',async()=>{
 let timer,interval,enabled=true,calls=0,release;
 const fn=async()=>{calls++;await new Promise(r=>release=r);};
 const scheduler=automaticUpdates(fn,()=>enabled,{setTimeout:(f,ms)=>{timer=f;interval=ms;return 1;},clearTimeout:()=>{timer=undefined;}});
 expect(interval).toBe(30000);const first=timer();await scheduler.check();expect(calls).toBe(1);release();await first;expect(interval).toBe(21600000);
 enabled=false;await timer();expect(calls).toBe(1);scheduler.dispose();expect(timer).toBeUndefined();
 const offline=setup({check:async()=>{throw Error('offline');}});await offline.command.runAutomatic();expect(offline.messages).toEqual([]);expect(offline.calls).toEqual([]);
});

test('an unanswered optional reload notification cannot hold automatic scheduling open',async()=>{
 const f=setup();f.vscode.window.showInformationMessage=()=>new Promise(()=>{});
 await f.command.runAutomatic();expect(f.command.diagnostics().active).toBe(false);expect(f.command.diagnostics().installedVersion).toBe('0.3.5');
});
