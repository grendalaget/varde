# Game driver docs

Per-game driver notes (runtime requirements, config schema, snapshot
include/exclude lists) land here as each driver is implemented.

## Valheim Dedicated

Valheim installs SteamCMD app 896660. On Linux, the host needs glibc 2.29 or
newer and the runtime shared libraries `libatomic1` and `libpulse0`
(`libpulse-dev` is also listed by the official guide). The Valheim driver
checks glibc and uses `ldd` to report missing shared libraries during prepare;
package recommendations are weak dependencies so hosts that only run
Minecraft do not install them automatically.

The `modifiers` setting accepts `normal`, `casual`, `easy`, `hard`,
`hardcore`, `immersive`, or `hammer` and maps to Valheim's capitalized
`-preset` argument. `save_interval_s` defaults to 300 seconds and accepts
60–3600. Shorter intervals reduce progress loss after a hard kill, at the cost
of more frequent world writes. The maximum expected loss on a hard kill is
approximately the configured save interval.

Snapshots include only the selected world's `_main.*.db2`, `_main.*.fwl2`,
`_main.*.chunks`, `_main.*.ok`, and `*.chunk` files under
`saves/worlds_local/<world_name>/`, plus `saves/*.txt`. The world name is the
directory name (including spaces); current Valheim saves use numbered DB2/FWL2
files and chunk data rather than `<world_name>.db`/`.fwl`. Valheim logs save
progress as `World save (n/5)` phases and completion as `World save (5/5)
done`; the barrier is immediate unless the output tail shows a later phase
after the most recent completion, in which case it waits up to 60 seconds for
phase 5.
Graceful shutdown takes the final snapshot only after Valheim exits and writes
its last save.

The Windows stop signal behavior remains unchanged; whether CTRL_BREAK
triggers a complete save before exit still needs separate validation.
