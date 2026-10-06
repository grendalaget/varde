# Windows packaging

`varde-agent.iss` (Inno Setup 6) builds `VardeSetup-<ver>.exe`, which:

- installs `varde-agent.exe`, `varde-mesh.exe` and `varde-tray.exe`
  (`apps/desktop`) into `Program Files\Varde`, plus a Start menu entry
  for the tray
- uses `varde.ico` for setup and uninstall and installs it with the binaries
- asks for the Varde address (prefilled `https://varde.games`; checked with
  `GET /v1/version`, warning only). Skipped when the PC is already linked
- installs Microsoft Edge WebView2 if missing (for the link window; without
  it the tray links via message boxes)
- creates `%ProgramData%\Varde` with a SYSTEM/Administrators-only ACL on
  `identity\`
- registers and starts the `VardeAgent` Windows service (the agent's own
  `service install` path — STOP and PRESHUTDOWN map to the graceful-stop
  path, with a 120 s preshutdown timeout). Unlinked, the service waits idle
- task "Start Varde at login" (on by default): HKLM `Run` value
  `varde-tray.exe --autostart`; each user can turn it off in the tray
- at the end, launches the tray as the installing user with
  `--link <address>`: the service starts the device flow, the tray shows
  the code and opens the dashboard to approve it

Upgrades stop the tray and remove the old service (waiting for it to stop)
before replacing files. Uninstall stops the tray, waits for the service to
stop (final save), removes the `Run` value and Program Files, and keeps
`%ProgramData%\Varde` (saves, machine identity) unless the user answers
Yes to "Also delete Varde's data on this PC?" (default No; silent uninstall
always keeps it).

Silent install: `VardeSetup.exe /VERYSILENT /CPURL=https://cp.example`
(then link with `varde-agent enroll --token …`, or from the tray).

```sh
iscc /DVersion=0.1.0 packaging/windows/varde-agent.iss
```

expects `packaging/windows/dist/` to contain `varde-agent.exe`,
`varde-mesh.exe`, `varde-tray.exe` and `MicrosoftEdgeWebview2Setup.exe`
(the Evergreen bootstrapper). CI job `windows-installer` builds them (MSVC
toolchain for the Rust binaries), downloads the bootstrapper, runs ISCC via
`choco install innosetup`, and uploads `VardeSetup-*` as an artifact.
