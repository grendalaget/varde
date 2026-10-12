//! Update check: poll the channel's latest.json (Tauri static manifest) on a
//! timer, verify its minisign signature, and report the result over local IPC
//! (Status.update) and in the heartbeat (update_available). Applying an
//! update is platform-specific and lives elsewhere — this only discovers it.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use anyhow::{Context, Result};
use serde::Deserialize;
use tracing::debug;

use crate::Agent;

/// How often to re-check when the config doesn't say otherwise.
const DEFAULT_INTERVAL_S: i64 = 6 * 3600;

/// minisign public key of the release signer (baked in at build time by the
/// release pipeline; empty until the first signed release exists). The
/// `update.pubkey` config key overrides it.
pub const RELEASE_PUBKEY: &str = "";

#[derive(Debug, Default)]
pub struct UpdateState {
    /// unix ms of the last completed check; 0 = never.
    pub checked_at_ms: i64,
    /// newer version offered by the feed.
    pub available: Option<Available>,
    /// last check/verification failure.
    pub error: Option<String>,
}

#[derive(Debug, Clone)]
pub struct Available {
    pub version: String,
    /// download URL + signature for this install's own package; absent when
    /// the feed signs no entry for it — the update is then notify-only.
    pub url: Option<String>,
    pub signature: Option<String>,
}

#[derive(Debug, Deserialize)]
struct Manifest {
    version: String,
    platforms: std::collections::HashMap<String, Platform>,
}

#[derive(Debug, Deserialize)]
struct Platform {
    url: String,
    signature: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Channel {
    Stable,
    Nightly,
    Off,
}

pub fn channel(cfg: &crate::config::Config) -> Channel {
    match cfg.update.channel.as_deref().unwrap_or("stable") {
        "nightly" => Channel::Nightly,
        "off" | "none" | "disabled" => Channel::Off,
        _ => Channel::Stable,
    }
}

fn feed_url(cfg: &crate::config::Config) -> Option<String> {
    if let Some(u) = &cfg.update.feed_url {
        return Some(u.clone());
    }
    let release = match channel(cfg) {
        Channel::Stable => "latest/download",
        Channel::Nightly => "nightly/download",
        Channel::Off => return None,
    };
    Some(format!(
        "https://github.com/grendalaget/varde/releases/{release}/latest.json"
    ))
}

/// The manifest `platforms` key for this install's own package, when the
/// install flavor is detectable. None = notify-only (manual/source install).
pub fn platform_key() -> Option<String> {
    let arch = std::env::consts::ARCH;
    #[cfg(windows)]
    {
        let _ = arch;
        Some("windows-x86_64".into())
    }
    #[cfg(target_os = "macos")]
    {
        Some(format!("darwin-{arch}-agent"))
    }
    #[cfg(target_os = "linux")]
    {
        // Snap confinement always exports SNAP. Outside it, ask the package
        // manager which format installed us; neither = manual install.
        if std::env::var_os("SNAP").is_some() {
            return Some(format!("linux-{arch}-agent-snap"));
        }
        let has = |prog: &str, args: &[&str]| {
            std::process::Command::new(prog)
                .args(args)
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .status()
                .map(|s| s.success())
                .unwrap_or(false)
        };
        if has("dpkg-query", &["-W", "-f=${Status}", "varde-agent"]) {
            return Some(format!("linux-{arch}-agent-deb"));
        }
        if has("rpm", &["-q", "varde-agent"]) {
            return Some(format!("linux-{arch}-agent-rpm"));
        }
        None
    }
    #[cfg(not(any(windows, target_os = "macos", target_os = "linux")))]
    {
        let _ = arch;
        None
    }
}

fn pubkey(cfg: &crate::config::Config) -> Option<String> {
    let k = cfg
        .update
        .pubkey
        .clone()
        .unwrap_or_else(|| RELEASE_PUBKEY.to_string());
    if k.is_empty() {
        None
    } else {
        Some(k)
    }
}

/// One check. Writes the outcome into the shared state; errors land there too
/// (never crash the agent for an update hiccup).
pub async fn check_once(agent: &Arc<Agent>) {
    let res = fetch_update(&agent.cfg).await;
    let mut st = agent.update.lock().unwrap();
    st.checked_at_ms = crate::now_ms();
    match res {
        Ok(av) => {
            st.error = None;
            if let Some(a) = &av {
                debug!(version = %a.version, "update available");
            }
            st.available = av;
        }
        Err(e) => {
            st.error = Some(format!("{e:#}"));
            // keep the last confirmed offer: a transient check failure
            // shouldn't hide a verified update badge.
        }
    }
}

async fn fetch_update(cfg: &crate::config::Config) -> Result<Option<Available>> {
    let Some(url) = feed_url(cfg) else {
        return Ok(None);
    };
    let body = cp_api::client::fetch_url(&url, MAX_MANIFEST_BYTES)
        .await
        .with_context(|| format!("fetch {url}"))?;
    // Unsigned feeds are never trusted: without a key there is no way to
    // tell a real release from a tampered one. Dev builds pass update.pubkey.
    let Some(key) = pubkey(cfg) else {
        anyhow::bail!("no update pubkey configured (release key or [update] pubkey)");
    };
    let sig_body = cp_api::client::fetch_url(&format!("{url}.sig"), MAX_MANIFEST_BYTES)
        .await
        .context("fetch latest.json.sig")?;
    let pk = minisign_verify::PublicKey::from_base64(&key)
        .map_err(|e| anyhow::anyhow!("bad update pubkey: {e}"))?;
    let sig = minisign_verify::Signature::decode(
        std::str::from_utf8(&sig_body).context("manifest sig not utf8")?,
    )
    .map_err(|e| anyhow::anyhow!("bad manifest sig: {e}"))?;
    pk.verify(&body, &sig, false)
        .map_err(|e| anyhow::anyhow!("manifest signature invalid: {e}"))?;
    pick(
        &body,
        env!("CARGO_PKG_VERSION"),
        platform_key().as_deref(),
        channel(cfg),
    )
}

/// latest.json and its .sig are kilobytes; cap the download so a bad feed
/// can't exhaust agent memory.
const MAX_MANIFEST_BYTES: usize = 4 * 1024 * 1024;

/// Manifest bytes + current version + this install's platform key → the
/// update to offer (None = current or feed is behind). The key may be absent
/// on manual installs: the version still surfaces, notify-only.
fn pick(body: &[u8], current: &str, key: Option<&str>, ch: Channel) -> Result<Option<Available>> {
    let m: Manifest = serde_json::from_slice(body).context("parse latest.json")?;
    if ch == Channel::Nightly {
        // Nightly versions are dated build metadata (0.0.0-nightly.<date>+<sha>):
        // a stable install opted into nightly, or the same nightly rebuilt on
        // a new sha, reads as "different" — but an OLDER dated nightly is a
        // downgrade, never an offer.
        if m.version == current {
            return Ok(None);
        }
        match (nightly_date(&m.version), nightly_date(current)) {
            (Some(o), Some(c)) if o < c => return Ok(None),
            _ => {}
        }
    } else {
        let current =
            semver::Version::parse(current).unwrap_or_else(|_| semver::Version::new(0, 0, 0));
        let offered = semver::Version::parse(m.version.trim_start_matches('v'))
            .context("manifest version not semver")?;
        if offered <= current {
            return Ok(None);
        }
    }
    let entry = key.and_then(|k| m.platforms.get(k));
    Ok(Some(Available {
        version: m.version,
        url: entry.map(|p| p.url.clone()),
        signature: entry.map(|p| p.signature.clone()),
    }))
}

/// `0.0.0-nightly.<YYYYMMDD>+<sha>` → the date as a sortable u32.
fn nightly_date(v: &str) -> Option<u32> {
    let d = v.trim_start_matches('v').strip_prefix("0.0.0-nightly.")?;
    d.get(..8)?.parse::<u32>().ok()
}

/// Background loop: check on start, then on the configured interval.
pub async fn update_loop(agent: Arc<Agent>, mut stop: tokio::sync::watch::Receiver<bool>) {
    if channel(&agent.cfg) == Channel::Off {
        return;
    }
    let interval = Duration::from_secs(
        agent
            .cfg
            .update
            .interval_s
            .unwrap_or(DEFAULT_INTERVAL_S)
            .clamp(300, 7 * 24 * 3600) as u64,
    );
    loop {
        check_once(&agent).await;
        tokio::select! {
            _ = tokio::time::sleep(interval) => {},
            _ = stop.changed() => return,
        }
    }
}

/// Shared state slot, cloned into `Agent`.
pub type Shared = Mutex<UpdateState>;

#[cfg(test)]
mod tests {
    use super::*;

    const MANIFEST: &str = r#"{
        "version": "1.4.0",
        "platforms": {
            "windows-x86_64": {"url": "https://x/VardeSetup-1.4.0.exe", "signature": "SIG-W"},
            "linux-x86_64-agent-deb": {"url": "https://x/varde-agent-1.4.0-linux-amd64.deb", "signature": "SIG-D"}
        }
    }"#;

    #[test]
    fn newer_version_offers_signed_entry() {
        let a = pick(
            MANIFEST.as_bytes(),
            "1.3.0",
            Some("linux-x86_64-agent-deb"),
            Channel::Stable,
        )
        .unwrap()
        .unwrap();
        assert_eq!(a.version, "1.4.0");
        assert_eq!(
            a.url.as_deref().unwrap().rsplit('/').next().unwrap(),
            "varde-agent-1.4.0-linux-amd64.deb"
        );
        assert_eq!(a.signature.as_deref(), Some("SIG-D"));
    }

    #[test]
    fn same_or_older_offers_nothing() {
        assert!(pick(
            MANIFEST.as_bytes(),
            "1.4.0",
            Some("windows-x86_64"),
            Channel::Stable
        )
        .unwrap()
        .is_none());
        assert!(pick(
            MANIFEST.as_bytes(),
            "9.9.9",
            Some("windows-x86_64"),
            Channel::Stable
        )
        .unwrap()
        .is_none());
    }

    #[test]
    fn unknown_platform_key_is_notify_only() {
        let a = pick(
            MANIFEST.as_bytes(),
            "1.0.0",
            Some("linux-x86_64-agent-snap"),
            Channel::Stable,
        )
        .unwrap()
        .unwrap();
        assert!(a.url.is_none());
        // and with no key at all (manual install)
        assert!(pick(MANIFEST.as_bytes(), "1.0.0", None, Channel::Stable)
            .unwrap()
            .unwrap()
            .url
            .is_none());
    }

    #[test]
    fn leading_v_is_tolerated() {
        let m = MANIFEST.replace("\"1.4.0\"", "\"v1.4.0\"");
        assert_eq!(
            pick(m.as_bytes(), "1.3.0", None, Channel::Stable)
                .unwrap()
                .unwrap()
                .version,
            "v1.4.0"
        );
    }

    #[test]
    fn nightly_offers_on_difference_not_order() {
        let m = r#"{"version": "0.0.0-nightly.20261012+def456", "platforms": {}}"#;
        // stable install opted into nightly: semver-lower still counts
        assert!(pick(m.as_bytes(), "1.4.0", None, Channel::Nightly)
            .unwrap()
            .is_some());
        // same nightly rebuilt on a new sha: semver-equal still counts
        assert!(pick(
            m.as_bytes(),
            "0.0.0-nightly.20261012+abc123",
            None,
            Channel::Nightly
        )
        .unwrap()
        .is_some());
        // identical build: no update
        assert!(pick(
            m.as_bytes(),
            "0.0.0-nightly.20261012+def456",
            None,
            Channel::Nightly
        )
        .unwrap()
        .is_none());
        // an older dated nightly is a downgrade, not an offer
        let older = r#"{"version":"0.0.0-nightly.20261001+abc123","platforms":{}}"#;
        assert!(pick(
            older.as_bytes(),
            "0.0.0-nightly.20261012+def456",
            None,
            Channel::Nightly
        )
        .unwrap()
        .is_none());
    }
}

/// pb view of the shared state (called on every status build — keep cheap).
pub fn status_pb(state: &UpdateState) -> agent_ipc::pb::Update {
    agent_ipc::pb::Update {
        checked_at_unix_ms: state.checked_at_ms,
        available_version: state
            .available
            .as_ref()
            .map(|a| a.version.clone())
            .unwrap_or_default(),
        url: state
            .available
            .as_ref()
            .and_then(|a| a.url.clone())
            .unwrap_or_default(),
        signature: state
            .available
            .as_ref()
            .and_then(|a| a.signature.clone())
            .unwrap_or_default(),
        error: state.error.clone().unwrap_or_default(),
    }
}
