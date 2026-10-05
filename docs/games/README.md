# Game driver docs

Per-game driver notes (runtime requirements, config schema, snapshot
include/exclude lists) land here as each driver is implemented.

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
