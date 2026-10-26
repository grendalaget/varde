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
    child: ProtectedChild,
    stderr_tail: Arc<Mutex<VecDeque<String>>>,
}

struct ProtectedChild {
    child: Child,
    #[cfg(windows)]
    _job: executor_native::ChildJob,
}

impl std::ops::Deref for ProtectedChild {
    type Target = Child;
    fn deref(&self) -> &Self::Target {
        &self.child
    }
}

impl std::ops::DerefMut for ProtectedChild {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.child
    }
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

/// Spawns the mesh binary; the supervisor calls this again after a crash.
pub fn spawn(bin: PathBuf, ipc: String) -> SupervisedChild {
    spawn_inner(&bin, &ipc)
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

fn start(bin: &PathBuf, ipc: &str) -> Result<ProtectedChild> {
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
    protected_spawn(&mut cmd).with_context(|| format!("spawn {}", bin.display()))
}

fn protected_spawn(cmd: &mut Command) -> std::io::Result<ProtectedChild> {
    cmd.kill_on_drop(true);
    #[cfg(target_os = "linux")]
    unsafe {
        let parent = std::process::id() as libc::pid_t;
        cmd.pre_exec(move || {
            if libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            if libc::getppid() != parent {
                return Err(std::io::Error::other("mesh parent exited before spawn"));
            }
            Ok(())
        });
    }
    #[cfg(windows)]
    let (child, job) = executor_native::ChildJob::spawn(cmd).map_err(std::io::Error::other)?;
    #[cfg(not(windows))]
    let child = cmd.spawn()?;
    Ok(ProtectedChild {
        child,
        #[cfg(windows)]
        _job: job,
    })
}

#[cfg(all(test, any(windows, target_os = "linux")))]
mod lifetime_tests {
    use super::*;
    use std::path::Path;
    use std::time::{Duration, Instant};

    #[test]
    fn parent_probe() {
        let Some(dir) = std::env::var_os("VARDE_MESH_LIFETIME_PROBE") else {
            return;
        };
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        runtime.block_on(async {
            #[cfg(windows)]
            let mut cmd = {
                let mut cmd = Command::new("cmd.exe");
                cmd.args(["/D", "/C", "ping -n 60 127.0.0.1 > NUL"]);
                cmd
            };
            #[cfg(target_os = "linux")]
            let mut cmd = {
                let mut cmd = Command::new("/bin/sleep");
                cmd.arg("60");
                cmd
            };
            let mut child = protected_spawn(&mut cmd).unwrap();
            std::fs::write(Path::new(&dir).join("pid"), child.id().unwrap().to_string()).unwrap();
            child.wait().await.unwrap();
        });
    }

    fn running(pid: u32) -> bool {
        #[cfg(target_os = "linux")]
        {
            let Ok(stat) = std::fs::read_to_string(format!("/proc/{pid}/stat")) else {
                return false;
            };
            !stat.rsplit_once(") ").unwrap().1.starts_with('Z')
        }
        #[cfg(windows)]
        unsafe {
            use windows_sys::Win32::System::Threading::*;
            let handle = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid);
            if handle.is_null() {
                return false;
            }
            let mut code = 0;
            let ok = GetExitCodeProcess(handle, &mut code);
            windows_sys::Win32::Foundation::CloseHandle(handle);
            ok != 0 && code == 259
        }
    }

    #[test]
    fn mesh_child_dies_when_parent_is_killed() {
        let dir = tempfile::tempdir().unwrap();
        let mut parent = std::process::Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "mesh_child::lifetime_tests::parent_probe",
                "--nocapture",
            ])
            .env("VARDE_MESH_LIFETIME_PROBE", dir.path())
            .spawn()
            .unwrap();
        let deadline = Instant::now() + Duration::from_secs(10);
        let pid = loop {
            if let Ok(value) = std::fs::read_to_string(dir.path().join("pid")) {
                if let Ok(pid) = value.parse::<u32>() {
                    break pid;
                }
            }
            if Instant::now() >= deadline {
                let _ = parent.kill();
                let _ = parent.wait();
                panic!("mesh probe did not start");
            }
            std::thread::sleep(Duration::from_millis(10));
        };
        assert!(running(pid));
        parent.kill().unwrap();
        parent.wait().unwrap();
        let deadline = Instant::now() + Duration::from_secs(5);
        while running(pid) && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(10));
        }
        assert!(!running(pid), "mesh child survived parent death");
    }
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
