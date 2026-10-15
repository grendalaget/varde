//! Supervised mesh child process. The agent spawns varde-mesh with
//! `--ipc <endpoint>` and restarts it with backoff when it exits.

use std::collections::VecDeque;
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::{Arc, Mutex};

use anyhow::{Context, Result};
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::{Child, Command};
use tracing::{error, info};

/// Lines kept of the child's stderr for crash reports.
const STDERR_TAIL: usize = 64;

/// Handle to the supervised child. Awaiting it resolves with the child's
/// exit status when the process dies.
pub struct SupervisedChild {
    child: Child,
    stderr_tail: Arc<Mutex<VecDeque<String>>>,
}

impl SupervisedChild {
    /// Waits for the child to exit, returning its status.
    pub async fn wait(&mut self) -> Option<std::process::ExitStatus> {
        self.child.wait().await.ok()
    }

    /// Last stderr lines the child emitted (crash-report context).
    pub fn stderr_tail(&self) -> Vec<String> {
        self.stderr_tail.lock().unwrap().iter().cloned().collect()
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
    match start(bin, ipc) {
        Ok(mut child) => {
            info!(pid = child.id(), mesh = %bin.display(), "mesh child started");
            let tail = Arc::new(Mutex::new(VecDeque::with_capacity(STDERR_TAIL)));
            if let Some(err) = child.stderr.take() {
                let t = tail.clone();
                tokio::spawn(async move {
                    let mut lines = BufReader::new(err).lines();
                    while let Ok(Some(line)) = lines.next_line().await {
                        let mut q = t.lock().unwrap();
                        if q.len() == STDERR_TAIL {
                            q.pop_front();
                        }
                        q.push_back(line);
                    }
                });
            }
            SupervisedChild {
                child,
                stderr_tail: tail,
            }
        }
        Err(e) => {
            // Without the mesh binary the agent is useless; exit and let the
            // service manager restart us.
            error!(mesh = %bin.display(), error = %format!("{e:#}"), "cannot spawn mesh child");
            std::process::exit(1);
        }
    }
}

fn start(bin: &PathBuf, ipc: &str) -> Result<Child> {
    let mut cmd = Command::new(bin);
    cmd.arg("--ipc")
        .arg(ipc)
        .stdin(Stdio::null())
        .stderr(Stdio::piped());
    #[cfg(target_os = "macos")]
    cmd.arg("--parent-pid").arg(std::process::id().to_string());
    if let Ok(lvl) = std::env::var("VARDE_MESH_LOG_LEVEL") {
        cmd.arg("--log-level").arg(lvl);
    }
    // Linux can kill the mesh on abrupt agent death to release its routes and socket.
    #[cfg(target_os = "linux")]
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

#[cfg(all(test, unix))]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;
    use std::time::Duration;

    #[tokio::test]
    async fn passes_parent_pid_only_on_macos() {
        let dir = tempfile::tempdir().unwrap();
        let bin = dir.path().join("mesh");
        let output = dir.path().join("arguments");
        std::fs::write(&bin, "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$2\"\n").unwrap();
        std::fs::set_permissions(&bin, std::fs::Permissions::from_mode(0o700)).unwrap();
        let mut child = start(&bin, output.to_str().unwrap()).unwrap();
        let status = tokio::time::timeout(Duration::from_secs(5), child.wait())
            .await
            .unwrap()
            .unwrap();
        assert!(status.success());
        let mut expected = if cfg!(target_os = "macos") {
            vec![
                "--ipc".to_string(),
                output.to_str().unwrap().to_string(),
                "--parent-pid".to_string(),
                std::process::id().to_string(),
            ]
        } else {
            vec!["--ipc".to_string(), output.to_str().unwrap().to_string()]
        };
        if let Ok(level) = std::env::var("VARDE_MESH_LOG_LEVEL") {
            expected.extend(["--log-level".to_string(), level]);
        }
        let actual = std::fs::read_to_string(output).unwrap();
        assert_eq!(actual.lines().collect::<Vec<_>>(), expected);
    }
}
