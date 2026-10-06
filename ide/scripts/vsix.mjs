// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB
import { canonicalArchive, validateArchive } from './archive.mjs';

const xml = value => {
  const text = String(value);
  if (/[\u0000-\u0008\u000b\u000c\u000e-\u001f]/.test(text)) throw new Error('Invalid XML metadata');
  return text.replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&apos;' }[ch]));
};

// This builds the fixed Mesh UI-extension inventory. Unsupported runtime
// payloads must be refused rather than silently omitted from a generic package.
export function installerMetadata(pkg) {
  if (!/^[a-z0-9-]+$/.test(pkg.name) || !/^[a-z0-9-]+$/.test(pkg.publisher) || !/^\d+\.\d+\.\d+$/.test(pkg.version)) throw new Error('Invalid VSIX identity');
  if (pkg.main !== './src/extension.js' || pkg.extensionKind?.length !== 1 || pkg.extensionKind[0] !== 'ui' || !/^\^\d+\.\d+\.\d+$/.test(pkg.engines?.vscode || '')) throw new Error('Unsupported Mesh VSIX runtime');
  for (const key of ['dependencies', 'extensionDependencies', 'extensionPack', 'enabledApiProposals', 'browser', 'icon', 'targetPlatform']) {
    if (pkg[key] && Object.keys(pkg[key]).length) throw new Error('Unsupported Mesh VSIX field: ' + key);
  }
  const property = (id, value) => `<Property Id="${id}" Value="${xml(value)}"/>`;
  const asset = (type, file) => `<Asset Type="${type}" Path="extension/${file}" Addressable="true"/>`;
  return `<?xml version="1.0" encoding="utf-8"?>
<PackageManifest Version="2.0.0" xmlns="http://schemas.microsoft.com/developer/vsx-schema/2011">
<Metadata>
<Identity Id="${xml(pkg.name)}" Version="${xml(pkg.version)}" Publisher="${xml(pkg.publisher)}" Language="en-US"/>
<DisplayName>${xml(pkg.displayName || pkg.name)}</DisplayName>
<Description xml:space="preserve">${xml(pkg.description || '')}</Description>
<Categories>${xml((pkg.categories || []).join(','))}</Categories>
<GalleryFlags>Public</GalleryFlags>
<Properties>
${property('Microsoft.VisualStudio.Code.Engine', pkg.engines.vscode)}
${property('Microsoft.VisualStudio.Code.ExtensionKind', 'ui')}
${property('Microsoft.VisualStudio.Code.ExecutesCode', 'true')}
${property('Microsoft.VisualStudio.Services.GitHubFlavoredMarkdown', 'true')}
${property('Microsoft.VisualStudio.Services.Content.Pricing', 'Free')}
</Properties>
<License>extension/LICENSE.txt</License>
</Metadata>
<Installation><InstallationTarget Id="Microsoft.VisualStudio.Code"/></Installation>
<Dependencies/>
<Assets>
${asset('Microsoft.VisualStudio.Code.Manifest', 'package.json')}
${asset('Microsoft.VisualStudio.Services.Content.Details', 'readme.md')}
${asset('Microsoft.VisualStudio.Services.Content.License', 'LICENSE.txt')}
</Assets>
</PackageManifest>\n`;
}

export function packageArchive(expected, allowDirty = false) {
  const pkg = JSON.parse(Buffer.from(expected.contents['extension/package.json']).toString());
  const files = { ...expected.contents };
  files['extension.vsixmanifest'] = Buffer.from(installerMetadata(pkg));
  const types = { css: 'text/css', html: 'text/html', js: 'application/javascript', json: 'application/json', md: 'text/markdown', txt: 'text/plain', vsixmanifest: 'text/xml', woff2: 'font/woff2' };
  const declarations = Object.entries(types).map(([extension, contentType]) => `<Default Extension="${extension}" ContentType="${contentType}"/>`).join('\n');
  files['[Content_Types].xml'] = Buffer.from(`<?xml version="1.0" encoding="utf-8"?>\n<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">\n${declarations}\n</Types>\n`);
  const bytes = canonicalArchive(files);
  validateArchive(bytes, expected, allowDirty);
  return bytes;
}
