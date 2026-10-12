# Windows packaging

`apps/desktop/installer-hooks/` (Tauri bundler, NSIS) builds
`VardeSetup-<ver>.exe` — the installer the auto-update mechanism also
applies silently. The old Inno script (`varde-agent.iss`) was retired
when the NSIS installer shipped; new installers silently uninstall the
Inno install first (Inno→NSIS migration shim in `hooks.nsh`), keeping
`%ProgramData%\Varde`. The installer:

- installs `varde-agent.exe`, `varde-mesh.exe` and `varde-tray.exe`
  (`apps/desktop`) into `Program Files\Varde`, plus a Start menu entry
  for the tray
- uses `varde.ico` for setup and uninstall and installs it with the binaries
- shows the brand art in the wizard: `wizard/wizard-100.bmp` (mark + wordmark
  on Natt) as the NSIS sidebar and `apps/desktop/installer-hooks/header.bmp`
  (mark on white, page header); the extra `wizard/wizard-*-*.bmp` DPI variants
  were Inno-only
- never asks for an address: the PC joins the hosted Varde (cp.varde.games).
  Self-hosting instead? `/CPURL=cp.example` (or a full https URL) writes
  `%ProgramData%\Varde\server.url`, which the unlinked service reports to
  the tray — skipped when the PC is already linked. A remote `http://`
  address asks before it is kept
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
- task "Control plane" (component, on by default like Inno's): installs
  `varde-control-plane.exe`, registers and starts `VardeControlPlane`,
  creates `%ProgramData%\VardeCP`, and makes `http://localhost:8080` this
  PC's default server
- at the end, launches the tray as the installing user with
  `--link`: the tray asks the service to link, shows the code and opens
  the dashboard to approve it

Upgrades stop the tray and remove the old service (waiting for it to stop)
before replacing files. Uninstall stops the tray, waits for the service to
stop (final save), removes the `Run` value and Program Files, and keeps
`%ProgramData%\Varde` (saves, machine identity) unless the user answers
Yes to "Also delete Varde's data on this PC?" (default No; silent uninstall
always keeps it).

Silent install: `VardeSetup.exe /S` (add `/CPURL=…` only for a self-hosted
control plane, `/TASKS="autostart"` to skip the control-plane component;
then link with `varde-agent enroll --token …`, or from the tray). The
silent path is what `tauri-plugin-updater` runs on update — a silent
install is a full drain-and-swap upgrade.

```sh
pnpm exec tauri bundle --config apps/desktop/tauri.bundler.conf.json --bundles nsis
```

expects `apps/desktop/payload/` to contain `varde-agent.exe`,
`varde-mesh.exe`, `varde-control-plane.exe`, `varde.ico` and `LICENSE`.
`tauri.bundler.conf.json` merges `bundle.resources` (the payload map) onto
`tauri.conf.json`; it lives outside the base config so plain dev builds and
clippy don't demand files that only exist after staging. The bundle config
is `apps/desktop/tauri.conf.json` (`bundle.windows.nsis`):
`installer.nsi` is the Tauri v2.12 installer template with a components page
added; `hooks.nsh` carries the service/registry/ACL/migration logic.
WebView2's bootstrapper is embedded (`embedBootstrapper`) and only runs
when WebView2 is missing — same behavior as the Inno installer. CI builds
the exes, stages `payload/`, runs `tauri bundle --bundles nsis`,
and signs `VardeSetup-*` as the release asset.

## Authenticode signing

Release and nightly pipelines sign all Windows executables and the installer
via **Azure Artifact Signing** (~$10/month). The build signs when the
`AZURE_*` configuration below is complete — skipped, with a workflow-summary
note, when unconfigured and always for `channel: dry-run` builds such as the
PR release dry run. The publisher name is the validated organization's legal
name (the free SignPath Foundation application was declined).

With signing configured:

1. `dist/varde-*.exe` are signed, then the NSIS bundle packages the signed
   executables.
2. `VardeSetup-<ver>.exe` is signed; it is the only Windows asset (no zip
   of raw binaries is published).

SHA256SUMS and cosign/Sigstore signing still run last, covering the signed
payloads.

### Setup: Azure Artifact Signing

Setup:

1. In the Azure portal: create an *Artifact Signing* account, complete
   identity validation for the organization (can take a few business days),
   and create a *Public Trust* certificate profile on the account.
2. Create an Entra app registration with a **federated credential** for this
   repository (OIDC — no client secret needed): entity type **Environment**,
   name `release` — the build job pins `environment: release` so tag
   releases, nightlies and dispatches all share one subject —
   `repo:grendalaget/varde:environment:release`. Then assign the app the
   *Artifact Signing Certificate Profile Signer* role on the account.
3. In this repo's Settings → Secrets and variables → Actions, add secrets
   `AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and `AZURE_SUBSCRIPTION_ID`, and
   variables `AZURE_TSA_ENDPOINT` (region endpoint, e.g.
   `https://weu.codesigning.azure.net/`), `AZURE_TSA_ACCOUNT` and
   `AZURE_TSA_CERT_PROFILE`.

Verify a signed file locally:

```powershell
Get-AuthenticodeSignature .\VardeSetup-1.2.3.exe
# Status: Valid — signer certificate subject names the provider
```

A signed binary is not instantly trusted by SmartScreen — per-file download
reputation still applies — but the warning no longer shows an unknown
publisher and clears faster under the certificate's reputation.
