# Windows packaging

`apps/setup` (varde-setup, a bespoke Tauri 2 app) builds
`VardeSetup-<ver>.exe` via `build-setup.ps1` — the installer the
auto-update mechanism also applies silently. It replaces the former
Tauri-bundler NSIS wizard (`apps/desktop/installer-hooks/`, removed):
the NSIS template and hooks were ported one-to-one into the Rust engine
(`apps/setup/src/{engine,win,args}.rs`) and the wizard art was
superseded by the mouve webview UI, so the NSIS sources are gone
rather than kept as a second implementation. On an existing NSIS or
older Inno (`Varde Agent_is1`) install the setup app performs the same
migration the hooks did, keeping `%ProgramData%\Varde`.

The installer:

- installs `varde-agent.exe`, `varde-mesh.exe` and `varde-tray.exe`
  (`apps/desktop`) into `Program Files\Varde`, plus `varde.ico` and
  `LICENSE`, a Start menu entry and an Add/Remove Programs entry whose
  uninstaller is the embedded `uninstall.exe` (the same engine built
  `--no-default-features`, no payload)
- never asks for an address: the PC joins the hosted Varde (cp.varde.games).
  Self-hosting instead? `/CPURL=cp.example` (or a full https URL) writes
  `%ProgramData%\Varde\server.url`, which the unlinked service reports to
  the tray — skipped when the PC is already linked. A remote `http://`
  address asks before it is kept
- installs Microsoft Edge WebView2 if missing (bootstrapper embedded in
  the payload, run only when the EdgeUpdate client key reports no
  version)
- creates `%ProgramData%\Varde`; `service install` makes it
  SYSTEM/Administrators-only (`logs\` stays readable by Users)
- registers and starts the `VardeAgent` Windows service (the agent's own
  `service install` path — STOP and PRESHUTDOWN map to the graceful-stop
  path, with a 120 s preshutdown timeout). Unlinked, the service waits
  idle for provisioning and exits 0 so SCM doesn't count restarts
- adds `varde-tray.exe --autostart` to `HKLM\...\Run` when the
  autostart task is selected (default)
- optionally installs `varde-control-plane.exe` as the
  `VardeControlPlane` service + `%ProgramData%\VardeCP` and presets
  `server.url` to `http://localhost:8080` (off by default)
- remembers the component selection in `HKLM\Software\Varde!Components`
  (present-but-empty means all off); a `/TASKS=` list replaces the set
  for that run
- is a single 470x560 mouve-styled window; on finish it launches the
  tray as the installing user with `--link`
- ends interactive uninstall by removing the Run value and Program
  Files; `%ProgramData%\Varde` is kept unless the user ticks delete
  (silent always keeps)

## Engine

One Rust binary, two link modes. The default `payload` feature embeds
`payload.zip` (agent/mesh/tray/control-plane, ico, LICENSE, the
WebView2 bootstrapper and `uninstall.exe` itself); the lite
`--no-default-features` build is what ships as `uninstall.exe` in the
image. Running `uninstall*.exe` from inside the install dir re-launches
a staged `%TEMP%\varde-uninstall-<pid>.exe` copy (with
`/waitpid=<parent>`), because Windows can't delete a running image;
the staged copy waits for the parent process handle to close and is
marked delete-on-reboot.

## Flags (NSIS-compatible surface)

Silent update (`tauri-plugin-updater` runs `VardeSetup.exe /S /R /ARGS
--autostart`), interactive flags mirror the old wizard:

- `/S` silent, `/P` passive (reserved), `/NS` no shortcuts
- `/R` silent reinstall, `/UPDATE` keeps data on uninstall,
  `/ARGS <tray args>` forwarded to the relaunched tray
- `/TASKS="autostart controlplane"` — whole set for that run
- `/CPURL=<host|url>` (see above)
- `/uninstall [/target=<dir>] [/S] [/UPDATE] [/waitpid=<pid>]` — used by
  ARP and the staged re-launch

## Build + sign

`packaging/windows/build-setup.ps1 -Version <ver>` stages
`apps/setup/payload/` from `dist\varde-*.exe` (+ `varde.ico`,
`LICENSE`, the Edge bootstrapper from the stable fwlink), compiles the
lite engine into `payload\uninstall.exe`, then compiles the installer
to `release-assets\VardeSetup-<ver>.exe`. Flags: `-SkipUiBuild`,
`-SkipPayloadBuild` for incremental rebuilds. `apps/setup` is a
workspace member; `packaging/release/version.py` bumps it with the
rest. CI (`build-release.yml`, `ci.yml`) calls the same script, then
Azure Artifact Signing signs `VardeSetup-<ver>.exe` unchanged.

## Migration order on install

1. WebView2 bootstrap (skip if already present)
2. Inno `Varde Agent_is1` → run its uninstaller `/VERYSILENT
   /SUPPRESSMSGBOXES /NORESTART`, then NSIS `Uninstall\Varde` (non-setup
   engine) → run its uninstaller `/S` and wait for the ARP key / files
   to actually disappear
3. drain-and-swap: stop tray, `service uninstall` + wait-unlocked,
   extract payload, rebuild dirs/registry/services/shortcuts
4. launch tray `--link` as the installing user (interactive only)
