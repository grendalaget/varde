//! Hot standby: pre-install the deployments (runtime + game files) of servers
//! this node would take over, so a failover or shutdown handoff skips the
//! download. The control plane picks the standby and replicates every
//! snapshot here; this module only installs.

use std::path::Path;
use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use cp_api::StandbyDirective;
use game_driver_api::{DeploymentSpec, DriverContext, GameDriver};
use tracing::{info, warn};

use crate::Agent;

/// Written into a deployment dir after a successful prepare; the heartbeat
/// reports those dirs as `cached_deployments` (scheduler bonus).
const PREPARED_MARKER: &str = ".varde-prepared";
const RETRY_AFTER: Duration = Duration::from_secs(60);

/// Deployment ids with a completed install under `<data>/deployments`.
pub fn prepared_deployments(data_dir: &Path) -> Vec<String> {
    let Ok(entries) = std::fs::read_dir(data_dir.join("deployments")) else {
        return vec![];
    };
    let mut out: Vec<String> = entries
        .filter_map(|e| e.ok())
        .filter(|e| e.path().join(PREPARED_MARKER).is_file())
        .filter_map(|e| e.file_name().into_string().ok())
        .collect();
    out.sort();
    out
}

/// `driver.prepare` serialized per deployment dir, so an execution and a
/// standby pre-install never download into the same dir at once.
pub async fn prepare_locked(
    agent: &Agent,
    driver: &dyn GameDriver,
    ctx: &DriverContext<'_>,
) -> Result<()> {
    let lock = agent
        .prepare_locks
        .lock()
        .unwrap()
        .entry(ctx.deployment_dir.to_string_lossy().into_owned())
        .or_default()
        .clone();
    let _guard = lock.lock().await;
    driver.prepare(ctx).await.map_err(crate::exec::dyn_err)?;
    if let Err(e) = std::fs::write(ctx.deployment_dir.join(PREPARED_MARKER), b"") {
        warn!(error = %e, "could not mark deployment prepared");
    }
    Ok(())
}

/// Start pre-installs for standby directives not yet handled in this agent
/// run. Prepare is idempotent, so a restart just re-checks the files.
pub fn apply(agent: &Arc<Agent>, list: &[StandbyDirective]) {
    if agent.shutting_down.load(Ordering::SeqCst) {
        return;
    }
    for sd in list {
        let key = standby_key(sd);
        if !agent.standby_inflight.lock().unwrap().insert(key.clone()) {
            continue;
        }
        let agent = agent.clone();
        let sd = sd.clone();
        tokio::spawn(async move {
            match pre_install(&agent, &sd).await {
                Ok(()) => info!(
                    server = %sd.server_id,
                    deployment = %sd.deployment.id,
                    "standby: deployment pre-installed"
                ),
                Err(e) => {
                    warn!(server = %sd.server_id, error = %format!("{e:#}"), "standby pre-install failed");
                    tokio::time::sleep(RETRY_AFTER).await;
                    agent.standby_inflight.lock().unwrap().remove(&key);
                }
            }
        });
    }
}

/// The install depends on the deployment and the server config (e.g. the
/// Minecraft version), so a config change installs again.
fn standby_key(sd: &StandbyDirective) -> String {
    let config = sd
        .config
        .as_ref()
        .map(|c| c.to_string())
        .unwrap_or_default();
    format!("{}|{}|{config}", sd.server_id, sd.deployment.id)
}

async fn pre_install(agent: &Agent, sd: &StandbyDirective) -> Result<()> {
    let driver = agent
        .drivers
        .get(&sd.deployment.game_id)
        .with_context(|| format!("no driver for {}", sd.deployment.game_id))?;
    let config = sd.config.clone().unwrap_or(serde_json::json!({}));
    driver.validate(&config).map_err(crate::exec::dyn_err)?;
    let deployment_dir = agent.data_dir.join("deployments").join(&sd.deployment.id);
    // prepare must not touch a live server dir; it only installs into the
    // deployment dir, but give it a private scratch dir regardless
    let scratch = agent.data_dir.join("standby").join(&sd.server_id);
    std::fs::create_dir_all(&deployment_dir)?;
    std::fs::create_dir_all(&scratch)?;
    let deployment = DeploymentSpec::parse(&sd.deployment.spec);
    let ctx = DriverContext {
        server_id: &sd.server_id,
        server_dir: &scratch,
        deployment_dir: &deployment_dir,
        runtimes: &*agent.runtimes,
        deployment: &deployment,
        config: &config,
        ports: &[],
        memory_mb: 0,
        addons: &[],
    };
    prepare_locked(agent, driver, &ctx).await
}
