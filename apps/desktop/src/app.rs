//! Tauri app: the Varde tray and the app's one window — the dashboard while
//! linked, the local link/status UI otherwise — plus the agent-IPC watch.

use std::sync::Mutex;
use std::time::Duration;

use crate::crash;
use agent_ipc::pb;
use serde::Serialize;
use tauri::image::Image;
use tauri::menu::{CheckMenuItem, Menu, MenuEvent, MenuItem, PredefinedMenuItem};
use tauri::tray::TrayIconBuilder;
use tauri::{AppHandle, Emitter, Manager, WebviewUrl, WebviewWindow, WebviewWindowBuilder};
use tauri_plugin_clipboard_manager::ClipboardExt;
use tauri_plugin_notification::NotificationExt;
use tauri_plugin_opener::OpenerExt;

use crate::autostart;
use crate::svcctl;
use crate::view::{self, Facet, Icon, TrayView, UiStatus};

const TRAY_ID: &str = "varde";
/// One face of the Varde window (mouve UI in ui/): linking, this-PC status,
/// service start when it's down.
const MAIN_WINDOW: &str = "main";
/// The other face: the control-plane dashboard as an external webview
/// (remote — it gets no IPC bridge), not a browser tab.
const DASHBOARD_WINDOW: &str = "dashboard";

static ICON_IDLE: &[u8] = include_bytes!("../icons/tray-idle.png");
static ICON_HOSTING: &[u8] = include_bytes!("../icons/tray-hosting.png");
static ICON_WARN: &[u8] = include_bytes!("../icons/tray-warn.png");

#[derive(Default)]
pub(crate) struct State {
    pub(crate) status: Mutex<Option<pb::Status>>,
    /// server_id → since of the ready prompt already notified.
    rejoin_notified: Mutex<std::collections::HashMap<String, i64>>,
    /// User picked "update when idle": apply on the next hosting-drained status.
    pub(crate) update_when_idle: std::sync::atomic::AtomicBool,
    view: Mutex<Option<TrayView>>,
    /// Launched with `--link` (installer): start linking once the window opens.
    auto_link: std::sync::atomic::AtomicBool,
    /// Elevated `--relink` instance: link window only, no tray.
    relink: bool,
    /// CP origin the dashboard webview was last pointed at — publish()
    /// retargets only while it's still sitting there (an auth bounce off
    /// to varde.games must not be yanked back mid-sign-in).
    dashboard_origin: Mutex<Option<tauri::Url>>,
    /// Fresh varde.games federation JWT, kept briefly after a hosted
    /// sign-in: the dashboard webview loads with it as #fed_token= and
    /// exchanges it for its own session cookie, so the window opens
    /// already signed in — the app and CP share one auth backend.
    fed_handoff: Mutex<Option<(String, std::time::Instant)>>,
    /// Federated sign-in paused at "pick a group": the CP session waits here
    /// for `finish_varde_login`.
    fed: Mutex<Option<FedPending>>,
    /// Set once our own start_link_ipc ran — cancel_varde_login only
    /// cancels the agent's pending link when it's OURS (a code-flow link
    /// in progress must survive a dismissed confirm card).
    fed_started_link: std::sync::atomic::AtomicBool,
    /// Bumped for every callback (and on begin/cancel): only the newest
    /// exchange may write its result — a cancelled or superseded callback
    /// can't overwrite a newer sign-in's confirm.
    fed_seq: std::sync::atomic::AtomicU64,
    /// Last emitted federated-auth stage — a window that opened after the
    /// event (deep link launched the app) hydrates from this.
    fed_last: Mutex<Option<FedEvent>>,
    /// Cleared by cancel_varde_login mid-flight: approve_link checks it
    /// before starting a new link so a cancelled sign-in can't still link.
    fed_active: std::sync::atomic::AtomicBool,
    /// The dashboard asked to move this PC into a group (varde://relink):
    /// its re-link code is approved there, not on the setup face.
    move_to: Mutex<Option<MoveTo>>,
}

/// A move the dashboard asked for: the target group, the link code opened
/// for approval ("" until the agent has one), and when it was asked.
struct MoveTo {
    group: String,
    code: String,
    at: std::time::Instant,
}

/// A move whose re-link never showed a code (the helper failed, or UAC sat
/// unanswered) stops waiting after this, so a later re-link from the tray
/// isn't taken for it.
const MOVE_START_TTL: Duration = Duration::from_secs(300);

/// A CP session minted by `/v1/auth/external`, held across the confirm
/// and pick-a-group stages.
struct FedPending {
    session: String,
    cp_url: String,
    /// Verified display name, for the auto-created group's name.
    name: String,
    /// Valid choices — finish_varde_login refuses anything else. Empty
    /// until the group list has been fetched.
    groups: Vec<String>,
}

pub fn run() {
    crash::install_hook();
    let launch = view::parse_launch(std::env::args().skip(1));
    // Point varde:// at this exe; the installer writes the same keys machine-
    // wide, this keeps portable/dev launches reachable too. Skipped in the
    // elevated re-link window: a callback there would land in a non-elevated
    // instance that the agent refuses to re-link anyway. On macOS the
    // bundle's CFBundleURLTypes claims varde:// instead.
    #[cfg(windows)]
    if !launch.relink {
        register_protocol();
    }
    if launch.autostart && autostart::opted_out() {
        return;
    }
    // Elevated headless service control: UAC prompts first, so just do the
    // SCM work and exit — no tray, no window.
    if launch.service_start || launch.service_stop {
        let r = if launch.service_start {
            svcctl::start()
        } else {
            svcctl::stop()
        };
        if let Err(e) = r {
            let action = if launch.service_start {
                "start"
            } else {
                "stop"
            };
            message_box(&format!(
                "Varde couldn't {action} its service. Restart Varde and try again.\n\n{e:#}"
            ));
            std::process::exit(1);
        }
        return;
    }
    // Elevated headless re-link for the dashboard's "Move this PC here":
    // start it and exit — the tray instance opens the code for approval.
    if launch.relink_start {
        if let Err(e) = tauri::async_runtime::block_on(relink_start()) {
            message_box(&e);
            std::process::exit(1);
        }
        return;
    }
    let relink = launch.relink;
    #[cfg(windows)]
    if relink {
        // UIPI blocks the shared-profile browser handshake: a medium-IL
        // WebView2 browser already running as the tray can't reply to this
        // high-IL client, and the window fails to build. Give the elevated
        // instance its own user data folder.
        // safe here: called before any threads or webviews exist
        std::env::set_var(
            "WEBVIEW2_USER_DATA_FOLDER",
            logs_dir()
                .parent()
                .unwrap_or(std::path::Path::new(r"C:\ProgramData\Varde"))
                .join("EBWebView-admin"),
        );
    }
    // First launch from a drag-installed .app: there was no installer to
    // register the service or the login item, so the app does it itself.
    // Bootstrapping launchd takes a moment — do it off the startup path.
    #[cfg(target_os = "macos")]
    {
        let _ = autostart::ensure_installed();
        std::thread::spawn(|| {
            let _ = svcctl::ensure_started();
        });
    }
    let mut builder = tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_clipboard_manager::init());
    if !relink {
        builder = builder.plugin(tauri_plugin_single_instance::init(|app, argv, _cwd| {
            let l = view::parse_launch(argv.into_iter().skip(1));
            if let Some(u) = l.deep_link {
                handle_deep_link(app, &u);
            }
            if l.open_link {
                app.state::<State>()
                    .auto_link
                    .store(true, std::sync::atomic::Ordering::SeqCst);
            }
            // started again (Start menu, installer): show the window; an
            // already open one re-reads its defaults (it may need to link)
            if !l.autostart {
                let open = app.get_webview_window(MAIN_WINDOW).is_some();
                open_varde_window(app);
                if open {
                    let _ = app.emit_to(MAIN_WINDOW, "link-defaults", defaults(app));
                }
            }
        }));
    }
    let app = builder
        .manage(State {
            auto_link: launch.open_link.into(),
            relink,
            ..Default::default()
        })
        .invoke_handler(tauri::generate_handler![
            get_status,
            link_defaults,
            start_link,
            cancel_link,
            start_service,
            stop_service,
            open_varde,
            open_url,
            close_window,
            exit_varde,
            begin_varde_login,
            confirm_varde_login,
            cancel_varde_login,
            federated_state,
            finish_varde_login
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
                    .on_tray_icon_event(|tray, e| {
                        // left click on the icon opens the app (Windows; on
                        // macOS the default menu_on_left_click shows the menu
                        // and this never fires). A double click is a click
                        // followed by DoubleClick — the first already opened
                        // the window, so both paths land here.
                        if let tauri::tray::TrayIconEvent::Click {
                            button: tauri::tray::MouseButton::Left,
                            button_state: tauri::tray::MouseButtonState::Up,
                            ..
                        } = e
                        {
                            open_varde_window(tray.app_handle());
                        }
                    })
                    .build(app)?;
                tauri::async_runtime::spawn(tick(h.clone()));
            }
            tauri::async_runtime::spawn(watch(h.clone()));
            tauri::async_runtime::spawn(crash::forward_loop());
            if relink {
                open_main_window(&h);
            } else {
                dock_sync(&h);
                if launch.open_link {
                    open_varde_window(&h);
                }
            }
            if let Some(u) = &launch.deep_link {
                handle_deep_link(&h, u);
            }
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("start the Varde tray");
    app.run(move |_app, event| {
        match event {
            // closing the window keeps the tray; only Quit (exit code) ends it
            tauri::RunEvent::ExitRequested { api, code, .. } if code.is_none() && !relink => {
                api.prevent_exit();
            }
            // macOS delivers varde:// callbacks as open events (the
            // CFBundleURLTypes claim), not as argv.
            #[cfg(target_os = "macos")]
            tauri::RunEvent::Opened { urls } => {
                for u in urls {
                    if u.scheme() == "varde" {
                        handle_deep_link(_app, u.as_str());
                    }
                }
            }
            _ => {}
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
        if let Ok(mut c) = agent_ipc::connect(&agent_ipc::default_endpoint()).await {
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
    let ui = s.as_ref().map(|st| UiStatus::from_status(st, now_ms()));
    let cp = s
        .as_ref()
        .map(|s| s.control_plane_url.clone())
        .filter(|u| !u.is_empty());
    let link_pending = s.as_ref().is_some_and(|s| s.link.is_some());
    let node = s.as_ref().map(|s| s.node_id.clone()).unwrap_or_default();
    sync_move(app, s.as_ref());
    if let Some(st) = &s {
        notify_rejoin(app, &st.rejoin);
    }
    *app.state::<State>().status.lock().unwrap() = s;
    render(app, false);
    let _ = app.emit_to(MAIN_WINDOW, "status", ui);
    // a re-link to another control plane retargets an existing dashboard
    // webview — but only once no link is pending, and only while the
    // webview still sits on the previous CP origin: status carries the
    // proposed address during a re-link, and mid-flight (a varde.games
    // sign-in bounce) the webview is off-origin — yanking it back would
    // lose the sign-in.
    if !link_pending {
        if let (Some(u), Some(w)) = (cp, app.get_webview_window(DASHBOARD_WINDOW)) {
            if let Ok(parsed) = tauri::Url::parse(&u) {
                let st = app.state::<State>();
                let prev = st.dashboard_origin.lock().unwrap().clone();
                let still_on_cp = w
                    .url()
                    .ok()
                    .zip(prev.as_ref())
                    .map(|(cur, p)| cur.origin() == p.origin())
                    .unwrap_or(false);
                let handoff_pending = st
                    .fed_handoff
                    .lock()
                    .unwrap()
                    .as_ref()
                    .is_some_and(|(_, at)| at.elapsed() < FED_HANDOFF_TTL);
                // re-linked on the same CP: a new node, maybe in another group
                let want = Some(node.as_str()).filter(|n| !n.is_empty());
                let relinked = prev
                    .as_ref()
                    .is_some_and(|p| query_param(p, "node").as_deref() != want);
                if still_on_cp
                    && (prev
                        .as_ref()
                        .map(|p| p.origin() != parsed.origin())
                        .unwrap_or(true)
                        || handoff_pending
                        || relinked)
                {
                    // dashboard_load_url attaches a fresh sign-in token —
                    // a re-link to the same CP re-auths the open window.
                    let url = dashboard_load_url(app, &u)
                        .map(|(u, _)| u)
                        .unwrap_or(parsed);
                    *st.dashboard_origin.lock().unwrap() = Some(url.clone());
                    let _ = w.navigate(url);
                }
            }
        }
    }
    sync_visible_facet(app);
}

/// Keep an open window on the right face as the PC's state changes: a
/// finished link morphs setup into the dashboard, and the service going
/// offline or unlinking drops a showing dashboard back to setup. Hidden
/// windows stay hidden — a status tick must never create one.
fn sync_visible_facet(app: &AppHandle) {
    let visible = |l: &str| {
        app.get_webview_window(l)
            .map(|w| w.is_visible().unwrap_or(false))
            .unwrap_or(false)
    };
    let (main, dashboard) = (visible(MAIN_WINDOW), visible(DASHBOARD_WINDOW));
    if !main && !dashboard {
        return;
    }
    match desired_facet(app) {
        Facet::Dashboard if main => {
            let _ = swap_to_dashboard(app);
        }
        Facet::Setup if dashboard => {
            open_main_window(app);
        }
        _ => {}
    }
    dock_sync(app);
}

/// One notification per failover, when the server is ready to rejoin.
fn notify_rejoin(app: &AppHandle, rejoin: &[pb::Rejoin]) {
    let st = app.state::<State>();
    let mut sent = st.rejoin_notified.lock().unwrap();
    sent.retain(|id, _| rejoin.iter().any(|r| &r.server_id == id));
    for r in rejoin.iter().filter(|r| r.phase == "ready") {
        if sent.get(&r.server_id) == Some(&r.since_unix_ms) {
            continue;
        }
        sent.insert(r.server_id.clone(), r.since_unix_ms);
        let (title, body) = view::rejoin_notice(r);
        let _ = app.notification().builder().title(title).body(body).show();
    }
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
    crate::update::maybe_apply_when_idle(app);
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
    if let Some(r) = &v.rejoin {
        m.append(&MenuItem::with_id(app, "rejoin", r, false, none)?)?;
    }
    if v.rejoin_address.is_some() {
        m.append(&MenuItem::with_id(
            app,
            "copyaddr",
            "Copy address",
            true,
            none,
        )?)?;
    }
    m.append(&MenuItem::with_id(app, "open", "Open Varde", true, none)?)?;
    if let Some(l) = &v.update_label {
        m.append(&MenuItem::with_id(app, "update", l, true, none)?)?;
    }
    if let Some(h) = &v.hosting {
        m.append(&MenuItem::with_id(app, "hosting", h, false, none)?)?;
    }
    if let Some(s) = &v.safe_save {
        m.append(&MenuItem::with_id(app, "save", s, false, none)?)?;
    }
    m.append(&MenuItem::with_id(
        app,
        "link",
        v.link_label,
        v.link_enabled,
        none,
    )?)?;
    m.append(&MenuItem::with_id(
        app,
        "logs",
        "Open logs folder",
        true,
        none,
    )?)?;
    m.append(&MenuItem::with_id(
        app,
        "checkupdate",
        "Check for updates…",
        true,
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
    m.append(&CheckMenuItem::with_id(
        app,
        "crashreports",
        "Send crash reports",
        true,
        v.crash_reporting,
        none,
    )?)?;
    m.append(&PredefinedMenuItem::separator(app)?)?;
    m.append(&MenuItem::with_id(app, "exit", "Exit Varde", true, none)?)?;
    Ok(m)
}

fn on_menu(app: &AppHandle, e: MenuEvent) {
    let v = app.state::<State>().view.lock().unwrap().clone();
    match e.id().as_ref() {
        "open" => open_varde_window(app),
        "link" => match v {
            Some(v) if v.relink => relink_window(app),
            _ => open_varde_window(app),
        },
        "copyaddr" => {
            if let Some(a) = v.and_then(|v| v.rejoin_address) {
                let _ = app.clipboard().write_text(a);
            }
        }
        "logs" => {
            let _ = app
                .opener()
                .open_path(logs_dir().to_string_lossy(), None::<&str>);
        }
        "autostart" => {
            let _ = autostart::set(!autostart::enabled());
            render(app, true);
        }
        "crashreports" => {
            let cur = app
                .state::<State>()
                .status
                .lock()
                .unwrap()
                .as_ref()
                .map(|s| s.crash_reporting)
                .unwrap_or(true);
            tauri::async_runtime::spawn(async move {
                if let Ok(mut c) = agent_ipc::connect(agent_ipc::PIPE_NAME).await {
                    let _ = c
                        .set_crash_reporting(pb::SetCrashReportingRequest { enabled: !cur })
                        .await;
                }
            });
        }
        "update" => crate::update::offer(app),
        "checkupdate" => crate::update::recheck(app.clone()),
        "exit" => request_exit(app),
        _ => {}
    }
}

/// The control-plane address the service reports, when it has one. The UI
/// never picks it — a swapped address would silently sign the user into
/// someone else's server.
fn cp_url(app: &AppHandle) -> Option<String> {
    app.state::<State>()
        .status
        .lock()
        .unwrap()
        .as_ref()
        .map(|s| s.control_plane_url.clone())
        .filter(|u| !u.is_empty())
}

/// Which face the one window shows right now.
fn desired_facet(app: &AppHandle) -> Facet {
    let st = app.state::<State>();
    let status = st.status.lock().unwrap().clone();
    // a move the dashboard asked for is approved in the dashboard
    let moving = st
        .move_to
        .lock()
        .unwrap()
        .as_ref()
        .is_some_and(|m| !m.code.is_empty());
    if moving && status.as_ref().is_some_and(|s| s.link.is_some()) {
        return Facet::Dashboard;
    }
    view::facet(
        status.as_ref(),
        st.fed_active.load(std::sync::atomic::Ordering::SeqCst),
        st.relink,
    )
}

/// "Open Varde": raise the app's single window on the face that matches
/// the PC's state — the dashboard while linked, the link/status UI
/// otherwise — and hide the other webview. Only one is ever visible, so
/// the app reads as one window whose content changes.
/// Morph the visible setup window into the dashboard, taking its spot
/// on screen. Setup goes down only once the dashboard is really up, so
/// a failed open leaves it in place.
fn swap_to_dashboard(app: &AppHandle) -> bool {
    // read setup's position while it's still visible — a hidden window
    // may not report it
    let at = app
        .get_webview_window(MAIN_WINDOW)
        .filter(|w| w.is_visible().unwrap_or(false))
        .and_then(|w| w.outer_position().ok());
    let swapped = cp_url(app)
        .map(|u| open_dashboard_window(app, &u).is_ok())
        .unwrap_or(false);
    if swapped {
        if let (Some(p), Some(d)) = (at, app.get_webview_window(DASHBOARD_WINDOW)) {
            let _ = d.set_position(p);
        }
    }
    swapped
}

fn open_varde_window(app: &AppHandle) {
    let dashboard = desired_facet(app) == Facet::Dashboard && swap_to_dashboard(app);
    if !dashboard {
        open_main_window(app);
    }
    dock_sync(app);
}

/// Closing a window hides it to the tray instead of ending Varde — only
/// Exit shuts the app down. The dashboard keeps its webview (and sign-in)
/// across hides, so reopening is instant.
fn hide_to_tray(w: &WebviewWindow) {
    let w2 = w.clone();
    w.on_window_event(move |event| {
        if let tauri::WindowEvent::CloseRequested { api, .. } = event {
            api.prevent_close();
            let _ = w2.hide();
            dock_sync(w2.app_handle());
        }
    });
}

/// macOS: the Dock icon exists only while a window is open — tray-only
/// means Accessory, so Varde reads as one quiet background app. The
/// re-link window keeps the Dock entry (it's a foreground tool, no tray).
#[cfg(target_os = "macos")]
fn dock_sync(app: &AppHandle) {
    if app.state::<State>().relink {
        return;
    }
    let visible = [MAIN_WINDOW, DASHBOARD_WINDOW]
        .iter()
        .filter_map(|l| app.get_webview_window(l))
        .any(|w| w.is_visible().unwrap_or(false));
    let policy = if visible {
        tauri::ActivationPolicy::Regular
    } else {
        tauri::ActivationPolicy::Accessory
    };
    let _ = app.set_activation_policy(policy);
}

#[cfg(not(target_os = "macos"))]
fn dock_sync(_app: &AppHandle) {}

/// "Exit Varde": shut the whole thing down — the agent service (and the
/// mesh it supervises) and then this app. Confirms first while this PC
/// hosts a server: the drain is graceful but the game still moves.
fn request_exit(app: &AppHandle) {
    let hosting = app
        .state::<State>()
        .status
        .lock()
        .unwrap()
        .as_ref()
        .and_then(|s| view::hosting_line(&s.hosting));
    if let Some(h) = hosting {
        if !confirm_exit(&h) {
            return;
        }
    }
    stop_and_exit(app.clone());
}

#[cfg(windows)]
fn stop_and_exit(app: AppHandle) {
    // no UAC prompt when the service is provably down — just quit. A
    // failed SCM query means "maybe running", so still elevate and try.
    if svcctl::running() == Some(false) {
        app.exit(0);
        return;
    }
    if let Err(e) = elevate_self("--service-stop") {
        message_box(&format!(
            "Varde couldn't stop its service, so it's still running.\n\n{e}"
        ));
        return;
    }
    // the elevated instance drains the service — up to minutes when a
    // save still has to replicate — so quit only once the service is
    // actually gone. Still up after the drain window: keep the app and
    // say so — exiting anyway would leave a service running under a
    // tray that just died.
    std::thread::spawn(move || {
        let deadline = std::time::Instant::now() + Duration::from_secs(240);
        loop {
            if svcctl::running() == Some(false) {
                app.exit(0);
                return;
            }
            if std::time::Instant::now() > deadline {
                message_box("Varde's service is still stopping. Try Exit again in a moment.");
                return;
            }
            std::thread::sleep(Duration::from_secs(1));
        }
    });
}

#[cfg(target_os = "macos")]
fn stop_and_exit(app: AppHandle) {
    // the drain can take a while — run it off the event loop and quit once
    // the service is gone. A failure keeps the app up: a tray-less Varde
    // still running is exactly the state the user tried to leave.
    std::thread::spawn(move || match svcctl::stop() {
        Ok(()) => app.exit(0),
        Err(e) => message_box(&format!(
            "Varde couldn't stop its service, so it's still running.\n\n{e:#}"
        )),
    });
}

#[cfg(windows)]
fn confirm_exit(hosting: &str) -> bool {
    use windows_sys::Win32::UI::WindowsAndMessaging::{
        MessageBoxW, IDYES, MB_ICONWARNING, MB_YESNO,
    };
    let text = format!(
        "{hosting}.\n\nExiting Varde drains and stops it — the server moves to another machine if your group's saves allow. Exit Varde anyway?"
    );
    let (t, c) = (wide(&text), wide("Exit Varde"));
    // SAFETY: NUL-terminated UTF-16 strings that outlive the call.
    unsafe {
        MessageBoxW(
            std::ptr::null_mut(),
            t.as_ptr(),
            c.as_ptr(),
            MB_YESNO | MB_ICONWARNING,
        ) == IDYES
    }
}

#[cfg(target_os = "macos")]
fn confirm_exit(hosting: &str) -> bool {
    rfd::MessageDialog::new()
        .set_title("Exit Varde")
        .set_description(format!(
            "{hosting}.\n\nExiting Varde drains and stops it — the server moves to another machine if your group's saves allow. Exit anyway?"
        ))
        .set_level(rfd::MessageLevel::Warning)
        .set_buttons(rfd::MessageButtons::YesNo)
        .show()
        == rfd::MessageDialogResult::Yes
}

/// Window background while the page loads. The UI is dark-only (mouve
/// Natt), matching the dashboard.
fn window_background() -> tauri::window::Color {
    tauri::window::Color(0x11, 0x0c, 0x0a, 0xff)
}

// Glass is gated to Mica-capable Windows (11+): it's the one recipe that
// reliably composites a transparent webview over a real backdrop. Verified
// on Server 2022: the webview's alpha doesn't punch through on an opaque
// host, and acrylic behind a transparent (layered) window doesn't blend
// either — so older builds get a deliberate opaque window instead of a
// broken see-through one. macOS keeps the flat Natt window (no Mica analog).
#[cfg(windows)]
fn glass_supported() -> bool {
    winreg::RegKey::predef(winreg::enums::HKEY_LOCAL_MACHINE)
        .open_subkey(r"SOFTWARE\Microsoft\Windows NT\CurrentVersion")
        .and_then(|k| k.get_value::<String, _>("CurrentBuildNumber"))
        .ok()
        .and_then(|b| b.parse::<u32>().ok())
        .map(|b| b >= 22000)
        .unwrap_or(false)
}

/// Mica on Windows 11; acrylic (blurred glass) as the fallback elsewhere.
/// Returns whether a backdrop actually composited.
#[cfg(windows)]
fn apply_glass(w: &WebviewWindow) -> bool {
    if window_vibrancy::apply_mica(w, Some(true)).is_ok() {
        return true;
    }
    // Natt tint over the blur so the panel still matches the theme
    window_vibrancy::apply_acrylic(w, Some((0x11, 0x0c, 0x0a, 0xcc))).is_ok()
}

/// Where the service writes its logs (see packaging/windows; the agent's
/// data dir on unix).
fn logs_dir() -> std::path::PathBuf {
    #[cfg(windows)]
    {
        std::env::var_os("ProgramData")
            .map(std::path::PathBuf::from)
            .unwrap_or_else(|| r"C:\ProgramData".into())
            .join(r"Varde\logs")
    }
    #[cfg(not(windows))]
    {
        agent_ipc::default_data_dir().join("logs")
    }
}

fn open_main_window(app: &AppHandle) {
    // one window ever: a varde:// callback or sign-in must morph an open
    // dashboard away, not stack on top of it
    if let Some(d) = app.get_webview_window(DASHBOARD_WINDOW) {
        let _ = d.hide();
    }
    if let Some(w) = app.get_webview_window(MAIN_WINDOW) {
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
    #[cfg(windows)]
    let glass = glass_supported();
    #[cfg(not(windows))]
    let glass = false;
    let built = tauri::webview_version().is_ok()
        && WebviewWindowBuilder::new(app, MAIN_WINDOW, WebviewUrl::App("index.html".into()))
            .title(title)
            .inner_size(440.0, 540.0)
            .resizable(false)
            .maximizable(false)
            .center()
            // the UI is dark-only; pin dark so the webview doesn't flash a
            // light background or light scrollbars
            .theme(Some(tauri::Theme::Dark))
            .background_color(window_background())
            // glass panels: transparent window + transparent webview over
            // a Mica/acrylic backdrop (the layered host is required for the
            // webview's alpha to composite)
            .transparent(glass)
            .initialization_script(format!("window.__VARDE_GLASS__ = {};", glass))
            .build()
            .map(|w| {
                // closing minimizes to the tray; only Exit ends the app.
                // The elevated re-link window keeps real close semantics —
                // it owns no tray to minimize to and must exit on close.
                if !app.state::<State>().relink {
                    hide_to_tray(&w);
                }
                #[cfg(windows)]
                {
                    if glass {
                        // only let the webview go transparent when a backdrop
                        // really composited — with nothing behind it the window
                        // reads as broken; the builder's themed background stays
                        if apply_glass(&w) {
                            let _ = w.set_background_color(None);
                        }
                    }
                    // re-composite the backdrop if Windows reports a theme flip
                    // (the window stays dark either way)
                    let app = w.app_handle().clone();
                    w.on_window_event(move |event| {
                        if let tauri::WindowEvent::ThemeChanged(_) = event {
                            if let Some(w) = app.get_webview_window(MAIN_WINDOW) {
                                let composited = glass && apply_glass(&w);
                                let _ = w.set_background_color(if composited {
                                    None
                                } else {
                                    Some(window_background())
                                });
                            }
                        }
                    });
                }
                let _ = w;
            })
            .is_ok();
    if !built {
        // no WebView2: link with message boxes instead
        tauri::async_runtime::spawn(fallback_link(app.clone()));
    }
}

/// A federation JWT the site mints is replayable while unexpired — keep
/// the margin tighter than its ~5-minute life.
const FED_HANDOFF_TTL: Duration = Duration::from_secs(240);

/// Take a still-fresh sign-in token: one shot only, used or not.
fn fresh_handoff_token(app: &AppHandle) -> Option<String> {
    let st = app.state::<State>();
    let mut g = st.fed_handoff.lock().unwrap();
    match g.as_ref() {
        Some((_, at)) if at.elapsed() < FED_HANDOFF_TTL => g.take().map(|(t, _)| t),
        _ => None,
    }
}

/// URL to point the dashboard at — `cp`, plus #fed_token= when a fresh
/// hosted sign-in is waiting: the CP web app exchanges the fragment for
/// its own session cookie on load, so the window opens signed in as the
/// same account the app just linked with. The bool says whether that
/// handoff was attached — the only reason to ever re-navigate an
/// already-open dashboard.
fn dashboard_load_url(app: &AppHandle, cp: &str) -> Option<(tauri::Url, bool)> {
    let mut url = tauri::Url::parse(cp).ok()?;
    // The token only ever goes to the hosted control plane — its audience
    // is that CP, and anywhere else a web app could be made to POST it
    // back to a server that replays it against the real one. Stray
    // fragments in the configured address never propagate either: only
    // the handoff carries one.
    url.set_fragment(None);
    // open on this PC's own group, not whichever one the browser used last,
    // and say which PC this is (the dashboard offers it when a start fails)
    let (group, node) = app
        .state::<State>()
        .status
        .lock()
        .unwrap()
        .as_ref()
        .map(|s| (s.group_id.clone(), s.node_id.clone()))
        .unwrap_or_default();
    if !group.is_empty() {
        url.query_pairs_mut()
            .clear()
            .append_pair("group", &group)
            .append_pair("node", &node);
    }
    let mut handoff = false;
    if tauri::Url::parse(agent_ipc::DEFAULT_CP_URL)
        .map(|hosted| hosted.origin() == url.origin())
        .unwrap_or(false)
    {
        if let Some(token) = fresh_handoff_token(app) {
            url.set_fragment(Some(&format!("fed_token={token}")));
            handoff = true;
        }
    }
    Some((url, handoff))
}

/// A query parameter of a dashboard URL (`?group=`, `?node=`; see
/// dashboard_load_url).
fn query_param(u: &tauri::Url, key: &str) -> Option<String> {
    u.query_pairs()
        .find(|(k, _)| k == key)
        .map(|(_, v)| v.into_owned())
}

/// `varde://relink?group=<id>`: the dashboard's "Move this PC here".
fn move_request(u: &tauri::Url) -> Option<String> {
    if u.scheme() != "varde" || u.host_str() != Some("relink") {
        return None;
    }
    query_param(u, "group").filter(|g| !g.is_empty())
}

/// The dashboard's "Move this PC here": start a re-link (through the
/// elevated helper on Windows, where the agent runs as SYSTEM). publish()
/// then opens its code for approval in the dashboard, group picked.
fn request_move(app: &AppHandle, group: String) {
    *app.state::<State>().move_to.lock().unwrap() = Some(MoveTo {
        group,
        code: String::new(),
        at: std::time::Instant::now(),
    });
    let h = app.clone();
    tauri::async_runtime::spawn(async move {
        // the helper reports its own errors; declining UAC needs none
        #[cfg(windows)]
        let r: Result<(), String> = tauri::async_runtime::spawn_blocking(|| {
            elevate_self("--relink-start").map_err(|_| String::new())
        })
        .await
        .unwrap_or(Err(String::new()));
        #[cfg(target_os = "macos")]
        let r = relink_start().await;
        if let Err(e) = r {
            *h.state::<State>().move_to.lock().unwrap() = None;
            if !e.is_empty() {
                let _ = tauri::async_runtime::spawn_blocking(move || message_box(&e)).await;
            }
        }
    });
}

/// Ask the agent to re-link to the control plane it's on. It needs an
/// administrator: the elevated helper on Windows, the user's own agent on
/// macOS. Refused while this PC hosts a server.
async fn relink_start() -> Result<(), String> {
    let mut c = agent_ipc::connect(&agent_ipc::default_endpoint())
        .await
        .map_err(|_| "The Varde service on this PC isn't running.".to_string())?;
    let s = c
        .get_status(pb::GetStatusRequest {})
        .await
        .map_err(grpc_err)?
        .into_inner()
        .status
        .unwrap_or_default();
    c.start_link(pb::StartLinkRequest {
        control_plane_url: s.control_plane_url,
        relink: true,
    })
    .await
    .map_err(grpc_err)?;
    Ok(())
}

/// Follow a dashboard-requested move: once the agent has a code, open its
/// approval in the dashboard with the group picked; done when the link ends.
fn sync_move(app: &AppHandle, s: Option<&pb::Status>) {
    let st = app.state::<State>();
    let mut mv = st.move_to.lock().unwrap();
    let Some(m) = mv.as_mut() else {
        return;
    };
    let code = s
        .and_then(|s| s.link.as_ref())
        .map(|l| l.user_code.as_str())
        .unwrap_or_default();
    if code.is_empty() {
        // approved, expired or cancelled — or it never started
        if !m.code.is_empty() || m.at.elapsed() > MOVE_START_TTL {
            *mv = None;
        }
        return;
    }
    if m.code == code {
        return;
    }
    m.code = code.to_string();
    // dashboard_origin stays on the old node, so the finished re-link
    // still re-navigates the window to the new one
    let base = st.dashboard_origin.lock().unwrap().clone();
    if let (Some(mut u), Some(w)) = (base, app.get_webview_window(DASHBOARD_WINDOW)) {
        u.set_path("/link");
        u.set_fragment(None);
        u.query_pairs_mut()
            .clear()
            .append_pair("code", code)
            .append_pair("group", &m.group);
        let _ = w.navigate(u);
    }
}

/// The dashboard lives in the app, not the browser: a plain external
/// webview on the control-plane address (it can't reach app commands —
/// remote IPC is off — and keeps its own cookies, so the sign-in sticks).
fn open_dashboard_window(app: &AppHandle, url: &str) -> Result<(), String> {
    // one window ever — but only take the setup window down once the
    // dashboard is really up: a failed open leaves it where it was
    let hide_main = |app: &AppHandle| {
        if let Some(m) = app.get_webview_window(MAIN_WINDOW) {
            let _ = m.hide();
        }
    };
    let (parsed, handoff) =
        dashboard_load_url(app, url).ok_or_else(|| format!("bad control-plane address: {url}"))?;
    if let Some(w) = app.get_webview_window(DASHBOARD_WINDOW) {
        // a re-link may have moved the group to another control plane or
        // group, or carried a fresh sign-in token — otherwise keep the
        // user's place in the dashboard
        let last = app
            .state::<State>()
            .dashboard_origin
            .lock()
            .unwrap()
            .clone();
        if handoff
            || w.url()
                .map(|u| u.origin() != parsed.origin())
                .unwrap_or(true)
            || last.as_ref().map(|l| query_param(l, "node")) != Some(query_param(&parsed, "node"))
        {
            *app.state::<State>().dashboard_origin.lock().unwrap() = Some(parsed.clone());
            w.navigate(parsed).map_err(|e| e.to_string())?;
        }
        let _ = w.unminimize();
        let _ = w.show();
        let _ = w.set_focus();
        hide_main(app);
        return Ok(());
    }
    *app.state::<State>().dashboard_origin.lock().unwrap() = Some(parsed.clone());
    WebviewWindowBuilder::new(app, DASHBOARD_WINDOW, WebviewUrl::External(parsed))
        .title("Varde")
        .inner_size(1200.0, 800.0)
        .min_inner_size(720.0, 480.0)
        .center()
        .theme(Some(tauri::Theme::Dark))
        .background_color(tauri::window::Color(0x10, 0x16, 0x1a, 0xff))
        // inside the window: the control-plane origin itself, plus
        // https://*.varde.games — the hosted-CP sign-in bounces through
        // /federate/cp and account/billing live there — and
        // https://steamcommunity.com for linking Steam. Anything else
        // stays out of the app chrome (an off-origin page must never
        // impersonate the dashboard, and the local frontend's command
        // bridge is unreachable either way).
        .on_navigation({
            let ah = app.clone();
            move |u| {
                // "Move this PC here" in the dashboard: re-link into that
                // group, approved back in this window. Never loaded.
                if let Some(group) = move_request(u) {
                    request_move(&ah, group);
                    return false;
                }
                // Tauri serves the bundled UI from http(s)://tauri.localhost
                // — a control-plane URL pointing there would load the local
                // app pages, privileged IPC bridge included, inside this
                // webview. It's never a real dashboard.
                if u.domain()
                    .is_some_and(|h| h.eq_ignore_ascii_case("tauri.localhost"))
                {
                    return false;
                }
                let st = ah.state::<State>();
                let on_cp = st
                    .dashboard_origin
                    .lock()
                    .unwrap()
                    .as_ref()
                    .is_some_and(|d| {
                        u.origin() == d.origin()
                            // a self-hosted CP may terminate TLS in front:
                            // allow http→https on the same host only
                            || (d.scheme() == "http"
                                && u.scheme() == "https"
                                && u.domain().zip(d.domain()).is_some_and(
                                    |(a, b)| a.eq_ignore_ascii_case(b),
                                ))
                    });
                // Steam's OpenID sign-in (Settings → link Steam) stays in this
                // window: its callback needs this webview's CP session cookie
                on_cp
                    || u.scheme() == "https"
                        && u.domain().is_some_and(|d| {
                            d == "varde.games"
                                || d.ends_with(".varde.games")
                                || d == "steamcommunity.com"
                        })
            }
        })
        // window.open()/target=_blank (docs, community, a deep link into
        // varde.games account/billing) belongs in the user's browser,
        // not a chromeless popup
        .on_new_window({
            let ah = app.clone();
            move |u, _features| {
                let _ = ah.opener().open_url(u.as_str(), None::<&str>);
                tauri::webview::NewWindowResponse::Deny
            }
        })
        .build()
        .map(|w| {
            hide_to_tray(&w);
            hide_main(app);
        })
        .map_err(|e| e.to_string())
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

/// NUL-terminated UTF-16 for Win32 W calls; takes `&str` and paths alike.
#[cfg(windows)]
pub(crate) fn wide(s: impl AsRef<std::ffi::OsStr>) -> Vec<u16> {
    use std::os::windows::ffi::OsStrExt;
    s.as_ref().encode_wide().chain(std::iter::once(0)).collect()
}

#[cfg(windows)]
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

#[cfg(target_os = "macos")]
fn message_box(text: &str) {
    rfd::MessageDialog::new()
        .set_title("Varde")
        .set_description(text)
        .set_level(rfd::MessageLevel::Info)
        .show();
}

/// Re-link opens a second instance in relink mode (elevated on Windows,
/// where the agent runs as SYSTEM; plain on macOS, where the LaunchAgent
/// is the user's own).
fn relink_window(_app: &AppHandle) {
    #[cfg(windows)]
    let _ = elevate_self("--relink");
    #[cfg(target_os = "macos")]
    if let Ok(exe) = std::env::current_exe() {
        let _ = std::process::Command::new(exe).arg("--relink").spawn();
    }
}

/// Privileged actions relaunch this exe elevated (UAC) with a single flag:
/// `--relink`, `--service-start`, `--service-stop`. Declining the prompt
/// surfaces as an error the caller can show or ignore.
#[cfg(windows)]
fn elevate_self(arg: &str) -> Result<(), String> {
    let exe = std::env::current_exe().map_err(|e| e.to_string())?;
    run_elevated(&exe, arg).map_err(|e| format!("{e:#}"))
}

/// ShellExecuteExW "runas": the only way to raise the UAC prompt —
/// CreateProcess (std::process::Command) refuses a requireAdministrator
/// manifest (varde-setup's) with ERROR_ELEVATION_REQUIRED. Returns once the
/// elevated process exists; declining the prompt is an error.
#[cfg(windows)]
pub(crate) fn run_elevated(file: &std::path::Path, args: &str) -> anyhow::Result<()> {
    use windows_sys::Win32::Foundation::{CloseHandle, ERROR_CANCELLED};
    use windows_sys::Win32::UI::Shell::{
        ShellExecuteExW, SEE_MASK_FLAG_NO_UI, SEE_MASK_NOCLOSEPROCESS, SHELLEXECUTEINFOW,
    };
    use windows_sys::Win32::UI::WindowsAndMessaging::SW_SHOWNORMAL;
    let (verb, path, params) = (wide("runas"), wide(file), wide(args));
    let mut info = SHELLEXECUTEINFOW {
        cbSize: std::mem::size_of::<SHELLEXECUTEINFOW>() as u32,
        // NOCLOSEPROCESS: return only once the process exists (and hand us
        // its handle); FLAG_NO_UI: errors come back to us, not a shell dialog
        fMask: SEE_MASK_NOCLOSEPROCESS | SEE_MASK_FLAG_NO_UI,
        lpVerb: verb.as_ptr(),
        lpFile: path.as_ptr(),
        lpParameters: params.as_ptr(),
        nShow: SW_SHOWNORMAL,
        ..Default::default()
    };
    // SAFETY: `info` is sized and zero-initialised; the NUL-terminated UTF-16
    // buffers it points to outlive the call.
    let ok = unsafe { ShellExecuteExW(&mut info) };
    if ok == 0 {
        let e = std::io::Error::last_os_error();
        if e.raw_os_error() == Some(ERROR_CANCELLED as i32) {
            anyhow::bail!("the administrator prompt was declined");
        }
        return Err(e.into());
    }
    if !info.hProcess.is_null() {
        // SAFETY: a process handle ShellExecuteExW opened for us; closed once.
        unsafe { CloseHandle(info.hProcess) };
    }
    Ok(())
}

// ---- commands for the link window (typed; no generic passthrough) ----

#[tauri::command]
fn get_status(state: tauri::State<'_, State>) -> Option<UiStatus> {
    state
        .status
        .lock()
        .unwrap()
        .as_ref()
        .map(|st| UiStatus::from_status(st, now_ms()))
}

#[derive(Serialize, Clone)]
struct LinkDefaults {
    /// The service's address, else the hosted Varde (agent_ipc::DEFAULT_CP_URL).
    url: String,
    /// Start linking as soon as the service reports not_linked.
    auto: bool,
    relink: bool,
    /// "windows" or "macos" — the UI swaps a few service-management strings.
    os: &'static str,
}

fn defaults(app: &AppHandle) -> LinkDefaults {
    let st = app.state::<State>();
    let known = st
        .status
        .lock()
        .unwrap()
        .as_ref()
        .map(|s| s.control_plane_url.clone())
        .filter(|u| !u.is_empty());
    LinkDefaults {
        auto: st
            .auto_link
            .swap(false, std::sync::atomic::Ordering::SeqCst),
        url: known.unwrap_or_else(|| agent_ipc::DEFAULT_CP_URL.into()),
        relink: st.relink,
        os: if cfg!(target_os = "macos") {
            "macos"
        } else {
            "windows"
        },
    }
}

#[tauri::command]
fn link_defaults(app: AppHandle) -> LinkDefaults {
    defaults(&app)
}

#[tauri::command]
async fn start_link(app: AppHandle, url: String) -> Result<UiStatus, String> {
    let s = start_link_ipc(&app, url).await?;
    if let Some(l) = &s.link {
        let _ = app.opener().open_url(&l.link_url, None::<&str>);
    }
    let ui = UiStatus::from_status(&s, now_ms());
    Ok(ui)
}

/// gRPC failures the user reads. Statuses the service returns carry
/// user-facing text already; transport errors (service died mid-call) come
/// back as Unknown with hyper plumbing in the message.
fn grpc_err(e: agent_ipc::GrpcStatus) -> String {
    match e.code() {
        agent_ipc::GrpcCode::Unknown => {
            "The Varde service on this PC isn't answering. Restart Varde and try again.".to_string()
        }
        _ => e.message().to_string(),
    }
}

async fn start_link_ipc(app: &AppHandle, url: String) -> Result<pb::Status, String> {
    let relink = app.state::<State>().relink;
    let mut c = agent_ipc::connect(&agent_ipc::default_endpoint())
        .await
        .map_err(|_| "The Varde service on this PC isn't running.".to_string())?;
    let s = c
        .start_link(pb::StartLinkRequest {
            control_plane_url: url,
            relink,
        })
        .await
        .map_err(grpc_err)?
        .into_inner()
        .status
        .unwrap_or_default();
    publish(app, Some(s.clone()));
    Ok(s)
}

// ---- "Log in with Varde": varde.games federation ----

/// The site's desktop sign-in page; it bounces back through varde://.
const SITE_DESKTOP_URL: &str = "https://varde.games/auth/desktop";

/// Progress of a federated sign-in, emitted to the window as
/// "federated-auth". The form renders these instead of the code field.
#[derive(Serialize, Clone)]
#[serde(tag = "stage", rename_all = "snake_case")]
enum FedEvent {
    Confirm {
        email: String,
        name: String,
    },
    WaitingBrowser,
    Exchanging,
    PickGroup {
        groups: Vec<FedGroup>,
    },
    Approving,
    Error {
        message: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        detail: Option<String>,
    },
}

/// An error the window shows: `message` is the friendly sentence, `detail`
/// the technical original behind the note's "Additional information".
#[derive(Serialize)]
struct UiErr {
    message: String,
    detail: Option<String>,
}

impl UiErr {
    fn msg(m: impl Into<String>) -> Self {
        UiErr {
            message: m.into(),
            detail: None,
        }
    }
}

/// cp_api failures: the user_message is what's read; the Display form
/// ("api 404 http_error: 404 page not found") stays available for support.
fn ui_err(e: cp_api::Error) -> UiErr {
    UiErr {
        message: e.user_message(),
        detail: Some(e.to_string()),
    }
}

#[derive(Serialize, Clone)]
struct FedGroup {
    id: String,
    name: String,
}

fn emit_fed(app: &AppHandle, ev: FedEvent) {
    *app.state::<State>().fed_last.lock().unwrap() = Some(ev.clone());
    let _ = app.emit_to(MAIN_WINDOW, "federated-auth", ev);
}

/// Register varde:// for the current user so the browser can return the
/// sign-in token. HKCU needs no admin; missing-write failures just leave
/// deep links unhandled. macOS claims the scheme via the bundle's
/// CFBundleURLTypes instead.
#[cfg(windows)]
fn register_protocol() {
    let exe = match std::env::current_exe() {
        Ok(e) => e,
        Err(_) => return,
    };
    let hkcu = winreg::RegKey::predef(winreg::enums::HKEY_CURRENT_USER);
    if let Ok((k, _)) = hkcu.create_subkey(r"Software\Classes\varde") {
        let _ = k.set_value("", &"URL:Varde Protocol");
        let _ = k.set_value("URL Protocol", &"");
    }
    if let Ok((k, _)) = hkcu.create_subkey(r"Software\Classes\varde\shell\open\command") {
        let _ = k.set_value("", &format!("\"{}\" \"%1\"", exe.display()));
    }
}

/// Extract the federation token from varde://auth/callback?token=….
fn deep_link_token(url: &str) -> Option<String> {
    let u = tauri::Url::parse(url).ok()?;
    if u.scheme() != "varde" || u.host_str() != Some("auth") || u.path() != "/callback" {
        return None;
    }
    u.query_pairs()
        .find(|(k, _)| k == "token")
        .map(|(_, v)| v.into_owned())
}

fn handle_deep_link(app: &AppHandle, url: &str) {
    let Some(token) = deep_link_token(url) else {
        return;
    };
    // mark the flow before the window morph: a status tick between the
    // two would otherwise pull the just-shown setup back to the dashboard
    app.state::<State>()
        .fed_active
        .store(true, std::sync::atomic::Ordering::SeqCst);
    open_main_window(app);
    let seq = app
        .state::<State>()
        .fed_seq
        .fetch_add(1, std::sync::atomic::Ordering::SeqCst)
        + 1;
    let h = app.clone();
    tauri::async_runtime::spawn(async move {
        emit_fed(&h, FedEvent::Exchanging);
        match fed_exchange(token.clone()).await {
            // cancelled or superseded while the exchange was in flight —
            // stay silent rather than overwrite the newer flow
            _ if seq
                != h.state::<State>()
                    .fed_seq
                    .load(std::sync::atomic::Ordering::SeqCst) => {}
            Ok((session, cp_url, user)) => {
                *h.state::<State>().fed_handoff.lock().unwrap() =
                    Some((token, std::time::Instant::now()));
                *h.state::<State>().fed.lock().unwrap() = Some(FedPending {
                    session,
                    cp_url,
                    name: user.display_name.clone(),
                    groups: Vec::new(),
                });
                // A varde:// link can be unsolicited (any page can open it),
                // so linking always waits for an explicit confirm. The
                // identity shown comes from the CP's verified response, not
                // the raw token — it can't lie about who it will link to.
                emit_fed(
                    &h,
                    FedEvent::Confirm {
                        email: user.email,
                        name: user.display_name,
                    },
                );
            }
            Err(e) => emit_fed(
                &h,
                FedEvent::Error {
                    message: e.message,
                    detail: e.detail,
                },
            ),
        }
    });
}

/// Exchange the site's JWT at the hosted CP. The token's audience is the
/// hosted CP — a self-hosted one can't verify it, so this always goes to
/// DEFAULT_CP_URL.
async fn fed_exchange(fed_token: String) -> Result<(String, String, cp_api::AuthUser), UiErr> {
    let cp = agent_ipc::DEFAULT_CP_URL.to_string();
    let resp: cp_api::AuthResponse = cp_api::CpClient::new(&cp)
        .map_err(ui_err)?
        .json(
            "POST",
            "/v1/auth/external",
            Some(&cp_api::ExternalLoginRequest { token: fed_token }),
        )
        .await
        .map_err(ui_err)?;
    Ok((resp.token, cp, resp.user))
}

/// After the user confirms: resolve which group this PC joins (create one
/// when the account has none, auto-pick a single one, ask otherwise), then
/// approve the device link.
async fn federated_login(app: &AppHandle, pending: FedPending) -> Result<(), UiErr> {
    let session = pending.session;
    let cp = pending.cp_url;
    let authed = cp_api::CpClient::new(&cp)
        .map_err(ui_err)?
        .with_bearer(session.clone());
    let groups: cp_api::GroupList = authed
        .json("GET", "/v1/groups", None::<&serde_json::Value>)
        .await
        .map_err(ui_err)?;
    let group_id = match groups.groups.as_slice() {
        [] => {
            let g: cp_api::Group = authed
                .json(
                    "POST",
                    "/v1/groups",
                    Some(&cp_api::CreateGroupRequest {
                        name: format!("{}'s group", pending.name),
                    }),
                )
                .await
                .map_err(ui_err)?;
            g.id
        }
        [one] => one.id.clone(),
        many => {
            *app.state::<State>().fed.lock().unwrap() = Some(FedPending {
                session,
                cp_url: cp.clone(),
                name: pending.name,
                groups: many.iter().map(|g| g.id.clone()).collect(),
            });
            emit_fed(
                app,
                FedEvent::PickGroup {
                    groups: many
                        .iter()
                        .map(|g| FedGroup {
                            id: g.id.clone(),
                            name: g.name.clone(),
                        })
                        .collect(),
                },
            );
            return Ok(());
        }
    };
    approve_link(app, authed, cp, group_id).await
}

#[tauri::command]
async fn begin_varde_login(app: AppHandle) -> Result<(), String> {
    app.state::<State>()
        .fed_active
        .store(true, std::sync::atomic::Ordering::SeqCst);
    // a click starts a new flow — invalidate any exchange still in flight
    app.state::<State>()
        .fed_seq
        .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    // and any token still waiting to authenticate the dashboard: a new
    // sign-in must not land on the previous account's session
    *app.state::<State>().fed_handoff.lock().unwrap() = None;
    emit_fed(&app, FedEvent::WaitingBrowser);
    app.opener()
        .open_url(SITE_DESKTOP_URL, None::<&str>)
        .map_err(|_| {
            "Couldn't open your browser. Check your default browser, then try again.".to_string()
        })
}

/// The confirm card approved: continue with the session the exchange
/// already minted.
#[tauri::command]
async fn confirm_varde_login(app: AppHandle) -> Result<(), String> {
    let pending = app
        .state::<State>()
        .fed
        .lock()
        .unwrap()
        .take()
        .ok_or("No sign-in is waiting for confirmation.")?;
    emit_fed(&app, FedEvent::Exchanging);
    tauri::async_runtime::spawn(async move {
        if let Err(e) = federated_login(&app, pending).await {
            emit_fed(
                &app,
                FedEvent::Error {
                    message: e.message,
                    detail: e.detail,
                },
            );
        }
    });
    Ok(())
}

/// Cancel whatever stage a federated sign-in is in (confirm, group pick,
/// or mid-flight before approval). Only cancels the agent's pending link
/// when it's OURS — a code-flow link started elsewhere isn't touched.
#[tauri::command]
async fn cancel_varde_login(app: AppHandle) -> Result<(), String> {
    app.state::<State>()
        .fed_active
        .store(false, std::sync::atomic::Ordering::SeqCst);
    *app.state::<State>().fed.lock().unwrap() = None;
    *app.state::<State>().fed_last.lock().unwrap() = None;
    // a cancelled sign-in must not authenticate the dashboard either
    *app.state::<State>().fed_handoff.lock().unwrap() = None;
    // invalidate any exchange still in flight so it can't reappear
    app.state::<State>()
        .fed_seq
        .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    if app
        .state::<State>()
        .fed_started_link
        .swap(false, std::sync::atomic::Ordering::SeqCst)
    {
        cancel_link().await
    } else {
        Ok(())
    }
}

/// The last emitted stage, for a window that subscribed after the event.
#[tauri::command]
fn federated_state(app: AppHandle) -> Option<FedEvent> {
    app.state::<State>().fed_last.lock().unwrap().clone()
}

#[tauri::command]
async fn finish_varde_login(app: AppHandle, group_id: String) -> Result<(), UiErr> {
    let pending = app
        .state::<State>()
        .fed
        .lock()
        .unwrap()
        .take()
        .ok_or_else(|| UiErr::msg("No sign-in is waiting for a group choice."))?;
    if !pending.groups.contains(&group_id) {
        return Err(UiErr::msg("That group isn't one of yours."));
    }
    let client = cp_api::CpClient::new(&pending.cp_url)
        .map_err(ui_err)?
        .with_bearer(pending.session);
    approve_link(&app, client, pending.cp_url, group_id).await
}

/// Ask the agent to start linking (no browser tab this time), then approve
/// the pending device link with the website session.
async fn approve_link(
    app: &AppHandle,
    authed: cp_api::CpClient,
    cp_url: String,
    group_id: String,
) -> Result<(), UiErr> {
    // a cancel landing mid-flight (between exchange and here) must not
    // still start a link
    if !app
        .state::<State>()
        .fed_active
        .load(std::sync::atomic::Ordering::SeqCst)
    {
        return Ok(());
    }
    emit_fed(app, FedEvent::Approving);
    app.state::<State>()
        .fed_started_link
        .store(true, std::sync::atomic::Ordering::SeqCst);
    let s = start_link_ipc(app, cp_url).await.map_err(UiErr::msg)?;
    if !app
        .state::<State>()
        .fed_active
        .load(std::sync::atomic::Ordering::SeqCst)
    {
        // cancelled while the link request was in flight — abort the link
        // we just registered before approving it
        app.state::<State>()
            .fed_started_link
            .store(false, std::sync::atomic::Ordering::SeqCst);
        let _ = cancel_link().await;
        return Ok(());
    }
    let link = s
        .link
        .ok_or_else(|| UiErr::msg("The Varde service didn't produce a link code."))?;
    let dl: cp_api::DeviceLink = authed
        .json(
            "GET",
            &format!("/v1/device-links/{}", link.user_code),
            None::<&serde_json::Value>,
        )
        .await
        .map_err(ui_err)?;
    let _: serde_json::Value = authed
        .json(
            "POST",
            "/v1/device-links/approve",
            Some(&cp_api::ApproveDeviceLinkRequest {
                user_code: link.user_code,
                group_id,
                name: if dl.hostname.is_empty() {
                    "this PC".into()
                } else {
                    dl.hostname
                },
            }),
        )
        .await
        .map_err(ui_err)?;
    // the sign-in completed: the pending link itself keeps the window on
    // the setup face until approval clears it, then the facet flips to the
    // dashboard — a stale true here would pin the window to setup forever
    app.state::<State>()
        .fed_active
        .store(false, std::sync::atomic::Ordering::SeqCst);
    Ok(())
}

#[tauri::command]
async fn cancel_link() -> Result<(), String> {
    let mut c = agent_ipc::connect(&agent_ipc::default_endpoint())
        .await
        .map_err(|_| "The Varde service on this PC isn't running.".to_string())?;
    c.cancel_link(pb::CancelLinkRequest {})
        .await
        .map_err(grpc_err)?;
    Ok(())
}

#[tauri::command]
fn start_service() -> Result<(), String> {
    // the macOS LaunchAgent is user-owned — no elevation needed
    #[cfg(target_os = "macos")]
    return svcctl::start().map_err(|e| format!("{e:#}"));
    #[cfg(windows)]
    elevate_self("--service-start")
}

#[tauri::command]
fn stop_service() -> Result<(), String> {
    #[cfg(target_os = "macos")]
    return svcctl::stop().map_err(|e| format!("{e:#}"));
    #[cfg(windows)]
    elevate_self("--service-stop")
}

/// Raise the app's one window on the right face (same rule as the tray's
/// "Open Varde"): the dashboard while linked, the setup/status UI otherwise.
#[tauri::command]
fn open_varde(app: AppHandle) {
    open_varde_window(&app);
}

/// The window's own "Exit Varde" — identical to the tray item: drains the
/// service (confirms while hosting) and quits the app.
#[tauri::command]
fn exit_varde(app: AppHandle) {
    request_exit(&app);
}

#[tauri::command]
fn open_url(app: AppHandle, url: String) -> Result<(), String> {
    if !(url.starts_with("https://") || url.starts_with("http://")) {
        return Err("not a web address".into());
    }
    app.opener().open_url(url, None::<&str>).map_err(|_| {
        "Couldn't open your browser. Check your default browser, then try again.".to_string()
    })
}

#[tauri::command]
fn close_window(window: WebviewWindow) {
    let _ = window.close();
}
