//! Windows service control for the VardeAgent service. The elevated
//! `--service-start` / `--service-stop` instance calls these headlessly and
//! exits; the normal tray instance uses them nowhere — it only relaunches
//! itself elevated (see app.rs), since LocalSystem services need an
//! administrator.

use anyhow::{Context, Result};
use std::ffi::OsString;
use std::time::{Duration, Instant};
use windows_service::service::{ServiceAccess, ServiceState};
use windows_service::service_manager::{ServiceManager, ServiceManagerAccess};

const SERVICE_NAME: &str = "VardeAgent";

fn open(access: ServiceAccess) -> Result<windows_service::service::Service> {
    let mgr = ServiceManager::local_computer(None::<&str>, ServiceManagerAccess::CONNECT)
        .context("open the service manager")?;
    mgr.open_service(SERVICE_NAME, access)
        .context("find the Varde service — is Varde installed?")
}

fn wait(access: ServiceAccess, want: ServiceState, timeout: Duration) -> Result<()> {
    let svc = open(access)?;
    let deadline = Instant::now() + timeout;
    loop {
        let state = svc
            .query_status()
            .context("query the Varde service")?
            .current_state;
        if state == want {
            return Ok(());
        }
        anyhow::ensure!(
            Instant::now() < deadline,
            "the Varde service did not reach {want:?} within {} s",
            timeout.as_secs()
        );
        std::thread::sleep(Duration::from_millis(250));
    }
}

/// Start VardeAgent and wait until it reports Running.
pub fn start() -> Result<()> {
    open(ServiceAccess::START)?
        .start(&[] as &[OsString])
        .context("start the Varde service")?;
    wait(
        ServiceAccess::QUERY_STATUS,
        ServiceState::Running,
        Duration::from_secs(60),
    )
}

/// Stop VardeAgent and wait until it reports Stopped. The service drains on
/// stop (final save + replication hold), so this can take a couple of minutes.
pub fn stop() -> Result<()> {
    let svc = open(ServiceAccess::STOP | ServiceAccess::QUERY_STATUS)?;
    svc.stop().context("ask the Varde service to stop")?;
    let deadline = Instant::now() + Duration::from_secs(180);
    loop {
        let state = svc
            .query_status()
            .context("query the Varde service")?
            .current_state;
        if state == ServiceState::Stopped {
            return Ok(());
        }
        anyhow::ensure!(
            Instant::now() < deadline,
            "the Varde service did not stop in time"
        );
        std::thread::sleep(Duration::from_secs(1));
    }
}
