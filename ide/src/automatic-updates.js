// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
// The first check runs after startup has settled. Failed/offline checks retry on
// the same bounded schedule; they never interrupt normal use with an error.
function automaticUpdates(run, enabled, dependencies = {}) {
  const schedule = dependencies.setTimeout || setTimeout;
  const unschedule = dependencies.clearTimeout || clearTimeout;
  let disposed = false, active = false, timer;
  const check = async () => {
    if (disposed || active) return;
    active = true;
    try { if (enabled()) await run(); } catch (_) { /* next scheduled check retries */ }
    finally { active = false; if (!disposed) timer = schedule(check, 6 * 60 * 60 * 1000); }
  };
  timer = schedule(check, 30000);
  return { check, dispose: () => { disposed = true; unschedule(timer); } };
}
module.exports = { automaticUpdates };
