// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
// Credentials stay inside the engine's existing private .mesh/credentials store.
// This seam deliberately cannot read/export a credential to JavaScript or duplicate it.
class PrivateFileCredentialProvider {
  capabilities() { return Object.freeze({ provider:'engine-private-file', osProtected:false, exportable:false }); }
  engineConfiguration() { return Object.freeze({ mode:'private-file' }); }
}
class UnconfiguredOSCredentialProvider {
  capabilities() { return Object.freeze({ provider:'unconfigured', osProtected:false, exportable:false }); }
  engineConfiguration() { throw new Error('CREDENTIAL_PROVIDER_UNAVAILABLE'); }
}
module.exports = { PrivateFileCredentialProvider, UnconfiguredOSCredentialProvider };
