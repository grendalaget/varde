//! Windows service integration: `service install|uninstall` plus the SCM
//! entrypoint (`service run`, hidden). The service runs the same agent
//! `run` path on a tokio runtime; SCM stop/shutdown controls feed the
//! same graceful-stop channel as SIGTERM/ctrl-c, with a 120 s preshutdown
//! hint so the final snapshot + replication hold can finish.
//!
//! Not linked yet is a running (idle) state, not a failure. Real failures
//! exit with a service-specific code, and the SCM restarts the service.

use anyhow::{Context, Result};
use std::ffi::OsString;
use std::sync::{mpsc, Arc, Mutex};
use std::time::{Duration, Instant};
use windows_service::service::{
    ServiceAccess, ServiceAction, ServiceActionType, ServiceControl, ServiceControlAccept,
    ServiceErrorControl, ServiceExitCode, ServiceFailureActions, ServiceFailureResetPeriod,
    ServiceInfo, ServiceStartType, ServiceState, ServiceStatus, ServiceType,
};
use windows_service::service_control_handler::{self, ServiceControlHandlerResult};
use windows_service::service_dispatcher;
use windows_service::service_manager::{ServiceManager, ServiceManagerAccess};

use crate::svc_status::{Status, StatusReporter};

const SERVICE_NAME: &str = "VardeAgent";
const SERVICE_DISPLAY: &str = "Varde Agent";
const PRESHUTDOWN_MS: u32 = 150_000;

enum ReporterEvent {
    Draining,
    Done,
}

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
        .create_service(&info, ServiceAccess::CHANGE_CONFIG | ServiceAccess::START)
        .context("create service")?;
    let restart = |s| ServiceAction {
        action_type: ServiceActionType::Restart,
        delay: Duration::from_secs(s),
    };
    svc.update_failure_actions(ServiceFailureActions {
        reset_period: ServiceFailureResetPeriod::After(Duration::from_secs(86_400)),
        reboot_msg: None,
        command: None,
        actions: Some(vec![restart(5), restart(15), restart(60)]),
    })
    .context("set failure actions")?;
    // also restart when we stop ourselves with a non-zero exit code
    svc.set_failure_actions_on_non_crash_failures(true)
        .context("set failure actions flag")?;
    svc.set_description("Varde node agent: peer-hosted game servers.")
        .ok();
    svc.set_preshutdown_timeout(Duration::from_millis(PRESHUTDOWN_MS as u64))
        .context("set preshutdown timeout")?;
    secure_data_dir().context("restrict the data dir to SYSTEM and Administrators")?;
    // WER LocalDumps so hard crashes (access violations, abort) leave a
    // minidump the agent can report next boot; panics come from the hook.
    if let Err(e) = crash_report::wer::register(&crate::default_data_dir().join("crash-reports")) {
        tracing::warn!(error = %e, "could not register WER local dumps");
    }
    println!("service {SERVICE_NAME} installed (LocalSystem, auto-start)");
    Ok(())
}

/// %ProgramData%\Varde is SYSTEM + Administrators only: the service runs as
/// SYSTEM and trusts config.toml and server.url there. ProgramData lets any
/// user create files and folders, so drop what the folder inherits, take back
/// ownership of anything a user created first, and reset explicit entries
/// below it. identity\ (the node key) and logs\ are then set up again.
fn secure_data_dir() -> Result<()> {
    const ADMINS: &str = "*S-1-5-32-544";
    const OWNERS: [&str; 2] = ["*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F"];
    let data = crate::default_data_dir();
    let ident = data.join("identity");
    std::fs::create_dir_all(&ident).context("create data dir")?;
    let icacls = |path: &std::path::Path, args: &[&str]| -> Result<()> {
        let out = std::process::Command::new("icacls")
            .arg(path)
            .args(args)
            .output()
            .context("run icacls")?;
        anyhow::ensure!(
            out.status.success(),
            "icacls {} {}: {}",
            path.display(),
            args.join(" "),
            String::from_utf8_lossy(&out.stdout).trim()
        );
        Ok(())
    };
    let protect = [&["/inheritance:r", "/grant:r"][..], &OWNERS].concat();
    icacls(&data, &["/setowner", ADMINS, "/T", "/C", "/Q"])?;
    // drop explicit entries on the folder itself, then what it inherits
    icacls(&data, &["/reset", "/Q"])?;
    icacls(&data, &protect)?;
    icacls(&data.join("*"), &["/reset", "/T", "/C", "/Q"])?;
    icacls(&ident, &protect)?;
    let logs = data.join("logs");
    if logs.is_dir() {
        icacls(&logs, &["/grant", "*S-1-5-32-545:(OI)(CI)RX"])?;
    }
    Ok(())
}
pub fn uninstall() -> Result<()> {
    let mgr = ServiceManager::local_computer(None::<&str>, ServiceManagerAccess::CONNECT)
        .context("open SCM")?;
    let svc = mgr
        .open_service(
            SERVICE_NAME,
            ServiceAccess::DELETE | ServiceAccess::STOP | ServiceAccess::QUERY_STATUS,
        )
        .context("open service")?;
    let _ = svc.stop();
    // wait for the graceful stop (final save + replication hold) before the
    // installer removes the binaries; bounded like the preshutdown budget
    let deadline = std::time::Instant::now() + Duration::from_secs(180);
    loop {
        match svc.query_status() {
            Ok(st) if st.current_state == ServiceState::Stopped => break,
            Ok(_) if std::time::Instant::now() < deadline => {
                std::thread::sleep(Duration::from_millis(500))
            }
            Ok(_) => {
                eprintln!("service {SERVICE_NAME} did not stop within 180 s; removing anyway");
                break;
            }
            Err(e) => {
                eprintln!("query service status: {e}");
                break;
            }
        }
    }
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
        tracing::error!("service failed: {e:#}");
    }
}

fn service_main_inner() -> Result<()> {
    // SCM -> agent graceful-stop channel
    let (stop_tx, stop_rx) = tokio::sync::oneshot::channel::<()>();
    let mut stop_tx = Some(stop_tx);
    let (reporter_tx, reporter_rx) = mpsc::channel();
    let handler_reporter = reporter_tx.clone();
    type Reporter = StatusReporter<Box<dyn FnMut(Status) -> windows_service::Result<()> + Send>>;
    let status: Arc<Mutex<Option<Reporter>>> = Arc::new(Mutex::new(None));
    let handler_status = status.clone();
    // Controls wait until the handle and initial reports are ready.
    let mut startup = status.lock().unwrap();
    let status_handle = service_control_handler::register(
        SERVICE_NAME,
        move |event| -> ServiceControlHandlerResult {
            match event {
                ServiceControl::Stop | ServiceControl::Shutdown | ServiceControl::Preshutdown => {
                    if let Err(e) = handler_status
                        .lock()
                        .unwrap()
                        .as_mut()
                        .unwrap()
                        .stop(Instant::now())
                    {
                        tracing::error!(error = %e, "report StopPending");
                    }
                    if let Some(tx) = stop_tx.take() {
                        let _ = handler_reporter.send(ReporterEvent::Draining);
                        let _ = tx.send(());
                    }
                    ServiceControlHandlerResult::NoError
                }
                _ => ServiceControlHandlerResult::NotImplemented,
            }
        },
    )
    .context("register handler")?;

    *startup = Some(StatusReporter::new(Box::new(move |status| {
        status_handle.set_service_status(service_status(status))
    })));
    startup.as_mut().unwrap().start()?;
    drop(startup);

    // re-report StopPending every few seconds while the agent drains; a
    // single short wait hint would let the SCM kill us mid-hold
    let reporter_status = status.clone();
    let reporter = std::thread::spawn(move || loop {
        match reporter_rx.recv_timeout(Duration::from_secs(3)) {
            Ok(ReporterEvent::Draining) => (),
            Ok(ReporterEvent::Done) | Err(mpsc::RecvTimeoutError::Disconnected) => break,
            Err(mpsc::RecvTimeoutError::Timeout) => {
                if let Err(e) = reporter_status
                    .lock()
                    .unwrap()
                    .as_mut()
                    .unwrap()
                    .tick(Instant::now())
                {
                    tracing::warn!(error = %e, "refresh StopPending");
                }
            }
        }
    });

    // run the real agent on this worker thread; SCM controls feed the
    // same graceful-stop path as SIGTERM/ctrl-c.
    let run_res = tokio::runtime::Runtime::new()
        .context("tokio runtime")
        .and_then(|rt| {
            rt.block_on(crate::run(
                crate::default_data_dir(),
                None,
                false,
                false,
                None,
                None,
                Some(stop_rx),
            ))
        });

    let _ = reporter_tx.send(ReporterEvent::Done);
    let _ = reporter.join();
    if let Err(e) = &run_res {
        tracing::error!("agent failed: {e:#}");
    }
    // a non-zero exit code makes the SCM apply the restart actions
    status
        .lock()
        .unwrap()
        .as_mut()
        .unwrap()
        .finish(run_res.is_err())?;
    run_res
}

fn service_status(status: Status) -> ServiceStatus {
    let mut report = ServiceStatus {
        service_type: ServiceType::OWN_PROCESS,
        current_state: ServiceState::StartPending,
        controls_accepted: ServiceControlAccept::empty(),
        exit_code: ServiceExitCode::Win32(0),
        checkpoint: 0,
        wait_hint: Duration::ZERO,
        process_id: None,
    };
    match status {
        Status::StartPending => report.wait_hint = Duration::from_secs(10),
        Status::Running => {
            report.current_state = ServiceState::Running;
            report.controls_accepted = ServiceControlAccept::STOP
                | ServiceControlAccept::SHUTDOWN
                | ServiceControlAccept::PRESHUTDOWN;
        }
        Status::StopPending { checkpoint } => {
            report.current_state = ServiceState::StopPending;
            report.checkpoint = checkpoint;
            report.wait_hint = Duration::from_secs(30);
        }
        Status::Stopped { failed } => {
            report.current_state = ServiceState::Stopped;
            if failed {
                report.exit_code = ServiceExitCode::ServiceSpecific(1);
            }
        }
    }
    report
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pending_status_disables_controls_and_retains_checkpoint_and_hint() {
        let status = service_status(Status::StopPending { checkpoint: 7 });
        assert_eq!(status.current_state, ServiceState::StopPending);
        assert!(status.controls_accepted.is_empty());
        assert_eq!(status.checkpoint, 7);
        assert_eq!(status.wait_hint, Duration::from_secs(30));
    }

    #[test]
    fn stopped_status_preserves_failure_restart_policy() {
        for failed in [false, true] {
            let status = service_status(Status::Stopped { failed });
            assert_eq!(status.current_state, ServiceState::Stopped);
            assert!(status.controls_accepted.is_empty());
            assert_eq!(status.checkpoint, 0);
            assert_eq!(status.wait_hint, Duration::ZERO);
            match status.exit_code {
                ServiceExitCode::ServiceSpecific(1) => assert!(failed),
                ServiceExitCode::Win32(0) => assert!(!failed),
                other => panic!("unexpected exit code: {other:?}"),
            }
        }
    }
}
