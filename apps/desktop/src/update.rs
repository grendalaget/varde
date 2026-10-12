//! Update apply: download the signed installer the agent's checker surfaced
//! (Status.update.url + .signature), verify it against the release key and run
//! it silently — the NSIS installer's own hooks drain services, swap binaries
//! and relaunch the tray. Never auto-applies while hosting.

use agent_ipc::pb;
use anyhow::{Context as _, Result};
use tauri::{AppHandle, Manager};

/// Release signing public key (same value as the agent's update::RELEASE_PUBKEY;
/// matches the TAURI_SIGNING_PRIVATE_KEY repo secret).
pub const RELEASE_PUBKEY: &str = "RWTGjRo20qP1eV0SYg+R+cQklu9rnXL+hHg1fS729LTc0IOdbuVXinns";

/// Decode a minisign signature. The release pipeline emits Tauri's
/// single-line base64 envelope of the minisign text; accept that or the raw
/// minisign text.
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

fn info_box(title: &str, text: &str) -> i32 {
    use std::ffi::OsStr;
    use std::os::windows::ffi::OsStrExt;
    use windows_sys::Win32::UI::WindowsAndMessaging::{MessageBoxW, MB_ICONINFORMATION, MB_OK};
    let wide = |s: &str| -> Vec<u16> { OsStr::new(s).encode_wide().chain(Some(0)).collect() };
    unsafe {
        MessageBoxW(
            std::ptr::null_mut(),
            wide(text).as_ptr(),
            wide(title).as_ptr(),
            MB_OK | MB_ICONINFORMATION,
        )
    }
}

fn ask_hosting(version: &str, hosting: &str) -> Choice {
    use std::ffi::OsStr;
    use std::os::windows::ffi::OsStrExt;
    use windows_sys::Win32::UI::WindowsAndMessaging::{
        MessageBoxW, IDNO, IDYES, MB_ICONQUESTION, MB_YESNOCANCEL,
    };
    let text = format!(
        "Update Varde to {version} now?\r\n\r\n{hosting} — updating drains the server, swaps Varde and restarts it. Players see a short break.\r\n\r\nYes = update now\r\nNo = update when the server stops\r\nCancel = later"
    );
    let wide = |s: &str| -> Vec<u16> { OsStr::new(s).encode_wide().chain(Some(0)).collect() };
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

/// Download → verify minisign → spawn `/S /R` (silent install + post-install
/// tray relaunch as the original user, which the updater flow wants).
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
            info_box("Update failed", &format!("{e:#}"));
        }
    });
}

async fn download_verify_run(version: &str, url: &str, sig: &str) -> Result<()> {
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
    pk.verify(&exe, &s, false)
        .map_err(|e| anyhow::anyhow!("installer signature invalid: {e}"))?;

    // Stage under %ProgramData%\Varde\update — the installer ACL'd that tree
    // to SYSTEM+Administrators, so a non-admin process can't swap the file
    // between signature verification and launch. %TEMP% would not give that.
    let dir = std::path::PathBuf::from(
        std::env::var("ProgramData").unwrap_or_else(|_| r"C:\ProgramData".to_string()),
    )
    .join(r"Varde\update");
    std::fs::create_dir_all(&dir).with_context(|| format!("create {}", dir.display()))?;
    let path = dir.join(format!("VardeSetup-{version}.exe"));
    std::fs::write(&path, &exe).with_context(|| format!("write {}", path.display()))?;
    // NSIS self-elevates (RequestExecutionLevel admin → UAC prompt). /S silent,
    // /R relaunches the main binary as the original user post-install, /ARGS
    // feeds it --autostart so the tray comes back in background mode.
    std::process::Command::new(&path)
        .args(["/S", "/R", "/ARGS", "--autostart"])
        .spawn()
        .context("launch installer")?;
    Ok(())
}

/// "Check for updates…" menu item: asks the agent to re-check the feed now;
/// the result lands on the WatchStatus stream the tray already consumes.
pub fn recheck(app: AppHandle) {
    tauri::async_runtime::spawn(async move {
        if let Ok(mut c) = agent_ipc::connect(agent_ipc::PIPE_NAME).await {
            let _ = c.check_update(pb::CheckUpdateRequest {}).await;
        }
        let _ = app;
    });
}
