//! varde-agent: local node agent. Owns identity, enrollment, the control
//! loop, execution supervision, the snapshot store and the chunk server, and
//! supervises the Go mesh subprocess over IPC.

mod agent;
mod chunks;
mod config;
mod control;
mod exec;
mod identity;
mod link;
mod local;
mod mesh_child;
mod mesh_ctl;
mod state;
mod sysinfo;

pub use agent::Agent;

use std::collections::BTreeSet;
use std::path::PathBuf;
use std::sync::atomic::AtomicBool;
use std::sync::{Arc, Mutex};

use anyhow::{bail, Context, Result};
use clap::{Parser, Subcommand};
use ed25519_dalek::SigningKey;
use tracing::info;

#[derive(Parser)]
#[command(name = "varde-agent", version, about = "varde local node agent")]
struct Cli {
    #[command(subcommand)]
    command: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// Enroll this node: device flow (default) or --token.
    Enroll {
        /// Control-plane address; defaults to the installer's server.url,
        /// then the hosted Varde.
        #[arg(long)]
        server: Option<String>,
        #[arg(long)]
        token: Option<String>,
        #[arg(long, env = "VARDE_DATA_DIR")]
        data_dir: Option<PathBuf>,
        /// Record this node as an anchor in the local config.
        #[arg(long)]
        anchor: bool,
        /// Test-only loopback prefix (e.g. 127.78.)
        #[arg(long)]
        loopback_prefix: Option<String>,
        /// Fencing margin subtracted from the lease window (ms, default 5000).
        #[arg(long)]
        fence_margin_ms: Option<i64>,
    },
    /// Run the agent in the foreground.
    Run {
        #[arg(long, env = "VARDE_DATA_DIR")]
        data_dir: Option<PathBuf>,
        #[arg(long, env = "VARDE_MESH_BIN")]
        mesh_bin: Option<PathBuf>,
        /// Treat config anchor=true as already set; flag form per agent.md.
        #[arg(long)]
        anchor: bool,
        /// Mark the node to only serve data to the mesh (force_relay).
        #[arg(long)]
        force_relay: bool,
        /// Override fencing margin from config (ms).
        #[arg(long)]
        fence_margin_ms: Option<i64>,
    },
    /// Show enrollment/config status.
    Status {
        #[arg(long, env = "VARDE_DATA_DIR")]
        data_dir: Option<PathBuf>,
    },
    /// Manage the Windows service (install/uninstall as SYSTEM).
    Service {
        #[command(subcommand)]
        action: ServiceAction,
    },
}

#[derive(Subcommand)]
enum ServiceAction {
    /// Register the agent as a Windows service (auto-start).
    Install,
    /// Remove the Windows service registration.
    Uninstall,
    /// Run under the service control manager (invoked by SCM).
    #[command(hide = true)]
    Run,
}

fn default_data_dir() -> PathBuf {
    if cfg!(windows) {
        std::env::var_os("ProgramData")
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from(r"C:\ProgramData"))
            .join("Varde")
    } else {
        PathBuf::from("/var/lib/varde")
    }
}

fn default_mesh_bin() -> Result<PathBuf> {
    let exe = std::env::current_exe().context("resolve agent exe path")?;
    let name = if cfg!(windows) {
        "varde-mesh.exe"
    } else {
        "varde-mesh"
    };
    let sibling = exe.with_file_name(name);
    if sibling.exists() {
        return Ok(sibling);
    }
    Ok(PathBuf::from("/usr/lib/varde").join(name))
}

#[cfg(windows)]
mod svc;

#[cfg(windows)]
async fn service_dispatch(action: ServiceAction) -> Result<()> {
    match action {
        ServiceAction::Install => svc::install(),
        ServiceAction::Uninstall => svc::uninstall(),
        ServiceAction::Run => svc::run_service(),
    }
}

#[cfg(not(windows))]
async fn service_dispatch(_action: ServiceAction) -> Result<()> {
    anyhow::bail!("service management is only supported on Windows")
}

pub(crate) fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// JSON lines on stdout. Nothing reads a service's stdout, so under the SCM
/// the agent writes daily files to <data>\logs instead, keeping a week.
#[cfg_attr(not(windows), allow(unused_variables))]
fn init_logging(cmd: &Cmd) {
    let filter =
        || tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into());
    #[cfg(windows)]
    if matches!(
        cmd,
        Cmd::Service {
            action: ServiceAction::Run
        }
    ) {
        use tracing_appender::rolling::{Builder, Rotation};
        match Builder::new()
            .rotation(Rotation::DAILY)
            .filename_prefix("agent")
            .filename_suffix("log")
            .max_log_files(7)
            .build(default_data_dir().join("logs"))
        {
            Ok(file) => {
                tracing_subscriber::fmt()
                    .json()
                    .with_ansi(false)
                    .with_writer(file)
                    .with_env_filter(filter())
                    .init();
                return;
            }
            Err(e) => eprintln!("log files unavailable, logging to stdout: {e}"),
        }
    }
    tracing_subscriber::fmt()
        .json()
        .with_env_filter(filter())
        .init();
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    init_logging(&cli.command);

    match cli.command {
        Cmd::Enroll {
            server,
            token,
            data_dir,
            anchor,
            loopback_prefix,
            fence_margin_ms,
        } => {
            let data_dir = data_dir.unwrap_or_else(default_data_dir);
            let server = server
                .or_else(|| link::preset_url(&data_dir))
                .unwrap_or_else(|| agent_ipc::DEFAULT_CP_URL.into());
            enroll(
                server,
                token,
                data_dir,
                anchor,
                loopback_prefix,
                fence_margin_ms,
            )
            .await
        }
        Cmd::Run {
            data_dir,
            mesh_bin,
            anchor,
            force_relay,
            fence_margin_ms,
        } => {
            run(
                data_dir.unwrap_or_else(default_data_dir),
                mesh_bin,
                anchor,
                force_relay,
                fence_margin_ms,
                None,
            )
            .await
        }
        Cmd::Status { data_dir } => status(data_dir.unwrap_or_else(default_data_dir)),
        Cmd::Service { action } => service_dispatch(action).await,
    }
}

// ---------------- enroll ----------------

async fn enroll(
    server: String,
    token: Option<String>,
    data_dir: PathBuf,
    anchor: bool,
    loopback_prefix: Option<String>,
    fence_margin_ms: Option<i64>,
) -> Result<()> {
    std::fs::create_dir_all(&data_dir)?;
    let server = link::normalize_url(&server)?;
    let key_path = link::key_path(&data_dir);
    identity::load_or_create(&key_path)?;
    let key = identity::load(&key_path)?;
    let public_key = identity::public_key_b64(&key);

    let client = cp_api::CpClient::new(&server)?;

    let result: cp_api::EnrollResult = if let Some(token) = token {
        client
            .json(
                "POST",
                "/v1/agent/enroll/token",
                Some(&cp_api::TokenEnrollRequest {
                    token,
                    public_key,
                    hostname: Some(link::hostname()),
                    os: Some(std::env::consts::OS.into()),
                    arch: Some(std::env::consts::ARCH.into()),
                    agent_version: Some(env!("CARGO_PKG_VERSION").into()),
                }),
            )
            .await?
    } else {
        // device flow: POST device → print code → poll
        let dev = link::start_device(&client, public_key).await?;
        println!(
            "Visit {} and enter code: {}",
            link::link_url(&server, &dev),
            dev.user_code
        );
        let deadline = now_ms() + dev.expires_in * 1000;
        loop {
            if now_ms() > deadline {
                bail!("device code expired");
            }
            tokio::time::sleep(std::time::Duration::from_secs(dev.interval.max(1) as u64)).await;
            match link::poll_device(&client, &dev.device_code).await? {
                link::Poll::Approved(r) => break r,
                link::Poll::Pending => continue,
                link::Poll::Expired => bail!("device code expired"),
            }
        }
    };

    let mut cfg = link::enrolled_config(&server, &result, None);
    cfg.anchor = anchor;
    cfg.loopback_prefix = loopback_prefix;
    cfg.fence_margin_ms = fence_margin_ms.unwrap_or(5000);
    cfg.save(&data_dir)?;
    info!(node_id = %result.node_id, group = %result.group_id, "enrolled; wrote config.toml");
    Ok(())
}

fn status(data_dir: PathBuf) -> Result<()> {
    match config::Config::load(&data_dir) {
        Ok(c) => {
            println!("node_id: {}", c.node_id);
            println!("group_id: {}", c.group_id);
            println!("control_plane: {}", c.control_plane_url);
            println!("anchor: {}", c.anchor);
            Ok(())
        }
        Err(e) => bail!("{e:#}"),
    }
}

// ---------------- run ----------------

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RunExit {
    Stopped,
    Relinked,
}

/// Runs until stopped. Not linked yet (no config.toml) is a normal, idle
/// state: the local IPC is up, nothing else runs, and the agent starts as soon
/// as a link (tray over IPC, or `varde-agent enroll`) writes the config.
async fn run(
    data_dir: PathBuf,
    mesh_bin: Option<PathBuf>,
    anchor_flag: bool,
    force_relay: bool,
    fence_margin_ms: Option<i64>,
    ext_stop: Option<tokio::sync::oneshot::Receiver<()>>,
) -> Result<()> {
    std::fs::create_dir_all(&data_dir)
        .with_context(|| format!("create data dir {}", data_dir.display()))?;
    let hub = local::Hub::new(data_dir.clone());
    match local::serve(hub.clone()) {
        Ok(ep) => info!(endpoint = %ep, "local status IPC listening"),
        Err(e) => tracing::warn!(error = %e, "local status IPC unavailable"),
    }
    let (stop_tx, stop_rx) = tokio::sync::watch::channel(false);
    tokio::spawn(async move {
        wait_shutdown(ext_stop).await;
        let _ = stop_tx.send(true);
    });

    loop {
        let Some(cfg) = wait_linked(&data_dir, &hub, stop_rx.clone()).await? else {
            hub.set_shutting_down();
            info!("agent stopped");
            return Ok(());
        };
        let exit = run_linked(
            cfg,
            &data_dir,
            mesh_bin.clone(),
            anchor_flag,
            force_relay,
            fence_margin_ms,
            &hub,
            stop_rx.clone(),
        )
        .await?;
        match exit {
            RunExit::Stopped => return Ok(()),
            RunExit::Relinked => info!("re-linked; restarting with the new config"),
        }
    }
}

/// Waits for config.toml; `None` when stopped first.
async fn wait_linked(
    data_dir: &std::path::Path,
    hub: &local::Hub,
    mut stop: tokio::sync::watch::Receiver<bool>,
) -> Result<Option<config::Config>> {
    let mut logged = false;
    loop {
        if *stop.borrow() {
            return Ok(None);
        }
        if let Some(c) = config::Config::load_opt(data_dir)? {
            return Ok(Some(c));
        }
        if !logged {
            info!(data = %data_dir.display(), "not linked yet: waiting (link from the Varde tray, or run `varde-agent enroll`)");
            logged = true;
        }
        tokio::select! {
            r = stop.changed() => if r.is_err() { return Ok(None) },
            _ = hub.linked.notified() => {},
            _ = tokio::time::sleep(std::time::Duration::from_secs(2)) => {},
        }
    }
}

#[allow(clippy::too_many_arguments)]
async fn run_linked(
    mut cfg: config::Config,
    data_dir: &std::path::Path,
    mesh_bin: Option<PathBuf>,
    anchor_flag: bool,
    force_relay: bool,
    fence_margin_ms: Option<i64>,
    hub: &local::Hub,
    mut ext_stop: tokio::sync::watch::Receiver<bool>,
) -> Result<RunExit> {
    let data_dir = data_dir.to_path_buf();
    if anchor_flag {
        cfg.anchor = true;
    }
    if force_relay {
        cfg.force_relay = true;
    }
    if let Some(m) = fence_margin_ms {
        cfg.fence_margin_ms = m;
    }
    let key_path = link::key_path(&data_dir);
    let key: SigningKey = identity::load(&key_path)?;
    let mesh_bin = mesh_bin
        .or(cfg.mesh_bin.clone())
        .unwrap_or_else(|| default_mesh_bin().unwrap_or_default());
    info!(node_id = %cfg.node_id, data = %data_dir.display(), mesh = %mesh_bin.display(), "agent starting");

    // storage
    let store = Arc::new(snapshot_store::Store::open(data_dir.join("storage"))?);

    // drivers
    let mut drivers = game_driver_api::DriverRegistry::new();
    let tg = match &cfg.testgame_bin {
        Some(b) => game_testgame::TestgameDriver::with_binary(b.clone()),
        None => game_testgame::TestgameDriver::new(),
    };
    drivers.register(Box::new(tg));
    drivers.register(Box::new(game_minecraft::MinecraftDriver::new()));
    drivers.register(Box::new(game_valheim::ValheimDriver::new()));
    let runtimes = Arc::new(runtimes::HttpRuntimes::new(data_dir.join("runtimes")));

    // mesh
    let ipc = mesh_ipc::ipc_endpoint(&data_dir);
    let mesh = Arc::new(mesh_ctl::MeshCtl::new(ipc));

    // kill orphans from previous run; never resume them
    let mut exec_state = state::ExecState::load(&data_dir);
    exec_state.reap_orphans();

    let cp = cp_api::CpClient::new(&cfg.control_plane_url)?
        .with_node_key(key.clone(), cfg.node_id.clone());

    let agent = Arc::new(Agent {
        cfg: cfg.clone(),
        data_dir: data_dir.clone(),
        key,
        key_path,
        cp,
        store,
        drivers,
        runtimes,
        executor: executor_native::NativeExecutor,
        mesh: mesh.clone(),
        execs: Mutex::new(std::collections::HashMap::new()),
        state: Mutex::new(exec_state),
        pending_delete_acks: Mutex::new(BTreeSet::new()),
        chunk_tracker: Arc::new(chunks::ServeTracker::default()),
        started_at_ms: now_ms(),
        repl: Mutex::new(agent::ReplQueue::default()),
        repl_notify: tokio::sync::Notify::new(),
        last_heartbeat_ok: Mutex::new(None),
        shutting_down: AtomicBool::new(false),
        cp_view: Mutex::new(Default::default()),
    });
    hub.attach(agent.clone());

    let (stop_tx, stop_rx) = tokio::sync::watch::channel(false);

    // chunk server first so the port is known for SetInternalServices
    let chunk = chunks::serve(
        agent.store.clone(),
        agent.chunk_tracker.clone(),
        stop_rx.clone(),
    )
    .await?;
    info!(addr = %chunk.addr, "chunk server listening");
    mesh.set_internal(vec![mesh_ipc::pb::InternalService {
        name: chunks::SERVICE_NAME.into(),
        target_port: chunk.addr.port() as u32,
    }])
    .await?;

    // spawn background loops
    let mesh_data = data_dir.clone();
    let mesh_task = tokio::spawn(mesh.clone().supervise(mesh_data, mesh_bin, stop_rx.clone()));
    let a = agent.clone();
    let s = stop_rx.clone();
    tokio::spawn(async move { control::control_loop(a, s).await });
    let a = agent.clone();
    let s = stop_rx.clone();
    tokio::spawn(async move { control::fence_watchdog(a, s).await });
    let a = agent.clone();
    let s = stop_rx.clone();
    tokio::spawn(async move { a.replication_worker(s).await });

    // GC loop
    {
        let agent = agent.clone();
        let mut stop = stop_rx.clone();
        tokio::spawn(async move {
            let mut tick = tokio::time::interval(std::time::Duration::from_secs(3600));
            loop {
                tokio::select! {
                    _ = tick.tick() => {
                        let store = agent.store.clone();
                        let _ = tokio::task::spawn_blocking(move || store.gc()).await;
                    }
                    _ = stop.changed() => return,
                }
            }
        });
    }

    // graceful shutdown: drain executions FIRST (final snapshot + replication
    // hold) while heartbeats, the fencing watchdog, the mesh, the chunk server
    // and the replication worker keep running — the node must stay CP-visible
    // and reachable as a snapshot source for the hold to succeed. Only after
    // the drain do the background loops get their stop signal.
    let exit = loop {
        tokio::select! {
            _ = ext_stop.wait_for(|s| *s) => break RunExit::Stopped,
            // a re-link from the tray signals; `varde-agent enroll` only
            // rewrites config.toml, so also look every few seconds
            _ = hub.linked.notified() => {}
            _ = tokio::time::sleep(std::time::Duration::from_secs(5)) => {}
        }
        let relinked = config::Config::load_opt(&data_dir)
            .ok()
            .flatten()
            .is_some_and(|c| c.node_id != agent.cfg.node_id);
        if relinked {
            break RunExit::Relinked;
        }
    };
    if exit == RunExit::Stopped {
        hub.set_shutting_down();
    }
    info!("shutting down: draining executions");
    agent
        .shutting_down
        .store(true, std::sync::atomic::Ordering::SeqCst);
    let execs: Vec<Arc<exec::ExecCtl>> = agent.execs.lock().unwrap().values().cloned().collect();
    for c in execs {
        c.request_stop(exec::StopKind::Graceful);
    }
    let deadline = std::time::Instant::now()
        + std::time::Duration::from_secs(
            agent.cfg.shutdown_replication_timeout_s.max(5) as u64 + 30,
        );
    while std::time::Instant::now() < deadline {
        if agent
            .execs
            .lock()
            .unwrap()
            .values()
            .all(|c| c.is_finished())
        {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(200)).await;
    }

    let _ = stop_tx.send(true);
    // let the mesh child exit before a re-link spawns a new one
    let _ = tokio::time::timeout(std::time::Duration::from_secs(10), mesh_task).await;
    hub.detach();
    info!("agent stopped");
    Ok(exit)
}

async fn wait_shutdown(ext_stop: Option<tokio::sync::oneshot::Receiver<()>>) {
    #[cfg(unix)]
    {
        let mut ext_stop = ext_stop;
        use tokio::signal::unix::{signal, SignalKind};
        let mut term = signal(SignalKind::terminate()).expect("sigterm");
        let ext = async {
            if let Some(r) = ext_stop.as_mut() {
                let _ = r.await;
            } else {
                std::future::pending::<()>().await
            }
        };
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {},
            _ = term.recv() => {},
            _ = ext => {},
        }
    }
    #[cfg(not(unix))]
    {
        if let Some(r) = ext_stop {
            let _ = r.await;
        } else {
            let _ = tokio::signal::ctrl_c().await;
        }
    }
}
