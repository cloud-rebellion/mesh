// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const vscode = require('vscode');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const { RemoteAuth, viewerURL, isRemote } = require('./client');
const { ViewerLifecycle, launchSpec } = require('./lifecycle');
const { Broker } = require('./broker');
const { renderView } = require('./view');
const { updateCommand } = require('./update-command');
const { automaticUpdates } = require('./automatic-updates');
const { coordinatedOverview } = require('./coordinated-updates');
const DEFAULT = 'http://127.0.0.1:7474';
let deactivateCurrent;
function activate(context) {
  let panel, broker, listener, lifecycle, ready = false;
  const auth = new RemoteAuth(context.secrets);
  let signInController, signingIn = false;
  const deliveredReads = new Set();
  const global = key => vscode.workspace.getConfiguration('mesh').inspect(key)?.globalValue;
  const configured = () => viewerURL(global('viewerUrl') || DEFAULT);
  const source = JSON.parse(fs.readFileSync(path.join(context.extensionPath, 'media/source.json'), 'utf8'));
  const version = context.extension.packageJSON.version;
  const updates = updateCommand(vscode, version, {
    context: configured,
    enabled: () => global('updates.automatic') !== false,
    overview: signal => {
      const base = configured();
      return coordinatedOverview(version, source, async requestSignal => {
        // Read using the exact user-approved endpoint and existing secure auth.
        // This creates no viewer process or approval request.
        const response = await auth.client(base).connect(requestSignal);
        return { status: JSON.parse(response.body), remote: isRemote(base) };
      }, { signal });
    }
  });
  context.subscriptions.push(automaticUpdates(updates.runAutomatic, () => vscode.workspace.isTrusted && global('updates.automatic') !== false), updates, vscode.commands.registerCommand('mesh.checkUpdates', updates.run), vscode.commands.registerCommand('mesh.updateNow', updates.run));
  const options = () => {
    const startup = global('startup') || {};
    if (isRemote(configured())) return { url: configured(), autoStart: false };
    return { url: configured(), binary: startup.binary, vault: startup.vault, autoStart: startup.enabled === true && vscode.workspace.isTrusted };
  };
  let previous = JSON.stringify(options());
  const status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 8);
  status.text = '$(symbol-misc) Mesh'; status.tooltip = 'Open Mesh knowledge workspace'; status.command = 'mesh.open'; status.show();
  function disconnect() { ready = false; broker?.dispose(); listener?.dispose(); broker = listener = undefined; }
  function loading(state) {
    if (!panel?.visible) return;
    disconnect();
    const escape = text => String(text).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
    // Native command links are permitted only on this script-free host-owned
    // page, never in the data-bearing graph/note renderer.
    panel.webview.html = '';
    panel.webview.options = { ...panel.webview.options, enableScripts: false, enableCommandUris: ['mesh.signIn', 'mesh.configureServer', 'mesh.configureStartup', 'mesh.refresh'] };
    const help = isRemote(configured()) ? '<a href="command:mesh.signIn">Connect to Mesh</a> and approve your account permissions in the browser.' : '<a href="command:mesh.configureStartup">Configure local viewer startup</a>.';
    panel.webview.html = `<html><head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'"></head><body style="font-family:var(--vscode-font-family);padding:24px"><h2>Mesh</h2><p role="status">${escape(state.detail)}</p><p>${escape(configured())}</p><p>${help}</p><p><a href="command:mesh.configureServer">Set viewer URL</a> · <a href="command:mesh.refresh">Retry connection</a></p></body></html>`;
  }
  function render() {
    if (!panel || !panel.visible) return;
    disconnect();
    panel.webview.options = { ...panel.webview.options, enableScripts: true, enableCommandUris: false };
    const current = panel;
    deliveredReads.clear();
    broker = new Broker(lifecycle.client, message => current.webview.postMessage(message), { ready: () => { ready = true; }, read: path => { deliveredReads.add(path.split('?')[0]); } });
    const active = broker;
    listener = current.webview.onDidReceiveMessage(message => { void active.handle(message); });
    current.webview.html = renderView(fs.readFileSync(path.join(context.extensionPath, 'media/index.html'), 'utf8'), {
      cspSource: current.webview.cspSource,
      resource: name => current.webview.asWebviewUri(vscode.Uri.joinPath(context.extensionUri, name)).toString()
    });
  }
  function newLifecycle() {
    lifecycle?.dispose();
    const config = options();
    lifecycle = new ViewerLifecycle(auth.client(config.url), config, state => {
      const connected = ['connected', 'legacy'].includes(state.kind);
      status.text = connected ? '$(symbol-misc) Mesh' : state.kind === 'offline' ? '$(warning) Mesh' : '$(sync~spin) Mesh';
      status.tooltip = state.detail;
      if (connected) { if (!broker) render(); } else loading(state);
    });
  }
  newLifecycle();
  function connect() {
    loading({ detail: 'Connecting to Mesh…' });
    lifecycle.setVisible(true); lifecycle.retry();
  }
  function attach(created) {
    panel = created;
    panel.webview.options = { enableScripts: true, localResourceRoots: ['media', 'src'].map(p => vscode.Uri.joinPath(context.extensionUri, p)) };
    const disposables = [];
    disposables.push(panel.onDidDispose(() => { disconnect(); lifecycle.setVisible(false); panel = undefined; disposables.forEach(d => d.dispose()); }));
    let visible = panel.visible;
    disposables.push(panel.onDidChangeViewState(({ webviewPanel }) => {
      if (visible === webviewPanel.visible) return;
      visible = webviewPanel.visible; if (visible) connect(); else { disconnect(); lifecycle.setVisible(false); }
    }));
    connect();
  }
  function open() {
    if (!vscode.workspace.isTrusted) return vscode.window.showWarningMessage('Trust this workspace before opening Mesh.');
    if (panel) { panel.reveal(vscode.ViewColumn.Active); return; }
    attach(vscode.window.createWebviewPanel('mesh.workspace', 'Mesh', vscode.ViewColumn.Active, { enableScripts: true }));
  }
  async function configureServer() {
    const value = await vscode.window.showInputBox({ title: 'Mesh viewer URL', value: configured(), prompt: 'HTTPS server (for example https://mesh.cloudrebellion.tech/app), or a local HTTP loopback viewer. Not the MCP endpoint.', validateInput: value => { try { viewerURL(value); } catch (_) { return 'Use HTTPS or HTTP numeric loopback, without credentials, query or fragment.'; } } });
    if (value) await vscode.workspace.getConfiguration('mesh').update('viewerUrl', viewerURL(value), vscode.ConfigurationTarget.Global);
  }
  async function signInKey() {
    if (!vscode.workspace.isTrusted || signingIn) return;
    const base = configured();
    if (!isRemote(base)) return vscode.window.showInformationMessage('Set an HTTPS URL with Mesh: Set Viewer URL before signing in.');
    signingIn = true;
    const controller = signInController = new AbortController();
    try {
      const token = await vscode.window.showInputBox({ title: 'Sign in to Mesh', password: true, ignoreFocusOut: true, prompt: `Mesh access key for ${base}. Prefer your scoped member key. This is not your Stage password or MCP token.` });
      if (!token || controller.signal.aborted || configured() !== base) return;
      await auth.signIn(base, token, controller.signal);
      if (controller.signal.aborted || configured() !== base) return;
      disconnect(); newLifecycle(); if (panel?.visible) connect(); else open();
      void vscode.window.showInformationMessage('Mesh access key verified and saved in secure storage.');
    } catch (_) {
      if (!controller.signal.aborted) void vscode.window.showErrorMessage('Mesh sign-in failed. Check the viewer URL, Mesh access key and network. No new key was saved unless verification completed.');
    } finally { signingIn = false; if (signInController === controller) signInController = undefined; }
  }
  async function signIn() {
    if (!vscode.workspace.isTrusted || signingIn) return;
    const base = configured();
    if (!isRemote(base)) return vscode.window.showInformationMessage('Set your HTTPS Mesh viewer URL before connecting. Local viewers do not need browser approval.');
    signingIn = true;
    const controller = signInController = new AbortController();
    try {
      await vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title: 'Connect to Mesh', cancellable: true }, async (progress, cancellation) => {
        const subscription = cancellation.onCancellationRequested(() => controller.abort());
        try {
          await auth.signInBrowser(base, controller.signal, async request => {
            progress.report({ message: `Code ${request.code}: approve your permissions in the browser.` });
            const choice = await vscode.window.showInformationMessage(`Connect to ${request.server}. Check code ${request.code} in the browser, review your account and permissions, then click Connect.`, { modal: true }, 'Open approval page');
            if (choice !== 'Open approval page' || controller.signal.aborted) { controller.abort(); throw new Error('Connection cancelled.'); }
            if (!await vscode.env.openExternal(vscode.Uri.parse(request.url))) throw new Error('The browser could not open. Start Connect again.');
          });
        } finally { subscription.dispose(); }
      });
      if (controller.signal.aborted || configured() !== base) return;
      disconnect(); newLifecycle(); if (panel?.visible) connect(); else open();
      void vscode.window.showInformationMessage('Connected to Mesh. Your approved connection is saved in secure storage.');
    } catch (err) {
      if (!controller.signal.aborted) void vscode.window.showErrorMessage(err.message || 'Mesh could not connect. Try Connect again.');
    } finally { signingIn = false; if (signInController === controller) signInController = undefined; }
  }
  async function signOut() {
    const base = configured();
    if (!isRemote(base)) return;
    signInController?.abort(); updates.cancel();
    disconnect(); lifecycle.dispose();
    try {
      const result = await auth.signOut(base);
      newLifecycle();
      loading({ detail: result.revoked ? 'Disconnected. This connection was revoked on Mesh and removed from the IDE.' : 'Signed out. The saved access key was removed from this IDE; the server key was not revoked.' });
    } catch (err) { loading({ detail: 'Disconnection was not confirmed. Retry or revoke this connection in your Mesh browser.' }); void vscode.window.showErrorMessage(err.message); }
  }
  async function configureStartup() {
    if (!vscode.workspace.isTrusted) return;
    if (isRemote(configured())) return vscode.window.showInformationMessage('Remote Mesh is managed on the server. Local startup is disabled for HTTPS viewers.');
    const old = options();
    const binaries = await vscode.window.showOpenDialog({ title: 'Choose the Mesh executable', canSelectMany: false, canSelectFiles: true, canSelectFolders: false, defaultUri: vscode.Uri.file(old.binary || path.join(os.homedir(), '.local/bin/mesh')) });
    if (!binaries?.length) return;
    const vaults = await vscode.window.showOpenDialog({ title: 'Choose the existing Mesh vault', canSelectMany: false, canSelectFiles: false, canSelectFolders: true, defaultUri: vscode.Uri.file(old.vault || path.join(os.homedir(), 'Corpus')) });
    if (!vaults?.length) return;
    if (binaries[0].scheme !== 'file' || vaults[0].scheme !== 'file') throw new Error('Local paths required');
    const binary = fs.realpathSync(binaries[0].fsPath), vault = fs.realpathSync(vaults[0].fsPath);
    launchSpec({ url: configured(), binary, vault });
    const answer = await vscode.window.showWarningMessage(`Allow Mesh to start ${binary} as a read-only viewer for ${vault} at ${configured()} when unavailable? No index owner will be started.`, { modal: true }, 'Enable Startup');
    if (answer === 'Enable Startup') await vscode.workspace.getConfiguration('mesh').update('startup', { enabled: true, binary, vault }, vscode.ConfigurationTarget.Global);
  }
  const safe = fn => () => Promise.resolve().then(fn).catch(() => { disconnect(); void vscode.window.showErrorMessage('Mesh could not open. Check Mesh: Set Viewer URL and the extension installation.'); });
  for (const [name, fn] of Object.entries({ open, refresh: () => { if (!panel) open(); else connect(); }, configureServer, configureStartup, signIn, signInKey, signOut })) context.subscriptions.push(vscode.commands.registerCommand('mesh.' + name, safe(fn)));
  context.subscriptions.push(vscode.window.registerWebviewPanelSerializer('mesh.workspace', { deserializeWebviewPanel: async restored => {
    if (!vscode.workspace.isTrusted) { restored.dispose(); return; } attach(restored);
  } }));
  context.subscriptions.push(vscode.workspace.onDidChangeConfiguration(event => {
    if (!event.affectsConfiguration('mesh.viewerUrl') && !event.affectsConfiguration('mesh.startup')) return;
    updates.cancel();
    try { const next = JSON.stringify(options()); if (next === previous) return; previous = next; signInController?.abort(); disconnect(); newLifecycle(); if (panel?.visible) connect(); } catch (_) { signInController?.abort(); disconnect(); lifecycle.dispose(); panel?.dispose(); }
  }));
  deactivateCurrent = () => { updates.dispose(); signInController?.abort(); disconnect(); lifecycle.dispose(); };
  context.subscriptions.push(status, { dispose: deactivateCurrent });
  return { diagnostics: () => ({ viewReady: ready, open: Boolean(panel), pending: broker?.pending.size || 0, deliveredReads: [...deliveredReads], connection: lifecycle.state.kind, ownedChild: Boolean(lifecycle.child), ownedChildPID: lifecycle.child?.pid }) };
}
module.exports = { activate, deactivate: () => deactivateCurrent?.() };
