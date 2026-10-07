# Windows packaging

`varde-agent.iss` (Inno Setup 6) builds `VardeSetup-<ver>.exe`, which:

- installs `varde-agent.exe`, `varde-mesh.exe` and `varde-tray.exe`
  (`apps/desktop`) into `Program Files\Varde`, plus a Start menu entry
  for the tray
- uses `varde.ico` for setup and uninstall and installs it with the binaries
- shows the brand art in the wizard: `wizard/wizard-*.bmp` (mark + wordmark on
  Natt, left panel) and `wizard/wizard-small-*.bmp` (mark on white, page header),
  each at 100/125/150/200/250% DPI, rendered from `brand/svg/`
- never asks for an address: the PC joins the hosted Varde (varde.games).
  Self-hosting instead? `/CPURL=https://cp.example` writes
  `%ProgramData%\Varde\server.url` (http/https only), which the unlinked
  service reports to the tray — skipped when the PC is already linked
- installs Microsoft Edge WebView2 if missing (for the link window; without
  it the tray links via message boxes)
- creates `%ProgramData%\Varde`; `service install` makes it SYSTEM/Administrators-only
  (drops what it inherits from ProgramData, takes ownership back, resets explicit
  entries below it; `logs\` stays readable by Users)
- registers and starts the `VardeAgent` Windows service (the agent's own
  `service install` path — STOP and PRESHUTDOWN map to the graceful-stop
  path, with a 120 s preshutdown timeout). Unlinked, the service waits idle
- task "Start Varde at login" (on by default): HKLM `Run` value
  `varde-tray.exe --autostart`; each user can turn it off in the tray.
  Unticking it on an upgrade removes the earlier value
- at the end, launches the tray as the installing user with
  `--link`: the tray asks the service to link, shows the code and opens
  the dashboard to approve it

Upgrades stop the tray and remove the old service (waiting for it to stop)
before replacing files. Uninstall stops the tray, waits for the service to
stop (final save), removes the `Run` value and Program Files, and keeps
`%ProgramData%\Varde` (saves, machine identity) unless the user answers
Yes to "Also delete Varde's data on this PC?" (default No; silent uninstall
always keeps it).

Silent install: `VardeSetup.exe /VERYSILENT` (add `/CPURL=…` only for a
self-hosted control plane; then link with `varde-agent enroll --token …`,
or from the tray).

```sh
iscc /DVersion=0.1.0 packaging/windows/varde-agent.iss
```

expects `packaging/windows/dist/` to contain `varde-agent.exe`,
`varde-mesh.exe`, `varde-tray.exe` and `MicrosoftEdgeWebview2Setup.exe`
(the Evergreen bootstrapper). CI job `windows-installer` builds them (MSVC
toolchain for the Rust binaries), downloads the bootstrapper, runs ISCC via
`choco install innosetup`, and uploads `VardeSetup-*` as an artifact.
