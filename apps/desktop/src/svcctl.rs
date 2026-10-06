//! Windows service control for the VardeAgent service. The elevated
//! `--service-start` / `--service-stop` instance calls these headlessly and
//! exits; the normal tray instance uses them nowhere — it only relaunches
//! itself elevated (see app.rs), since LocalSystem services need an
//! administrator.

use anyhow::{Context, Result};
use std::ffi::OsString;
use std::time::{Duration, Instant};
use windows_service::service::{Service, ServiceAccess, ServiceState};
use windows_service::service_manager::{ServiceManager, ServiceManagerAccess};

const SERVICE_NAME: &str = "VardeAgent";
/// How long a drain or a pending transition may take before it counts as
/// stuck. Matches the service's own preshutdown budget (svc.rs) and the
/// uninstaller's wait.
const DRAIN: Duration = Duration::from_secs(180);
const START_WAIT: Duration = Duration::from_secs(60);

fn open(access: ServiceAccess) -> Result<Service> {
    let mgr = ServiceManager::local_computer(None::<&str>, ServiceManagerAccess::CONNECT)
        .context("open the service manager")?;
    mgr.open_service(SERVICE_NAME, access)
        .context("find the Varde service — is Varde installed?")
}

fn state(svc: &Service) -> Result<ServiceState> {
    svc.query_status()
        .map(|s| s.current_state)
        .context("query the Varde service")
}

/// Wait for Running or Stopped. Pending transitions (the drain reports
/// StopPending) settle on their own; one that never does is stuck.
fn settled(svc: &Service, timeout: Duration) -> Result<ServiceState> {
    let deadline = Instant::now() + timeout;
    loop {
        let st = state(svc)?;
        if matches!(st, ServiceState::Running | ServiceState::Stopped) {
            return Ok(st);
        }
        anyhow::ensure!(
            Instant::now() < deadline,
            "the Varde service is stuck {st:?}"
        );
        std::thread::sleep(Duration::from_millis(500));
    }
}

fn wait_for(svc: &Service, want: ServiceState, timeout: Duration) -> Result<()> {
    let deadline = Instant::now() + timeout;
    loop {
        if state(svc)? == want {
            return Ok(());
        }
        anyhow::ensure!(
            Instant::now() < deadline,
            "the Varde service did not reach {want:?} within {} s",
            timeout.as_secs()
        );
        std::thread::sleep(Duration::from_millis(500));
    }
}

/// Ensure VardeAgent runs. The tray offers Start only while the service's
/// IPC is unreachable — which is a stopped service OR a running-but-dead
/// one (SCM still shows Running). Stop/restart the dead one so Varde
/// recovers; SCM start on an already-running service would just fail.
pub fn start() -> Result<()> {
    let svc = open(ServiceAccess::START | ServiceAccess::STOP | ServiceAccess::QUERY_STATUS)?;
    if settled(&svc, DRAIN)? == ServiceState::Running {
        svc.stop().context("ask the Varde service to stop")?;
        wait_for(&svc, ServiceState::Stopped, DRAIN)?;
    }
    svc.start(&[] as &[OsString])
        .context("start the Varde service")?;
    wait_for(&svc, ServiceState::Running, START_WAIT)
}

/// Stop VardeAgent and wait until it reports Stopped. The service drains on
/// stop (final save + replication hold), so this can take a couple of minutes.
pub fn stop() -> Result<()> {
    let svc = open(ServiceAccess::STOP | ServiceAccess::QUERY_STATUS)?;
    if settled(&svc, DRAIN)? == ServiceState::Running {
        svc.stop().context("ask the Varde service to stop")?;
        wait_for(&svc, ServiceState::Stopped, DRAIN)?;
    }
    Ok(())
}
