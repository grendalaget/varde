//! Crash capture for the tray: panics land in
//! %LOCALAPPDATA%\Varde\crash-reports (WER writes hard-crash minidumps to the
//! same dir — see crash_report::wer::register at service install). The
//! forward loop hands pending reports to the agent over IPC; the agent
//! uploads them to this machine's own control plane when the user pref is on.

use std::path::PathBuf;
use std::time::Duration;

use agent_ipc::pb;

/// The WER LocalDumps key for varde-tray.exe points at this same dir.
pub fn dir() -> PathBuf {
    std::env::var_os("LOCALAPPDATA")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(r"C:\"))
        .join("Varde")
        .join("crash-reports")
}

/// Records panics as `panic` reports; must run before any code that can panic.
pub fn install_hook() {
    crash_report::install_panic_hook(dir(), "varde-tray", env!("CARGO_PKG_VERSION"));
}

/// Every 30 s push pending reports to the agent and file them under `sent/`.
/// Reports simply accumulate while the service is down — capture never
/// depends on connectivity.
pub async fn forward_loop() {
    let mut tick = tokio::time::interval(Duration::from_secs(30));
    loop {
        tick.tick().await;
        if let Ok(mut c) = agent_ipc::connect(agent_ipc::PIPE_NAME).await {
            forward_once(&mut c).await;
        }
    }
}

async fn forward_once(c: &mut agent_ipc::LocalServiceClient<tonic::transport::Channel>) {
    let dir = dir();
    let Ok(pending) = crash_report::collect(&dir) else {
        return;
    };
    for p in pending {
        let r = &p.report;
        let mut dump = Vec::new();
        let mut dump_name = String::new();
        let mut detail = r.detail.clone();
        if let Some(dmp) = &p.dump_path {
            match std::fs::read(dmp) {
                Ok(bytes) if bytes.len() as u64 <= crash_report::MAX_DUMP_BYTES => {
                    dump = bytes;
                    dump_name = if r.dump_name.is_empty() {
                        dmp.file_name()
                            .map(|f| f.to_string_lossy().into_owned())
                            .unwrap_or_default()
                    } else {
                        r.dump_name.clone()
                    };
                }
                Ok(bytes) => {
                    detail = format!(
                        "{detail}\n[minidump omitted: {} bytes exceeds cap]",
                        bytes.len()
                    );
                }
                Err(e) => detail = format!("{detail}\n[minidump unreadable: {e}]"),
            }
        }
        let msg = pb::CrashReportMsg {
            kind: r.kind.clone(),
            exe: r.exe.clone(),
            version: r.version.clone(),
            pid: r.pid as i32,
            message: r.message.clone(),
            location: r.location.clone(),
            backtrace: r.backtrace.clone(),
            detail,
            created_at_unix_ms: r.created_at_unix_ms,
            dump,
            dump_name,
        };
        if c.submit_crash_report(pb::SubmitCrashReportRequest { report: Some(msg) })
            .await
            .is_err()
        {
            return;
        }
        let _ = crash_report::mark_sent(&dir, &p);
    }
    let _ = crash_report::gc(&dir, 20);
}
