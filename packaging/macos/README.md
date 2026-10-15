# macOS packaging — Varde.app and the release DMG

`Varde.app` is the drag-installed macOS client: the `varde-tray` menubar app with
`varde-agent` and `varde-mesh` alongside it in `Contents/MacOS`. The release
pipeline ships it as a branded `Varde-<version>-macos-<arch>.dmg` built with
`appdmg`.

`varde-control-plane` is **not** in the app bundle. It is a headless host
component (same opt-in role it plays in the Windows installer) and keeps
shipping as its own flat `varde-control-plane-*-darwin-<arch>.dmg`; bundling it
would run a control plane every time someone logs in. A "host this PC" toggle
that installs it per-user is a possible follow-up.

## Files

- `package-app.sh` — bundles `Varde.app` via `tauri bundle`
  (`apps/desktop/tauri.bundle.macos.conf.json`), then drops `dist/varde-agent`
  and `dist/varde-mesh` into `Contents/MacOS` and codesigns.
- `make-dmg.sh` — renders `dmg-background.svg` into the retina background pair
  `dmg-background.png`/`dmg-background@2x.png` that `appdmg` merges into a
  TIFF, writes `dmg.json`, and runs `npx appdmg`.
- `dmg-background.svg` — the DMG window artwork (Natt backdrop, mark +
  wordmark, drag arrow). Window 540×360, authored at 1x and rendered at 2x.
- `Info-overlay.plist` — merged into `Info.plist`: `LSUIElement` (no Dock icon),
  the `varde://` URL scheme, `VardeAgentLabel` (`games.varde.agent`).
- `entitlements.plist` — hardened-runtime entitlements (`network.client`,
  `disable-library-validation` for the Go-built mesh/agent companions).

## Building locally

```sh
bun install --frozen-lockfile
bun run --filter "@varde/desktop-ui" build   # ui/dist is embedded at compile time
cargo build --release -p varde-agent -p varde-tray
cp target/release/{varde-agent,varde-tray} dist/
VERSION=0.0.0-dev packaging/macos/package-app.sh
packaging/macos/make-dmg.sh target/release/bundle/macos/Varde.app Varde-dev.dmg
```

`make-dmg.sh` renders the SVG with headless Chrome when present and falls back
to `qlmanage`; `appdmg` runs via `npx`, so Node/npm must be on PATH.

## Service model: LaunchAgent, not LaunchDaemon

The agent runs as a per-user LaunchAgent (`~/Library/LaunchAgents/
games.varde.agent.plist`) installed by the tray on first launch via
`launchctl bootstrap gui/<uid>`. It keeps the drag-install UX at zero privilege
prompts and survives crashes (`KeepAlive`) and reboots (`RunAtLoad`).

Tradeoff vs the Windows model (service as SYSTEM, independent of any session):
the agent only runs while *this* user is logged in — no always-on hosting on a
headless or multi-user Mac, and hosting stops on logout. The alternative, a
`SMAppService` privileged helper or a `/Library/LaunchDaemons` installer, buys
back that behavior but needs an authorization prompt, a signed helper tool,
and (for SMAppService) `LSUIElement` workarounds — disproportionate for v1,
since self-hosted always-on control planes already run headless via the flat
control-plane DMG. The tradeoff can be revisited without changing the tray:
only `svcctl_macos.rs` and `autostart.rs` talk to launchd.

The tray registers itself as a login LaunchAgent (`games.varde.tray`) when
"Start at login" is on; the agent LaunchAgent is installed and bootstrapped
independently so hosting works even when the tray quits.

## Updates

Two DMG flavors ship per macOS arch and both stay in the updater manifest:

- `Varde-<ver>-macos-<arch>.dmg` — the app bundle. An agent running inside a
  `.app` advertises `darwin-<arch>-app` and `apply_dmg` replaces the binaries
  under `Contents/MacOS` (then clears `com.apple.quarantine`); the tray
  relaunches itself afterward.
- `varde-agent-<ver>-darwin-<arch>.dmg` — the flat agent for bare-binary and
  self-hosted installs, unchanged (`darwin-<arch>-agent`).

## CI signing + notarization

`.github/workflows/build-release.yml` produces a signed, notarized, stapled
DMG when Apple credentials are configured; otherwise it still ships an ad-hoc
signed (Gatekeeper-flagged) DMG, so dry runs and forks work unchanged. The
gate mirrors the Azure Artifact Signing pattern: `provider=apple` only when
`CHANNEL != dry-run` **and** every value below is set, on the macOS leg only.

Repository **variables**:

- `APPLE_TEAM_ID` — 10-character Apple Developer team ID.

Repository **secrets**:

- `APPLE_CERTIFICATE_P12` — base64 `.p12` export of the **Developer ID
  Application** certificate with its private key (Keychain Access → export).
- `APPLE_CERTIFICATE_PASSWORD` — the `.p12` export password.
- `APPLE_API_KEY_ID`, `APPLE_API_ISSUER`, `APPLE_API_KEY` — an App Store
  Connect API key (Users and Access → Integrations → App Store Connect API):
  key ID, issuer UUID, and the full `.p8` file contents respectively. Used by
  `xcrun notarytool submit`.

What Arne must provide: an Apple Developer Program membership (~$99/yr), the
Developer ID Application certificate (Xcode → Settings → Accounts → Manage
Certificates, or Certificates, Identifiers & Profiles on the web), and an App
Store Connect API key. If an app-specific password is preferred over an API
key, `notarytool` also accepts `--apple-id`/`--password`/`--team-id` or a
stored keychain profile (`--keychain-profile`); the workflow uses the API key
because it is the least-personal credential.
