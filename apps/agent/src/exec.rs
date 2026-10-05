//! Execution supervisor: `prepare → restore → configure → spawn → probe →
//! running`; periodic + requested snapshots behind the driver barrier;
//! restart policy 3-in-10-min; graceful stop with final snapshot and a
//! replication hold; fencing watchdog targets the same handles.

use std::collections::VecDeque;
use std::future::Future;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use anyhow::{bail, Context, Result};
use cp_api::{AgentSnapshot, ExecutionDirective};
use executor_api::{Executor, OutputStream, ProcessHandle};
use game_driver_api::{
    DeploymentSpec, DriverContext, GameDriver, GameHealth, PortBinding, PortSpec, SnapshotBarrier,
};
use snapshot_store::{SnapshotId, SnapshotMeta};
use tokio::sync::watch;

use crate::chunks;
use crate::Agent;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Phase {
    Preparing,
    Restoring,
    Starting,
    Running,
    Stopping,
    Stopped,
    Failed,
    /// killed by the fencing watchdog; uploads forbidden
    Fenced,
}

impl Phase {
    pub fn as_str(&self) -> &'static str {
        match self {
            Phase::Preparing => "preparing",
            Phase::Restoring => "restoring",
            Phase::Starting => "starting",
            Phase::Running => "running",
            Phase::Stopping => "stopping",
            Phase::Stopped => "stopped",
            Phase::Failed => "failed",
            Phase::Fenced => "fenced",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StopKind {
    /// Directive says action=stop: graceful → final snapshot → stopped.
    Graceful,
    /// Gone from directives / fenced: kill now, no uploads.
    Hard,
}

pub struct ExecCtl {
    pub dir: ExecutionDirective,
    phase: Mutex<Phase>,
    health: Mutex<String>,
    message: Mutex<String>,
    /// fencing deadline; refreshed each heartbeat
    pub deadline: Mutex<Option<Instant>>,
    fenced: AtomicBool,
    stop: watch::Sender<Option<StopKind>>,
    pgid: Mutex<u32>,
    proc: Mutex<Option<Arc<dyn ProcessHandle>>>,
    snapshot_reqs: Mutex<VecDeque<(String, String)>>, // (request_id, reason)
    task: Mutex<Option<tokio::task::JoinHandle<()>>>,
    /// set when a graceful stop finished + reported stopped
    finished: AtomicBool,
    /// service ports -> agent-allocated local target ports; fixed for the
    /// whole execution so in-execution restarts reuse them
    pub port_bindings: Mutex<Vec<PortBinding>>,
}

impl ExecCtl {
    pub fn new(dir: ExecutionDirective) -> Arc<ExecCtl> {
        let (tx, _rx) = watch::channel(None);
        Arc::new(ExecCtl {
            dir,
            phase: Mutex::new(Phase::Preparing),
            health: Mutex::new("unknown".into()),
            message: Mutex::new(String::new()),
            deadline: Mutex::new(None),
            fenced: AtomicBool::new(false),
            stop: tx,
            pgid: Mutex::new(0),
            proc: Mutex::new(None),
            snapshot_reqs: Mutex::new(VecDeque::new()),
            task: Mutex::new(None),
            finished: AtomicBool::new(false),
            port_bindings: Mutex::new(Vec::new()),
        })
    }

    pub fn phase(&self) -> Phase {
        *self.phase.lock().unwrap()
    }
    pub fn set_phase(&self, p: Phase) {
        *self.phase.lock().unwrap() = p;
    }
    pub fn health(&self) -> String {
        self.health.lock().unwrap().clone()
    }
    pub fn message(&self) -> String {
        self.message.lock().unwrap().clone()
    }
    pub fn set_health(&self, h: &str) {
        *self.health.lock().unwrap() = h.to_string();
    }
    pub fn set_message(&self, m: &str) {
        *self.message.lock().unwrap() = m.to_string();
    }
    pub fn is_fenced(&self) -> bool {
        self.fenced.load(Ordering::SeqCst)
    }
    pub fn mark_fenced(&self) {
        self.fenced.store(true, Ordering::SeqCst);
        self.set_phase(Phase::Fenced);
    }
    pub fn is_finished(&self) -> bool {
        self.finished.load(Ordering::SeqCst)
    }
    pub fn mark_finished(&self) {
        self.finished.store(true, Ordering::SeqCst);
    }
    pub fn request_stop(&self, kind: StopKind) {
        // send_replace stores even with no receivers yet — a stop requested
        // before run() subscribes must not be lost
        let _ = self.stop.send_replace(Some(kind));
    }
    fn stop_rx(&self) -> watch::Receiver<Option<StopKind>> {
        self.stop.subscribe()
    }
    pub fn set_pgid(&self, pid: u32) {
        *self.pgid.lock().unwrap() = pid;
    }
    pub fn pgid(&self) -> u32 {
        *self.pgid.lock().unwrap()
    }
    pub fn set_proc(&self, p: Arc<dyn ProcessHandle>) {
        *self.proc.lock().unwrap() = Some(p);
    }
    pub fn proc(&self) -> Option<Arc<dyn ProcessHandle>> {
        self.proc.lock().unwrap().clone()
    }
    pub fn queue_snapshot(&self, request_id: String, reason: String) {
        self.snapshot_reqs
            .lock()
            .unwrap()
            .push_back((request_id, reason));
    }
    fn next_snapshot_req(&self) -> Option<(String, String)> {
        self.snapshot_reqs.lock().unwrap().pop_front()
    }
    pub fn set_task(&self, t: tokio::task::JoinHandle<()>) {
        *self.task.lock().unwrap() = Some(t);
    }
}

/// executor/driver APIs return Box<dyn Error>; anyhow can't `?` them.
fn dyn_err(e: executor_api::DynError) -> anyhow::Error {
    anyhow::anyhow!("{e}")
}

fn unix_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// Report an immediate state transition to the CP (best effort — heartbeat
/// also carries it).
pub async fn report(agent: &Agent, ctl: &ExecCtl) {
    let body = cp_api::ExecutionStatusUpdate {
        server_id: ctl.dir.server_id.clone(),
        epoch: ctl.dir.epoch,
        state: ctl.phase().as_str().into(),
        health: Some(ctl.health()),
        message: Some(ctl.message()),
    };
    let path = format!("/v1/agent/executions/{}/status", ctl.dir.execution_id);
    if let Err(e) = agent
        .cp
        .json::<_, serde_json::Value>("POST", &path, Some(&body))
        .await
    {
        tracing::warn!(exec = %ctl.dir.execution_id, error = %e, "status report failed");
    }
}

pub fn spawn_supervisor(agent: Arc<Agent>, ctl: Arc<ExecCtl>) {
    let a = agent.clone();
    let c = ctl.clone();
    let task = tokio::spawn(async move {
        if let Err(e) = run(a.clone(), c.clone()).await {
            tracing::error!(exec = %c.dir.execution_id, error = %format!("{e:#}"), "supervisor failed");
            c.set_phase(Phase::Failed);
            c.set_message(&format!("{e:#}"));
            report(&a, &c).await;
        }
        a.execs.lock().unwrap().remove(&c.dir.execution_id);
        a.state.lock().unwrap().remove(&c.dir.execution_id);
        a.sync_hosted_services().await;
    });
    ctl.set_task(task);
}

async fn run(agent: Arc<Agent>, ctl: Arc<ExecCtl>) -> Result<()> {
    let dir = ctl.dir.clone();
    let game_id = dir
        .deployment
        .as_ref()
        .map(|d| d.game_id.clone())
        .unwrap_or_else(|| "testgame".into());
    let driver: &dyn GameDriver = agent
        .drivers
        .get(&game_id)
        .context(format!("no driver for {game_id}"))?;
    let config = dir.config.clone().unwrap_or(serde_json::json!({}));
    driver.validate(&config).map_err(dyn_err)?;

    let server_dir = agent.data_dir.join("servers").join(&dir.server_id);
    let deployment_dir = agent.data_dir.join("deployments").join(
        dir.deployment
            .as_ref()
            .map(|d| d.id.clone())
            .unwrap_or_else(|| game_id.clone()),
    );
    std::fs::create_dir_all(&server_dir)?;
    std::fs::create_dir_all(&deployment_dir)?;
    let deployment = DeploymentSpec::parse(
        &dir.deployment
            .as_ref()
            .map(|d| d.spec.clone())
            .unwrap_or(serde_json::json!({})),
    );
    // allocate the local port block before configure so drivers can bind
    // 127.0.0.1:<local_port> while the mesh owns <loopback>:<service_port>
    let bindings = alloc_port_block(&driver.ports(&config))?;
    *ctl.port_bindings.lock().unwrap() = bindings.clone();
    let ctx = DriverContext {
        server_dir: &server_dir,
        deployment_dir: &deployment_dir,
        runtimes: &*agent.runtimes,
        deployment: &deployment,
        config: &config,
        ports: &bindings,
        memory_mb: 0,
    };

    // ---- prepare ----
    ctl.set_phase(Phase::Preparing);
    report(&agent, &ctl).await;
    driver.prepare(&ctx).await.map_err(dyn_err)?;
    ensure_live(&ctl)?;

    // ---- restore ----
    if let Some(r) = &dir.restore {
        ctl.set_phase(Phase::Restoring);
        report(&agent, &ctl).await;
        let sid = SnapshotId(r.snapshot_id.clone());
        chunks::fetch_snapshot(
            &agent.mesh,
            &agent.store,
            &sid,
            &r.manifest_digest,
            &r.source_node_ids,
        )
        .await?;
        let store = agent.store.clone();
        let inc = driver.persistent_paths(&config);
        let dst = server_dir.clone();
        tokio::task::spawn_blocking(move || store.restore(&sid, &dst, &inc)).await??;
    }
    ensure_live(&ctl)?;

    driver.configure(&ctx).await.map_err(dyn_err)?;
    ensure_live(&ctl)?;

    // ---- spawn + probe + supervise ----
    let mut attempts: VecDeque<Instant> = VecDeque::new();
    let mut last_snap = Instant::now();
    let snap_interval = Duration::from_secs(dir.snapshot_interval_s.unwrap_or(120).max(5) as u64);
    let mut stop_rx = ctl.stop_rx();

    'outer: loop {
        ensure_live(&ctl)?;
        // restart policy: 3 attempts in 10 minutes
        let now = Instant::now();
        while attempts
            .front()
            .map(|t| now.duration_since(*t) > Duration::from_secs(600))
            .unwrap_or(false)
        {
            attempts.pop_front();
        }
        if attempts.len() >= 3 {
            ctl.set_phase(Phase::Failed);
            ctl.set_message("restart limit reached (3 in 10 min)");
            report(&agent, &ctl).await;
            ctl.mark_finished();
            return Ok(());
        }
        attempts.push_back(now);

        ctl.set_phase(Phase::Starting);
        report(&agent, &ctl).await;
        let spec = driver.process_spec(&ctx).map_err(dyn_err)?;
        let proc = agent.executor.spawn(&spec).await.map_err(dyn_err)?;
        let proc: Arc<dyn ProcessHandle> = Arc::from(proc);
        tracing::info!(exec = %dir.execution_id, pid = proc.pid(), "game process spawned");
        ctl.set_pgid(proc.pid());
        ctl.set_proc(proc.clone());
        agent.note_exec_pgid(&dir.execution_id, proc.pid());
        spawn_log_pump(&agent, &ctl, proc.clone());

        // watch the process while probing so an early crash restarts now
        // instead of waiting out the probe deadline
        let exit_watch = {
            let p = proc.clone();
            tokio::spawn(async move { p.wait().await })
        };
        tokio::pin!(exit_watch);

        // probe until healthy; any early return past this point must kill the
        // spawned process — bail!/return would leave the game orphaned and the
        // exec removed from execs (the fencing watchdog could never reach it)
        let mut running = false;
        let mut probe_dead = false;
        let probe_deadline = Instant::now() + Duration::from_secs(120);
        while Instant::now() < probe_deadline && !exit_watch.is_finished() {
            match driver.probe(&ctx, &*proc).await {
                Ok(GameHealth::Healthy) => {
                    running = true;
                    break;
                }
                Ok(GameHealth::Dead(m)) => {
                    tracing::warn!(exec = %dir.execution_id, "probe: {m}");
                    probe_dead = true;
                    break;
                }
                Ok(GameHealth::Starting) => {
                    tokio::select! {
                        _ = tokio::time::sleep(Duration::from_millis(250)) => {}
                        _ = &mut exit_watch => {}
                    }
                }
                Err(e) => {
                    tracing::warn!(exec = %dir.execution_id, error = %format!("{e:#}"), "probe error");
                }
            }
            if ctl.is_fenced() {
                running = false;
                break;
            }
        }
        if probe_dead {
            let _ = proc.kill().await;
            continue 'outer;
        }
        if !running && exit_watch.is_finished() {
            if let Ok(st) = (&mut exit_watch).await {
                tracing::warn!(exec = %dir.execution_id, status = ?st, "game exited during startup");
            }
        }
        if ctl.is_fenced() {
            let _ = proc.kill().await;
            ctl.mark_finished();
            return Ok(());
        }
        if !running {
            let _ = proc.kill().await;
            continue 'outer;
        }
        ctl.set_phase(Phase::Running);
        ctl.set_health("ok");
        report(&agent, &ctl).await;
        agent.sync_hosted_services().await;

        // ---- running loop ----
        let mut exited = false;
        let mut graceful_stop = false;
        loop {
            tokio::select! {
                _ = stop_rx.changed() => {
                    let kind = *stop_rx.borrow();
                    match kind {
                        Some(StopKind::Hard) => {
                            let _ = proc.kill().await;
                            ctl.mark_finished();
                            return Ok(());
                        }
                        Some(StopKind::Graceful) => { graceful_stop = true; break; }
                        None => continue,
                    }
                }
                _ = proc.wait() => { exited = true; break; }
                _ = tokio::time::sleep(Duration::from_millis(200)) => {}
            }
            if ctl.is_fenced() {
                let _ = proc.kill().await;
                ctl.mark_finished();
                return Ok(());
            }
            // periodic + requested snapshots
            let due_req = ctl.next_snapshot_req();
            let due_time = last_snap.elapsed() >= snap_interval;
            if let Some((req_id, reason)) = due_req {
                let _ = do_snapshot(
                    &agent,
                    &ctl,
                    driver,
                    &ctx,
                    &proc,
                    SnapshotOptions {
                        reason: &reason,
                        request_id: Some(&req_id),
                        barrier: true,
                    },
                )
                .await;
                last_snap = Instant::now();
            } else if due_time {
                let _ = do_snapshot(
                    &agent,
                    &ctl,
                    driver,
                    &ctx,
                    &proc,
                    SnapshotOptions {
                        reason: "scheduled",
                        request_id: None,
                        barrier: true,
                    },
                )
                .await;
                last_snap = Instant::now();
            }
        }

        if exited && !graceful_stop && !ctl.is_fenced() {
            tracing::warn!(exec = %dir.execution_id, "game exited; restarting");
            continue 'outer;
        }
        if graceful_stop {
            break;
        }
        if ctl.is_fenced() {
            ctl.mark_finished();
            return Ok(());
        }
    }

    // ---- graceful stop: final snapshot + replication hold ----
    ctl.set_phase(Phase::Stopping);
    report(&agent, &ctl).await;
    let proc = ctl.proc().unwrap();

    let mut final_snap = None;
    if driver.snapshot_after_stop() {
        final_snap = stop_then_snapshot(
            &ctl,
            &proc,
            || driver.graceful_stop(&ctx, &*proc),
            |barrier| {
                do_snapshot(
                    &agent,
                    &ctl,
                    driver,
                    &ctx,
                    &proc,
                    SnapshotOptions {
                        reason: "final",
                        request_id: None,
                        barrier,
                    },
                )
            },
        )
        .await;
    } else {
        // The driver's barrier needs a running process, so these games must
        // be snapshotted before graceful_stop asks the process to exit.
        if !ctl.is_fenced() {
            match do_snapshot(
                &agent,
                &ctl,
                driver,
                &ctx,
                &proc,
                SnapshotOptions {
                    reason: "final",
                    request_id: None,
                    barrier: true,
                },
            )
            .await
            {
                Ok(info) => final_snap = Some(info),
                Err(e) => {
                    tracing::warn!(exec = %dir.execution_id, error = %format!("{e:#}"), "final snapshot failed")
                }
            }
        }

        let _ = driver
            .graceful_stop(&ctx, &*proc)
            .await
            .map_err(|e| tracing::warn!("graceful stop: {e}"));
        let _ = tokio::time::timeout(Duration::from_secs(30), proc.wait()).await;
        let _ = proc.kill().await;
    }

    if let Some(info) = final_snap {
        let _ = hold_for_replication(&agent, &info.id).await;
    }

    ctl.set_phase(Phase::Stopped);
    ctl.mark_finished();
    report(&agent, &ctl).await;
    agent.sync_hosted_services().await;
    Ok(())
}

/// Barrier → Store::snapshot → POST /v1/agent/snapshots. Returns the info on
/// success; marks the manifest invalid locally on 409 stale_epoch.
struct SnapshotOptions<'a> {
    reason: &'a str,
    request_id: Option<&'a str>,
    barrier: bool,
}

async fn do_snapshot(
    agent: &Agent,
    ctl: &ExecCtl,
    driver: &dyn GameDriver,
    ctx: &DriverContext<'_>,
    proc: &Arc<dyn ProcessHandle>,
    options: SnapshotOptions<'_>,
) -> Result<snapshot_store::SnapshotInfo> {
    let reason = options.reason.to_string();
    let reason2 = reason.clone();
    if ctl.is_fenced() {
        bail!("fenced execution never uploads snapshots");
    }
    if options.barrier {
        match driver
            .prepare_snapshot(ctx, &**proc)
            .await
            .map_err(dyn_err)?
        {
            SnapshotBarrier::Live => {}
            SnapshotBarrier::RequiresStop => {
                bail!("driver requires stop for snapshot")
            }
        }
    }
    let server_dir = ctx.server_dir.to_path_buf();
    let inc = driver.persistent_paths(ctx.config);
    let store = agent.store.clone();
    let dir = ctl.dir.clone();
    let dep_id = dir
        .deployment
        .as_ref()
        .map(|d| d.id.clone())
        .unwrap_or_else(|| game_id(ctx));
    let node_id = agent.cfg.node_id.clone();
    let info = tokio::task::spawn_blocking(move || {
        store.snapshot(
            &server_dir,
            &inc,
            SnapshotMeta {
                server_id: dir.server_id.clone(),
                execution_id: dir.execution_id.clone(),
                epoch: dir.epoch,
                node_id,
                // parent is advisory; the CP derives it from its own rows
                parent: None,
                deployment_id: dep_id,
                created_at_unix_ms: unix_ms(),
                reason: reason2,
            },
        )
    })
    .await??;
    if options.barrier {
        driver
            .resume_after_snapshot(ctx, &**proc)
            .await
            .map_err(dyn_err)?;
    }
    // the fence margin can fire while the snapshot was being built — a fenced
    // exec drops its manifest instead of uploading
    if ctl.is_fenced() {
        let _ = agent.store.delete_snapshot(&info.id);
        bail!("fenced execution never uploads snapshots");
    }

    let body = AgentSnapshot {
        snapshot_id: info.id.0.clone(),
        server_id: ctl.dir.server_id.clone(),
        execution_id: ctl.dir.execution_id.clone(),
        epoch: ctl.dir.epoch,
        parent_id: None,
        manifest_digest: info.digest.clone(),
        deployment_id: ctl
            .dir
            .deployment
            .as_ref()
            .map(|d| d.id.clone())
            .unwrap_or_default(),
        reason: reason.clone(),
        size_bytes: Some(info.size_bytes as i64),
        stored_bytes: Some(info.stored_bytes as i64),
        file_count: Some(info.file_count as i64),
        chunk_count: Some(info.chunk_count as i64),
        request_id: options.request_id.map(|s| s.to_string()),
    };
    if ctl.is_fenced() {
        let _ = agent.store.delete_snapshot(&info.id);
        bail!("fenced execution never uploads snapshots");
    }
    match agent
        .cp
        .json::<_, serde_json::Value>("POST", "/v1/agent/snapshots", Some(&body))
        .await
    {
        Ok(_) => {
            tracing::info!(exec = %ctl.dir.execution_id, snap = %info.id, reason = %reason, "snapshot accepted");
        }
        Err(e) if e.api_code() == Some("stale_epoch") => {
            // CP rejected: mark invalid locally, never upload again
            let _ = agent.store.delete_snapshot(&info.id);
            tracing::warn!(snap = %info.id, "snapshot rejected as stale; marked invalid");
            return Err(e.into());
        }
        Err(e) => return Err(e.into()),
    }
    Ok(info)
}

async fn stop_then_snapshot<T, Stop, StopFuture, Snapshot, SnapshotFuture>(
    ctl: &ExecCtl,
    proc: &Arc<dyn ProcessHandle>,
    stop: Stop,
    snapshot: Snapshot,
) -> Option<T>
where
    Stop: FnOnce() -> StopFuture,
    StopFuture: Future<Output = game_driver_api::Result<()>>,
    Snapshot: FnOnce(bool) -> SnapshotFuture,
    SnapshotFuture: Future<Output = Result<T>>,
{
    if let Err(e) = stop().await {
        tracing::warn!(error = %e, "graceful stop");
    }

    match tokio::time::timeout(Duration::from_secs(60), proc.wait()).await {
        Ok(Ok(_)) => {}
        Ok(Err(e)) => {
            tracing::warn!(error = %e, "could not confirm game exit; killing and skipping final snapshot");
            let _ = proc.kill().await;
            return None;
        }
        Err(_) => {
            tracing::warn!(
                "game did not exit after graceful stop; killing and skipping final snapshot"
            );
            let _ = proc.kill().await;
            return None;
        }
    }

    if ctl.is_fenced() {
        return None;
    }

    match snapshot(false).await {
        Ok(info) => Some(info),
        Err(e) => {
            tracing::warn!(exec = %ctl.dir.execution_id, error = %format!("{e:#}"), "final snapshot failed");
            None
        }
    }
}

/// Bail if the execution has been fenced or a stop was requested — a
/// fenced exec must never progress to spawn.
fn ensure_live(ctl: &ExecCtl) -> Result<()> {
    if ctl.is_fenced() {
        bail!("fenced");
    }
    if ctl.stop.borrow().is_some() {
        bail!("stop requested");
    }
    Ok(())
}

/// Allocate one contiguous block of local ports covering `service_ports`.
/// Offsets are preserved relative to the lowest service port (so a game
/// using p and p+1 gets base/base+1). The base is random in 20000–59999
/// and accepted only when every offset test-binds on TCP and UDP on both
/// 127.0.0.1 and 0.0.0.0; retries up to 50 bases.
pub fn alloc_port_block(service_ports: &[PortSpec]) -> Result<Vec<PortBinding>> {
    use rand::Rng;
    if service_ports.is_empty() {
        return Ok(vec![]);
    }
    let lo = service_ports.iter().map(|p| p.port).min().unwrap();
    let hi = service_ports.iter().map(|p| p.port).max().unwrap();
    let span = (hi - lo) as usize + 1;
    let mut rng = rand::thread_rng();
    for _ in 0..50 {
        let base = rng.gen_range(20000u32..=59999u32.saturating_sub(span as u32 - 1));
        let free = (0..span).all(|off| port_free(base + off as u32));
        if !free {
            continue;
        }
        return Ok(service_ports
            .iter()
            .map(|p| PortBinding {
                service_port: p.port,
                local_port: base + (p.port - lo),
                protocol: p.protocol,
            })
            .collect());
    }
    bail!("no free contiguous port block after 50 attempts")
}

fn port_free(port: u32) -> bool {
    let p = port as u16;
    std::net::TcpListener::bind(("127.0.0.1", p)).is_ok()
        && std::net::TcpListener::bind(("0.0.0.0", p)).is_ok()
        && std::net::UdpSocket::bind(("127.0.0.1", p)).is_ok()
        && std::net::UdpSocket::bind(("0.0.0.0", p)).is_ok()
}

fn game_id(ctx: &DriverContext<'_>) -> String {
    ctx.deployment
        .get("game_id")
        .and_then(|v| v.as_str())
        .unwrap_or("testgame")
        .to_string()
}

/// Hold the stop until a peer fetched the whole snapshot through our chunk
/// server (≈ "reports ready"), bounded by shutdown_replication_timeout.
async fn hold_for_replication(agent: &Agent, id: &SnapshotId) -> Result<()> {
    let timeout = Duration::from_secs(agent.cfg.shutdown_replication_timeout_s.max(1) as u64);
    let deadline = Instant::now() + timeout;
    let m = agent.store.manifest(id)?;
    while Instant::now() < deadline {
        if agent.chunk_tracker.fully_served(&id.0, &m) || agent.chunk_tracker.peer_ready(&id.0) {
            tracing::info!(snap = %id, "final snapshot fully fetched by a peer");
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    tracing::warn!(snap = %id, "replication hold timed out; save remains local for now");
    Ok(())
}

/// Forward process output to the CP logs endpoint (throttled).
fn spawn_log_pump(agent: &Arc<Agent>, ctl: &Arc<ExecCtl>, proc: Arc<dyn ProcessHandle>) {
    let mut rx = proc.output();
    let a = agent.clone();
    let exec_id = ctl.dir.execution_id.clone();
    tokio::spawn(async move {
        let mut batch = Vec::new();
        let mut tick = tokio::time::interval(Duration::from_secs(2));
        loop {
            tokio::select! {
                l = rx.recv() => {
                    match l {
                        Ok(l) => {
                            batch.push(cp_api::LogLine {
                                at: l.at_unix_ms,
                                stream: match l.stream {
                                    OutputStream::Stdout => "stdout",
                                    OutputStream::Stderr => "stderr",
                                    OutputStream::Agent => "agent",
                                }.into(),
                                line: l.line.to_string(),
                            });
                            if batch.len() >= 100 {
                                flush_logs(&a, &exec_id, &mut batch).await;
                            }
                        }
                        Err(_) => {
                            flush_logs(&a, &exec_id, &mut batch).await;
                            return;
                        }
                    }
                }
                _ = tick.tick() => {
                    if !batch.is_empty() {
                        flush_logs(&a, &exec_id, &mut batch).await;
                    }
                }
            }
        }
    });
}

async fn flush_logs(agent: &Agent, exec_id: &str, batch: &mut Vec<cp_api::LogLine>) {
    let body = cp_api::LogBatch {
        lines: std::mem::take(batch),
    };
    let path = format!("/v1/agent/executions/{exec_id}/logs");
    let _ = agent
        .cp
        .json::<_, serde_json::Value>("POST", &path, Some(&body))
        .await;
}

#[cfg(test)]
mod port_alloc_tests {
    use super::*;
    use game_driver_api::GameProtocol;

    fn spec(port: u32) -> PortSpec {
        PortSpec {
            port,
            protocol: GameProtocol::Tcp,
        }
    }

    #[test]
    fn contiguous_offsets_preserved() {
        // valheim-style: two service ports one apart
        let b = alloc_port_block(&[spec(2456), spec(2457)]).unwrap();
        assert_eq!(b.len(), 2);
        assert_eq!(b[0].local_port + 1, b[1].local_port);
        assert_eq!(b[0].service_port, 2456);
        assert_eq!(b[1].service_port, 2457);
        assert!((20000..=59999).contains(&b[0].local_port));
    }

    #[test]
    fn allocated_ports_are_free() {
        let b = alloc_port_block(&[spec(25565), spec(25566), spec(25568)]).unwrap();
        for p in &b {
            for addr in ["127.0.0.1", "0.0.0.0"] {
                std::net::TcpListener::bind((addr, p.local_port as u16))
                    .expect("tcp bind must succeed on allocated port");
                std::net::UdpSocket::bind((addr, p.local_port as u16))
                    .expect("udp bind must succeed on allocated port");
            }
        }
        // offsets relative to the lowest service port
        assert_eq!(b[1].local_port - b[0].local_port, 1);
        assert_eq!(b[2].local_port - b[0].local_port, 3);
    }

    #[test]
    fn empty_is_empty() {
        assert!(alloc_port_block(&[]).unwrap().is_empty());
    }
}

#[cfg(test)]
mod ensure_live_tests {
    use super::*;

    fn ctl() -> Arc<ExecCtl> {
        ExecCtl::new(ExecutionDirective {
            execution_id: "e".into(),
            server_id: "s".into(),
            server_name: "s".into(),
            epoch: 1,
            lease_expires_at_unix_ms: 0,
            action: "run".into(),
            stop_reason: None,
            deployment: None,
            config: None,
            service: None,
            restore: None,
            snapshot_interval_s: None,
            snapshot_requests: vec![],
        })
    }

    #[test]
    fn live_ctl_ok() {
        assert!(ensure_live(&ctl()).is_ok());
    }

    #[test]
    fn fenced_ctl_bails() {
        let c = ctl();
        c.mark_fenced();
        assert!(ensure_live(&c).is_err());
    }

    #[test]
    fn stop_requested_bails() {
        let c = ctl();
        c.request_stop(StopKind::Hard);
        assert!(ensure_live(&c).is_err());
    }

    struct StopWritingProcess {
        path: std::path::PathBuf,
        output: tokio::sync::broadcast::Sender<executor_api::OutputLine>,
    }

    #[async_trait::async_trait]
    impl ProcessHandle for StopWritingProcess {
        fn pid(&self) -> u32 {
            1
        }
        async fn write_stdin(&self, _line: &str) -> executor_api::Result<()> {
            Ok(())
        }
        fn output(&self) -> tokio::sync::broadcast::Receiver<executor_api::OutputLine> {
            self.output.subscribe()
        }
        fn output_tail(&self, _n: usize) -> Vec<executor_api::OutputLine> {
            Vec::new()
        }
        async fn wait(&self) -> executor_api::Result<executor_api::ExitStatus> {
            Ok(executor_api::ExitStatus::default())
        }
        async fn terminate(&self) -> executor_api::Result<()> {
            Ok(())
        }
        async fn interrupt(&self) -> executor_api::Result<()> {
            std::fs::write(&self.path, b"written during graceful stop")?;
            Ok(())
        }
        async fn kill(&self) -> executor_api::Result<()> {
            Ok(())
        }
        fn resource_usage(&self) -> Option<executor_api::ResourceUsage> {
            None
        }
    }

    #[tokio::test]
    async fn post_stop_snapshot_includes_writes_from_graceful_stop_without_barrier() {
        let tmp = tempfile::tempdir().unwrap();
        let server_dir = tmp.path().join("server");
        std::fs::create_dir_all(&server_dir).unwrap();
        let file = server_dir.join("state");
        std::fs::write(&file, b"before stop").unwrap();
        let (output, _) = tokio::sync::broadcast::channel(8);
        let proc: Arc<dyn ProcessHandle> = Arc::new(StopWritingProcess { path: file, output });
        let store = snapshot_store::Store::open(tmp.path().join("store")).unwrap();
        let snapshot_store = &store;
        let snapshot_server_dir = &server_dir;
        let snap = stop_then_snapshot(
            &ctl(),
            &proc,
            || proc.interrupt(),
            |barrier| async move {
                assert!(!barrier);
                let patterns = [snapshot_store::PathPattern::new("state")];
                Ok(snapshot_store.snapshot(
                    snapshot_server_dir,
                    &patterns,
                    SnapshotMeta {
                        server_id: "s".into(),
                        execution_id: "e".into(),
                        epoch: 1,
                        node_id: "n".into(),
                        parent: None,
                        deployment_id: "d".into(),
                        created_at_unix_ms: 0,
                        reason: "final".into(),
                    },
                )?)
            },
        )
        .await
        .expect("snapshot after stop");

        let restored = tmp.path().join("restored");
        store
            .restore(
                &snap.id,
                &restored,
                &[snapshot_store::PathPattern::new("state")],
            )
            .unwrap();
        assert_eq!(
            std::fs::read(restored.join("state")).unwrap(),
            b"written during graceful stop"
        );
    }
}
