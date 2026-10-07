import { createHash } from 'node:crypto';
import { zipSync, unzipSync } from 'fflate';

export const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');
export const shippedSources = ['automatic-updates.js', 'bridge.js', 'broker.js', 'client.js', 'connections.js', 'coordinated-updates.js', 'extension.js', 'lifecycle.js', 'update-command.js', 'updates.js', 'view.css', 'view.js'];
export const shippedAssets = ['index.html', 'style.css', 'gl3d.js', 'app.js', 'search.js', 'dashboard.js', 'docs.js', 'shell.js', 'fonts/geist.woff2', 'fonts/jetbrains-mono.woff2'];
// New builds must declare the shared-viewer source release. This is not an
// assertion that the IDE and the connected server have been installed together.
export function releaseMetadata(value) {
  if (!value || typeof value.mesh_release !== 'string' || value.mesh_release !== value.mesh_release.trim() || !/^v(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})$/.test(value.mesh_release) || value.viewer_api !== 1) throw new Error('Invalid Mesh release pairing metadata');
  return value;
}
export function validateArchive(bytes, expected, allowDirty = false) {
  releaseMetadata(expected);
  const files = unzipSync(bytes);
  const names = ['[Content_Types].xml', 'extension.vsixmanifest', 'extension/LICENSE.txt', 'extension/package.json', 'extension/readme.md', 'extension/media/source.json', ...shippedSources.map(n => 'extension/src/' + n), ...shippedAssets.map(n => 'extension/media/' + n)];
  if (JSON.stringify(Object.keys(files).sort()) !== JSON.stringify(names.sort())) throw new Error('VSIX file allowlist mismatch');
  const json = name => JSON.parse(Buffer.from(files[name]).toString());
  const pkg = json('extension/package.json');
  if (pkg.name !== expected.name || pkg.publisher !== expected.publisher || pkg.version !== expected.version || pkg.main !== './src/extension.js') throw new Error('VSIX package identity mismatch');
  const identities = [...Buffer.from(files['extension.vsixmanifest']).toString().matchAll(/<Identity\b([^>]+)>/g)];
  const attributes = Object.fromEntries([...(identities[0]?.[1] || '').matchAll(/(\w+)="([^"]*)"/g)].map(m => [m[1], m[2]]));
  if (identities.length !== 1 || attributes.Id !== expected.name || attributes.Publisher !== expected.publisher || attributes.Version !== expected.version) throw new Error('VSIX installer identity mismatch');
  const source = json('extension/media/source.json');
  if (source.base_commit !== expected.commit || (!allowDirty && source.dirty !== false)) throw new Error('VSIX source provenance mismatch');
  releaseMetadata(source);
  if (source.mesh_release !== expected.mesh_release || source.viewer_api !== expected.viewer_api) throw new Error('VSIX Mesh release pairing mismatch');
  if (JSON.stringify(Object.keys(source.assets).sort()) !== JSON.stringify([...shippedAssets].sort())) throw new Error('VSIX asset inventory mismatch');
  for (const name of shippedAssets) if (sha256(files['extension/media/' + name]) !== source.assets[name]) throw new Error('VSIX asset hash mismatch: ' + name);
  // Compare every content-bearing file to the current reviewed build, not only
  // to an attacker-controlled manifest carried inside the archive.
  for (const [name, content] of Object.entries(expected.contents || {})) {
    if (!files[name] || sha256(files[name]) !== sha256(content)) throw new Error('VSIX source content mismatch: ' + name);
  }
  return files;
}
export function canonicalArchive(files) {
  const ordered = {};
  for (const name of Object.keys(files).sort()) {
    ordered[name] = [files[name], { mtime: new Date(1980, 0, 1, 0, 0, 0), os: 3, attrs: 0o100644 << 16 }];
  }
  return Buffer.from(zipSync(ordered, { level: 9 }));
}
