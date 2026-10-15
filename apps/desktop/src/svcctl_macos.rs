//! macOS service control: the agent runs as the per-user LaunchAgent
//! `games.varde.agent` (~/Library/LaunchAgents, launched by launchd in the
//! gui domain). Everything here runs as the logged-in user — a user-session
//! service never needs elevation, so the tray calls these directly instead
//! of relaunching a privileged helper like the Windows path does.

use anyhow::{Context, Result};
use std::path::PathBuf;
use std::time::{Duration, Instant};

const LABEL: &str = "games.varde.agent";
/// How long a drain may take before it counts as stuck. Matches the
/// Windows service's preshutdown budget.
const DRAIN: Duration = Duration::from_secs(180);
const START_WAIT: Duration = Duration::from_secs(60);
/// How long the IPC socket may refuse connections before a loaded service
/// counts as dead. Far longer than the tray's 2 s reconnect loop.
const IPC_DEAD: Duration = Duration::from_secs(15);

fn plist_path() -> Result<PathBuf> {
    let home = std::env::var_os("HOME").context("HOME is not set")?;
    Ok(PathBuf::from(home)
        .join("Library/LaunchAgents")
        .join(format!("{LABEL}.plist")))
}

/// The varde-agent binary shipped beside the tray inside Varde.app.
fn agent_bin() -> Result<PathBuf> {
    let exe = std::env::current_exe().context("resolve exe")?;
    let bin = exe.with_file_name("varde-agent");
    anyhow::ensure!(bin.exists(), "no varde-agent next to {}", exe.display());
    Ok(bin)
}

fn gui_domain() -> String {
    // SAFETY: getuid has no failure modes.
    format!("gui/{}", unsafe { libc::getuid() })
}

fn launchctl(args: &[&str]) -> Result<()> {
    let out = std::process::Command::new("launchctl")
        .args(args)
        .output()
        .context("spawn launchctl")?;
    anyhow::ensure!(
        out.status.success(),
        "launchctl {} -> {}: {}",
        args.join(" "),
        out.status,
        String::from_utf8_lossy(&out.stderr).trim()
    );
    Ok(())
}

fn loaded() -> bool {
    std::process::Command::new("launchctl")
        .args(["print", &format!("{}/{LABEL}", gui_domain())])
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .status()
        .map(|s| s.success())
        .unwrap_or(false)
}

/// The LaunchAgent plist contents for the sibling varde-agent: RunAtLoad
/// starts the service at every login — the closest macOS analog to the
/// Windows service's auto-start; KeepAlive on crash only, so a clean
/// shutdown stays down.
fn plist_body() -> Result<String> {
    let agent = agent_bin()?;
    let logs = agent_ipc::default_data_dir().join("logs");
    let esc = |s: &str| {
        s.replace('&', "&amp;")
            .replace('<', "&lt;")
            .replace('>', "&gt;")
    };
    Ok(format!(
        concat!(
            "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n",
            "<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n",
            "<plist version=\"1.0\">\n<dict>\n",
            "\t<key>Label</key><string>{}</string>\n",
            "\t<key>ProgramArguments</key>\n\t<array>\n",
            "\t\t<string>{}</string>\n\t\t<string>run</string>\n\t</array>\n",
            "\t<key>RunAtLoad</key><true/>\n",
            "\t<key>KeepAlive</key>\n\t<dict>\n",
            "\t\t<key>Crashed</key><true/>\n\t\t<key>SuccessfulExit</key><false/>\n",
            "\t</dict>\n",
            // bootout sends SIGTERM; without this launchd SIGKILLs after 20 s,
            // cutting a hosted server's drain (up to our 180 s budget) short.
            "\t<key>ExitTimeout</key><integer>180</integer>\n",
            "\t<key>StandardOutPath</key><string>{}</string>\n",
            "\t<key>StandardErrorPath</key><string>{}</string>\n",
            "</dict>\n</plist>\n"
        ),
        LABEL,
        esc(&agent.display().to_string()),
        esc(&logs.join("launchd.out.log").display().to_string()),
        esc(&logs.join("launchd.err.log").display().to_string()),
    ))
}

/// Write (or refresh) the LaunchAgent plist on disk.
fn write_plist() -> Result<PathBuf> {
    let path = plist_path()?;
    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir).with_context(|| format!("create {}", dir.display()))?;
    }
    let logs = agent_ipc::default_data_dir().join("logs");
    std::fs::create_dir_all(&logs).with_context(|| format!("create {}", logs.display()))?;
    std::fs::write(&path, plist_body()?).with_context(|| format!("write {}", path.display()))?;
    Ok(path)
}

/// True while the service answers on its local IPC socket — the tray's own
/// definition of healthy. Probed before restarting: a brief socket outage
/// must not stop an agent that may be hosting a server right now.
fn ipc_answers(timeout: Duration) -> bool {
    let rt = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(rt) => rt,
        Err(_) => return false,
    };
    let deadline = Instant::now() + timeout;
    rt.block_on(async {
        loop {
            if agent_ipc::connect(&agent_ipc::default_endpoint())
                .await
                .is_ok()
            {
                return true;
            }
            if Instant::now() >= deadline {
                return false;
            }
            tokio::time::sleep(Duration::from_secs(1)).await;
        }
    })
}

/// A stale plist is anything that doesn't match what we'd write today:
/// a moved Varde.app (dead binary path) or an older template (missing
/// keys an upgrade added, like ExitTimeout).
fn plist_targets_current(plist: &PathBuf) -> Result<bool> {
    let body =
        std::fs::read_to_string(plist).with_context(|| format!("read {}", plist.display()))?;
    Ok(body == plist_body()?)
}

/// Installed but not started yet (fresh drag-install launch): register the
/// LaunchAgent and let RunAtLoad start the service. A plist that exists but
/// isn't loaded is the user's own stop — left alone until Start is chosen,
/// unless it points at a moved-away Varde.app, which start() repairs.
pub fn ensure_started() -> Result<()> {
    let plist = plist_path()?;
    if loaded() {
        // A move while running (or a deleted file) leaves launchd pointed
        // at the old path; refresh it so the next login finds the new one.
        let stale = !plist.exists() || !plist_targets_current(&plist).unwrap_or(false);
        if stale {
            let _ = write_plist();
        }
        return Ok(());
    }
    if plist.exists() && plist_targets_current(&plist)? {
        return Ok(());
    }
    start()
}

/// Ensure the service runs. Loaded + answering IPC: nothing to do. Loaded
/// but silent past IPC_DEAD: restart it. Not loaded: (re)write the plist —
/// the binary may have moved since install — and bootstrap.
pub fn start() -> Result<()> {
    let domain = gui_domain();
    if loaded() {
        if ipc_answers(IPC_DEAD) {
            return Ok(());
        }
        launchctl(&["kickstart", "-k", &format!("{domain}/{LABEL}")])
            .context("restart the Varde service")?;
    } else {
        let plist = write_plist()?;
        launchctl(&[
            "bootstrap",
            &domain,
            plist.to_str().context("plist path is not utf-8")?,
        ])
        .context("register the Varde service")?;
    }
    anyhow::ensure!(
        ipc_answers(START_WAIT),
        "the Varde service did not come up within {} s",
        START_WAIT.as_secs()
    );
    Ok(())
}

/// Drain then restart, after an update swapped the binaries: SIGTERM lets
/// the agent drain on its own budget, then kickstart runs the new build.
/// A plain `kickstart -k` would start the new process while the old one is
/// still draining — and the new one can't take the socket until it exits.
pub fn restart_service() -> Result<()> {
    if !loaded() {
        // The update wrote new binaries; leaving a stopped service down
        // would strand the relaunched tray on an agent that never comes
        // up. start() also repairs a stale plist while it's at it.
        return start();
    }
    let domain = gui_domain();
    launchctl(&["kill", "SIGTERM", &format!("{domain}/{LABEL}")])
        .context("drain the Varde service")?;
    let deadline = Instant::now() + DRAIN;
    loop {
        let out = std::process::Command::new("launchctl")
            .args(["print", &format!("{domain}/{LABEL}")])
            .output();
        let running = out
            .as_ref()
            .map(|o| String::from_utf8_lossy(&o.stdout).contains("state = running"))
            .unwrap_or(false);
        if !running {
            break;
        }
        anyhow::ensure!(
            Instant::now() < deadline,
            "the Varde service is stuck draining"
        );
        std::thread::sleep(Duration::from_millis(500));
    }
    launchctl(&["kickstart", &format!("{domain}/{LABEL}")])
        .context("start the updated Varde service")?;
    anyhow::ensure!(
        ipc_answers(START_WAIT),
        "the Varde service did not come up within {} s",
        START_WAIT.as_secs()
    );
    Ok(())
}

/// Stop the service and wait for its drain. Booting out unloads the agent;
/// it comes back at next login (RunAtLoad) or when Start is chosen again.
pub fn stop() -> Result<()> {
    if !loaded() {
        return Ok(());
    }
    launchctl(&["bootout", &format!("{}/{LABEL}", gui_domain())])
        .context("ask the Varde service to stop")?;
    let deadline = Instant::now() + DRAIN;
    while loaded() {
        anyhow::ensure!(
            Instant::now() < deadline,
            "the Varde service is stuck stopping"
        );
        std::thread::sleep(Duration::from_millis(500));
    }
    Ok(())
}
