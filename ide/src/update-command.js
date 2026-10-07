// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const { checkUpdate, downloadUpdate, verifyBytes, manifest, compare } = require('./updates');

async function stageUpdate(info, bytes) {
  verifyBytes(bytes, info);
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), 'mesh-ide-update-'));
  // Only the exact directory created by this invocation can be removed.
  const cleanup = () => fs.rm(directory, { recursive: true, force: true });
  try {
    const file = path.join(directory, info.file);
    await fs.writeFile(file, bytes, { flag: 'wx', mode: 0o600 });
    verifyBytes(await fs.readFile(file), info);
    return { file, cleanup };
  } catch (error) { await cleanup(); throw error; }
}

function updateCommand(vscode, version, dependencies = {}) {
  const check = dependencies.check || checkUpdate, download = dependencies.download || downloadUpdate;
  const write = dependencies.write || ((file, bytes) => fs.writeFile(file, bytes, { flag: 'wx', mode: 0o600 }));
  const stage = dependencies.stage || stageUpdate;
  let active = false, disposed = false, controller;
  let installedVersion;
  const run = async () => {
    if (active || disposed || !vscode.workspace.isTrusted) return;
    active = true; controller = new AbortController();
    const signal = controller.signal;
    let context;
    const stopped = () => {
      if (signal.aborted || disposed || !vscode.workspace.isTrusted) return true;
      try { return dependencies.context?.() !== context; } catch (_) { return true; }
    };
    let staged, phase = 'download';
    const reload = async () => {
      if (stopped()) return;
      const choice = await vscode.window.showInformationMessage(`Mesh IDE ${installedVersion} installed. Reload this VS Code window to activate it? This reloads all extensions in this window. Save your work first. The Mesh server/core binary was not upgraded.`, 'Reload window', 'Later');
      if (choice === 'Reload window' && !stopped()) {
        phase = 'reload';
        await vscode.commands.executeCommand('workbench.action.reloadWindow');
      }
    };
    try {
      context = dependencies.context?.();
      if (installedVersion) { phase = 'installed'; await reload(); return; }
      const progress = (title, fn) => vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title, cancellable: true }, async (_, token) => {
        const subscription = token.onCancellationRequested(() => controller.abort());
        if (token.isCancellationRequested) controller.abort();
        try { return await fn(); } finally { subscription.dispose(); }
      });
      const overview = dependencies.overview ? await progress('Checking Mesh components…', () => dependencies.overview(signal)) : null;
      const info = overview ? overview.info : await progress('Checking public Mesh IDE releases…', () => check(version, { signal }));
      if (stopped()) return;
      if (!info && !overview) { await vscode.window.showInformationMessage('No newer stable Mesh IDE release found in the most recent 300 public releases.'); return; }
      if (info) {
        manifest(info, info.version);
        if (compare(info.version, version) <= 0) throw new Error('Update must be newer');
      }
      const buttons = [...(info ? ['Update now', 'Download VSIX'] : []), ...(overview ? ['Server update steps'] : [])];
      const choice = await vscode.window.showInformationMessage(overview?.message || `Mesh IDE ${info.version} is available (installed: ${version}). Update now downloads from cloud-rebellion/mesh, verifies SHA-256 and installs the extension. Reload is offered separately; your Mesh server/core is not upgraded.`, { modal: true }, ...buttons);
      if (choice === 'Server update steps' && overview && !stopped()) {
        await vscode.window.showInformationMessage(overview.serverInstructions, { modal: true }, 'Done');
        return;
      }
      if (!info) return;
      if (!['Update now', 'Download VSIX'].includes(choice) || stopped()) return;
      if (choice === 'Update now') {
        const bytes = await progress('Downloading and verifying Mesh IDE…', () => download(info, { signal }));
        if (stopped()) return;
        staged = await stage(info, bytes);
        if (stopped()) return;
        phase = 'install';
        // The installer is not cancellable. Await it before deleting its input;
        // never reload after an error or a disposed extension host.
        await vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title: 'Installing Mesh IDE…', cancellable: false }, () => vscode.commands.executeCommand('workbench.extensions.installExtension', vscode.Uri.file(staged.file)));
        installedVersion = info.version;
        phase = 'installed';
        try { await staged.cleanup(); } catch (_) {
          if (!stopped()) await vscode.window.showWarningMessage('Mesh IDE installed, but its temporary download could not be removed.');
        }
        staged = undefined;
        await reload();
        return;
      }
      const target = await vscode.window.showSaveDialog({ title: 'Save verified Mesh IDE update (choose a new file)', defaultUri: vscode.Uri.file(path.join(os.homedir(), 'Downloads', info.file)), filters: { 'VS Code Extension': ['vsix'] } });
      if (!target || stopped()) return;
      if (target.scheme !== 'file') throw new Error('Local file required');
      const bytes = await progress('Downloading and verifying Mesh IDE…', () => download(info, { signal }));
      if (stopped()) return;
      await write(target.fsPath, bytes);
      await vscode.window.showInformationMessage(`Mesh IDE ${info.version} saved and SHA-256 verified. Use Extensions: Install from VSIX… and select the saved file. Your Mesh binary and viewer were not changed.`);
    } catch (_) {
      if (!stopped()) await vscode.window.showErrorMessage(phase === 'reload' || phase === 'installed'
        ? 'Mesh IDE was installed, but the reload step did not complete. Save your work and use Developer: Reload Window.'
        : phase === 'install'
          ? 'VS Code did not confirm the Mesh IDE installation. No reload was requested. Check the Extensions view before retrying.'
          : 'Mesh IDE update could not be verified or saved. Check your connection and choose a new writable local filename. No update was installed.');
    } finally {
      if (staged) {
        try { await staged.cleanup(); } catch (_) {
          if (!disposed) await vscode.window.showWarningMessage('Mesh IDE temporary download could not be removed.');
        }
      }
      active = false; controller = undefined;
    }
  };
  return { run, cancel: () => controller?.abort(), dispose: () => { disposed = true; controller?.abort(); } };
}
module.exports = { updateCommand, stageUpdate };
