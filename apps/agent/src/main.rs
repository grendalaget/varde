//! p2pgames-agent: local node agent. Owns identity, enrollment, the control
//! loop, execution supervision, the snapshot store and the chunk server, and
//! supervises the Go mesh subprocess over IPC.

mod agent;
mod chunks;
mod config;
mod control;
mod exec;
mod identity;
mod mesh_child;
mod mesh_ctl;
mod state;
mod sysinfo;

pub use agent::Agent;

use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use anyhow::{bail, Context, Result};
use clap::{Parser, Subcommand};
use ed25519_dalek::SigningKey;
use tracing::info;

#[derive(Parser)]
#[command(name = "p2pgames-agent", version, about = "p2pgames local node agent")]
struct Cli {
    #[command(subcommand)]
    command: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// Enroll this node: device flow (default) or --token.
    Enroll {
        #[arg(long)]
        server: String,
        #[arg(long)]
        token: Option<String>,
        #[arg(long, env = "P2PGAMES_DATA_DIR")]
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
        #[arg(long, env = "P2PGAMES_DATA_DIR")]
        data_dir: Option<PathBuf>,
        #[arg(long, env = "P2PGAMES_MESH_BIN")]
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
        #[arg(long, env = "P2PGAMES_DATA_DIR")]
        data_dir: Option<PathBuf>,
    },
}

fn default_data_dir() -> PathBuf {
    if cfg!(windows) {
        std::env::var_os("ProgramData")
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from(r"C:\ProgramData"))
            .join("P2PGames")
    } else {
        PathBuf::from("/var/lib/p2pgames")
    }
}

fn default_mesh_bin() -> Result<PathBuf> {
    let exe = std::env::current_exe().context("resolve agent exe path")?;
    let name = if cfg!(windows) {
        "p2pgames-mesh.exe"
    } else {
        "p2pgames-mesh"
    };
    let sibling = exe.with_file_name(name);
    if sibling.exists() {
        return Ok(sibling);
    }
    Ok(PathBuf::from("/usr/lib/p2pgames").join(name))
}

fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .json()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();

    match Cli::parse().command {
        Cmd::Enroll {
            server,
            token,
            data_dir,
            anchor,
            loopback_prefix,
            fence_margin_ms,
        } => {
            enroll(
                server,
                token,
                data_dir.unwrap_or_else(default_data_dir),
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
            )
            .await
        }
        Cmd::Status { data_dir } => status(data_dir.unwrap_or_else(default_data_dir)),
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
    let key_path = data_dir.join("identity").join("node.key");
    identity::load_or_create(&key_path)?;
    let key = identity::load(&key_path)?;
    let public_key = identity::public_key_b64(&key);

    let client = cp_api::CpClient::new(&server)?;
    let hostname = std::env::var("HOSTNAME").unwrap_or_else(|_| "unknown".into());

    let result: cp_api::EnrollResult = if let Some(token) = token {
        client
            .json(
                "POST",
                "/v1/agent/enroll/token",
                Some(&cp_api::TokenEnrollRequest {
                    token,
                    public_key,
                    hostname: Some(hostname),
                    os: Some(std::env::consts::OS.into()),
                    arch: Some(std::env::consts::ARCH.into()),
                    agent_version: Some(env!("CARGO_PKG_VERSION").into()),
                }),
            )
            .await?
    } else {
        // device flow: POST device → print code → poll
        let dev: cp_api::DeviceEnrollResponse = client
            .json(
                "POST",
                "/v1/agent/enroll/device",
                Some(&cp_api::DeviceEnrollRequest {
                    public_key,
                    hostname: Some(hostname),
                    os: Some(std::env::consts::OS.into()),
                    arch: Some(std::env::consts::ARCH.into()),
                    agent_version: Some(env!("CARGO_PKG_VERSION").into()),
                }),
            )
            .await?;
        println!(
            "Visit {} and enter code: {}",
            dev.verification_url, dev.user_code
        );
        let deadline = now_ms() + dev.expires_in * 1000;
        loop {
            if now_ms() > deadline {
                bail!("device code expired");
            }
            tokio::time::sleep(std::time::Duration::from_secs(dev.interval.max(1) as u64)).await;
            let (status, body) = client
                .call(
                    "POST",
                    "/v1/agent/enroll/device/poll",
                    Some(&serde_json::to_vec(&cp_api::DevicePollRequest {
                        device_code: dev.device_code.clone(),
                    })?),
                )
                .await?;
            match status {
                200 => break serde_json::from_slice(&body)?,
                202 => continue, // pending
                410 => bail!("device code expired"),
                s => bail!("enroll poll failed: {s} {}", String::from_utf8_lossy(&body)),
            }
        }
    };

    let cfg = config::Config {
        control_plane_url: server,
        node_id: result.node_id.clone(),
        group_id: result.group_id.clone(),
        control_plane_public_key: result.control_plane_public_key.clone(),
        anchor,
        mesh_bin: None,
        testgame_bin: None,
        loopback_prefix,
        fence_margin_ms: fence_margin_ms.unwrap_or(5000),
        shutdown_replication_timeout_s: 60,
        force_relay: false,
    };
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

async fn run(
    data_dir: PathBuf,
    mesh_bin: Option<PathBuf>,
    anchor_flag: bool,
    force_relay: bool,
    fence_margin_ms: Option<i64>,
) -> Result<()> {
    let mut cfg = config::Config::load(&data_dir)?;
    if anchor_flag {
        cfg.anchor = true;
    }
    if force_relay {
        cfg.force_relay = true;
    }
    if let Some(m) = fence_margin_ms {
        cfg.fence_margin_ms = m;
    }
    let key_path = data_dir.join("identity").join("node.key");
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
        executor: executor_native::NativeExecutor,
        mesh: mesh.clone(),
        execs: Mutex::new(std::collections::HashMap::new()),
        state: Mutex::new(exec_state),
        chunk_tracker: Arc::new(chunks::ServeTracker::default()),
        started_at_ms: now_ms(),
        repl: Mutex::new(agent::ReplQueue::default()),
        repl_notify: tokio::sync::Notify::new(),
    });

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
    tokio::spawn(mesh.clone().supervise(mesh_data, mesh_bin, stop_rx.clone()));
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

    // graceful shutdown
    wait_shutdown().await;
    info!("shutting down: stopping executions");
    let _ = stop_tx.send(true);

    // graceful stop of all hosted executions (final snapshot + replication hold)
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

    info!("agent stopped");
    Ok(())
}

async fn wait_shutdown() {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{signal, SignalKind};
        let mut term = signal(SignalKind::terminate()).expect("sigterm");
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {},
            _ = term.recv() => {},
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
    }
}
