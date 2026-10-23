# Game driver docs

Per-game driver notes (runtime requirements, config schema, snapshot
include/exclude lists) land here as each driver is implemented.

## Counter-Strike 2 Dedicated

CS2 runs the dedicated binary included in an existing Steam app 730 client
installation on Linux/Windows x86_64. No second 69 GB download is needed.
Discovery reads complete (`StateFlags == 4`) app manifests and
`steamapps/libraryfolders.vdf`, including additional Steam libraries.
Linux roots are `~/.local/share/Steam`, `~/.steam/steam`, and Flatpak's
`~/.var/app/com.valvesoftware.Steam/.local/share/Steam`. Active systemd login
sessions also supply desktop users' homes when visible to the agent.
Windows checks HKCU/HKLM `Software\Valve\Steam` (`SteamPath`/`InstallPath`,
both registry views) and `C:\Program Files (x86)\Steam`. macOS is unsupported.
Set `VARDE_CS2_DIR` to a game install to override discovery, or
`VARDE_STEAM_DIR` to add a Steam root (useful for nonstandard installations).

The packaged Linux service runs as `varde` with `ProtectHome=true`; it cannot
read desktop home directories by default. Run the agent as the desktop user,
or have an administrator expose the game and Steam libraries read-only outside
the protected home (e.g. a service `BindReadOnlyPaths` override), then set the
override in that service's environment. The driver never relaxes permissions.

Heartbeats report generic `steam_apps` IDs, not deployment-cache IDs. The
agent also recognizes complete SteamCMD installations in previous local
deployment directories and can reuse them. The catalog's optional `steam_app`
policy prefers installed nodes (+120 score) and removes the `min_disk_mb`
install budget and SteamCMD runtime requirement for those nodes. Restore and
deployment artifact space checks still apply. Other nodes retain the SteamCMD
fallback and 75 GB install budget; incomplete downloads count allocated bytes
already on disk, and routine complete-install updates skip full validation.
Fallback Linux hosts need SteamCMD's 32-bit runtime (`lib32gcc-s1` on Debian/Ubuntu).

The driver launches the original binary with
`-game <deployment>/cs2-runtime/csgo`. A private `gameinfo.gi` mounts the
installed content while configuration, bans, logs and runtime writes stay in
that deployment. Private empty `autoexec.cfg`/`server.cfg` prevent execution
of the player's own startup configuration. Driver-generated files stay outside
steamapps, so there is no shared config to clean up on stop. CS2 itself creates
or rewrites `steam_appid.txt` beside its executable and rewrites
`game/csgo/cfg/boot.vcfg` without changing its content. The gated test seeds
nonempty client-style boot settings and requires byte-identical content after
both servers stop. It also verifies that the app-ID marker contains `730` and
that all other installed files retain their sizes and modification times.
Linux library search uses
the binary directory plus an existing `steamclient.so` from that directory,
Steam's `linux64`, or `~/.steam/sdk64`. No library is copied into the client
install. The SteamCMD fallback retains its deployment-local library copy.

In CS2 open the console and run `connect <address>` using the Varde address.
Only UDP 27015 is exposed through the mesh, not RCON or SourceTV. CS2's UDP
socket can remain wildcard-bound despite loopback settings; the driver uses
the agent-allocated port and local UDP health checks. Real gameplay over the
mesh and gameplay packet sizes still need client validation.

Modes: casual, competitive (default), wingman, arms_race and deathmatch.
The default map is `de_dust2`, player limit 10 (2–64), and bot quota 0 (0–64).
Hostname, password and bot quota are written to private generated `varde.cfg`, not
process arguments. Optional GSLT is passed via `+sv_setsteamaccount`, so it is
visible to local process inspection. Catalog secret fields mask inputs, not
stored/API configuration. Without a GSLT the server reports LAN-only operation.

Readiness requires an A2S_INFO reply with the configured map and nonzero
player capacity. Stop uses stdin `quit`, falling back to an interrupt after
five seconds (or immediately if stdin is unavailable).
There is no world save: only `game/csgo/cfg/banned_user.cfg` and `banned_ip.cfg`
are snapshotted. The live snapshot hook copies these files into the state
directory without stopping the game; configure restores and executes them.
For shared installs these are the private runtime's ban files, never the
player's files. As with standalone CS2, `writeid`/`writeip` save in-memory bans;
the driver snapshots those saved files rather than issuing administrative commands.
Generated configuration, Steam content, logs, demos and plugins are excluded.

Real Linux test (two concurrently healthy instances, separate ports, no
SteamCMD calls, and an assertion that the shared installation is unchanged):

```sh
VARDE_NET_TESTS=1 VARDE_CS2_TEST=1 \
VARDE_STEAM_DIR=/path/to/Steam \
cargo test -p game-cs2 --test net -- --nocapture
```

Alternatively use `VARDE_CS2_DIR=/path/to/install`. The gated test requires an
existing install; normal driver operation still supports SteamCMD fallback.

## Valheim Dedicated

Valheim installs SteamCMD app 896660. On Linux, the host needs glibc 2.29 or
newer and the runtime shared libraries `libatomic1` and `libpulse0`
(`libpulse-dev` is also listed by the official guide). The Valheim driver
checks glibc and uses `ldd` to report missing linked libraries during prepare.
A separate `ldconfig -p` check warns, without blocking prepare, if the
runtime-loaded `libatomic.so.1` or `libpulse.so.0` is unavailable. Package
recommendations are weak dependencies so hosts that only run Minecraft do not
install them automatically.

The `modifiers` setting accepts `normal`, `casual`, `easy`, `hard`,
`hardcore`, `immersive`, or `hammer` and maps to Valheim's capitalized
`-preset` argument. `save_interval_s` defaults to 300 seconds and accepts
60–3600. Shorter intervals reduce progress loss after a hard kill, at the cost
of more frequent world writes. The maximum expected loss on a hard kill is
approximately the configured save interval.

Snapshots include legacy `saves/worlds_local/<world_name>.db` and
`saves/worlds_local/<world_name>.fwl` files, plus the selected world's
`_main.*.db2`, `_main.*.fwl2`,
`_main.*.chunks`, `_main.*.ok`, and `*.chunk` files under
`saves/worlds_local/<world_name>/`, and `saves/*.txt`. The world name is the
directory name (including spaces); current Valheim saves use numbered DB2/FWL2
files and chunk data. Valheim logs save
progress as `World save (n/5)` phases and completion as `World save (5/5)
done`; the barrier is immediate unless the output tail shows a later phase
after the most recent completion, in which case it waits up to 60 seconds for
phase 5.
Valheim opts into `snapshot_after_stop`, so the agent skips its snapshot barrier
and snapshots after graceful stop. Drivers whose barrier returns
`RequiresStop` also snapshot after stopping. Both paths require a clean exit
(status 0, no signal) within 30 seconds and an unfenced execution; otherwise
the final snapshot is skipped.

The Windows stop signal behavior remains unchanged; whether CTRL_BREAK
triggers a complete save before exit still needs separate validation.

### Crossplay

Set `crossplay` to `true` to start Valheim with `-crossplay` and a
server-specific `-instanceid`. Players join through Valheim's **Join game →
Join by code** flow instead of using the Varde address. Varde displays the
current join code on the server detail and list pages; it is read from the
Valheim server log and is updated as soon as the agent sees it.

Valheim printed a six-digit join code about two seconds after `Game server connected`, after an initial empty-code line. Across restarts with the same instance ID, port, server name and save directory, the code changed once (124841 → 589208) and stayed the same once (776580 on two quick restarts). Treat the code as able to change on any restart or move; Varde republishes whatever code the agent last saw.

Console clients have not been tested; manual PS5 and Xbox validation is still
needed.
