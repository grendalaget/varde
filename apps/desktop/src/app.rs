//! Tauri app: tray icon + menu, the link/status window, and the IPC watch.

use std::sync::Mutex;
use std::time::Duration;

use agent_ipc::pb;
use serde::Serialize;
use tauri::image::Image;
use tauri::menu::{CheckMenuItem, Menu, MenuEvent, MenuItem, PredefinedMenuItem};
use tauri::tray::TrayIconBuilder;
use tauri::{AppHandle, Emitter, Manager, WebviewUrl, WebviewWindow, WebviewWindowBuilder};
use tauri_plugin_opener::OpenerExt;

use crate::autostart;
use crate::view::{self, Icon, TrayView, UiStatus};

const TRAY_ID: &str = "varde";
const LINK_WINDOW: &str = "link";
const DEFAULT_CP: &str = "https://varde.games";

static ICON_IDLE: &[u8] = include_bytes!("../icons/tray-idle.png");
static ICON_HOSTING: &[u8] = include_bytes!("../icons/tray-hosting.png");
static ICON_WARN: &[u8] = include_bytes!("../icons/tray-warn.png");

#[derive(Default)]
struct State {
    status: Mutex<Option<pb::Status>>,
    view: Mutex<Option<TrayView>>,
    /// URL from `--link URL`, used once to start linking right away.
    link_arg: Mutex<Option<String>>,
    /// Elevated `--relink` instance: link window only, no tray.
    relink: bool,
}

pub fn run() {
    let launch = view::parse_launch(std::env::args().skip(1));
    if launch.autostart && autostart::opted_out() {
        return;
    }
    let relink = launch.relink;
    let mut builder = tauri::Builder::default().plugin(tauri_plugin_opener::init());
    if !relink {
        builder = builder.plugin(tauri_plugin_single_instance::init(|app, argv, _cwd| {
            let l = view::parse_launch(argv.into_iter().skip(1));
            if l.url.is_some() {
                *app.state::<State>().link_arg.lock().unwrap() = l.url;
            }
            // started again (Start menu, installer): show the window
            if !l.autostart {
                open_link_window(app);
            }
        }));
    }
    let app = builder
        .manage(State {
            link_arg: Mutex::new(launch.url.clone()),
            relink,
            ..Default::default()
        })
        .invoke_handler(tauri::generate_handler![
            get_status,
            link_defaults,
            start_link,
            cancel_link,
            open_url,
            close_window
        ])
        .setup(move |app| {
            let h = app.handle().clone();
            if !relink {
                let v = view::tray_view(None, now_ms());
                TrayIconBuilder::with_id(TRAY_ID)
                    .icon(icon(v.icon))
                    .tooltip(&v.tooltip)
                    .menu(&build_menu(&h, &v)?)
                    .on_menu_event(on_menu)
                    .build(app)?;
                tauri::async_runtime::spawn(tick(h.clone()));
            }
            tauri::async_runtime::spawn(watch(h.clone()));
            if relink || launch.open_link {
                open_link_window(&h);
            }
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("start the Varde tray");
    app.run(move |_app, event| {
        if let tauri::RunEvent::ExitRequested { api, code, .. } = event {
            // closing the window keeps the tray; only Quit (exit code) ends it
            if code.is_none() && !relink {
                api.prevent_exit();
            }
        }
    });
}

fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

fn icon(i: Icon) -> Image<'static> {
    let bytes = match i {
        Icon::Idle => ICON_IDLE,
        Icon::Hosting => ICON_HOSTING,
        Icon::Warn => ICON_WARN,
    };
    Image::from_bytes(bytes).expect("embedded tray icon")
}

/// Follows the agent's status; reconnects every 2 s while the service is down.
async fn watch(app: AppHandle) {
    loop {
        if let Ok(mut c) = agent_ipc::connect(agent_ipc::PIPE_NAME).await {
            if let Ok(r) = c.watch_status(pb::WatchStatusRequest {}).await {
                let mut stream = r.into_inner();
                while let Ok(Some(m)) = stream.message().await {
                    publish(&app, m.status);
                }
            }
        }
        publish(&app, None);
        tokio::time::sleep(Duration::from_secs(2)).await;
    }
}

/// Keeps "N s ago" fresh.
async fn tick(app: AppHandle) {
    loop {
        tokio::time::sleep(Duration::from_secs(5)).await;
        render(&app, false);
    }
}

fn publish(app: &AppHandle, s: Option<pb::Status>) {
    let ui = s.as_ref().map(UiStatus::from);
    *app.state::<State>().status.lock().unwrap() = s;
    render(app, false);
    let _ = app.emit_to(LINK_WINDOW, "status", ui);
}

fn render(app: &AppHandle, force: bool) {
    let st = app.state::<State>();
    if st.relink {
        return;
    }
    let v = view::tray_view(st.status.lock().unwrap().as_ref(), now_ms());
    let mut last = st.view.lock().unwrap();
    if !force && last.as_ref() == Some(&v) {
        return;
    }
    if let Some(tray) = app.tray_by_id(TRAY_ID) {
        let _ = tray.set_icon(Some(icon(v.icon)));
        let _ = tray.set_tooltip(Some(&v.tooltip));
        if let Ok(m) = build_menu(app, &v) {
            let _ = tray.set_menu(Some(m));
        }
    }
    *last = Some(v);
}

fn build_menu(app: &AppHandle, v: &TrayView) -> tauri::Result<Menu<tauri::Wry>> {
    let none = None::<&str>;
    let m = Menu::new(app)?;
    m.append(&MenuItem::with_id(
        app,
        "headline",
        &v.headline,
        false,
        none,
    )?)?;
    if let Some(h) = &v.hosting {
        m.append(&MenuItem::with_id(app, "hosting", h, false, none)?)?;
    }
    if let Some(s) = &v.safe_save {
        m.append(&MenuItem::with_id(app, "save", s, false, none)?)?;
    }
    m.append(&PredefinedMenuItem::separator(app)?)?;
    m.append(&MenuItem::with_id(
        app,
        "dashboard",
        "Open dashboard",
        v.dashboard_url.is_some(),
        none,
    )?)?;
    m.append(&MenuItem::with_id(
        app,
        "link",
        v.link_label,
        v.link_enabled,
        none,
    )?)?;
    m.append(&CheckMenuItem::with_id(
        app,
        "autostart",
        "Start at login",
        true,
        autostart::enabled(),
        none,
    )?)?;
    m.append(&PredefinedMenuItem::separator(app)?)?;
    m.append(&MenuItem::with_id(
        app,
        "quit",
        "Quit tray (Varde keeps running)",
        true,
        none,
    )?)?;
    Ok(m)
}

fn on_menu(app: &AppHandle, e: MenuEvent) {
    let v = app.state::<State>().view.lock().unwrap().clone();
    match e.id().as_ref() {
        "dashboard" => {
            if let Some(url) = v.and_then(|v| v.dashboard_url) {
                let _ = app.opener().open_url(url, None::<&str>);
            }
        }
        "link" => match v {
            Some(v) if v.relink => elevate_relink(),
            _ => open_link_window(app),
        },
        "autostart" => {
            let _ = autostart::set(!autostart::enabled());
            render(app, true);
        }
        "quit" => app.exit(0),
        _ => {}
    }
}

fn open_link_window(app: &AppHandle) {
    if let Some(w) = app.get_webview_window(LINK_WINDOW) {
        let _ = w.unminimize();
        let _ = w.show();
        let _ = w.set_focus();
        return;
    }
    let title = if app.state::<State>().relink {
        "Re-link this PC – Varde"
    } else {
        "Varde"
    };
    let built = tauri::webview_version().is_ok()
        && WebviewWindowBuilder::new(app, LINK_WINDOW, WebviewUrl::App("index.html".into()))
            .title(title)
            .inner_size(440.0, 540.0)
            .resizable(false)
            .maximizable(false)
            .center()
            .theme(Some(tauri::Theme::Dark))
            .background_color(tauri::window::Color(0x10, 0x16, 0x1a, 0xff))
            .build()
            .is_ok();
    if !built {
        // no WebView2: link with message boxes instead
        tauri::async_runtime::spawn(fallback_link(app.clone()));
    }
}

async fn fallback_link(app: AppHandle) {
    let status = app.state::<State>().status.lock().unwrap().clone();
    let relink = app.state::<State>().relink;
    let msg = match status.as_ref().map(|s| s.state()) {
        None => "The Varde service on this PC isn't running.".to_string(),
        Some(pb::State::NotLinked) | Some(pb::State::Linking) => link_and_describe(&app).await,
        Some(_) if relink => link_and_describe(&app).await,
        Some(_) => {
            let s = status.unwrap_or_default();
            format!("This PC is linked to {} as {}.", s.group_name, s.node_name)
        }
    };
    let _ = tauri::async_runtime::spawn_blocking(move || message_box(&msg)).await;
    if relink {
        app.exit(0);
    }
}

async fn link_and_describe(app: &AppHandle) -> String {
    let url = defaults(app).url;
    match start_link(app.clone(), url).await {
        Ok(s) if !s.user_code.is_empty() => format!(
            "Approve this PC in your browser.\n\nCode: {}\n\nIf the browser didn't open, go to {}",
            s.user_code, s.link_url
        ),
        Ok(_) => "This PC is already linked.".into(),
        Err(e) => e,
    }
}

fn wide(s: &str) -> Vec<u16> {
    s.encode_utf16().chain(std::iter::once(0)).collect()
}

fn message_box(text: &str) {
    use windows_sys::Win32::UI::WindowsAndMessaging::{MessageBoxW, MB_ICONINFORMATION, MB_OK};
    let (t, c) = (wide(text), wide("Varde"));
    // SAFETY: NUL-terminated UTF-16 strings that outlive the call.
    unsafe {
        MessageBoxW(
            std::ptr::null_mut(),
            t.as_ptr(),
            c.as_ptr(),
            MB_OK | MB_ICONINFORMATION,
        )
    };
}

/// Re-link needs an administrator: relaunch elevated (UAC) with `--relink`.
fn elevate_relink() {
    use windows_sys::Win32::UI::Shell::ShellExecuteW;
    use windows_sys::Win32::UI::WindowsAndMessaging::SW_SHOWNORMAL;
    let Ok(exe) = std::env::current_exe() else {
        return;
    };
    let (verb, file, args) = (
        wide("runas"),
        wide(&exe.display().to_string()),
        wide("--relink"),
    );
    // SAFETY: NUL-terminated UTF-16 strings that outlive the call. Declining
    // the UAC prompt just returns an error code.
    unsafe {
        ShellExecuteW(
            std::ptr::null_mut(),
            verb.as_ptr(),
            file.as_ptr(),
            args.as_ptr(),
            std::ptr::null(),
            SW_SHOWNORMAL,
        )
    };
}

// ---- commands for the link window (typed; no generic passthrough) ----

#[tauri::command]
fn get_status(state: tauri::State<'_, State>) -> Option<UiStatus> {
    state.status.lock().unwrap().as_ref().map(UiStatus::from)
}

#[derive(Serialize)]
struct LinkDefaults {
    url: String,
    /// Start linking right away (installer passed the address).
    auto: bool,
    relink: bool,
}

fn defaults(app: &AppHandle) -> LinkDefaults {
    let st = app.state::<State>();
    let arg = st.link_arg.lock().unwrap().take();
    let known = st
        .status
        .lock()
        .unwrap()
        .as_ref()
        .map(|s| s.control_plane_url.clone())
        .filter(|u| !u.is_empty());
    LinkDefaults {
        auto: arg.is_some(),
        url: arg.or(known).unwrap_or_else(|| DEFAULT_CP.into()),
        relink: st.relink,
    }
}

#[tauri::command]
fn link_defaults(app: AppHandle) -> LinkDefaults {
    defaults(&app)
}

#[tauri::command]
async fn start_link(app: AppHandle, url: String) -> Result<UiStatus, String> {
    let relink = app.state::<State>().relink;
    let mut c = agent_ipc::connect(agent_ipc::PIPE_NAME)
        .await
        .map_err(|_| "The Varde service on this PC isn't running.".to_string())?;
    let s = c
        .start_link(pb::StartLinkRequest {
            control_plane_url: url,
            relink,
        })
        .await
        .map_err(|e| e.message().to_string())?
        .into_inner()
        .status
        .unwrap_or_default();
    if let Some(l) = &s.link {
        let _ = app.opener().open_url(&l.link_url, None::<&str>);
    }
    let ui = UiStatus::from(&s);
    publish(&app, Some(s));
    Ok(ui)
}

#[tauri::command]
async fn cancel_link() -> Result<(), String> {
    let mut c = agent_ipc::connect(agent_ipc::PIPE_NAME)
        .await
        .map_err(|e| e.to_string())?;
    c.cancel_link(pb::CancelLinkRequest {})
        .await
        .map_err(|e| e.message().to_string())?;
    Ok(())
}

#[tauri::command]
fn open_url(app: AppHandle, url: String) -> Result<(), String> {
    if !(url.starts_with("https://") || url.starts_with("http://")) {
        return Err("not a web address".into());
    }
    app.opener()
        .open_url(url, None::<&str>)
        .map_err(|e| e.to_string())
}

#[tauri::command]
fn close_window(window: WebviewWindow) {
    let _ = window.close();
}
