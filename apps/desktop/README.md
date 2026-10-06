# varde-tray (apps/desktop)

Windows tray app (Tauri 2). Runs as the logged-in user and talks only to the
local agent service over `agent-ipc` (`\\.\pipe\varde-agent`); the service
owns the identity, `config.toml` and the device-code flow. On other
platforms the binary is a stub, so the workspace builds everywhere.

- Tray icon: Tåke stones when linked; host stone lit Glød only while this PC
  hosts a server; yellow badge when not linked, offline or the service isn't
  running.
- Menu: status, hosted server + phase, latest safe save, Open dashboard,
  Link this PC… / Re-link… (relaunches elevated with `--relink`; refused by
  the service while hosting), Open logs folder, Start at login, Quit tray (the service keeps
  running).
- Link window (`ui/`, static HTML, no build step): Varde address → code,
  Copy code / Open again / Cancel, countdown, "Linked to <group> as <name>".
  Without WebView2 it falls back to message boxes.

The address comes from the service: its control-plane URL once linked, before
that the one the installer chose (`%ProgramData%\Varde\server.url`).

Flags: `--link` (installer; starts linking with the service's address), `--relink`,
`--autostart` (login; exits if the user turned it off).

```sh
cargo build --release -p varde-tray   # on Windows
```

`ui/fonts`: Schibsted Grotesk (OFL, see `ui/fonts/OFL.txt`).
