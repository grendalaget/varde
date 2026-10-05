# Agent

`varde-agent` (Rust, tokio). One OS service: Windows Service (`windows-service` crate, accepts STOP + PRESHUTDOWN)
or systemd unit (`Type=notify` optional). Runs without a logged-in user. Subcommands: `run`, `enroll --server URL
[--token vde_…]` (device-code flow if no token: prints/opens the link), `status`, `service install|uninstall`
(Windows), `version`.

## Local layout (spec §35)

```
<data>/                     /var/lib/varde  |  %ProgramData%\Varde
  config.toml               control_plane_url, node_id, group_id, control_plane_public_key, overrides
  identity/node.key         ed25519 PKCS#8 PEM, 0600 / SYSTEM-only ACL
  run/mesh.sock             IPC (Linux)
  deployments/<dep_id>/     prepared game installs (cache, keyed by deployment digest)
  runtimes/                 downloaded JREs, steamcmd
  chunks/  snapshots/       snapshot-store
  servers/<srv_id>/         live working directory of a hosted server
  state/executions.json     last known executions + fencing status (survives agent restarts)
logs: /var/log/varde (Linux) | <data>\logs (Windows); JSON lines, rotated
```

## Main loops

* **Control loop**: heartbeat (`control-plane.md`) → apply directives: `SetPeers / SetRoutes / SetHostedServices /
  Configure(relays)` on the mesh; start/stop executions; schedule replication tasks; delete snapshots.
  Heartbeat failures back off (1 s → 5 s) but never block fencing.
* **Fencing watchdog**: per execution, deadline per `control-plane.md#leases-epochs-fencing`, checked on a monotonic
  clock every 250 ms. Expired ⇒ stop game (bounded graceful, then kill), mark `fenced`, drop from `SetHostedServices`,
  never upload its snapshots. On agent restart, executions found in `state/` are not resumed: running orphan game
  processes are killed (process groups / job objects make this reliable) and the CP decides afresh.
* **Execution supervisor** per execution: `prepare deployment → restore snapshot (if any) → driver.start → probe until
  healthy → running`; periodic snapshots every `snapshot_interval_s` and on `snapshot_requests`; restart policy:
  game crash ⇒ restart up to 3 times in 10 minutes, then report `failed`.
* **Mesh supervisor**: spawn, restart with backoff, re-apply all `Set*` state after restart.
* **Replication workers** (storage.md) and **chunk server**.

On shutdown the agent first drains executions (graceful stop, final snapshot, replication hold) while heartbeats,
the fencing watchdog, the mesh, the chunk server and the replication worker keep running — the node stays CP-visible
as a snapshot source and refuses new executions — and only then signals the loops to stop.

## Executor API (`crates/executor-api`)

```rust
pub struct ProcessSpec { pub program: PathBuf, pub args: Vec<OsString>, pub env: Vec<(OsString, OsString)>,
                         pub cwd: PathBuf, pub stdin: bool }
#[async_trait] pub trait Executor: Send + Sync {
    fn kind(&self) -> &'static str;                                   // "native"
    async fn spawn(&self, spec: &ProcessSpec) -> Result<Box<dyn ProcessHandle>>;
}
#[async_trait] pub trait ProcessHandle: Send + Sync {
    fn pid(&self) -> u32;
    async fn write_stdin(&self, line: &str) -> Result<()>;
    fn output(&self) -> broadcast::Receiver<OutputLine>;             // stdout/stderr lines; also kept in a ring buffer
    async fn wait(&self) -> Result<ExitStatus>;
    async fn terminate(&self) -> Result<()>;                         // SIGTERM to group / CTRL_BREAK + job
    async fn kill(&self) -> Result<()>;                              // SIGKILL group / TerminateJobObject
    fn resource_usage(&self) -> Option<ResourceUsage>;               // cpu %, rss (sysinfo)
}
```
`native` executor: Linux `setsid`/process group, Windows Job Object with `KILL_ON_JOB_CLOSE` and
`CREATE_NEW_PROCESS_GROUP`. Programs are always executed directly with argv — **never through a shell** (I9).
Runtime installers (`java` → Eclipse Temurin via Adoptium API, `steamcmd`) live in the agent and are invoked by drivers
through a `RuntimeProvider` with checksum verification.

## Game driver API (`crates/game-driver-api`)

```rust
pub struct DriverContext<'a> { pub server_dir: &'a Path, pub deployment_dir: &'a Path, pub runtimes: &'a dyn RuntimeProvider,
                               pub deployment: &'a DeploymentSpec, pub config: &'a serde_json::Value,
                               pub ports: &'a [PortBinding], pub memory_mb: u32 }
#[async_trait] pub trait GameDriver: Send + Sync {
    fn id(&self) -> &'static str;
    fn ports(&self, config: &Value) -> Vec<PortSpec>;   // service ports; ctx.ports maps each to an agent-allocated local port (ctx.local_port(p))
    fn persistent_paths(&self, config: &Value) -> Vec<PathPattern>;
    fn validate(&self, config: &Value) -> Result<()>;
    async fn prepare(&self, ctx: &DriverContext<'_>) -> Result<()>;                         // install into deployment_dir (idempotent)
    async fn configure(&self, ctx: &DriverContext<'_>) -> Result<()>;                       // write config files into server_dir
    fn process_spec(&self, ctx: &DriverContext<'_>) -> Result<ProcessSpec>;
    async fn probe(&self, ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<GameHealth>;
    async fn prepare_snapshot(&self, ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<SnapshotBarrier>; // Live | RequiresStop
    async fn resume_after_snapshot(&self, ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<()>;
    async fn graceful_stop(&self, ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<()>;
}
```
Drivers are compiled into the agent and registered in a `DriverRegistry` by id; the CP's catalog lists the same ids.

### Drivers

* **testgame** (`games/testgame`, also builds `varde-testgame`): a tiny server used by tests and the e2e demo.
  TCP line protocol on `:7777` (`INCR`, `GET`, `SET <n>`), UDP `:7777` (`GET` → value). State in `data/state.json`,
  autosaved; stdin understands `save-off`, `save-all` (prints `Saved the game`), `save-on`, `stop` — mirroring
  Minecraft so the barrier path is exercised.
* **minecraft** (Java edition): Java ≥ 21 detection or Temurin download; server jar from Mojang's version manifest
  (sha1 verified); `eula_accepted` must be true in config (dashboard checkbox) → `eula.txt`; manages
  `server.properties` (port 25565, `online-mode`, `motd`, `max-players`, `difficulty`, `gamemode`); `-Xmx` from
  memory. Barrier: stdin `save-off`, `save-all flush`, wait for `Saved the game`; resume: `save-on`; stop: `stop`.
  Health: log `Done (` + TCP connect. Persistent paths: `world*/`, `server.properties`, `ops.json`, `whitelist.json`,
  `banned-*.json`.
* **valheim**: SteamCMD app 896660 (`+force_install_dir … +login anonymous +app_update 896660 validate +quit`); start
  `valheim_server(.x86_64|.exe) -nographics -batchmode -name -port 2456 -world -password -public 0
  -saveinterval 300 -backups 0 -savedir <server_dir>/saves`, plus `-preset <Normal|Casual|Easy|Hard|Hardcore|Immersive|Hammer>`
  when `modifiers` is configured; ports 2456–2457/udp; password ≥ 5 chars; world names are non-empty and exclude
  `/`, `\`, `*`, and `?`. `save_interval_s` defaults to 300 and is validated to 60..=3600 seconds.
  Barrier: inspect the output tail for a `World save (n/5)` phase after the last `World save (5/5) done`;
  only then wait up to 60 s for phase 5, otherwise snapshots are live immediately. Persistent paths include the
  legacy `saves/worlds_local/<world_name>.db` and `saves/worlds_local/<world_name>.fwl` files, plus current
  `_main.*.db2`, `_main.*.fwl2`, `_main.*.chunks`, `_main.*.ok`, and `*.chunk` files under
  `saves/worlds_local/<world_name>/`, plus `saves/*.txt`.
  Stop: SIGINT (Linux) / CTRL_BREAK (Windows). The final snapshot is taken from disk after graceful stop and
  process exit; if the process must be killed or the execution is fenced, the final snapshot is skipped.
  Linux requires glibc ≥ 2.29; `ldd` checks linked libraries, while `ldconfig -p` warns if runtime-loaded
  `libatomic.so.1` or `libpulse.so.0` is missing (`libatomic1` and `libpulse0` packages).
