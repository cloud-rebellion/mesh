# Mesh in VS Code

Open **Cmd+Shift+P → Mesh: Open** (Ctrl+Shift+P on Windows/Linux). Mesh opens as a reusable editor tab, like Stage. The Mesh status-bar item appears after editor startup and opens the same tab. Activation alone starts no viewer or network requests.

This first version includes the existing Graph, Search, Dashboard and Docs views. Click a graph node to read its note. Search runs only when you press Enter or click Search; graph filtering remains local. The extension adds no LLM or answer-generation service. Search still follows your existing Mesh embedding/reranking configuration.

## Remote setup (like Stage)

Use **Mesh: Set Viewer URL** and enter your HTTPS viewer URL (for example `https://mesh.cloudrebellion.tech/app`), then **Mesh: Connect**. The IDE shows a short code and opens Mesh's approval page. Check the matching code, your account and the requested permissions, then click **Connect**. Full access means your account's current permissions, never an administrator upgrade. You may narrow the request to read access.

An existing Mesh browser session is reused. On a team hub configured for account sign-in, **Continue with your account** signs you in through the team's identity provider and returns to the same pending request. Sign-in does not approve it: check the matching code and click Connect. The viewer shows your account and current role. Access-key sign-in remains an explicit compatibility alternative. Stage credentials are not automatically Mesh credentials. Your team administrator must enable account sign-in; operator prerequisites are documented in `docs/BROWSER-SIGN-IN.md` in the Mesh source repository.

Approved access/renewal credentials are verified against the viewer and saved only in VS Code SecretStorage, bound to the exact server and base path. They never enter settings or the webview. Access lasts fifteen minutes and renews automatically while in use, within a thirty-day connection lifetime. Renewal is serialized; an interrupted/uncertain renewal requires reconnecting rather than replaying a potentially consumed token. **Mesh: Sign Out of Remote Viewer** revokes a browser-approved connection on the server before removing it locally. If the server cannot confirm revocation, credentials are retained for retry and the IDE reports that disconnection is unconfirmed. You can also revoke connections at the viewer's `/connect` page.

Older servers can still use the explicitly separate **Mesh: Sign In with Access Key** command. Its native prompt is masked. Signing out removes that saved key only; it does not revoke the key itself or sign out browser sessions. Browser connection failures never silently fall back to a shared key.

Server operators enable browser approval on Mesh v0.41.0 or later with `MESH_UI_PUBLIC_URL` set to the exact public HTTPS viewer URL, including `/app` when that is the configured base path. Existing member authentication or a standalone token is required. Connection state is stored separately in `<vault>/.mesh/auth/connections.db`, not the knowledge index. `MESH_UI_CONNECTIONS_DB` can select another absolute `connections.db` path in a private directory. Reverse proxies must preserve the public Host and Origin; untrusted forwarded headers never choose the approval URL. Back up this credential state privately. Changing the public URL requires new approval. Deploying this candidate and enabling it on a server are separate operator actions.

Remote connections never start local processes and ignore local startup/vault settings. Authentication failures stop background retries until you connect, refresh or reopen the view. No DNS change, new model or LLM service is needed. Browser-only VS Code is not supported. The IDE renderer remains read-only even when the approved connection has full account permissions.

## Local setup

The extension reuses an existing local `mesh ui` viewer. If none is running, either use **Mesh: Configure Viewer Startup** to select a local executable and existing vault and explicitly enable read-only startup, or start it in a terminal:

```sh
mesh ui ~/Corpus --own-index=false
```

Do **not** add `--own-index` when your sync/watch process owns the vault. Opt-in startup uses the selected absolute executable, literal arguments and `--own-index=false`, with inherited web-owner settings removed. It never starts an index owner, reindexes, installs binaries or mutates notes. The native configuration commands write only user-level extension settings. `Mesh: Refresh View` reconnects the viewer, not the index.

The default is `http://127.0.0.1:7474`; a root URL also tries `/app` when the root status endpoint returns 404. Use **Mesh: Set Viewer URL** for a different loopback port/base path. Process startup requires an HTTP numeric loopback root URL with an explicit nonzero port; existing viewers can still use base paths. Only user-level settings are used; workspace overrides cannot redirect requests or launch programs. HTTP loopback connections never carry stored remote keys.

While the tab is visible, readiness is checked every 30 seconds. Failures retry with bounded backoff, and startup is attempted only for a refused connection: at most three starts per ten minutes. Wrong-service or wrong-vault responses never trigger another process. The status tooltip reports viewer ownership and observed index freshness; legacy viewers explicitly show these as unknown. Automatically started viewers must confirm modern read-only status (Mesh v0.38.0 or later). No health polling occurs while hidden. Hiding stops a child still starting; an already-ready child is reused until settings change or the extension shuts down. External viewers are never stopped.

## Security and limits

The shipped viewer assets are bundled in the extension. A bounded host bridge allows only selected GET endpoints on the user-approved HTTPS server or HTTP numeric loopback address. There is no iframe, remote executable content, renderer credential access, arbitrary fetch, Ask, review mutation or renderer settings write. HTTPS uses normal certificate verification. Note links cannot launch commands, files or external sites. Scripts are nonce-only; the graph's inline styles remain allowed. Hidden/closed views abort pending work and the singleton tab restores its selected section when shown again.

## Build and test

The additional **`bun run test:https`** integration check requires `openssl` on PATH. It creates disposable certificates and loopback-only servers, checks trusted/untrusted TLS plus authenticated reads, redirects, cancellation and sign-out, then removes its fixture files. Trust is limited to its child process; system trust and production credentials are untouched. It runs with Bun by default. Set `MESH_IDE_NODE_BIN` to a Node-compatible executable (including VS Code's executable on desktop) to verify the extension's actual runtime: `MESH_IDE_NODE_BIN="/Applications/Visual Studio Code.app/Contents/MacOS/Code" bun run test:https` on macOS. This is separate from the default unit/package gate, whose CI image does not declare OpenSSL; fixture success is not a production sign-in receipt.

From the Mesh module root, `MESH_CONNECT_E2E=1 go test -race ./internal/web -run '^TestConnectionIDEHTTPSJourney$'` tests this IDE client against the real Go HTTPS handlers in a disposable vault. It requires Bun and localhost-listener permissions. Certificate trust is restricted to the child process; no real account or system trust is changed. The rendered approval UI has a separate synthetic-API regression: run `bun integration/consent-browser.mjs` from this directory with an installed Playwright module (`PLAYWRIGHT_MODULE` can select it; `MESH_BROWSER_EXECUTABLE` can select a local Chromium executable). Neither fixture replaces the installed-user production acceptance gate.

```sh
bun install --frozen-lockfile
bun run build
bun run test
bun run package
bun run ci
```

Install `release/mesh-workspace-0.3.2.vsix` with VS Code's **Extensions: Install from VSIX…** command. `media/source.json` records the bundled Mesh release, viewer API, source revision, dirty state and asset hashes. Packaging includes the Mesh license. The server and editor extension have independent component versions under a shared Mesh source release; upgrading this viewer does not upgrade your installed Mesh binary.

Packaging uses the committed lockfile, fixed ZIP timestamps/permissions and sorted entries. It checks the complete archive allowlist, identity, source revision, asset hashes and shipped contents against the build inputs. A release build refuses uncommitted IDE/shared-viewer changes. `bun run ci` audits dependencies, runs the unit suite and requires two packages to be byte-identical. For local development only, `bun run ci --allow-dirty` or `bun run package --allow-dirty` produces an explicitly dirty, non-release artifact. The fixed Mesh UI archive builder emits the installer metadata directly and refuses unsupported runtime payloads. It uses the existing ZIP library; signing-tool postinstall scripts are unnecessary for this unsigned VSIX workflow.

The ignored `release/` directory also contains `manifest.json` and `SHA256SUMS`, identifying the exact version, source commit, byte count and archive hash. These are release artifacts, not an authenticity signature. Verify them against a trusted release source; do not trust a downloaded checksum from an unrelated source.

## Downloads and updates

Use the **Update Mesh** download-icon button in the Mesh editor title, **Mesh: Update Mesh**, or **Mesh: Check for Mesh Updates**. One native overview reports the installed IDE, its bundled Mesh viewer release, the connected server release and independently available updates. Existing command IDs remain supported. The command explicitly contacts the fixed public `bright-interaction/mesh` GitHub releases API and reads status from your configured viewer using its existing connection. No new connection is approved and no viewer is started. Changing the configured endpoint, signing out or cancelling aborts the update flow; stale completion cannot install against the old selection.

The IDE release check sends no vault content, workspace paths, Mesh credentials or telemetry to GitHub. It reads at most 300 recent releases, selects a newer stable `ide-vX.Y.Z` release (ignoring core tags, drafts and prereleases), and checks its manifest and checksum list. New manifests record `mesh_release` and `viewer_api`, matching the archived viewer metadata and checked-out `mesh/VERSION`. This is a source pairing, not proof that your server has been upgraded or that every feature has been accepted. A reported incompatible viewer API blocks installation. Legacy or unavailable server metadata is explicitly unknown; legacy IDE manifests without a pairing still require your explicit installation approval. An offline server does not hide available IDE releases, and a failed public release check is not reported as up to date. No public update endpoint can be supplied by workspace settings.

**Server update steps** explains the deployment boundary. Hosted Mesh must be updated by its deployment operator with backup, rollback and acceptance checks; local `mesh upgrade` does not update a remote container. Local viewers require the supported updater for their exact installation and an explicit service restart. This flow does not replace server binaries, execute returned commands or restart any index owner. The web app also has an **Update Mesh** overview for its own server; it cannot inventory your IDE or install an extension. The IDE hides that browser-only control and uses its native overview.

Activation, opening the Mesh tab and viewer polling do not query the public IDE release feed. The server's existing core release check remains cached (normally 24 hours) and may be disabled. A missing release, offline connection or rate limit does not interrupt the viewer. Server status retains its running release even if that check is unavailable; legacy servers may report only unknown.

When a release is found, choose **Update now** in that overview to download, verify and install it through [VS Code's built-in extension installer](https://code.visualstudio.com/api/references/commands). The VSIX is staged in a unique private temporary directory, checked again after writing, and removed after the installer completes. Download/check operations are cancellable with 20-second network deadlines; once installation starts it must finish before cleanup. Failed installation never triggers reload. After success, choose **Reload window** or **Later**. Reload affects the entire VS Code window and its extensions, not just the Mesh tab, so save your work first. Clicking Update Mesh again before reloading offers the reload without reinstalling.

Alternatively choose **Download VSIX**, then a new local filename for manual installation. Both paths download at most 16 MiB from the fixed release URL, permitting only HTTPS redirects to GitHub's release-asset CDN, and verify exact byte count and SHA-256. Exclusive file creation preserves existing files and symlinks. There is no unattended installation, core-binary upgrade, remote viewer/index-owner restart or marketplace integration. VS Code's own installation policies still apply; Mesh does not bypass them. An extension-owned local viewer may stop/restart during window reload according to its existing lifecycle settings; external viewers are not stopped. Older IDE versions require this updater to be installed once before they can use its new button.

Checksums establish integrity relative to the official HTTPS release, not an independent publisher signature. A compromised release account remains a trust risk. No release is available through this command until its three reviewed assets have actually been published. A bounded search finding nothing is not proof that no older release exists outside the search window.

## Release handoff (operator)

The CI static-artifact gate includes IDE source, shared viewer assets and the Mesh license, with frozen installation and no deploy credentials. Confirm the exact source SHA's remote gate result before publication; local success alone is not a remote CI receipt.

1. Commit reviewed source and pass `bun run ci` plus the required remote checks. An IDE change gets a new independent version; never reuse a published version for different bytes.
2. Run `bun run release:plan`. It refuses dirty source, stale provenance, inconsistent manifests/checksums and archive contents that differ from the reviewed build. It prints a JSON handoff including exact `gh` arguments; it does not execute them, use credentials, create tags, upload or publish. There is no dirty-source bypass.
3. After publication approval, verify the public mirror's tree matches the reviewed monorepo `mesh/` subtree and create the independent `ide-vX.Y.Z` tag at that public source commit. The recorded monorepo source SHA is not necessarily the public split-repository SHA. Do not blindly tag public `main`.
4. Review and run the printed draft command from `mesh/ide`. GitHub CLI's [`--verify-tag`, `--draft` and `--latest=false`](https://cli.github.com/manual/gh_release_create) require the existing tag, stage reviewable assets, and keep the IDE release separate from core latest. The command never overwrites existing assets. A failed partial draft upload needs manual review, not a clobber retry.
5. Download the draft's VSIX, `manifest.json` and `SHA256SUMS` into a fresh directory and compare all three to the approved local bundle. After explicit approval, publish the reviewed draft with `latest=false`; confirm the public downloads and a disposable IDE install/update journey. Only then report delivery as live.

Public Mesh core tags and IDE versions remain independent. Keep core `vX.Y.Z` and its latest-release channel unchanged: the existing web/TUI banners are not an IDE delivery channel. Source merge, tag creation, draft upload, publication and normal-profile installation are separate operations; none is performed by packaging or CI.

`scripts/browser-smoke.mjs` checks fixture journeys in installed Chrome; set `MESH_PLAYWRIGHT_MODULE` to your Playwright module if needed. `test/editor.cjs` runs with VS Code's `--extensionDevelopmentPath` and `--extensionTestsPath` in an isolated `--user-data-dir`/`--extensions-dir` profile; set `MESH_IDE_TEST_URL` to your local test viewer. Optionally set `MESH_IDE_TEST_STARTUP_BINARY` to a built Mesh executable to test an isolated temporary vault, read-only child startup, exit/reconnect and disposal. This test changes only that disposable editor profile and deletes its own temporary vault after child shutdown. It writes a content-free receipt to ignored `test-results/editor-result.json`. Implementation follows the official [VS Code webview guidance](https://code.visualstudio.com/api/extension-guides/webview).
