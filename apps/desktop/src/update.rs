//! Update apply. Windows: download the signed installer the agent's checker
//! surfaced (Status.update.url + .signature), verify it against the release
//! key and run it elevated and silent — varde-setup drains services, swaps
//! binaries and relaunches the tray. macOS: the sibling `varde-agent
//! update` verifies the feed itself and swaps every binary in this .app;
//! the tray then restarts the LaunchAgent and relaunches itself.
//! Never auto-applies while hosting.

use agent_ipc::pb;
use tauri::{AppHandle, Manager};

/// Release signing public key (same value as the agent's update::RELEASE_PUBKEY;
/// matches the TAURI_SIGNING_PRIVATE_KEY repo secret).
#[cfg(windows)]
pub const RELEASE_PUBKEY: &str = "RWTGjRo20qP1eV0SYg+R+cQklu9rnXL+hHg1fS729LTc0IOdbuVXinns";

/// Decode a minisign signature. The release pipeline emits Tauri's
/// single-line base64 envelope of the minisign text; accept that or the raw
/// minisign text.
#[cfg(windows)]
fn decode_sig(text: &str) -> anyhow::Result<minisign_verify::Signature> {
    use anyhow::Context as _;
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

#[cfg(windows)]
fn pubkey() -> Option<String> {
    std::env::var("VARDE_UPDATE_PUBKEY")
        .ok()
        .filter(|k| !k.is_empty())
        .or_else(|| (!RELEASE_PUBKEY.is_empty()).then(|| RELEASE_PUBKEY.to_string()))
}

/// "Update to X" clicked. Applies immediately when idle; while hosting, asks:
/// now (drains the hosted server), when idle (applies when hosting ends) or
/// later.
pub fn offer(app: &AppHandle) {
    let (hosting, version) = {
        let st = app.state::<crate::app::State>();
        let s = st.status.lock().unwrap().clone();
        let Some(s) = s else { return };
        let hosting = crate::view::hosting_line(&s.hosting);
        let version = s
            .update
            .as_ref()
            .map(|u| u.available_version.clone())
            .unwrap_or_default();
        (hosting, version)
    };
    if let Some(h) = hosting {
        match ask_hosting(&version, &h) {
            Choice::Now => spawn_apply(app.clone()),
            Choice::WhenIdle => {
                app.state::<crate::app::State>()
                    .update_when_idle
                    .store(true, std::sync::atomic::Ordering::Relaxed);
                info_box(
                    "Update queued",
                    "Varde will update when this PC stops hosting.",
                );
            }
            Choice::Later => {}
        }
    } else {
        spawn_apply(app.clone());
    }
}

enum Choice {
    Now,
    WhenIdle,
    Later,
}

#[cfg(windows)]
fn info_box(title: &str, text: &str) {
    use crate::app::wide;
    use windows_sys::Win32::UI::WindowsAndMessaging::{MessageBoxW, MB_ICONINFORMATION, MB_OK};
    // SAFETY: `wide` produces NUL-terminated UTF-16 strings; MessageBoxW
    // takes no ownership.
    unsafe {
        MessageBoxW(
            std::ptr::null_mut(),
            wide(text).as_ptr(),
            wide(title).as_ptr(),
            MB_OK | MB_ICONINFORMATION,
        )
    };
}

#[cfg(target_os = "macos")]
fn info_box(title: &str, text: &str) {
    let _ = rfd::MessageDialog::new()
        .set_level(rfd::MessageLevel::Info)
        .set_title(title)
        .set_description(text)
        .set_buttons(rfd::MessageButtons::Ok)
        .show();
}

#[cfg(windows)]
fn ask_hosting(version: &str, hosting: &str) -> Choice {
    use crate::app::wide;
    use windows_sys::Win32::UI::WindowsAndMessaging::{
        MessageBoxW, IDNO, IDYES, MB_ICONQUESTION, MB_YESNOCANCEL,
    };
    let text = format!(
        "Update Varde to {version} now?\r\n\r\n{hosting} — updating drains the server, swaps Varde and restarts it. Players see a short break.\r\n\r\nYes = update now\r\nNo = update when the server stops\r\nCancel = later"
    );
    // SAFETY: `wide` produces NUL-terminated UTF-16 strings; MessageBoxW
    // takes no ownership.
    let r = unsafe {
        MessageBoxW(
            std::ptr::null_mut(),
            wide(&text).as_ptr(),
            wide("Varde update").as_ptr(),
            MB_ICONQUESTION | MB_YESNOCANCEL,
        )
    };
    match r {
        IDYES => Choice::Now,
        IDNO => Choice::WhenIdle,
        _ => Choice::Later,
    }
}

#[cfg(target_os = "macos")]
fn ask_hosting(version: &str, hosting: &str) -> Choice {
    let text = format!(
        "Update Varde to {version} now?\n\n{hosting} — updating drains the server, swaps Varde and restarts it. Players see a short break."
    );
    match rfd::MessageDialog::new()
        .set_level(rfd::MessageLevel::Info)
        .set_title("Varde update")
        .set_description(&text)
        .set_buttons(rfd::MessageButtons::YesNoCancelCustom(
            "Update now".into(),
            "Update when idle".into(),
            "Later".into(),
        ))
        .show()
    {
        // custom labels come back as Custom(<label>); Yes/No cover the
        // platforms that keep the stock wording
        rfd::MessageDialogResult::Yes => Choice::Now,
        rfd::MessageDialogResult::No => Choice::WhenIdle,
        rfd::MessageDialogResult::Custom(ref s) if s == "Update now" => Choice::Now,
        rfd::MessageDialogResult::Custom(ref s) if s == "Update when idle" => Choice::WhenIdle,
        _ => Choice::Later,
    }
}

/// After each status publish: fire the queued "when idle" update once hosting
/// drains.
pub fn maybe_apply_when_idle(app: &AppHandle) {
    let st = app.state::<crate::app::State>();
    if !st
        .update_when_idle
        .load(std::sync::atomic::Ordering::Relaxed)
    {
        return;
    }
    let s = st.status.lock().unwrap().clone();
    // only fire once we KNOW the node is connected and not hosting — a
    // missing status means the service is unreachable, not "idle".
    if s.is_some_and(|s| s.hosting.is_empty()) {
        st.update_when_idle
            .store(false, std::sync::atomic::Ordering::Relaxed);
        spawn_apply(app.clone());
    }
}

/// Download → stage + verify minisign → launch elevated `/S /R` (silent
/// install + post-install tray relaunch as the original user, which the
/// updater flow wants).
#[cfg(windows)]
fn spawn_apply(app: AppHandle) {
    let found = {
        let st = app.state::<crate::app::State>();
        let s = st.status.lock().unwrap().clone();
        s.and_then(|s| s.update.map(|u| (u.available_version, u.url, u.signature)))
    };
    let Some((version, url, sig)) = found else {
        return;
    };
    tauri::async_runtime::spawn(async move {
        if let Err(e) = download_verify_run(&version, &url, &sig).await {
            info_box(
                "Update failed",
                &format!("Varde couldn't install the update. It will try again later.\n\n{e:#}"),
            );
        }
    });
}

#[cfg(windows)]
async fn download_verify_run(version: &str, url: &str, sig: &str) -> anyhow::Result<()> {
    use anyhow::Context as _;
    let Some(key) = pubkey() else {
        anyhow::bail!("this build has no update signing key; can't verify the installer");
    };
    if url.is_empty() || sig.is_empty() {
        anyhow::bail!("the update feed has no installer for this platform");
    }
    let exe = cp_api::client::fetch_url(url, 512 * 1024 * 1024)
        .await
        .context("download installer")?;
    let pk = minisign_verify::PublicKey::from_base64(&key)
        .map_err(|e| anyhow::anyhow!("bad update pubkey: {e}"))?;
    let s = decode_sig(sig).map_err(|e| anyhow::anyhow!("bad sig: {e}"))?;
    // the UAC prompt blocks ShellExecuteExW until answered; keep it off the
    // async runtime
    let version = version.to_string();
    tauri::async_runtime::spawn_blocking(move || stage_verify_launch(&version, &exe, &pk, &s))
        .await
        .context("installer task")?
}

/// Stage the installer under a fresh %LOCALAPPDATA%\Varde\update\<nonce>,
/// verify the bytes as read back from disk and launch it elevated.
///
/// Not %ProgramData%\Varde: the installer ACLs that tree to
/// SYSTEM+Administrators, so this non-elevated tray can't write there. The
/// per-user dir is writable by anything running as this user, so the file is
/// pinned instead: it's reopened sharing only FILE_SHARE_READ (no writers,
/// no delete/rename while the handle lives) and the signature is checked over
/// the bytes read through that handle — what's verified is what's on disk.
/// The handle stays open until ShellExecuteExW returns; by then the installer
/// process exists and its image is mapped, which Windows won't let anyone
/// overwrite. (varde-setup install mode never rereads its own exe — the
/// payload is embedded.)
#[cfg(windows)]
fn stage_verify_launch(
    version: &str,
    exe: &[u8],
    pk: &minisign_verify::PublicKey,
    sig: &minisign_verify::Signature,
) -> anyhow::Result<()> {
    use anyhow::Context as _;
    use std::io::{Read as _, Write as _};
    use std::os::windows::fs::OpenOptionsExt as _;
    use windows_sys::Win32::Storage::FileSystem::FILE_SHARE_READ;

    let base = std::path::PathBuf::from(
        std::env::var_os("LOCALAPPDATA").context("LOCALAPPDATA is not set")?,
    )
    .join(r"Varde\update");
    std::fs::create_dir_all(&base).with_context(|| format!("create {}", base.display()))?;
    // sweep earlier staging dirs; one whose installer still runs fails to
    // delete and is left for next time
    if let Ok(rd) = std::fs::read_dir(&base) {
        for e in rd.flatten() {
            let _ = std::fs::remove_dir_all(e.path());
        }
    }
    let nonce = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or_default();
    let dir = base.join(format!("{}-{nonce}", std::process::id()));
    // create_dir, not _all: the leaf must be new, never a pre-planted one
    std::fs::create_dir(&dir).with_context(|| format!("create {}", dir.display()))?;
    let path = dir.join(format!("VardeSetup-{version}.exe"));
    {
        let mut f = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&path)
            .with_context(|| format!("create {}", path.display()))?;
        f.write_all(exe)
            .with_context(|| format!("write {}", path.display()))?;
    }
    // A sharing violation here means someone else still holds the file open
    // for writing — treat it like any other failed write.
    let mut pinned = std::fs::OpenOptions::new()
        .read(true)
        .share_mode(FILE_SHARE_READ)
        .open(&path)
        .with_context(|| format!("reopen {}", path.display()))?;
    let mut on_disk = Vec::with_capacity(exe.len());
    pinned
        .read_to_end(&mut on_disk)
        .with_context(|| format!("read back {}", path.display()))?;
    pk.verify(&on_disk, sig, false)
        .map_err(|e| anyhow::anyhow!("installer signature invalid: {e}"))?;
    // /S silent, /R relaunches the tray as the original user post-install,
    // /ARGS feeds it --autostart so it comes back in background mode
    // (apps/setup/src/args.rs).
    let launched =
        crate::app::run_elevated(&path, "/S /R /ARGS --autostart").context("launch installer");
    drop(pinned);
    launched
}

/// The sibling `varde-agent update` verifies the feed signature and swaps
/// every binary in this .app (agent, mesh AND the tray), then the service
/// is restarted and the app relaunched — same end state as the Windows flow.
#[cfg(target_os = "macos")]
fn spawn_apply(app: AppHandle) {
    tauri::async_runtime::spawn(async move {
        let r = tokio::task::spawn_blocking(apply_via_agent).await;
        match r {
            Ok(Ok(Some(done))) => {
                // binaries on disk are new; service + app relaunch run them
                if let Err(e) = crate::svcctl::restart_service() {
                    info_box(
                        "Update failed",
                        &format!(
                            "Varde updated but its service couldn't restart. Restart Varde and try again.\n\n{e:#}"
                        ),
                    );
                    return;
                }
                let _ = done;
                relaunch_app(&app);
            }
            Ok(Ok(None)) => {} // already up to date / feed has no package
            Ok(Err(e)) => info_box(
                "Update failed",
                &format!("Varde couldn't install the update. It will try again later.\n\n{e:#}"),
            ),
            Err(e) => info_box(
                "Update failed",
                &format!("Varde couldn't install the update. It will try again later.\n\n{e}"),
            ),
        }
    });
}

/// Run `varde-agent update` next to this exe. Ok(Some) = a newer release was
/// applied (binaries swapped); Ok(None) = nothing applied, and the CLI's
/// own message ("already up to date" / "feed has no package") is shown.
#[cfg(target_os = "macos")]
fn apply_via_agent() -> anyhow::Result<Option<String>> {
    use anyhow::Context as _;
    let exe = std::env::current_exe().context("resolve exe")?;
    let agent = exe.with_file_name("varde-agent");
    anyhow::ensure!(agent.exists(), "no varde-agent next to {}", exe.display());
    let out = std::process::Command::new(&agent)
        .arg("update")
        .output()
        .context("run varde-agent update")?;
    let text = format!(
        "{}{}",
        String::from_utf8_lossy(&out.stdout),
        String::from_utf8_lossy(&out.stderr)
    );
    anyhow::ensure!(out.status.success(), "varde-agent update: {}", text.trim());
    Ok(text.contains("updated to").then_some(text))
}

/// Relaunch the app after the binaries under it changed. Outside a bundle
/// (dev builds) there's nothing to relaunch — the user restarts it.
#[cfg(target_os = "macos")]
fn relaunch_app(app: &AppHandle) {
    let bundle = std::env::current_exe().ok().and_then(|exe| {
        exe.ancestors()
            .find(|p| p.extension().is_some_and(|e| e == "app"))
            .map(|p| p.to_path_buf())
    });
    match bundle {
        Some(path) => {
            // `open` can race our own exit, and the single-instance guard
            // would eat the new process — defer it past the quit.
            let _ = std::process::Command::new("sh")
                .args(["-c", "sleep 1; open -n \"$1\"", "relaunch"])
                .arg(&path)
                .spawn();
            app.exit(0);
        }
        None => info_box(
            "Varde update",
            "Varde updated — quit and reopen it to run the new version.",
        ),
    }
}

/// "Check for updates…" menu item: asks the agent to re-check the feed now;
/// the result lands on the WatchStatus stream the tray already consumes.
pub fn recheck(app: AppHandle) {
    tauri::async_runtime::spawn(async move {
        if let Ok(mut c) = agent_ipc::connect(&agent_ipc::default_endpoint()).await {
            let _ = c.check_update(pb::CheckUpdateRequest {}).await;
        }
        let _ = app;
    });
}
