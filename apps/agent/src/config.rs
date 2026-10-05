//! `config.toml` in the data dir, written by `enroll` and read by `run`.

use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Config {
    pub control_plane_url: String,
    pub node_id: String,
    pub group_id: String,
    /// base64 ed25519 public key of the CP (relay token verification later)
    pub control_plane_public_key: String,
    /// This node is an always-on replica anchor.
    #[serde(default)]
    pub anchor: bool,
    /// Optional overrides
    #[serde(default)]
    pub mesh_bin: Option<PathBuf>,
    #[serde(default)]
    pub testgame_bin: Option<PathBuf>,
    /// Test-only: rewrite service loopback IPs to this prefix (e.g. "127.78.")
    /// so several agents can coexist on one host. CP allocation is unchanged.
    #[serde(default)]
    pub loopback_prefix: Option<String>,
    /// Fencing margin subtracted from the lease window (ms).
    #[serde(default = "default_fence_margin_ms")]
    pub fence_margin_ms: i64,
    /// How long a graceful host shutdown waits for the final snapshot to be
    /// reported ready on an anchor/peer (s).
    #[serde(default = "default_shutdown_repl_s")]
    pub shutdown_replication_timeout_s: i64,
    /// Never attempt direct paths (passed through to mesh Configure).
    #[serde(default)]
    pub force_relay: bool,
}

fn default_fence_margin_ms() -> i64 {
    5000
}
fn default_shutdown_repl_s() -> i64 {
    60
}

impl Config {
    pub fn load(data_dir: &Path) -> Result<Config> {
        let p = data_dir.join("config.toml");
        let s = std::fs::read_to_string(&p)
            .with_context(|| format!("read {} (run `p2pgames-agent enroll` first)", p.display()))?;
        toml::from_str(&s).with_context(|| format!("parse {}", p.display()))
    }

    pub fn save(&self, data_dir: &Path) -> Result<()> {
        let p = data_dir.join("config.toml");
        std::fs::write(&p, toml::to_string_pretty(self)?)?;
        Ok(())
    }

    /// Translate a CP-assigned service loopback IP for this host (tests only).
    pub fn translate_loopback(&self, ip: &str) -> String {
        match &self.loopback_prefix {
            Some(pre) if ip.starts_with("127.77.") => {
                format!("{}{}", pre, &ip["127.77.".len()..])
            }
            _ => ip.to_string(),
        }
    }
}
