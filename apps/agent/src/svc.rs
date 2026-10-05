//! Windows service integration: `service install|uninstall` plus the SCM
//! entrypoint (`service run`, hidden). The service runs the same agent
//! `run` path on a tokio runtime; SCM STOP and PRESHUTDOWN both feed the
//! same graceful-stop channel as SIGTERM/ctrl-c, with a 120 s preshutdown
//! hint so the final snapshot + replication hold can finish.

use anyhow::{Context, Result};
use std::ffi::OsString;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;
use windows_service::service::{
    ServiceAccess, ServiceControl, ServiceControlAccept, ServiceErrorControl, ServiceExitCode,
    ServiceInfo, ServiceStartType, ServiceState, ServiceStatus, ServiceType,
};
use windows_service::service_control_handler::{self, ServiceControlHandlerResult};
use windows_service::service_dispatcher;
use windows_service::service_manager::{ServiceManager, ServiceManagerAccess};

const SERVICE_NAME: &str = "VardeAgent";
const SERVICE_DISPLAY: &str = "Varde Agent";
const PRESHUTDOWN_MS: u32 = 120_000;

pub fn install() -> Result<()> {
    let exe = std::env::current_exe().context("resolve exe path")?;
    let mgr = ServiceManager::local_computer(
        None::<&str>,
        ServiceManagerAccess::CONNECT | ServiceManagerAccess::CREATE_SERVICE,
    )
    .context("open SCM")?;
    let info = ServiceInfo {
        name: OsString::from(SERVICE_NAME),
        display_name: OsString::from(SERVICE_DISPLAY),
        service_type: ServiceType::OWN_PROCESS,
        start_type: ServiceStartType::AutoStart,
        error_control: ServiceErrorControl::Normal,
        executable_path: exe,
        launch_arguments: vec![OsString::from("service"), OsString::from("run")],
        dependencies: vec![],
        account_name: None, // LocalSystem
        account_password: None,
    };
    let svc = mgr
        .create_service(&info, ServiceAccess::CHANGE_CONFIG)
        .context("create service")?;
    svc.set_description("Varde node agent: peer-hosted game servers.")
        .ok();
    svc.set_preshutdown_timeout(Duration::from_millis(PRESHUTDOWN_MS as u64))
        .context("set preshutdown timeout")?;
    // %ProgramData%\Varde\identity holds the node key — restrict to
    // SYSTEM + Administrators only.
    let ident = std::env::var_os("ProgramData")
        .map(std::path::PathBuf::from)
        .unwrap_or_else(|| std::path::PathBuf::from(r"C:\ProgramData"))
        .join("Varde")
        .join("identity");
    std::fs::create_dir_all(&ident).ok();
    let _ = std::process::Command::new("icacls")
        .arg(&ident)
        .args([
            "/inheritance:r",
            "/grant:r",
            "*S-1-5-18:(OI)(CI)F",
            "Administrators:(OI)(CI)F",
        ])
        .output();
    println!("service {SERVICE_NAME} installed (LocalSystem, auto-start)");
    Ok(())
}

pub fn uninstall() -> Result<()> {
    let mgr = ServiceManager::local_computer(None::<&str>, ServiceManagerAccess::CONNECT)
        .context("open SCM")?;
    let svc = mgr
        .open_service(SERVICE_NAME, ServiceAccess::DELETE | ServiceAccess::STOP)
        .context("open service")?;
    let _ = svc.stop();
    svc.delete().context("delete service")?;
    println!("service {SERVICE_NAME} removed");
    Ok(())
}

pub fn run_service() -> Result<()> {
    service_dispatcher::start(SERVICE_NAME, ffi_service_main).context("service dispatcher")
}

windows_service::define_windows_service!(ffi_service_main, service_main);

fn service_main(_args: Vec<OsString>) {
    if let Err(e) = service_main_inner() {
        eprintln!("service failed: {e:#}");
    }
}

fn service_main_inner() -> Result<()> {
    // SCM -> agent graceful-stop channel
    let (stop_tx, stop_rx) = tokio::sync::oneshot::channel::<()>();
    let mut stop_tx = Some(stop_tx);
    // set while a stop is being drained; the drain reporter below re-issues
    // StopPending with fresh wait hints so the SCM doesn't kill us mid-hold
    let draining = Arc::new(AtomicBool::new(false));
    let handler_draining = draining.clone();
    let status_handle = service_control_handler::register(
        SERVICE_NAME,
        move |event| -> ServiceControlHandlerResult {
            match event {
                ServiceControl::Stop | ServiceControl::Preshutdown => {
                    handler_draining.store(true, Ordering::SeqCst);
                    if let Some(tx) = stop_tx.take() {
                        let _ = tx.send(());
                    }
                    ServiceControlHandlerResult::NoError
                }
                _ => ServiceControlHandlerResult::NotImplemented,
            }
        },
    )
    .context("register handler")?;

    let set_status = |state: ServiceState, accept: ServiceControlAccept, hint: Duration| {
        status_handle.set_service_status(ServiceStatus {
            service_type: ServiceType::OWN_PROCESS,
            current_state: state,
            controls_accepted: accept,
            exit_code: ServiceExitCode::Win32(0),
            checkpoint: 0,
            wait_hint: hint,
            process_id: None,
        })
    };

    set_status(
        ServiceState::StartPending,
        ServiceControlAccept::empty(),
        Duration::from_secs(10),
    )?;
    set_status(
        ServiceState::Running,
        ServiceControlAccept::STOP | ServiceControlAccept::PRESHUTDOWN,
        Duration::ZERO,
    )?;

    // re-report StopPending every few seconds while the agent drains; a
    // single short wait hint would let the SCM kill us mid-hold
    let status_handle2 = status_handle;
    let reporter_done = Arc::new(AtomicBool::new(false));
    let reporter_done2 = reporter_done.clone();
    let reporter = std::thread::spawn(move || {
        let mut checkpoint: u32 = 1;
        while !reporter_done2.load(Ordering::SeqCst) {
            if draining.load(Ordering::SeqCst) {
                let _ = status_handle2.set_service_status(ServiceStatus {
                    service_type: ServiceType::OWN_PROCESS,
                    current_state: ServiceState::StopPending,
                    controls_accepted: ServiceControlAccept::empty(),
                    exit_code: ServiceExitCode::Win32(0),
                    checkpoint,
                    wait_hint: Duration::from_secs(30),
                    process_id: None,
                });
                checkpoint += 1;
            }
            std::thread::sleep(Duration::from_secs(3));
        }
    });

    // run the real agent on this worker thread; STOP/PRESHUTDOWN feeds the
    // same graceful-stop path as SIGTERM/ctrl-c.
    let rt = tokio::runtime::Runtime::new().context("tokio runtime")?;
    let run_res = rt.block_on(crate::run(
        crate::default_data_dir(),
        None,
        false,
        false,
        None,
        Some(stop_rx),
    ));

    reporter_done.store(true, Ordering::SeqCst);
    let _ = reporter.join();
    set_status(
        ServiceState::StopPending,
        ServiceControlAccept::empty(),
        Duration::from_secs(10),
    )?;
    set_status(
        ServiceState::Stopped,
        ServiceControlAccept::empty(),
        Duration::ZERO,
    )?;
    run_res
}
