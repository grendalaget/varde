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
    #[serde(default)]
    pub fenced: bool,
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
    /// clear the file. Executions are never resumed.
    pub fn reap_orphans(&mut self) {
        for r in &self.records {
            if r.pgid != 0 {
                kill_group(r.pgid);
                tracing::warn!(exec = %r.execution_id, pgid = r.pgid, "killed orphaned game process group");
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
fn kill_group(pgid: u32) {
    unsafe {
        libc::kill(-(pgid as i32), libc::SIGKILL);
    }
}
#[cfg(windows)]
fn kill_group(_pgid: u32) {
    // job-object tracking for restarts lands with the Windows service work
}
