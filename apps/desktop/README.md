# varde-tray (apps/desktop)

Windows desktop app (Tauri 2). Runs as the logged-in user and talks only to
the local agent service over `agent-ipc` (`\\.\pipe\varde-agent`); the service
owns the identity, `config.toml` and the device-code flow. On other
platforms the binary is a stub, so the workspace builds everywhere.

- Tray icon: Tåke stones when linked; host stone lit Glød only while this PC
  hosts a server; yellow badge when not linked, offline or the service isn't
  running.
- Menu: status, hosted server + phase, latest safe save, Open dashboard,
  Link this PC… / Re-link… (relaunches elevated with `--relink`; refused by
  the service while hosting), Open logs folder, Start at login, Quit tray (the service keeps
  running).
- App window (`main`, Fluent 2 UI in `ui/`): Varde address → code,
  Copy code / Open again / Cancel, countdown, "Linked to <group> as <name>",
  this-PC status, service start/stop.
  An administrator's pending re-link shows "An administrator is re-linking this
  PC" instead of Cancel outside the elevated window; refused calls show inline.
  Without WebView2 it falls back to message boxes.
- Dashboard window (`dashboard`): the control-plane dashboard loaded as an
  external webview inside the app — the tray never opens a browser. It has no
  IPC access (remote), keeps its own cookies (sign-in persists), and navigates
  only when the control-plane origin changes (e.g. re-link).

The address comes from the service: its control-plane URL once linked, before
that the one the installer chose (`%ProgramData%\Varde\server.url`).

Flags: `--link` (installer; starts linking with the service's address), `--relink`,
`--autostart` (login; exits if the user turned it off).

## UI (`ui/`)

React + Vite + TypeScript on **Fluent 2** (`@fluentui/react-components` +
`@fluentui/react-icons`), dark-only Varde theme (`src/theme.ts`: Natt
background, Skifer surfaces, Tåke text/primary — Glød stays off, it's the
hosting signal). The tray embeds the built bundle (`frontendDist: ui/dist`),
so `pnpm --dir apps/desktop/ui build` must run before `cargo build`.

```sh
pnpm --dir apps/desktop/ui dev      # browser dev server (mock statuses)
pnpm --dir apps/desktop/ui build
```

In a plain browser the UI runs on a mock bridge; pick a canned status with
`?state=not_linked|linking|online|hosting|offline|locked|noagent` (`locked`
shows the "an administrator is re-linking this PC" state).

`ui/src/assets/fonts`: Schibsted Grotesk (OFL, see `fonts/OFL.txt`).

```sh
cargo build --release -p varde-tray   # on Windows, after the UI build above
```
