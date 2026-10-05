//! p2pgames-agent: local node agent. Owns identity, data dir, supervision of
//! the Go mesh subprocess, and (later) executors, storage and the control
//! connection.

mod identity;
mod mesh_child;

use std::path::PathBuf;
use std::time::Duration;

use anyhow::{Context, Result};
use clap::{Parser, Subcommand};
use tracing::{error, info};

#[derive(Parser)]
#[command(name = "p2pgames-agent", version, about = "p2pgames local node agent")]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Run the agent in the foreground (what the service manager invokes).
    Run {
        /// Data directory. Default: /var/lib/p2pgames (Linux),
        /// %ProgramData%\P2PGames (Windows).
        #[arg(long, env = "P2PGAMES_DATA_DIR")]
        data_dir: Option<PathBuf>,

        /// Path to the p2pgames-mesh binary. Default: next to the agent exe,
        /// falling back to /usr/lib/p2pgames/p2pgames-mesh.
        #[arg(long, env = "P2PGAMES_MESH_BIN")]
        mesh_bin: Option<PathBuf>,
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

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .json()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();

    let cli = Cli::parse();
    match cli.command {
        Command::Run { data_dir, mesh_bin } => run(data_dir, mesh_bin).await,
    }
}

async fn run(data_dir: Option<PathBuf>, mesh_bin: Option<PathBuf>) -> Result<()> {
    let data_dir = data_dir.unwrap_or_else(default_data_dir);
    std::fs::create_dir_all(&data_dir)
        .with_context(|| format!("create data dir {}", data_dir.display()))?;

    let key_path = data_dir.join("identity").join("node.key");
    let (node_id, created) = identity::load_or_create(&key_path)?;
    if created {
        info!(node_id, key = %key_path.display(), "generated node identity");
    } else {
        info!(node_id, "loaded node identity");
    }

    let ipc = mesh_ipc::ipc_endpoint(&data_dir);
    let mesh_bin = mesh_bin.unwrap_or_else(|| default_mesh_bin().unwrap_or_default());
    info!(mesh = %mesh_bin.display(), ipc, "starting mesh child");

    let (mut child, child_spec) = mesh_child::spawn(mesh_bin, ipc.clone());
    let ipc_for_client = ipc.clone();
    let key_path_str = key_path.display().to_string();
    let node_id_for_client = node_id.clone();
    let _client = tokio::spawn(async move {
        // The mesh needs a moment to bind the IPC listener.
        for attempt in 0..50u32 {
            match mesh_ipc::connect(&ipc_for_client).await {
                Ok(mut c) => {
                    match c
                        .configure(mesh_ipc::pb::ConfigureRequest {
                            node_id: node_id_for_client.clone(),
                            identity_key_path: key_path_str.clone(),
                            listen_port: 0,
                            relays: vec![],
                            force_relay: false,
                        })
                        .await
                    {
                        Ok(_) => {
                            match c.get_status(mesh_ipc::pb::GetStatusRequest {}).await {
                                Ok(resp) => {
                                    let st = resp.into_inner();
                                    info!(
                                        node_id = st.node_id,
                                        version = st.version,
                                        configured = st.configured,
                                        listen_addrs = ?st.listen_addrs,
                                        "mesh status received"
                                    );
                                }
                                Err(e) => error!(error = %e, "GetStatus failed"),
                            }
                            return;
                        }
                        Err(e) => error!(error = %e, "Configure failed"),
                    }
                }
                Err(e) => {
                    if attempt == 49 {
                        error!(error = %e, "could not connect to mesh IPC");
                    }
                    tokio::time::sleep(Duration::from_millis(100)).await;
                }
            }
        }
    });

    // Supervision loop: restart the mesh child with backoff if it dies.
    let mut backoff = Duration::from_millis(250);
    loop {
        tokio::select! {
            status = child.wait() => {
                error!(status = ?status, "mesh child exited; restarting");
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(30));
                child = mesh_child::respawn(&child_spec);
            }
            _ = tokio::signal::ctrl_c() => {
                info!("shutting down");
                child.kill();
                return Ok(());
            }
        }
    }
}
