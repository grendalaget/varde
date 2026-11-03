//! Updates from the tray's side. The agent finds, verifies and installs
//! updates itself — automatically whenever this PC isn't hosting, or when
//! asked here ("Update to X"). Installs run in the service, so Windows
//! needs no UAC prompt; only "update now while hosting" and turning
//! automatic installs off need an administrator, and those relaunch this
//! exe elevated with `--update-now` / `--auto-update-off`.

use agent_ipc::pb;
use tauri::{AppHandle, Manager};
use tauri_plugin_notification::NotificationExt;

/// "Update to X" clicked. Installs right away when idle; while hosting,
/// asks: now (drains the hosted server), when idle (installs once hosting
/// ends; remembered by the agent) or later.
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
    if version.is_empty() {
        return;
    }
    if let Some(h) = hosting {
        match ask_hosting(&version, &h) {
            Choice::Now => request(true),
            Choice::WhenIdle => request(false),
            Choice::Later => {}
        }
    } else {
        request(false);
    }
}

fn request(now: bool) {
    tauri::async_runtime::spawn(async move {
        let r = match agent_ipc::connect(&agent_ipc::default_endpoint()).await {
            Ok(mut c) => c
                .apply_update(pb::ApplyUpdateRequest { now })
                .await
                .map(|_| ())
                .map_err(Some),
            Err(_) => Err(None),
        };
        match r {
            Ok(()) => {}
            Err(Some(e)) if e.code() == agent_ipc::GrpcCode::PermissionDenied => {
                elevated("--update-now")
            }
            Err(Some(e)) => info_box(
                "Update failed",
                &format!("Varde couldn't start the update.\n\n{}", e.message()),
            ),
            Err(None) => info_box(
                "Update failed",
                "Varde's service isn't running. Restart Varde and try again.",
            ),
        }
    });
}

/// "Install updates automatically" toggled.
pub fn set_auto(enabled: bool) {
    tauri::async_runtime::spawn(async move {
        let Ok(mut c) = agent_ipc::connect(&agent_ipc::default_endpoint()).await else {
            return;
        };
        match c
            .set_auto_update(pb::SetAutoUpdateRequest { enabled })
            .await
        {
            Err(e) if e.code() == agent_ipc::GrpcCode::PermissionDenied => {
                elevated("--auto-update-off")
            }
            Err(e) => info_box("Varde update", e.message()),
            Ok(_) => {}
        }
    });
}

/// Redo a refused request from an elevated copy of this exe (Windows UAC).
/// Elsewhere the tray already runs as the service's user, so a refusal is
/// final.
fn elevated(arg: &str) {
    #[cfg(windows)]
    if let Err(e) = crate::app::elevate_self(arg) {
        info_box("Varde update", &e);
    }
    #[cfg(not(windows))]
    info_box(
        "Varde update",
        &format!("This needs an administrator ({arg})."),
    );
}

/// Elevated headless `--update-now` / `--auto-update-off`: do the one
/// call and exit.
pub fn run_elevated_action(update_now: bool) -> Result<(), String> {
    tauri::async_runtime::block_on(async move {
        let mut c = agent_ipc::connect(&agent_ipc::default_endpoint())
            .await
            .map_err(|e| format!("Varde's service isn't running: {e}"))?;
        if update_now {
            c.apply_update(pb::ApplyUpdateRequest { now: true })
                .await
                .map_err(|e| e.message().to_string())?;
        } else {
            c.set_auto_update(pb::SetAutoUpdateRequest { enabled: false })
                .await
                .map_err(|e| e.message().to_string())?;
        }
        Ok(())
    })
}

/// "Varde updated to X" once, on the first start of a new version. The
/// last version seen is kept per user next to the webview data.
pub fn notify_if_updated(app: &AppHandle) {
    let Ok(dir) = app.path().app_local_data_dir() else {
        return;
    };
    let file = dir.join("last-version");
    let cur = env!("CARGO_PKG_VERSION");
    let prev = std::fs::read_to_string(&file).ok();
    if prev.as_deref().map(str::trim) == Some(cur) {
        return;
    }
    let _ = std::fs::create_dir_all(&dir);
    let _ = std::fs::write(&file, cur);
    if let Some(title) = updated_notice(prev.as_deref(), cur) {
        let _ = app
            .notification()
            .builder()
            .title(title)
            .body("The new version is running.")
            .show();
    }
}

/// None on a first install (no previous version recorded).
pub fn updated_notice(prev: Option<&str>, cur: &str) -> Option<String> {
    let prev = prev.map(str::trim).filter(|p| !p.is_empty())?;
    (prev != cur).then(|| format!("Varde updated to {cur}"))
}

/// macOS: the agent swaps every binary in this .app (tray included), then
/// restarts itself; the tray notices its own exe changed and relaunches.
/// Windows: the installer kills and relaunches the tray itself.
pub fn after_status(app: &AppHandle) {
    #[cfg(target_os = "macos")]
    {
        use std::sync::OnceLock;
        static STARTED: OnceLock<Option<std::time::SystemTime>> = OnceLock::new();
        let mtime = || {
            std::env::current_exe()
                .ok()
                .and_then(|p| std::fs::metadata(p).ok())
                .and_then(|m| m.modified().ok())
        };
        static FIRED: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);
        let started = *STARTED.get_or_init(mtime);
        if started.is_some()
            && mtime().is_some_and(|m| Some(m) != started)
            && !FIRED.swap(true, std::sync::atomic::Ordering::SeqCst)
        {
            relaunch_app(app);
        }
    }
    let _ = app;
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
