'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs/promises'),os=require('node:os'),path=require('node:path'),{createHash}=require('node:crypto');
const {parseRelease,validateArchive,crc32,BUNDLE_ID,compareVersions}=require('../src/release-contract.cjs');
const sha=b=>createHash('sha256').update(b).digest('hex');
function zip(files,deflated=false){const locals=[],central=[];let offset=0;for(const [name,value,mode=0x81ed]of files){const data=Buffer.from(value),packed=deflated?require('node:zlib').deflateRawSync(data):data,n=Buffer.from(name),l=Buffer.alloc(30);l.writeUInt32LE(0x04034b50);l.writeUInt16LE(20,4);l.writeUInt32LE(crc32(data),14);l.writeUInt16LE(deflated?8:0,8);l.writeUInt32LE(packed.length,18);l.writeUInt32LE(data.length,22);l.writeUInt16LE(n.length,26);locals.push(l,n,packed);const c=Buffer.alloc(46);c.writeUInt32LE(0x02014b50);c.writeUInt16LE(0x0314,4);c.writeUInt16LE(20,6);c.writeUInt32LE(crc32(data),16);c.writeUInt16LE(deflated?8:0,10);c.writeUInt32LE(packed.length,20);c.writeUInt32LE(data.length,24);c.writeUInt16LE(n.length,28);c.writeUInt32LE((mode<<16)>>>0,38);c.writeUInt32LE(offset,42);central.push(c,n);offset+=30+n.length+packed.length;}const cb=Buffer.concat(central),end=Buffer.alloc(22);end.writeUInt32LE(0x06054b50);end.writeUInt16LE(files.length,8);end.writeUInt16LE(files.length,10);end.writeUInt32LE(cb.length,12);end.writeUInt32LE(offset,16);return Buffer.concat([...locals,cb,end]);}
const installed={arch:'arm64',app_version:'0.1.0',team_id:'FIXTURE123'};
function release(archive){return{schema:1,product:'mesh-desktop',channel:'stable',bundle_id:BUNDLE_ID,platform:'darwin',arch:'arm64',app_version:'0.1.1',core:{protocol:1,version:'0.42.16',source:'a'.repeat(40),arch:'arm64'},artifact:{url:'https://mesh.brightinteraction.com/desktop/releases/0.1.1/Mesh-0.1.1-arm64.zip',size:archive.length,sha256:sha(archive)},signing:{team_id:'FIXTURE123',notarized:true},provenance:{source:'a'.repeat(40),checks_sha256:'b'.repeat(64),approval_sha256:'c'.repeat(64)}};}
const core=Buffer.alloc(40);core.writeUInt32LE(0xfeedfacf);core.writeUInt32LE(0x0100000c,4);
function files(){return[['Mesh.app/','',0x41ed],['Mesh.app/Contents/Info.plist',`<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>${BUNDLE_ID}</string><key>CFBundleShortVersionString</key><string>0.1.1</string><key>CFBundleVersion</key><string>0.1.1</string><key>ElectronSquirrelPreventDowngrades</key><true/></dict></plist>`],['Mesh.app/Contents/Resources/mesh-core/core.json',JSON.stringify({protocol:1,version:'0.42.16',source:'a'.repeat(40),platform:'darwin',arch:'arm64',sha256:sha(core)})],['Mesh.app/Contents/Resources/mesh-core/mesh-desktop-core',core],['Mesh.app/Contents/MacOS/Mesh',core],['Mesh.app/Contents/Frameworks/A/Versions/A/data','fixture'],['Mesh.app/Contents/Frameworks/A/Versions/Current','A',0xa1ff],['Mesh.app/Contents/Frameworks/A/data','Versions/Current/data',0xa1ff]];}
async function check(archive,r=release(archive),limits){const d=await fs.mkdtemp(path.join(os.tmpdir(),'mesh-release-contract-')),file=path.join(d,'fixture.zip');let h;try{await fs.writeFile(file,archive);h=await fs.open(file,'r');return await validateArchive(h,r,limits);}finally{await h?.close();await fs.rm(d,{recursive:true,force:true});}}
test('strict descriptor binds product, architecture, source, signing team, bounded immutable version slot and fixed destination',()=>{
 const valid=release(zip(files()));assert.equal(parseRelease(Buffer.from(JSON.stringify(valid)),installed).core.source,'a'.repeat(40));
 for(const mutate of [r=>r.product='stage',r=>r.arch='x64',r=>r.core.arch='amd64',r=>r.core.source='0'.repeat(40),r=>r.provenance.source='d'.repeat(40),r=>r.artifact.sha256='bad',r=>r.artifact.size=Number.MAX_VALUE,r=>r.signing.notarized=false,r=>r.signing.team_id='OTHER12345',r=>r.app_version='0.01.1',r=>r.artifact.url='https://evil.invalid/update.zip',r=>r.artifact.url+='?invite=secret',r=>r.extra=true]){const r=structuredClone(valid);mutate(r);assert.throws(()=>parseRelease(Buffer.from(JSON.stringify(r)),installed));}
 assert.throws(()=>parseRelease(Buffer.alloc(65537),installed));assert.throws(()=>parseRelease(Buffer.from('{}'),installed));assert.throws(()=>parseRelease(Buffer.from([0xff]),installed));
 assert.throws(()=>parseRelease(Buffer.from(JSON.stringify(valid)),{...installed,app_version:'0.1.1'}),/VERSION_NOT_NEW/);
 assert.throws(()=>parseRelease(Buffer.from(JSON.stringify(valid)),{...installed,slots:{'0.1.1':{sha256:'d'.repeat(64),size:valid.artifact.size,source:valid.core.source,arch:'arm64'}}}),/COLLISION/);
 assert.equal(compareVersions('0.2.0','0.10.0'),-1);
});
test('archive admits complete CRC-read regular source and bounded internal framework symlink chains',async()=>{
 const b=zip(files()),r=await check(b);assert.equal(r.entries,8);assert.equal(r.core.source,'a'.repeat(40));
});
test('archive refuses traversal, normalized aliases, symlink escapes/cycles, special files and decompression bounds before staging',async()=>{
 for(const extra of [['Mesh.app/../outside','x'],['/Mesh.app/a','x'],['Mesh.app/Contents/./alias','x'],['Mesh.app/Contents/Resources/mesh-core/core.json','duplicate'],['Mesh.app/Contents/escape','../../../../outside',0xa1ff],['Mesh.app/Contents/loop','loop',0xa1ff],['Mesh.app/Contents/fifo','x',0x11ed]])await assert.rejects(check(zip([...files(),extra])));
 await assert.rejects(check(zip(files()),undefined,{archive:1024*1024,expanded:1,entry:1024*1024,entries:4096}));
 await assert.rejects(check(zip(files()),undefined,{archive:1024*1024,expanded:1024*1024,entry:1024*1024,entries:2}));
 const damaged=zip(files());damaged[100]^=1;await assert.rejects(check(damaged));
 const wrong=files();wrong[3][1]=Buffer.from('CRC-valid changed executable');await assert.rejects(check(zip(wrong)),/SOURCE_MISMATCH/);
 const unsupported=files();unsupported[1][1]=unsupported[1][1].replace('<true/>','<false/>');await assert.rejects(check(zip(unsupported)),/SOURCE_MISMATCH/);
});

test('canonical plist identity belongs to the root dict and cannot be supplied by nested, duplicate or malformed XML',()=>{
 const{parsePlist}=require('../src/release-contract.cjs'),good='<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.brightinteraction.mesh.desktop</string></dict></plist>';
 assert.equal(parsePlist(Buffer.from(good)).CFBundleIdentifier,'com.brightinteraction.mesh.desktop');
 for(const bad of [good+'<',good.replace('</dict>','<key>CFBundleIdentifier</key><string>other</string></dict>'),good.replace('com.brightinteraction.mesh.desktop','&unknown;'),good.replace('<dict>','<!ENTITY evil "x"><dict>'),'not XML at all'])assert.throws(()=>parsePlist(Buffer.from(bad)));
});
test('same-root case aliases and CRC-valid wrong executable architecture cannot pass complete archive admission',async()=>{
 await assert.rejects(check(zip([...files(),['Mesh.app/Contents/MacOS/mesh',core]])));
 const f=files(),wrong=Buffer.from(core);wrong.writeUInt32LE(0x01000007,4);f[3][1]=wrong;const manifest=JSON.parse(f[2][1]);manifest.sha256=sha(wrong);f[2][1]=JSON.stringify(manifest);await assert.rejects(check(zip(f)),/ARCH_MISMATCH/);
});

test('deflated archives are fully consumed, and a forged tiny expansion declaration cannot hide a compressed payload',async()=>{
 await check(zip(files(),true));const bytes=zip([...files(),['Mesh.app/Contents/bounded-bomb',Buffer.alloc(1024*1024,65)]],true),end=bytes.length-22,central=bytes.readUInt32LE(end+16);let at=central;
 while(at<end){const nl=bytes.readUInt16LE(at+28),el=bytes.readUInt16LE(at+30),cl=bytes.readUInt16LE(at+32);if(bytes.subarray(at+46,at+46+nl).toString()==='Mesh.app/Contents/bounded-bomb'){bytes.writeUInt32LE(1,at+24);bytes.writeUInt32LE(1,bytes.readUInt32LE(at+42)+22);break;}at+=46+nl+el+cl;}
 await assert.rejects(check(bytes),/INVALID_UPDATE_ARCHIVE/);
});
test('ZIP filename interpretation extensions are refused instead of accepting an alternate Unicode extraction path',async()=>{
 const bytes=zip(files()),end=bytes.length-22,at=bytes.readUInt32LE(end+16),nl=bytes.readUInt16LE(at+28),extension=Buffer.from([0x75,0x70,0x00,0x00]),patched=Buffer.concat([bytes.subarray(0,at+46+nl),extension,bytes.subarray(at+46+nl)]);patched.writeUInt16LE(4,at+30);patched.writeUInt32LE(bytes.readUInt32LE(end+12)+4,patched.length-22+12);await assert.rejects(check(patched),/INVALID_UPDATE_ARCHIVE/);
});

test('parent steps resolve after symlinks and cannot briefly escape the app root',async()=>{
 const extras=[['Mesh.app/Contents/Resources/root','../../Contents',0xa1ff],['Mesh.app/Contents/outside','innocent in-root lexical target'],['Mesh.app/Contents/Resources/escape','root/../../outside',0xa1ff]];
 await assert.rejects(check(zip([...files(),...extras])),/INVALID_UPDATE_ARCHIVE/);
 // Parent traversal is legitimate when actual resolution remains inside the app.
 await check(zip([...files(),['Mesh.app/Contents/Resources/root','../../Contents',0xa1ff],['Mesh.app/Contents/Resources/safe','root/../Contents/Info.plist',0xa1ff]]));
});
test('case aliases and symlink descendants are refused across every implicit parent regardless of ZIP order',async()=>{
 const extras=[['Mesh.app/Contents/Resources/up','../../Contents',0xa1ff],['Mesh.app/Contents/resources/UP/Info.plist','would replace the real root plist on a case-insensitive volume']];
 for(const ordered of [extras,[...extras].reverse()])await assert.rejects(check(zip([...files(),...ordered])),/INVALID_UPDATE_ARCHIVE/);
 for(const extras of [[['Mesh.app/Contents/Resources/up','../../Contents',0xa1ff],['Mesh.app/Contents/Resources/up/other','through symlink']], [['Mesh.app/Contents/Resources/implicit/file','regular'],['Mesh.app/Contents/Resources/Implicit/other','case alias']]])await assert.rejects(check(zip([...files(),...extras])),/INVALID_UPDATE_ARCHIVE/);
});

test('ZIP name/depth and implicit namespace bounds refuse before unbounded prefix construction',async()=>{
 const prefix='Mesh.app/Contents/Resources/';
 await assert.rejects(check(zip([...files(),[prefix+'a'.repeat(1025-prefix.length),'bounded fixture']])),/INVALID_UPDATE_ARCHIVE/);
 await assert.rejects(check(zip([...files(),['Mesh.app/'+Array(63).fill('a').join('/')+'/z','bounded fixture']])),/INVALID_UPDATE_ARCHIVE/);
 await assert.rejects(check(zip(files()),undefined,{namespaceNodes:6,archive:1024*1024,expanded:1024*1024,entry:1024*1024,entries:4096}),/INVALID_UPDATE_ARCHIVE/);
 await check(zip([...files(),[prefix+'a'.repeat(1024-prefix.length),'bounded fixture']]));
 await check(zip([...files(),['Mesh.app/'+Array(62).fill('a').join('/')+'/z','bounded fixture']]));
});
