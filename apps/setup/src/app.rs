// Interactive path: one Tauri window that walks install or uninstall.
// The engine runs on a worker thread and reports back through
// `setup-progress`/`setup-confirm`/`setup-done`/`setup-error` events —
// the web UI is a thin state machine on top.
use serde::Serialize;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{mpsc, Mutex};
use tauri::{AppHandle, Emitter, WebviewUrl, WebviewWindowBuilder};

use crate::args::{Args, Mode};
use crate::engine;
use crate::win;

#[derive(Clone, Serialize)]
struct ProgressEvent {
    index: usize,
    total: usize,
    label: String,
}

#[derive(Clone, Serialize)]
struct ConfirmEvent {
    url: String,
}

#[derive(Clone, Serialize)]
struct ErrorEvent {
    message: String,
    detail: Option<String>,
}

#[derive(Serialize)]
struct UiState {
    mode: &'static str,
    version: String,
    existing_version: Option<String>,
    instdir: String,
    autostart: bool,
    controlplane: bool,
    cp_url: Option<String>,
    /// will launch the tray --link when done (fresh/unlinked install)
    will_link: bool,
    already_linked: bool,
    /// Windows personalization accent (DWM AccentColorMenu), #rrggbb — the
    /// web UI maps it onto --accent so controls react to the user's theme
    system_accent: Option<String>,
}

struct Shared {
    instdir: PathBuf,
    args: Args,
    mode: Mode,
    confirm_tx: std::sync::Arc<Mutex<Option<mpsc::Sender<bool>>>>,
}

#[tauri::command]
fn get_state(state: tauri::State<Shared>) -> UiState {
    let (autostart, controlplane) = engine::resolve_components(state.args.tasks.as_deref());
    let already_linked = win::data_dir().join("config.toml").exists();
    UiState {
        mode: match state.mode {
            Mode::Install => "install",
            Mode::Uninstall => "uninstall",
        },
        version: env!("CARGO_PKG_VERSION").to_string(),
        existing_version: win::existing_version(),
        instdir: state.instdir.to_string_lossy().to_string(),
        autostart,
        controlplane,
        cp_url: state.args.cp_url.clone(),
        will_link: !already_linked,
        already_linked,
        system_accent: system_accent(),
    }
}

/// HKCU\Software\Microsoft\Windows\DWM `AccentColorMenu` — the user's
/// personalization accent as ABGR; converted to #rrggbb for CSS.
fn system_accent() -> Option<String> {
    let k = winreg::RegKey::predef(winreg::enums::HKEY_CURRENT_USER)
        .open_subkey("Software\\Microsoft\\Windows\\DWM")
        .ok()?;
    let abgr: u32 = k.get_value("AccentColorMenu").ok()?;
    Some(format!(
        "#{:02x}{:02x}{:02x}",
        abgr & 0xff,
        (abgr >> 8) & 0xff,
        (abgr >> 16) & 0xff
    ))
}

fn step_index(steps: &'static [&'static str], id: &str) -> (usize, usize) {
    (
        steps.iter().position(|s| *s == id).unwrap_or(0) + 1,
        steps.len(),
    )
}

fn emit_error(app: &AppHandle, e: &anyhow::Error) {
    let _ = app.emit(
        "setup-error",
        ErrorEvent {
            message: e.to_string(),
            detail: Some(format!("{e:#}")),
        },
    );
}

#[tauri::command]
fn start_install(app: AppHandle, state: tauri::State<Shared>, autostart: bool, controlplane: bool) {
    let opts = engine::InstallOpts {
        instdir: state.instdir.clone(),
        autostart,
        controlplane,
        cp_url: state.args.cp_url.clone(),
        no_shortcut: state.args.no_shortcut,
        silent: false,
        relaunch: false,
        relaunch_args: String::new(),
        launch_tray: true,
    };
    let app2 = app.clone();
    let tx_cell2 = state.confirm_tx.clone();
    std::thread::spawn(move || {
        BUSY.store(true, Ordering::SeqCst);
        let mut prog = |id: &str| {
            let (index, total) = step_index(engine::INSTALL_STEPS, id);
            let _ = app2.emit(
                "setup-progress",
                ProgressEvent {
                    index,
                    total,
                    label: id.to_string(),
                },
            );
        };
        let app3 = app2.clone();
        let mut confirm = move |url: &str| -> bool {
            let (tx, rx) = mpsc::channel::<bool>();
            *tx_cell2.lock().unwrap() = Some(tx);
            let _ = app3.emit(
                "setup-confirm",
                ConfirmEvent {
                    url: url.to_string(),
                },
            );
            rx.recv().unwrap_or(false)
        };
        match engine::install(&opts, &mut prog, &mut confirm) {
            Ok(()) => {
                let _ = app2.emit("setup-done", ());
            }
            Err(e) => emit_error(&app2, &e),
        }
        BUSY.store(false, Ordering::SeqCst);
    });
}

#[tauri::command]
fn answer_http(state: tauri::State<Shared>, accept: bool) {
    if let Some(tx) = state.confirm_tx.lock().unwrap().take() {
        let _ = tx.send(accept);
    }
}

#[tauri::command]
fn start_uninstall(app: AppHandle, state: tauri::State<Shared>, delete_data: bool) {
    let instdir = state.instdir.clone();
    let update = state.args.update;
    let app2 = app.clone();
    std::thread::spawn(move || {
        BUSY.store(true, Ordering::SeqCst);
        let mut prog = |id: &str| {
            let (index, total) = step_index(engine::UNINSTALL_STEPS, id);
            let _ = app2.emit(
                "setup-progress",
                ProgressEvent {
                    index,
                    total,
                    label: id.to_string(),
                },
            );
        };
        match engine::uninstall(&instdir, delete_data, update, &mut prog) {
            Ok(()) => {
                let _ = app2.emit("setup-done", ());
            }
            Err(e) => emit_error(&app2, &e),
        }
        BUSY.store(false, Ordering::SeqCst);
    });
}

/// True while the engine thread owns the machine — closing mid-install
/// would leave services/files half-applied, so exit requests are refused.
static BUSY: AtomicBool = AtomicBool::new(false);

#[tauri::command]
fn exit_app(app: AppHandle) {
    if !BUSY.load(Ordering::SeqCst) {
        app.exit(0);
    }
}

pub fn run(args: Args, instdir: PathBuf, mode: Mode) {
    // A dedicated webview profile dir under ProgramData: the shared
    // EBWebView dir is owned by medium-IL clients (tray), and a %TEMP%
    // profile for an elevated webview would be writable by other users.
    let wv = crate::win::data_dir().join("WebView2-Setup");
    let _ = std::fs::create_dir_all(&wv);
    std::env::set_var("WEBVIEW2_USER_DATA_FOLDER", &wv);

    let shared = Shared {
        instdir,
        args,
        mode,
        confirm_tx: std::sync::Arc::new(Mutex::new(None)),
    };

    tauri::Builder::default()
        .manage(shared)
        .invoke_handler(tauri::generate_handler![
            get_state,
            start_install,
            answer_http,
            start_uninstall,
            exit_app
        ])
        .setup(|app| {
            let glass = winreg::RegKey::predef(winreg::enums::HKEY_LOCAL_MACHINE)
                .open_subkey(r"SOFTWARE\Microsoft\Windows NT\CurrentVersion")
                .and_then(|k| k.get_value::<String, _>("CurrentBuildNumber"))
                .ok()
                .and_then(|b| b.parse::<u32>().ok())
                .map(|b| b >= 22000)
                .unwrap_or(false);
            let w = WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
                .title("Varde")
                .inner_size(470.0, 560.0)
                .resizable(false)
                .maximizable(false)
                .minimizable(false)
                .decorations(false)
                .shadow(true)
                .center()
                .theme(Some(tauri::Theme::Dark))
                .background_color(tauri::window::Color(0x11, 0x0c, 0x0a, 0xff))
                .build()?;
            if glass {
                // Mica first (Win11), acrylic fallback like the tray
                if window_vibrancy::apply_mica(&w, Some(true)).is_err() {
                    let _ = window_vibrancy::apply_acrylic(&w, Some((0x11, 0x0c, 0x0a, 0xcc)));
                }
                w.eval("window.__VARDE_GLASS__ = true").ok();
            }
            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("run varde-setup ui");
}
