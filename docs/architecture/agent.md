# Agent

`varde-agent` (Rust, tokio). One OS service: Windows Service (`windows-service` crate, accepts STOP + PRESHUTDOWN)
or systemd unit (`Type=notify` optional). Runs without a logged-in user. Subcommands: `run`, `enroll --server URL
[--token vde_…]` (device-code flow if no token: prints the link), `status`, `service install|uninstall`
(Windows), `version`.

## Local layout (spec §35)

```
<data>/                     /var/lib/varde  |  %ProgramData%\Varde
  config.toml               control_plane_url, node_id, group_id, control_plane_public_key, overrides
  identity/node.key         ed25519 PKCS#8 PEM, 0600 / SYSTEM-only ACL
  run/mesh.sock             mesh IPC (Linux; Windows: \\.\pipe\varde-mesh-<hash>)
  run/agent.sock            local status/link IPC (Linux; Windows: \\.\pipe\varde-agent)
  deployments/<dep_id>/     prepared game installs (cache, keyed by deployment digest)
  runtimes/                 downloaded JREs, steamcmd
  chunks/  snapshots/       snapshot-store
  servers/<srv_id>/         live working directory of a hosted server
  state/executions.json     last known executions + fencing status (survives agent restarts)
logs: /var/log/varde (Linux) | <data>\logs (Windows); JSON lines, rotated
```

## Lifecycle: not linked → linked

The service starts and stays running whether or not the machine is linked (I10). Without `config.toml` it is
**not linked**: only the local IPC is up — no heartbeats, no mesh, no games — and it waits for a config to appear
(polled every 2 s, or signalled by its own link flow). Once the config exists the full agent starts in the same
process; no service restart. A missing config is never a failure. Real failures stop the service with a non-zero
service-specific exit code; `service install` sets SCM restart actions (5 s, 15 s, 60 s; reset after a day, also
for non-crash failures). `service uninstall` waits (≤ 180 s) for the service to reach Stopped before deleting it,
so the final save + replication hold finish before the installer removes the binaries.

Two ways to link, both writing the same `config.toml` + `identity/node.key`:

* **Desktop (tray)**: the tray runs as the logged-in user and can't write `%ProgramData%\Varde`, so it asks the
  service over the local IPC (`StartLink{control_plane_url}`). The service runs the device-code flow itself,
  publishes the code + link page in its status, polls for approval and writes the config. The link page is the
  control plane's `verification_url`, except when that points at loopback while the user's address doesn't
  (control plane without `--public-url`): then it is `<user's address>/link?code=…`. The control plane itself
  falls back to the request's scheme + Host when `--public-url` is unset. Before linking, status reports the
  address the installer wrote to `<data>/server.url`, so the tray can start linking without asking again.
* **Headless**: `varde-agent enroll --server URL [--token vde_…]` (needs Administrator/root); the idle service
  picks the config up within 2 s; a running agent notices a config for a different node within 5 s and
  restarts in-process as that node.

The device name sent with the link request is `%COMPUTERNAME%` on Windows, else `$HOSTNAME` / `gethostname()`.

### Local IPC (`proto/agent/v1/local.proto`, `crates/agent-ipc`)

gRPC over the named pipe `\\.\pipe\varde-agent` (Windows) or `<data>/run/agent.sock` (Unix, 0666). Never TCP.
A fixed set of typed calls — `GetStatus`, `WatchStatus`, `StartLink`, `CancelLink` — and no command channel (I9).
Status carries the state (`not_linked | linking | connecting | online | offline | shutting_down`; online = a
heartbeat succeeded within max(3 × heartbeat interval, 15 s)), control-plane URL, group and machine names, last
contact, the pending code, and each server this machine hosts with its phase and latest safe save
(`node.group_name` / `executions[].latest_safe_save_at_unix_ms` in directives).

* Pipe DACL (protected): SYSTEM + Administrators full; interactive users read + write data only, without
  FILE_CREATE_PIPE_INSTANCE. Remote clients are rejected and the service creates the first instance itself.
  Clients open with identification-level impersonation and refuse a pipe not served from session 0.
* Linking an unlinked machine is open to any interactive user (the installer flow). **Re-link**
  (`relink=true`) needs a client whose process token is in BUILTIN\Administrators (elevated; Unix: root or the
  agent's user) and is refused while the machine hosts a server (checked again when approval arrives). It uses
  a fresh node key, written only once approved (the old key is restored if the config can't be written), keeps
  local overrides, and restarts the agent in-process as the new node. Only an administrator can cancel a
  pending re-link; a newer StartLink/CancelLink always supersedes one still contacting the control plane.

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
`CREATE_NEW_PROCESS_GROUP`. A consoleless agent (Windows service) allocates a hidden console before spawning so
CTRL_BREAK can reach the child's process group. Programs are always executed directly with argv — **never through a shell** (I9).
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
  Stop: SIGINT (Linux) / CTRL_BREAK (Windows). Valheim opts into `snapshot_after_stop`, so the agent skips the
  barrier and snapshots after graceful stop. Generic `RequiresStop` barriers also snapshot after stopping. Both paths
  require a clean exit (status 0, no signal) within 30 s and an unfenced execution; otherwise the final snapshot is
  skipped.
  Linux requires glibc ≥ 2.29; `ldd` checks linked libraries, while `ldconfig -p` warns if runtime-loaded
  `libatomic.so.1` or `libpulse.so.0` is missing (`libatomic1` and `libpulse0` packages).

For live snapshots, the store rejects a file set that changes while it is read. The agent resumes the game and retries
the full barrier-and-snapshot attempt up to three times, so drivers such as Valheim can wait for an in-progress save
to finish on the next barrier. A changing live save during graceful stop instead falls back to the existing
stop-first, clean-exit snapshot path.
