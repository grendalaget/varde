//! Crash reporting: drain `<data>/crash-reports` to this node's own control
//! plane while the `crash_reporting` pref is on. Reports come from the panic
//! hook (agent + tray via IPC), Windows Error Reporting minidumps, and the
//! supervised mesh child's abnormal exits. Uploads go to the machine's own
//! control plane — a self-hosted group's reports never leave its infra.

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use anyhow::Result;
use tokio::sync::watch;
use tracing::warn;

use crate::Agent;

/// Where captured reports wait for upload.
pub fn dir(data_dir: &Path) -> PathBuf {
    data_dir.join("crash-reports")
}

/// Persist a report handed to us by a local client (the tray) over IPC; it
/// joins the same pending dir the upload loop drains.
pub fn submit_local(data_dir: &Path, m: agent_ipc::pb::CrashReportMsg) -> Result<()> {
    let dir = dir(data_dir);
    let mut r = crash_report::Report {
        kind: m.kind,
        exe: m.exe,
        version: m.version,
        // the tray runs on this machine: the agent's os/arch are correct
        os: std::env::consts::OS.into(),
        arch: std::env::consts::ARCH.into(),
        pid: m.pid.max(0) as u32,
        message: m.message,
        location: m.location,
        backtrace: m.backtrace,
        detail: m.detail,
        created_at_unix_ms: m.created_at_unix_ms,
        dump_name: String::new(),
    };
    if m.dump.is_empty() {
        crash_report::write_report(&dir, &r)?;
    } else {
        crash_report::write_report_with_dump(&dir, &mut r, &m.dump, &m.dump_name)?;
    }
    Ok(())
}

fn body_of(r: &crash_report::Report) -> cp_api::CrashReportBody {
    cp_api::CrashReportBody {
        kind: r.kind.clone(),
        exe: r.exe.clone(),
        version: r.version.clone(),
        os: r.os.clone(),
        arch: r.arch.clone(),
        pid: r.pid as i32,
        message: r.message.clone(),
        location: r.location.clone(),
        backtrace: r.backtrace.clone(),
        detail: r.detail.clone(),
        created_at_unix_ms: r.created_at_unix_ms,
        dump_b64: String::new(),
        dump_name: String::new(),
        dump_omitted: String::new(),
    }
}

/// Attach the report's minidump (base64) to `body`, flagging `dump_omitted`
/// when it exists but is unreadable or over the cap.
fn attach_dump(body: &mut cp_api::CrashReportBody, pending: &crash_report::Pending) {
    use base64::Engine;
    let Some(dmp) = &pending.dump_path else {
        return;
    };
    let name = if pending.report.dump_name.is_empty() {
        dmp.file_name()
            .unwrap_or_default()
            .to_string_lossy()
            .into_owned()
    } else {
        pending.report.dump_name.clone()
    };
    match std::fs::metadata(dmp).and_then(|_| std::fs::read(dmp)) {
        Ok(bytes) if bytes.len() as u64 > crash_report::MAX_DUMP_BYTES => {
            body.dump_omitted = format!(
                "{} bytes exceeds {}-byte cap",
                bytes.len(),
                crash_report::MAX_DUMP_BYTES
            );
        }
        Ok(bytes) => {
            body.dump_b64 = base64::engine::general_purpose::STANDARD.encode(bytes);
            body.dump_name = name;
        }
        Err(e) => body.dump_omitted = format!("unreadable: {e}"),
    }
}

/// Upload every pending report the user hasn't opted out of, oldest first.
async fn drain(agent: &Arc<Agent>, dir: &Path) -> Result<()> {
    if !crash_report::load_prefs(&agent.data_dir).crash_reporting {
        return Ok(());
    }
    for p in crash_report::collect(dir)? {
        let mut body = body_of(&p.report);
        attach_dump(&mut body, &p);
        agent.cp.submit_crash_report(&body).await?;
        crash_report::mark_sent(dir, &p)?;
    }
    Ok(())
}

/// Hourly-ish drain. Runs inside `run_linked`, so it stops on relink and a
/// fresh loop is spawned against the new control plane.
pub async fn upload_loop(agent: Arc<Agent>, mut stop: watch::Receiver<bool>) {
    let dir = dir(&agent.data_dir);
    let mut tick = tokio::time::interval(Duration::from_secs(300));
    loop {
        tokio::select! {
            _ = tick.tick() => {
                if let Err(e) = drain(&agent, &dir).await {
                    warn!(error = %format!("{e:#}"), "crash report upload failed");
                }
                if let Err(e) = crash_report::gc(&dir, 50) {
                    warn!(error = %e, "crash report gc failed");
                }
            }
            _ = stop.changed() => return,
        }
    }
}
