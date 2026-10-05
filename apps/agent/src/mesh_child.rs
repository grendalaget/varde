//! Supervised mesh child process. The agent spawns varde-mesh with
//! `--ipc <endpoint>` and restarts it with backoff when it exits.

use std::path::PathBuf;
use std::process::Stdio;

use anyhow::{Context, Result};
use tokio::process::{Child, Command};
use tracing::{error, info};

/// Handle to the supervised child. Awaiting it resolves with the child's
/// exit status when the process dies.
pub struct SupervisedChild {
    child: Child,
}

impl SupervisedChild {
    /// Waits for the child to exit, returning its status.
    pub async fn wait(&mut self) -> Option<std::process::ExitStatus> {
        self.child.wait().await.ok()
    }

    pub fn kill(&mut self) {
        let _ = self.child.start_kill();
    }
}

/// Spawn parameters kept for respawns after a crash.
pub struct ChildSpec {
    #[allow(dead_code)]
    bin: PathBuf,
    #[allow(dead_code)]
    ipc: String,
}

/// Spawns the mesh binary and returns the child plus a spec for `respawn`.
pub fn spawn(bin: PathBuf, ipc: String) -> (SupervisedChild, ChildSpec) {
    let spec = ChildSpec {
        bin: bin.clone(),
        ipc: ipc.clone(),
    };
    (spawn_inner(&bin, &ipc), spec)
}

/// Respawns after a crash using the saved spec.
#[allow(dead_code)]
pub fn respawn(spec: &ChildSpec) -> SupervisedChild {
    spawn_inner(&spec.bin, &spec.ipc)
}

fn spawn_inner(bin: &PathBuf, ipc: &str) -> SupervisedChild {
    let child = match start(bin, ipc) {
        Ok(child) => {
            info!(pid = child.id(), mesh = %bin.display(), "mesh child started");
            child
        }
        Err(e) => {
            // Without the mesh binary the agent is useless; exit and let the
            // service manager restart us.
            error!(mesh = %bin.display(), error = %format!("{e:#}"), "cannot spawn mesh child");
            std::process::exit(1);
        }
    };
    SupervisedChild { child }
}

fn start(bin: &PathBuf, ipc: &str) -> Result<Child> {
    let mut cmd = Command::new(bin);
    cmd.arg("--ipc").arg(ipc).stdin(Stdio::null());
    if let Ok(lvl) = std::env::var("VARDE_MESH_LOG_LEVEL") {
        cmd.arg("--log-level").arg(lvl);
    }
    // If the agent dies abruptly, the mesh must not orphan: it would keep
    // holding loopback routes and the QUIC socket.
    #[cfg(unix)]
    unsafe {
        cmd.pre_exec(|| {
            if libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    cmd.spawn()
        .with_context(|| format!("spawn {}", bin.display()))
}
