# varde-tray (apps/desktop)

The Varde desktop app (Tauri 2) for Windows and macOS. Runs as the logged-in
user and talks only to the local agent service over `agent-ipc`
(`\\.\pipe\varde-agent` / unix socket); the service owns the identity,
`config.toml` and the device-code flow. On other platforms the binary is a
stub, so the workspace builds everywhere.

Users see exactly one app: one installer, one tray icon while running, one
window.

- Tray icon: Tåke stones when linked; host stone lit Glød only while this PC
  hosts a server; yellow badge when not linked, offline or the service isn't
  running. Clicking (or double-clicking) the icon and the menu's "Open Varde"
  both raise the window.
- Menu: status, hosted server + phase, latest safe save, Open Varde, Link
  this PC… / Re-link… (relaunches elevated with `--relink`; refused by the
  service while hosting), Open logs folder, Check for updates…, Start at
  login, Send crash reports, Exit Varde.
- **The window** is a single surface with two faces — only one is ever
  visible, so it reads as one window that changes content:
  - *Setup face* (`main`, mouve design-system UI in `ui/`): Varde address →
    code, Copy code / Open again / Cancel, countdown, federated
    "Log in with Varde", "Linked to <group> as <name>", "Varde isn't
    running" with a Start button. An administrator's pending re-link shows
    "An administrator is re-linking this PC" instead of Cancel outside the
    elevated window. Without WebView2 it falls back to message boxes.
  - *Dashboard face* (`dashboard`): the control-plane dashboard loaded as an
    external webview inside the same window — the tray never opens a
    browser. It has no IPC access (remote), keeps its own cookies (sign-in
    persists), and navigates only when the control-plane origin changes
    (e.g. re-link).
  - The face is picked by the PC's state (`view::facet`): linked and
    reachable → dashboard; linking, signing in, pending approval, offline or
    service-down → setup. Finishing a link morphs the window into the
    dashboard. On macOS the Dock icon appears only while a window is open.
- Closing the window minimizes to the tray (the webview stays alive, so the
  dashboard keeps its place and sign-in). **Exit Varde** is the only way
  out: it confirms first while this PC is hosting, drains and stops the
  service — the mesh goes with it — and quits the app.

The address comes from the service: its control-plane URL once linked, before
that the one the installer chose (`%ProgramData%\Varde\server.url`).

Flags: `--link` (installer; opens the window and starts linking with the
service's address), `--relink`, `--autostart` (login; exits if the user
turned it off), `--service-start`/`--service-stop` (elevated headless service
control on Windows).

## UI (`ui/`)

React + Vite + TypeScript on the shared Varde design system
(`@heroui/react` + Tailwind + the vendored mouve theme, same as
`apps/web`): dark-only, Natt background, Tåke text, Glød accent. The tray
embeds the built bundle (`frontendDist: ui/dist`),
so `bun --cwd apps/desktop/ui run build` must run before `cargo build`.

```sh
bun --cwd apps/desktop/ui run dev      # browser dev server (mock statuses)
bun --cwd apps/desktop/ui run build
```

In a plain browser the UI runs on a mock bridge; pick a canned status with
`?state=not_linked|linking|online|hosting|offline|locked|noagent` (`locked`
shows the "an administrator is re-linking this PC" state).

`ui/src/assets/fonts`: Schibsted Grotesk (OFL, see `fonts/OFL.txt`).

```sh
cargo build --release -p varde-tray   # on Windows/macOS, after the UI build above
```
