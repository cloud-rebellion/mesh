// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
// No channel, signing identity or desktop publisher authority exists yet.
// Keep the future provider behind a capability boundary instead of accepting a renderer URL.
class DisabledUpdater {
  status() { return Object.freeze({state:'disabled',reason:'Signed application updates are not configured in this build.'}); }
  async check() { throw new Error('UPDATES_DISABLED'); }
  async install() { throw new Error('UPDATES_DISABLED'); }
}
function configureUpdater(configuration) {
  if (configuration !== null && configuration !== undefined) throw new Error('UNAPPROVED_UPDATE_CHANNEL');
  return new DisabledUpdater();
}
module.exports = { DisabledUpdater, configureUpdater };
