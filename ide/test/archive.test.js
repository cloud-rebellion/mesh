// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
import { test, expect } from 'bun:test';
import { zipSync } from 'fflate';
import { mkdtemp, mkdir, writeFile, readFile, copyFile, rm } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { canonicalArchive, validateArchive, sha256, shippedAssets, shippedSources } from '../scripts/archive.mjs';
import { releasePlan } from '../scripts/release-plan.mjs';
import { installerMetadata, packageArchive } from '../scripts/vsix.mjs';

function fixture() {
  const bytes = value => Buffer.from(typeof value === 'string' ? value : JSON.stringify(value));
  const expected = { name: 'mesh-workspace', publisher: 'bright-interaction', version: '0.2.1', commit: 'a'.repeat(40), mesh_release: 'v0.41.7', viewer_api: 1, contents: {} };
  const files = { '[Content_Types].xml': bytes('<Types/>'), 'extension.vsixmanifest': bytes('<PackageManifest><Identity Id="mesh-workspace" Publisher="bright-interaction" Version="0.2.1" /></PackageManifest>'), 'extension/LICENSE.txt': bytes('license'), 'extension/readme.md': bytes('readme'), 'extension/package.json': bytes({ ...expected, main: './src/extension.js' }) };
  const assets = {};
  for (const name of shippedAssets) { files['extension/media/' + name] = bytes(name); assets[name] = sha256(bytes(name)); }
  for (const name of shippedSources) files['extension/src/' + name] = bytes(name);
  files['extension/media/source.json'] = bytes({ base_commit: expected.commit, dirty: false, mesh_release: expected.mesh_release, viewer_api: expected.viewer_api, assets });
  for (const [name, content] of Object.entries(files)) if (name.startsWith('extension/') && name !== 'extension.vsixmanifest') expected.contents[name] = content;
  return { files, expected };
}

test('fixed VSIX builder carries the supported engine and installer assets without unshipped payloads', () => {
  const { expected } = fixture();
  const pkg = { name: expected.name, publisher: expected.publisher, version: expected.version, main: './src/extension.js', engines: { vscode: '^1.96.0' }, extensionKind: ['ui'], displayName: 'Mesh <test> & review', description: 'A "quoted" description', categories: ['Other'] };
  expected.contents['extension/package.json'] = Buffer.from(JSON.stringify(pkg));
  const bytes = packageArchive(expected);
  const files = validateArchive(bytes, expected);
  const manifest = Buffer.from(files['extension.vsixmanifest']).toString();
  expect(manifest).toContain('Microsoft.VisualStudio.Code.Engine" Value="^1.96.0"');
  expect(manifest).toContain('Microsoft.VisualStudio.Code.Manifest" Path="extension/package.json"');
  expect(manifest).toContain('Mesh &lt;test&gt; &amp; review');
  expect(manifest).toContain('&quot;quoted&quot;');
  expect(Buffer.from(files['[Content_Types].xml']).toString()).toContain('Extension="woff2" ContentType="font/woff2"');
  expect(packageArchive(expected).equals(bytes)).toBe(true);
  for (const patch of [{ main: 'other.js' }, { extensionKind: ['workspace'] }, { dependencies: { unsupported: '1' } }, { extensionDependencies: ['other.extension'] }, { browser: 'web.js' }, { icon: 'icon.png' }, { enabledApiProposals: ['unsafe'] }, { displayName: 'invalid\u0000xml' }]) expect(() => installerMetadata({ ...pkg, ...patch })).toThrow();
});

test('release handoff validates the exact clean bundle and emits draft-only arguments without execution', () => {
  const { files, expected } = fixture();
  const bytes = canonicalArchive(files);
  const info = { schema: 1, extension: 'bright-interaction.mesh-workspace', version: expected.version, source_commit: expected.commit, dirty: false, mesh_release: expected.mesh_release, viewer_api: expected.viewer_api, file: `mesh-workspace-${expected.version}.vsix`, bytes: bytes.length, sha256: sha256(bytes) };
  const sums = `${info.sha256}  ${info.file}\n`;
  const plan = releasePlan(info, bytes, sums, expected);
  expect(plan.repository).toBe('cloud-rebellion/mesh'); expect(plan.tag).toBe('ide-v0.2.1');
  expect(plan.mesh_release).toBe('v0.41.7'); expect(plan.viewer_api).toBe(1);
  expect(plan.prerequisites.join('\n')).toContain('paired core release v0.41.7 and its public tag');
  expect(plan.prerequisites.join('\n')).toContain('obtain approval for paired publication');
  expect(plan.draft_command.join(' ')).toContain('Does not upgrade the Mesh binary or connected server');
  expect(plan.draft_command).toContain('--draft'); expect(plan.draft_command).toContain('--verify-tag'); expect(plan.draft_command).toContain('--latest=false');
  expect(plan.draft_command).not.toContain('--clobber'); expect(plan.draft_command).not.toContain('--target');
  for (const patch of [{ dirty: true }, { source_commit: 'b'.repeat(40) }, { sha256: '0'.repeat(64) }, { file: '../bad.vsix' }, { mesh_release: 'v0.41.8' }, { mesh_release: undefined }, { viewer_api: undefined }, { mesh_release: undefined, viewer_api: undefined }, { viewer_api: 2 }]) expect(() => releasePlan({ ...info, ...patch }, bytes, sums, expected)).toThrow();
  expect(() => releasePlan(info, bytes, 'wrong sums', expected)).toThrow();
  files['extension/src/extension.js'] = Buffer.from('different runtime');
  const changed = canonicalArchive(files), tampered = { ...info, bytes: changed.length, sha256: sha256(changed) };
  expect(() => releasePlan(tampered, changed, `${tampered.sha256}  ${tampered.file}\n`, expected)).toThrow('source content');
});

test('canonical VSIX ignores original order and ZIP timestamps', () => {
  const { files, expected } = fixture();
  const reversed = Object.fromEntries(Object.entries(files).reverse());
  const first = canonicalArchive(validateArchive(zipSync(files, { mtime: new Date(2020, 1, 2) }), expected));
  const second = canonicalArchive(validateArchive(zipSync(reversed, { mtime: new Date(2025, 7, 8) }), expected));
  expect(first.equals(second)).toBe(true);
  expect(Object.keys(validateArchive(first, expected))).toHaveLength(27);
});

test('archive gate rejects missing, unexpected and traversal paths', () => {
  for (const name of ['extension/.env', 'extension/node_modules/x.js', 'extension/scripts/package.mjs', '../escape']) {
    const { files, expected } = fixture(); files[name] = Buffer.from('unshipped');
    expect(() => validateArchive(zipSync(files), expected)).toThrow('allowlist');
  }
  const { files, expected } = fixture(); delete files['extension/src/lifecycle.js'];
  expect(() => validateArchive(zipSync(files), expected)).toThrow('allowlist');
});

test('archive gate rejects dirty, stale and mismatched package identities', () => {
  for (const change of [{ dirty: true }, { base_commit: 'b'.repeat(40) }]) {
    const { files, expected } = fixture();
    const source = JSON.parse(files['extension/media/source.json']);
    files['extension/media/source.json'] = Buffer.from(JSON.stringify({ ...source, ...change }));
    expect(() => validateArchive(zipSync(files), expected)).toThrow('provenance');
  }
  const { files, expected } = fixture();
  const pkg = JSON.parse(files['extension/package.json']);
  files['extension/package.json'] = Buffer.from(JSON.stringify({ ...pkg, version: '9.0.0' }));
  expect(() => validateArchive(zipSync(files), expected)).toThrow('identity');
});

test('archive gate requires stable paired source metadata matching the reviewed VERSION', () => {
  for (const patch of [{ mesh_release: undefined }, { viewer_api: undefined }, { mesh_release: 'v0.41.8' }, { mesh_release: '0.41.7' }, { mesh_release: 'v00.41.7' }, { mesh_release: 'v0.41.7-rc.1' }, { mesh_release: 'v0.41.7\n' }, { viewer_api: 2 }, { viewer_api: '1' }]) {
    const { files, expected } = fixture();
    const source = JSON.parse(files['extension/media/source.json']);
    files['extension/media/source.json'] = Buffer.from(JSON.stringify({ ...source, ...patch }));
    expect(() => validateArchive(zipSync(files), expected)).toThrow('pairing');
  }
  const { files, expected } = fixture();
  expect(() => validateArchive(zipSync(files), { ...expected, mesh_release: 'v0.41.8' })).toThrow('pairing');
  expect(() => validateArchive(zipSync(files), { ...expected, viewer_api: undefined })).toThrow('pairing');
});

test('release gate refuses an archived pairing contradicting its manifest even with updated hashes', () => {
  const { files, expected } = fixture();
  const source = JSON.parse(files['extension/media/source.json']);
  files['extension/media/source.json'] = Buffer.from(JSON.stringify({ ...source, mesh_release: 'v0.41.8' }));
  const bytes = canonicalArchive(files);
  const info = { schema: 1, extension: 'bright-interaction.mesh-workspace', version: expected.version, source_commit: expected.commit, dirty: false, mesh_release: expected.mesh_release, viewer_api: expected.viewer_api, file: `mesh-workspace-${expected.version}.vsix`, bytes: bytes.length, sha256: sha256(bytes) };
  expect(() => releasePlan(info, bytes, `${info.sha256}  ${info.file}\n`, expected)).toThrow('pairing');
});

test('build stamps paired release metadata and includes core VERSION in dirty checks', async () => {
  const temp = await mkdtemp(path.join(tmpdir(), 'mesh-ide-source-test-'));
  const root = path.join(temp, 'mesh/ide');
  const git = args => execFileSync('git', args, { cwd: temp, encoding: 'utf8', stdio: 'pipe' });
  try {
    await mkdir(path.join(root, 'scripts'), { recursive: true });
    await copyFile(new URL('../scripts/build.mjs', import.meta.url), path.join(root, 'scripts/build.mjs'));
    await writeFile(path.join(root, '.gitignore'), 'media/\nLICENSE\n');
    await writeFile(path.join(root, '../VERSION'), 'v0.41.7\n');
    await writeFile(path.join(root, '../LICENSE'), 'license');
    for (const name of shippedAssets) {
      const asset = path.join(root, '../internal/web/assets', name);
      await mkdir(path.dirname(asset), { recursive: true });
      await writeFile(asset, name);
    }
    git(['init', '--quiet']); git(['add', '.']);
    git(['-c', 'user.name=Mesh fixture', '-c', 'user.email=fixture@example.invalid', '-c', 'commit.gpgsign=false', '-c', 'core.hooksPath=/dev/null', 'commit', '--quiet', '-m', 'fixture']);
    const build = () => execFileSync(process.execPath, ['scripts/build.mjs'], { cwd: root, stdio: 'pipe' });
    const source = async () => JSON.parse(await readFile(path.join(root, 'media/source.json'), 'utf8'));
    build();
    expect(await source()).toMatchObject({ base_commit: git(['rev-parse', 'HEAD']).trim(), dirty: false, mesh_release: 'v0.41.7', viewer_api: 1 });
    await writeFile(path.join(root, '../VERSION'), 'v0.41.8\n');
    build();
    expect(await source()).toMatchObject({ dirty: true, mesh_release: 'v0.41.8', viewer_api: 1 });
    await writeFile(path.join(root, '../VERSION'), 'v0.41.8-rc.1\n');
    expect(build).toThrow();
  } finally {
    await rm(temp, { recursive: true, force: true }); // exact fixture-created directory
  }
});

test('a forged self-consistent asset manifest cannot replace reviewed source', () => {
  const { files, expected } = fixture();
  files['extension/media/app.js'] = Buffer.from('tampered');
  expect(() => validateArchive(zipSync(files), expected)).toThrow('asset hash');
  const source = JSON.parse(files['extension/media/source.json']);
  source.assets['app.js'] = sha256(files['extension/media/app.js']);
  files['extension/media/source.json'] = Buffer.from(JSON.stringify(source));
  expect(() => validateArchive(zipSync(files), expected)).toThrow('source content');
});

test('installer manifest must match the extension identity, not only package.json', () => {
  const { files, expected } = fixture();
  files['extension.vsixmanifest'] = Buffer.from('<PackageManifest><Identity Id="different" Publisher="bright-interaction" Version="0.2.1" /></PackageManifest>');
  expect(() => validateArchive(zipSync(files), expected)).toThrow('installer identity');
});

test('runtime, commands, license and documentation are checked against build input', () => {
  for (const name of ['extension/src/extension.js', 'extension/LICENSE.txt', 'extension/readme.md']) {
    const { files, expected } = fixture(); files[name] = Buffer.from('changed');
    expect(() => validateArchive(zipSync(files), expected)).toThrow('source content');
  }
  const { files, expected } = fixture();
  const pkg = JSON.parse(files['extension/package.json']); pkg.contributes = { commands: [{ command: 'unexpected' }] };
  files['extension/package.json'] = Buffer.from(JSON.stringify(pkg));
  expect(() => validateArchive(zipSync(files), expected)).toThrow('source content');
});
