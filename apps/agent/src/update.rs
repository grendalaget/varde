//! Update check: poll the channel's latest.json (Tauri static manifest) on a
//! timer, verify its minisign signature, and report the result over local IPC
//! (Status.update) and in the heartbeat (update_available). Applying an
//! update is platform-specific and lives elsewhere — this only discovers it.

#[cfg(target_os = "linux")]
use std::io::Write;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use anyhow::{Context, Result};
use serde::Deserialize;
use tracing::debug;

use crate::Agent;

/// How often to re-check when the config doesn't say otherwise.
const DEFAULT_INTERVAL_S: i64 = 6 * 3600;

/// Decode a minisign signature. `tauri signer sign` emits the signature as a
/// single-line base64 envelope of the minisign text (the .sig files on the
/// release and the `signature` fields inside latest.json); accept that or the
/// raw minisign text.
fn decode_sig(text: &str) -> Result<minisign_verify::Signature> {
    if let Ok(sig) = minisign_verify::Signature::decode(text) {
        return Ok(sig);
    }
    use base64::Engine as _;
    let raw = base64::engine::general_purpose::STANDARD
        .decode(text.trim())
        .context("signature is neither minisign text nor its base64 envelope")?;
    let text = String::from_utf8(raw).context("signature envelope is not utf-8")?;
    minisign_verify::Signature::decode(&text).map_err(|e| anyhow::anyhow!("bad sig: {e}"))
}

/// minisign public key of the release signer; matches the
/// TAURI_SIGNING_PRIVATE_KEY repo secret the pipeline signs with. The
/// `update.pubkey` config key overrides it for checking and staging; the
/// root-side `apply-update` ignores that and trusts only this key or a
/// root-owned /etc/varde/update.pub.
pub const RELEASE_PUBKEY: &str = "RWTGjRo20qP1eV0SYg+R+cQklu9rnXL+hHg1fS729LTc0IOdbuVXinns";

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

pub fn channel(cfg: &crate::config::UpdateCfg) -> Channel {
    match cfg.channel.as_deref().unwrap_or("stable") {
        "nightly" => Channel::Nightly,
        "off" | "none" | "disabled" => Channel::Off,
        _ => Channel::Stable,
    }
}

fn feed_url(cfg: &crate::config::UpdateCfg) -> Option<String> {
    if let Some(u) = &cfg.feed_url {
        return Some(u.clone());
    }
    let release = match channel(cfg) {
        Channel::Stable => "latest/download",
        Channel::Nightly => "download/nightly",
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
        // Inside a .app the release's Varde-*.dmg is the matching package
        // (it replaces the whole app's binaries, tray included); a bare
        // binary install keeps the flat agent dmg.
        let flavor = if in_app_bundle() { "app" } else { "agent" };
        Some(format!("darwin-{arch}-{flavor}"))
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

fn pubkey(cfg: &crate::config::UpdateCfg) -> Option<String> {
    let k = cfg
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
    let res = fetch_update(&agent.cfg.update).await;
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

async fn fetch_update(cfg: &crate::config::UpdateCfg) -> Result<Option<Available>> {
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
    let sig = decode_sig(std::str::from_utf8(&sig_body).context("manifest sig not utf8")?)
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
    if channel(&agent.cfg.update) == Channel::Off {
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

// ---------- unix apply (linux systemd path-unit / macOS binary swap) ----------

/// `varde-agent update` on unix. Linux deb/rpm: stage the verified package
/// under <data>/update and drop the `.pending` marker — the packaged
/// varde-update.path unit then runs `varde-agent apply-update` as root.
/// macOS: mount the verified dmg and swap the binaries — inside
/// `Varde.app/Contents/MacOS` for app installs, next to the running exe for
/// flat installs (a launchd/supervisor restart picks it up).
/// Android binaries ship inside the host app — updates are APK upgrades.
#[cfg(target_os = "android")]
pub async fn cli_update(_data_dir: &std::path::Path) -> Result<()> {
    anyhow::bail!("updates are delivered by the Android app")
}

#[cfg(all(unix, not(target_os = "android")))]
pub async fn cli_update(data_dir: &std::path::Path) -> Result<()> {
    let cfg = crate::config::Config::load_opt(data_dir)?
        .map(|c| c.update)
        .unwrap_or_default();
    let Some(av) = fetch_update(&cfg).await.context("check update feed")? else {
        println!("already up to date ({})", env!("CARGO_PKG_VERSION"));
        return Ok(());
    };
    let (Some(url), Some(sig)) = (av.url.as_deref(), av.signature.as_deref()) else {
        println!(
            "update available: {} — but the feed has no package for this install; download it from {}",
            av.version,
            url_host(data_dir)
        );
        return Ok(());
    };
    // artifacts must come over TLS; loopback http stays allowed for dev feeds
    if !artifact_url_ok(url) {
        anyhow::bail!("refusing non-https artifact url: {url}");
    }
    let body = cp_api::client::fetch_url(url, MAX_PKG_BYTES)
        .await
        .with_context(|| format!("download {url}"))?;
    let pk = minisign_verify::PublicKey::from_base64(
        &pubkey(&cfg).context("no update pubkey configured")?,
    )
    .map_err(|e| anyhow::anyhow!("bad update pubkey: {e}"))?;
    let s = decode_sig(sig).map_err(|e| anyhow::anyhow!("bad sig: {e}"))?;
    pk.verify(&body, &s, false)
        .map_err(|e| anyhow::anyhow!("package signature invalid: {e}"))?;

    #[cfg(target_os = "linux")]
    {
        let name = if url.ends_with(".deb") {
            "pkg.deb"
        } else if url.ends_with(".rpm") {
            "pkg.rpm"
        } else {
            anyhow::bail!("feed URL is not a .deb/.rpm package: {url}");
        };
        let dir = data_dir.join("update");
        std::fs::create_dir_all(&dir)?;
        let tmp = dir.join("pkg.tmp");
        std::fs::write(&tmp, &body).context("write staged package")?;
        std::fs::rename(&tmp, dir.join(name))?;
        // the signature rides along: the root-side apply re-verifies these
        // bytes (this dir is varde-writable — trust nothing staged)
        std::fs::write(dir.join("pkg.sig"), sig).context("write staged signature")?;
        // the marker goes last: the path unit fires on it, never on a
        // half-written package
        std::fs::File::create(dir.join(".pending"))?.write_all(b"")?;
        println!(
            "staged {name} — varde-update.service applies it shortly (see `systemctl status varde-update.service`)"
        );
    }
    #[cfg(target_os = "macos")]
    {
        apply_dmg(&body).context("swap binaries")?;
        println!("updated to {} — restart the agent to run it", av.version);
    }
    Ok(())
}

/// Largest update package the agent downloads or root installs.
#[cfg(all(unix, not(target_os = "android")))]
const MAX_PKG_BYTES: usize = 512 * 1024 * 1024;

#[cfg(all(unix, not(target_os = "android")))]
fn url_host(_data_dir: &std::path::Path) -> &'static str {
    "https://varde.games/download"
}

/// Artifact download policy: https everywhere; http only to loopback hosts
/// (dev feeds). Anything else is a signed-but-wrong URL — never fetch it.
#[cfg(all(unix, not(target_os = "android")))]
fn artifact_url_ok(url: &str) -> bool {
    let u = url.to_ascii_lowercase();
    if u.starts_with("https://") {
        return true;
    }
    if let Some(rest) = u.strip_prefix("http://") {
        let host = rest.split([':', '/', '?', '#']).next().unwrap_or("");
        return host == "localhost" || host == "127.0.0.1" || host == "[::1]";
    }
    false
}

/// The .app bundle the running exe belongs to, if any.
#[cfg(target_os = "macos")]
fn app_bundle() -> Option<std::path::PathBuf> {
    let exe = std::env::current_exe().ok()?;
    exe.ancestors()
        .find(|p| p.extension().is_some_and(|e| e == "app"))
        .map(|p| p.to_path_buf())
}

#[cfg(target_os = "macos")]
fn in_app_bundle() -> bool {
    app_bundle().is_some()
}

/// Mount a dmg into a temp dir, copy the binaries it carries next to the
/// running exe, detach. The exe dir is wherever the user installed them —
/// inside Varde.app this swaps agent, mesh AND the tray in one go. Two
/// dmg layouts: the flat varde-agent dmg (binaries at the root) and the
/// Varde app dmg (inside Varde.app/Contents/MacOS).
#[cfg(target_os = "macos")]
fn apply_dmg(dmg: &[u8]) -> Result<()> {
    let exe_dir = std::env::current_exe()
        .context("resolve exe")?
        .parent()
        .context("exe has no dir")?
        .to_path_buf();
    let tmp = std::env::temp_dir().join(format!("varde-update-{}", std::process::id()));
    std::fs::create_dir_all(&tmp)?;
    let dmg_path = tmp.join("update.dmg");
    let mnt = tmp.join("mnt");
    std::fs::create_dir_all(&mnt)?;
    std::fs::write(&dmg_path, dmg)?;
    let run = |prog: &str, args: &[&str]| -> Result<()> {
        let st = std::process::Command::new(prog).args(args).status()?;
        if st.success() {
            Ok(())
        } else {
            anyhow::bail!("{prog} {args:?} -> {st}")
        }
    };
    run(
        "hdiutil",
        &[
            "attach",
            "-nobrowse",
            "-readonly",
            "-mountpoint",
            mnt.to_str().unwrap(),
            dmg_path.to_str().unwrap(),
        ],
    )?;
    let res = (|| -> Result<()> {
        // App dmgs hold the binaries inside the bundle; flat dmgs at the root.
        let src_dir = match mnt.join("Varde.app/Contents/MacOS") {
            d if d.is_dir() => d,
            _ => mnt.clone(),
        };
        for name in ["varde-agent", "varde-mesh", "varde-tray"] {
            let src = src_dir.join(name);
            if !src.exists() {
                continue;
            }
            let dst_tmp = exe_dir.join(format!(".{name}.new"));
            std::fs::copy(&src, &dst_tmp)?;
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                std::fs::set_permissions(&dst_tmp, std::fs::Permissions::from_mode(0o755))?;
            }
            std::fs::rename(&dst_tmp, exe_dir.join(name))?;
        }
        // Gatekeeper: a bundle that still verifies (--deep --strict covers
        // the binaries we just swapped) keeps its quarantine flag so the
        // next `open` re-checks it — tampered contents get flagged instead
        // of silently running. Unsigned/ad-hoc builds and seals our swap
        // invalidated have nothing left to verify: strip the flag so the
        // app doesn't read as damaged.
        if let Some(app) = app_bundle() {
            let verified = std::process::Command::new("codesign")
                .args(["--verify", "--deep", "--strict"])
                .arg(&app)
                .status()
                .map(|s| s.success())
                .unwrap_or(false);
            if !verified {
                let _ = std::process::Command::new("xattr")
                    .args(["-dr", "com.apple.quarantine"])
                    .arg(&app)
                    .status();
            }
        }
        Ok(())
    })();
    let _ = std::process::Command::new("hdiutil")
        .args(["detach", "-quiet", mnt.to_str().unwrap()])
        .status();
    let _ = std::fs::remove_dir_all(&tmp);
    res
}

/// Root-trusted override of the release key for `apply-update` (dev/test
/// feeds signed with their own key). Honoured only while root-owned and not
/// group/world-writable — unlike config.toml, which the agent's uid owns.
#[cfg(target_os = "linux")]
const ROOT_PUBKEY_FILE: &str = "/etc/varde/update.pub";

/// Where the root apply keeps its private copy of the verified package:
/// root-only tmpfs, outside anything the agent's uid can write.
#[cfg(target_os = "linux")]
const ROOT_APPLY_DIR: &str = "/run";

/// Key the root-side apply verifies with: the compiled-in release key, or
/// the contents of `path` when that file and its directory are owned by
/// `owner_uid` (root) and writable by nobody else. `[update] pubkey` in the
/// data dir's config.toml is never consulted here: the agent's uid — and so
/// any game server it runs — can write that file, and with it pick the key
/// root installs packages under.
#[cfg(all(unix, any(target_os = "linux", test)))]
fn root_apply_pubkey(path: &std::path::Path, owner_uid: u32) -> Result<String> {
    use std::os::unix::fs::MetadataExt;
    let md = match std::fs::symlink_metadata(path) {
        Ok(md) => md,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(RELEASE_PUBKEY.to_string()),
        Err(e) => return Err(e).with_context(|| format!("stat {}", path.display())),
    };
    let trusted = |md: &std::fs::Metadata| md.uid() == owner_uid && md.mode() & 0o022 == 0;
    if !md.is_file() || !trusted(&md) {
        anyhow::bail!(
            "refusing {}: must be a regular file owned by uid {owner_uid}, not group/world-writable",
            path.display()
        );
    }
    let dir = path.parent().context("key file has no directory")?;
    let dmd = std::fs::metadata(dir).with_context(|| format!("stat {}", dir.display()))?;
    if !trusted(&dmd) {
        anyhow::bail!(
            "refusing {}: its directory must be owned by uid {owner_uid}, not group/world-writable",
            path.display()
        );
    }
    let key = std::fs::read_to_string(path).with_context(|| format!("read {}", path.display()))?;
    let key = key.trim();
    if key.is_empty() {
        anyhow::bail!("{} is empty", path.display());
    }
    Ok(key.to_string())
}

/// Read a staged file without following a symlink or blocking on a FIFO the
/// agent's uid planted, capped at `max` bytes.
#[cfg(target_os = "linux")]
fn read_staged(path: &std::path::Path, max: usize) -> Result<Vec<u8>> {
    use std::io::Read;
    use std::os::unix::fs::OpenOptionsExt;
    let f = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
        .with_context(|| format!("open {}", path.display()))?;
    if !f.metadata()?.is_file() {
        anyhow::bail!("{} is not a regular file", path.display());
    }
    let mut buf = Vec::new();
    f.take(max as u64 + 1).read_to_end(&mut buf)?;
    if buf.len() > max {
        anyhow::bail!("{} exceeds {max} bytes", path.display());
    }
    Ok(buf)
}

/// Root-side apply invoked by varde-update.path (Type=oneshot): install the
/// staged package with the system package manager. The package's postinst
/// restarts the agent with the new binary.
#[cfg(target_os = "linux")]
pub fn apply_staged(data_dir: &std::path::Path) -> Result<()> {
    apply_staged_with(
        data_dir,
        std::path::Path::new(ROOT_PUBKEY_FILE),
        std::path::Path::new(ROOT_APPLY_DIR),
    )
}

#[cfg(target_os = "linux")]
fn apply_staged_with(
    data_dir: &std::path::Path,
    key_file: &std::path::Path,
    private_base: &std::path::Path,
) -> Result<()> {
    let dir = data_dir.join("update");
    let pending = dir.join(".pending");
    if !pending.exists() {
        return Ok(());
    }
    let name = ["pkg.deb", "pkg.rpm"]
        .into_iter()
        .find(|n| dir.join(n).exists())
        .context("no staged package")?;
    let pkg = dir.join(name);
    // Re-verify before root installs anything: the staging dir is
    // varde-writable, so a swapped package must fail signature check —
    // an attacker with the agent's uid can't forge minisign bytes. The key
    // comes from root-owned places only (see root_apply_pubkey).
    let key = root_apply_pubkey(key_file, 0)?;
    let pk = minisign_verify::PublicKey::from_base64(&key)
        .map_err(|e| anyhow::anyhow!("bad update pubkey: {e}"))?;
    let sig = read_staged(&dir.join("pkg.sig"), 64 * 1024).context("staged signature")?;
    let sig = std::str::from_utf8(&sig).context("staged signature not utf-8")?;
    let s = decode_sig(sig).map_err(|e| anyhow::anyhow!("bad sig: {e}"))?;
    let body = read_staged(&pkg, MAX_PKG_BYTES).context("read staged package")?;
    pk.verify(&body, &s, false)
        .map_err(|e| anyhow::anyhow!("staged package signature invalid: {e}"))?;
    // Install the bytes just verified, never the staged path: the agent's
    // uid could swap that file between verify and dpkg/rpm opening it.
    let private = private_base.join(format!("varde-update.{}", std::process::id()));
    let res = install_private_copy(&private, name, &body);
    let _ = std::fs::remove_dir_all(&private);
    // on failure keep .pending + the package: the trigger stays armed so a
    // re-run (or a fresh `varde-agent update`) can retry the failed install
    res?;
    std::fs::remove_file(&pending)?;
    std::fs::remove_file(&pkg)?;
    let _ = std::fs::remove_file(dir.join("pkg.sig"));
    println!("applied update");
    Ok(())
}

/// Write `body` into a fresh 0700 directory as `name` (created exclusively,
/// 0600) and install it from there.
#[cfg(target_os = "linux")]
fn install_private_copy(private: &std::path::Path, name: &str, body: &[u8]) -> Result<()> {
    use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
    // a leftover of a crashed run with this pid: the base is root-only, so
    // it is ours — but a dir that can't be replaced fails the create below
    let _ = std::fs::remove_dir_all(private);
    std::fs::DirBuilder::new()
        .mode(0o700)
        .create(private)
        .with_context(|| format!("create {}", private.display()))?;
    let copy = private.join(name);
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&copy)
        .context("create private package copy")?;
    f.write_all(body)?;
    f.sync_all()?;
    drop(f);
    let (prog, flag) = match copy.extension().and_then(|e| e.to_str()) {
        Some("deb") => ("dpkg", "-i"),
        Some("rpm") => ("rpm", "-U"),
        _ => anyhow::bail!("unknown staged package"),
    };
    let status = std::process::Command::new(prog)
        .arg(flag)
        .arg(&copy)
        .status()?;
    if !status.success() {
        anyhow::bail!("package install failed: {status}");
    }
    Ok(())
}

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

    #[cfg(target_os = "linux")]
    #[test]
    fn apply_staged_noops_without_pending() {
        let d = std::env::temp_dir().join(format!("varde-upd-{}", std::process::id()));
        std::fs::create_dir_all(d.join("update")).unwrap();
        assert!(apply_staged(&d).is_ok());
        std::fs::remove_dir_all(&d).unwrap();
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn apply_staged_errors_on_missing_pkg() {
        let d = std::env::temp_dir().join(format!("varde-upd2-{}", std::process::id()));
        let dir = d.join("update");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join(".pending"), b"").unwrap();
        let r = apply_staged(&d);
        assert!(r.is_err());
        assert!(dir.join(".pending").exists(), "trigger kept for retry");
        std::fs::remove_dir_all(&d).unwrap();
    }

    #[cfg(unix)]
    mod root_key {
        use super::super::*;
        use std::os::unix::fs::PermissionsExt;

        fn euid() -> u32 {
            unsafe { libc::geteuid() }
        }

        fn chmod(p: &std::path::Path, mode: u32) {
            std::fs::set_permissions(p, std::fs::Permissions::from_mode(mode)).unwrap();
        }

        /// key dir (0755) + key file (0644), both owned by the test's uid.
        fn key_file(tmp: &std::path::Path, key: &str) -> std::path::PathBuf {
            let dir = tmp.join("etc-varde");
            std::fs::create_dir(&dir).unwrap();
            chmod(&dir, 0o755);
            let f = dir.join("update.pub");
            std::fs::write(&f, format!("{key}\n")).unwrap();
            chmod(&f, 0o644);
            f
        }

        #[test]
        fn absent_override_means_release_key() {
            let tmp = tempfile::tempdir().unwrap();
            let k = root_apply_pubkey(&tmp.path().join("nope/update.pub"), euid()).unwrap();
            assert_eq!(k, RELEASE_PUBKEY);
        }

        #[test]
        fn owner_only_override_is_used() {
            let tmp = tempfile::tempdir().unwrap();
            let f = key_file(tmp.path(), "RWdevkey");
            assert_eq!(root_apply_pubkey(&f, euid()).unwrap(), "RWdevkey");
        }

        #[test]
        fn override_owned_by_another_uid_is_refused() {
            // in production the owner must be root (0); a file the agent's
            // uid owns is exactly what a compromised game server could write
            let tmp = tempfile::tempdir().unwrap();
            let f = key_file(tmp.path(), "RWdevkey");
            assert!(root_apply_pubkey(&f, euid().wrapping_add(1)).is_err());
        }

        #[test]
        fn writable_override_or_dir_is_refused() {
            let tmp = tempfile::tempdir().unwrap();
            let f = key_file(tmp.path(), "RWdevkey");
            chmod(&f, 0o664);
            assert!(
                root_apply_pubkey(&f, euid()).is_err(),
                "group-writable file"
            );
            chmod(&f, 0o646);
            assert!(
                root_apply_pubkey(&f, euid()).is_err(),
                "world-writable file"
            );
            chmod(&f, 0o644);
            chmod(f.parent().unwrap(), 0o777);
            assert!(root_apply_pubkey(&f, euid()).is_err(), "world-writable dir");
            chmod(f.parent().unwrap(), 0o755);
            assert!(root_apply_pubkey(&f, euid()).is_ok());
        }

        #[test]
        fn symlinked_override_is_refused() {
            let tmp = tempfile::tempdir().unwrap();
            let f = key_file(tmp.path(), "RWdevkey");
            let link = f.with_file_name("link.pub");
            std::os::unix::fs::symlink(&f, &link).unwrap();
            assert!(root_apply_pubkey(&link, euid()).is_err());
        }

        #[cfg(target_os = "linux")]
        #[test]
        fn staged_symlink_is_not_followed() {
            let tmp = tempfile::tempdir().unwrap();
            let real = tmp.path().join("real");
            std::fs::write(&real, b"bytes").unwrap();
            let link = tmp.path().join("pkg.deb");
            std::os::unix::fs::symlink(&real, &link).unwrap();
            assert!(read_staged(&link, 1024).is_err());
            assert_eq!(read_staged(&real, 1024).unwrap(), b"bytes");
            assert!(read_staged(&real, 2).is_err(), "size cap");
        }
    }
}
