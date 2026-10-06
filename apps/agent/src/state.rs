//! `state/executions.json` — last known executions + fencing status, survives
//! agent restarts. On startup the agent kills orphans and does not resume.

use std::path::{Path, PathBuf};

use anyhow::Result;
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ExecRecord {
    pub execution_id: String,
    pub server_id: String,
    pub epoch: i64,
    /// process group id of the game process (0 if none yet)
    #[serde(default)]
    pub pgid: u32,
    /// boot identity at record time — kills never cross a reboot
    #[serde(default)]
    pub boot_id: String,
    /// leader's starttime (/proc/<pgid>/stat field 22) — guards against pgid
    /// reuse within the same boot
    #[serde(default)]
    pub pgid_start_ticks: u64,
    #[serde(default)]
    pub fenced: bool,
}

impl ExecRecord {
    pub fn new(
        execution_id: String,
        server_id: String,
        epoch: i64,
        pgid: u32,
        fenced: bool,
    ) -> ExecRecord {
        ExecRecord {
            execution_id,
            server_id,
            epoch,
            pgid,
            boot_id: boot_id(),
            pgid_start_ticks: proc_start_ticks(pgid),
            fenced,
        }
    }

    /// A kill is only safe when the recorded boot id still matches and the
    /// process group leader is the same process we recorded (same starttime).
    fn kill_is_safe(&self) -> bool {
        self.pgid != 0
            && !self.boot_id.is_empty()
            && self.boot_id == boot_id()
            && self.pgid_start_ticks != 0
            && self.pgid_start_ticks == proc_start_ticks(self.pgid)
    }
}

pub struct ExecState {
    path: PathBuf,
    pub records: Vec<ExecRecord>,
}

impl ExecState {
    pub fn load(data_dir: &Path) -> ExecState {
        let path = data_dir.join("state").join("executions.json");
        let records = std::fs::read_to_string(&path)
            .ok()
            .and_then(|s| serde_json::from_str(&s).ok())
            .unwrap_or_default();
        ExecState { path, records }
    }

    pub fn save(&self) -> Result<()> {
        if let Some(p) = self.path.parent() {
            std::fs::create_dir_all(p)?;
        }
        let tmp = self.path.with_extension("tmp");
        std::fs::write(&tmp, serde_json::to_string_pretty(&self.records)?)?;
        std::fs::rename(&tmp, &self.path)?;
        Ok(())
    }

    /// Kill recorded process groups (orphans from a previous agent run) and
    /// clear the file. Executions are never resumed. A record whose boot id or
    /// leader starttime no longer matches is dropped without killing — the
    /// pgid may belong to an unrelated process after a reboot or PID reuse.
    pub fn reap_orphans(&mut self) {
        for r in &self.records {
            if r.kill_is_safe() {
                kill_group(r.pgid);
                tracing::warn!(exec = %r.execution_id, pgid = r.pgid, "killed orphaned game process group");
            } else {
                tracing::info!(exec = %r.execution_id, pgid = r.pgid,
                    boot_ok = !r.boot_id.is_empty() && r.boot_id == boot_id(),
                    rec_ticks = r.pgid_start_ticks,
                    cur_ticks = proc_start_ticks(r.pgid),
                    "dropping stale orphan record");
            }
        }
        self.records.clear();
        let _ = self.save();
    }

    pub fn upsert(&mut self, rec: ExecRecord) {
        if let Some(e) = self
            .records
            .iter_mut()
            .find(|e| e.execution_id == rec.execution_id)
        {
            *e = rec;
        } else {
            self.records.push(rec);
        }
        let _ = self.save();
    }

    pub fn remove(&mut self, execution_id: &str) {
        self.records.retain(|r| r.execution_id != execution_id);
        let _ = self.save();
    }
}

#[cfg(unix)]
fn boot_id() -> String {
    std::fs::read_to_string("/proc/sys/kernel/random/boot_id")
        .map(|s| s.trim().to_string())
        .unwrap_or_default()
}
#[cfg(not(unix))]
fn boot_id() -> String {
    String::new()
}

/// `/proc/<pid>/stat` field 22 (starttime, clock ticks since boot). The comm
/// field may contain spaces, so parse after the last ')'.
#[cfg(unix)]
fn proc_start_ticks(pid: u32) -> u64 {
    let Ok(s) = std::fs::read_to_string(format!("/proc/{pid}/stat")) else {
        return 0;
    };
    let Some(rp) = s.rfind(')') else {
        return 0;
    };
    s[rp + 2..]
        .split_whitespace()
        .nth(19)
        .and_then(|v| v.parse().ok())
        .unwrap_or(0)
}
#[cfg(not(unix))]
fn proc_start_ticks(_pid: u32) -> u64 {
    0
}

#[cfg(unix)]
fn kill_group(pgid: u32) {
    unsafe {
        libc::kill(-(pgid as i32), libc::SIGKILL);
    }
}
#[cfg(windows)]
fn kill_group(_pgid: u32) {
    // job-object tracking for restarts lands with the Windows service work
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;

    /// A record whose boot id doesn't match must never kill — the pgid may
    /// belong to an unrelated process after a reboot or PID reuse.
    #[test]
    fn reap_skips_mismatched_boot_id() {
        let tmp = tempfile::tempdir().unwrap();
        let mut child = std::process::Command::new("sleep")
            .arg("30")
            .spawn()
            .unwrap();
        let mut st = ExecState {
            path: tmp.path().join("state/executions.json"),
            records: vec![ExecRecord {
                execution_id: "exec_x".into(),
                server_id: "srv_x".into(),
                epoch: 1,
                pgid: child.id(),
                boot_id: "not-this-boot".into(),
                pgid_start_ticks: 1,
                fenced: false,
            }],
        };
        st.reap_orphans();
        assert!(
            Path::new(&format!("/proc/{}/stat", child.id())).exists(),
            "mismatched record must not kill"
        );
        child.kill().ok();
        child.wait().ok();
    }
}
