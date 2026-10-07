//! Linking this machine to a group: the device-code flow shared by
//! `varde-agent enroll` and the service-side flow the tray starts over IPC.

use std::path::Path;

use anyhow::{bail, Result};

use crate::config::Config;

/// The machine name shown on the dashboard's link page.
pub fn hostname() -> String {
    let from_env = |k: &str| std::env::var(k).ok().filter(|s| !s.trim().is_empty());
    if cfg!(windows) {
        if let Some(h) = from_env("COMPUTERNAME") {
            return h;
        }
    }
    if let Some(h) = from_env("HOSTNAME") {
        return h;
    }
    os_hostname().unwrap_or_else(|| "unknown".into())
}

#[cfg(unix)]
fn os_hostname() -> Option<String> {
    let mut buf = [0u8; 256];
    // SAFETY: buf is writable for its full length.
    if unsafe { libc::gethostname(buf.as_mut_ptr() as *mut libc::c_char, buf.len()) } != 0 {
        return None;
    }
    let end = buf.iter().position(|&b| b == 0).unwrap_or(buf.len());
    let s = String::from_utf8_lossy(&buf[..end]).trim().to_string();
    (!s.is_empty()).then_some(s)
}

#[cfg(not(unix))]
fn os_hostname() -> Option<String> {
    None
}

/// Validates a user-entered control-plane address: http(s), or a bare
/// host[:port] which is read as https. No path noise.
/// Address the installer chose (`<data>/server.url`), reported while not
/// linked so the tray can start the device flow without asking again.
pub fn preset_url(data_dir: &std::path::Path) -> Option<String> {
    let raw = std::fs::read_to_string(data_dir.join("server.url")).ok()?;
    normalize_url(raw.trim_start_matches('\u{feff}').lines().next()?).ok()
}

pub fn normalize_url(raw: &str) -> Result<String> {
    let s = raw.trim();
    // A bare host or host:port is assumed to be https — nobody should have to
    // type a scheme to point Varde at their own server.
    let s = if s.contains("://") {
        s.trim_end_matches('/').to_string()
    } else {
        format!("https://{}", s.trim_end_matches('/'))
    };
    let (scheme, rest) = if s
        .get(..8)
        .is_some_and(|p| p.eq_ignore_ascii_case("https://"))
    {
        ("https", &s[8..])
    } else if s
        .get(..7)
        .is_some_and(|p| p.eq_ignore_ascii_case("http://"))
    {
        ("http", &s[7..])
    } else {
        bail!("the address must start with https:// or http://")
    };
    if rest.is_empty() || rest.contains(char::is_whitespace) {
        bail!("not a valid address: {raw}");
    }
    Ok(format!("{scheme}://{rest}"))
}

fn host_of(url: &str) -> &str {
    let rest = url.split_once("://").map(|(_, r)| r).unwrap_or(url);
    let authority = rest.split(['/', '?', '#']).next().unwrap_or("");
    let host = authority
        .rsplit_once('@')
        .map(|(_, h)| h)
        .unwrap_or(authority);
    if let Some(v6) = host.strip_prefix('[') {
        return v6.split(']').next().unwrap_or("");
    }
    host.split(':').next().unwrap_or("")
}

fn is_loopback_host(h: &str) -> bool {
    h.eq_ignore_ascii_case("localhost") || h.starts_with("127.") || h == "::1"
}

/// The page the user opens to approve the code. The control plane's
/// `verification_url` wins unless it points at loopback while the address the
/// user gave doesn't (a control plane started without `--public-url`):
/// then the code page is built from the user's address.
pub fn link_url(cp_url: &str, dev: &cp_api::DeviceEnrollResponse) -> String {
    let v = dev.verification_url.trim();
    if v.is_empty() || (is_loopback_host(host_of(v)) && !is_loopback_host(host_of(cp_url))) {
        return format!(
            "{}/link?code={}",
            cp_url.trim_end_matches('/'),
            dev.user_code
        );
    }
    v.to_string()
}

pub async fn start_device(
    client: &cp_api::CpClient,
    public_key: String,
) -> Result<cp_api::DeviceEnrollResponse> {
    Ok(client
        .json(
            "POST",
            "/v1/agent/enroll/device",
            Some(&cp_api::DeviceEnrollRequest {
                public_key,
                hostname: Some(hostname()),
                os: Some(std::env::consts::OS.into()),
                arch: Some(std::env::consts::ARCH.into()),
                agent_version: Some(env!("CARGO_PKG_VERSION").into()),
            }),
        )
        .await?)
}

pub enum Poll {
    Pending,
    Approved(cp_api::EnrollResult),
    Expired,
}

pub async fn poll_device(client: &cp_api::CpClient, device_code: &str) -> Result<Poll> {
    let body = serde_json::to_vec(&cp_api::DevicePollRequest {
        device_code: device_code.to_string(),
    })?;
    let (status, out) = client
        .call("POST", "/v1/agent/enroll/device/poll", Some(&body))
        .await?;
    match status {
        200 => Ok(Poll::Approved(serde_json::from_slice(&out)?)),
        202 => Ok(Poll::Pending),
        410 => Ok(Poll::Expired),
        s => bail!("link poll failed: {s} {}", String::from_utf8_lossy(&out)),
    }
}

/// config.toml for a freshly linked machine. Local overrides (test prefixes,
/// mesh binary, margins…) carry over from `prev` on a re-link.
pub fn enrolled_config(cp_url: &str, r: &cp_api::EnrollResult, prev: Option<Config>) -> Config {
    let mut c = prev.unwrap_or_else(|| Config {
        control_plane_url: String::new(),
        node_id: String::new(),
        group_id: String::new(),
        control_plane_public_key: String::new(),
        anchor: false,
        mesh_bin: None,
        testgame_bin: None,
        loopback_prefix: None,
        fence_margin_ms: 5000,
        shutdown_replication_timeout_s: 60,
        force_relay: false,
        mesh_listen_port: 0,
    });
    c.control_plane_url = cp_url.to_string();
    c.node_id = r.node_id.clone();
    c.group_id = r.group_id.clone();
    c.control_plane_public_key = r.control_plane_public_key.clone();
    c
}

pub fn key_path(data_dir: &Path) -> std::path::PathBuf {
    data_dir.join("identity").join("node.key")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn dev(v: &str) -> cp_api::DeviceEnrollResponse {
        cp_api::DeviceEnrollResponse {
            device_code: "d".into(),
            user_code: "ABCD-EFGH".into(),
            verification_url: v.into(),
            expires_in: 600,
            interval: 2,
        }
    }

    #[test]
    fn link_url_prefers_control_plane_unless_loopback() {
        let d = dev("https://varde.games/link?code=ABCD-EFGH");
        assert_eq!(
            link_url("https://varde.games", &d),
            "https://varde.games/link?code=ABCD-EFGH"
        );
        let d = dev("http://localhost:8080/link?code=ABCD-EFGH");
        assert_eq!(
            link_url("http://192.168.1.10:8080/", &d),
            "http://192.168.1.10:8080/link?code=ABCD-EFGH"
        );
        // a local control plane really is on loopback
        assert_eq!(
            link_url("http://127.0.0.1:8080", &d),
            "http://localhost:8080/link?code=ABCD-EFGH"
        );
        assert_eq!(
            link_url("https://cp.example", &dev("")),
            "https://cp.example/link?code=ABCD-EFGH"
        );
    }

    #[test]
    fn normalize_url_rules() {
        assert_eq!(
            normalize_url(" https://varde.games/ ").unwrap(),
            "https://varde.games"
        );
        assert_eq!(normalize_url("varde.games").unwrap(), "https://varde.games");
        assert_eq!(
            normalize_url("HTTPS://Cp.Example/X").unwrap(),
            "https://Cp.Example/X"
        );
        assert_eq!(
            normalize_url("cp.example.com:8443").unwrap(),
            "https://cp.example.com:8443"
        );
        assert!(normalize_url("varde games").is_err());
        assert!(normalize_url("https://").is_err());
        assert!(normalize_url("ftp://x").is_err());
    }

    #[test]
    fn hostname_is_not_empty() {
        assert!(!hostname().is_empty());
    }
}
