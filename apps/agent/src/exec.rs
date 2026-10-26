//! Execution supervisor: `prepare → restore → configure → spawn → probe →
//! running`; periodic + requested snapshots behind the driver barrier;
//! restart policy 3-in-10-min; graceful stop with final snapshot and a
//! replication hold; fencing watchdog targets the same handles.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

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

type ProcessExit =
    std::result::Result<executor_api::Result<executor_api::ExitStatus>, tokio::task::JoinError>;

async fn startup_exit(
    watch: &mut tokio::task::JoinHandle<executor_api::Result<executor_api::ExitStatus>>,
    observed: Option<ProcessExit>,
) -> Option<ProcessExit> {
    match observed {
        Some(status) => Some(status),
        None if watch.is_finished() => Some(watch.await),
        None => None,
    }
}

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
    join_code: Mutex<Option<String>>,
    /// fencing deadline; refreshed each heartbeat
    pub deadline: Mutex<Option<Instant>>,
    /// Send instant of the CP response that set `deadline`; responses may
    /// arrive out of order, and only the newest-sent one may set it.
    pub deadline_sent: Mutex<Option<Instant>>,
    fence_margin_ms: AtomicI64,
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
    /// created_at of the latest snapshot the CP accepted in this execution
    latest_save_ms: AtomicI64,
    /// desired freeze state from directives (open warm migration), and the
    /// state actually applied via driver.freeze()/unfreeze().
    want_frozen: AtomicBool,
    frozen: AtomicBool,
    /// stop_reason from the latest directive (refreshable: dir is a snapshot
    /// of the directive that spawned this ctl, but a later heartbeat can
    /// change action/stop_reason — e.g. a warm flip's 'handoff' stop).
    stop_reason: Mutex<Option<String>>,
}

impl ExecCtl {
    pub fn new(dir: ExecutionDirective) -> Arc<ExecCtl> {
        let stop_reason = dir.stop_reason.clone();
        let (tx, _rx) = watch::channel(None);
        Arc::new(ExecCtl {
            dir,
            phase: Mutex::new(Phase::Preparing),
            health: Mutex::new("unknown".into()),
            message: Mutex::new(String::new()),
            join_code: Mutex::new(None),
            deadline: Mutex::new(None),
            deadline_sent: Mutex::new(None),
            fence_margin_ms: AtomicI64::new(0),
            fenced: AtomicBool::new(false),
            stop: tx,
            pgid: Mutex::new(0),
            proc: Mutex::new(None),
            snapshot_reqs: Mutex::new(VecDeque::new()),
            task: Mutex::new(None),
            finished: AtomicBool::new(false),
            port_bindings: Mutex::new(Vec::new()),
            latest_save_ms: AtomicI64::new(0),
            want_frozen: AtomicBool::new(false),
            frozen: AtomicBool::new(false),
            stop_reason: Mutex::new(stop_reason),
        })
    }

    pub fn set_stop_reason(&self, r: Option<String>) {
        *self.stop_reason.lock().unwrap() = r;
    }
    pub fn stop_reason(&self) -> Option<String> {
        self.stop_reason.lock().unwrap().clone()
    }

    pub fn set_want_frozen(&self, v: bool) {
        self.want_frozen.store(v, Ordering::SeqCst);
    }
    pub fn want_frozen(&self) -> bool {
        self.want_frozen.load(Ordering::SeqCst)
    }
    pub fn is_frozen(&self) -> bool {
        self.frozen.load(Ordering::SeqCst)
    }

    pub fn latest_save_ms(&self) -> i64 {
        self.latest_save_ms.load(Ordering::SeqCst)
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
    pub fn join_code(&self) -> Option<String> {
        self.join_code.lock().unwrap().clone()
    }
    pub fn clear_join_code(&self) {
        *self.join_code.lock().unwrap() = None;
    }
    fn set_join_code(&self, code: String) -> bool {
        let mut current = self.join_code.lock().unwrap();
        if current.as_ref() == Some(&code) {
            return false;
        }
        *current = Some(code);
        true
    }
    pub fn is_fenced(&self) -> bool {
        self.fenced.load(Ordering::SeqCst)
    }
    pub fn set_fence_margin_ms(&self, margin_ms: i64) {
        self.fence_margin_ms.store(margin_ms, Ordering::SeqCst);
    }
    pub fn fence_margin_ms(&self) -> i64 {
        self.fence_margin_ms.load(Ordering::SeqCst)
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
        self.stop.send_replace(Some(kind));
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
pub(crate) fn dyn_err(e: executor_api::DynError) -> anyhow::Error {
    anyhow::anyhow!("{e}")
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
        join_code: Some(ctl.join_code().unwrap_or_default()),
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
    let game_id = exec_game_id(&dir).to_string();
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
        server_id: &dir.server_id,
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
    crate::standby::prepare_locked(&agent, driver, &ctx).await?;
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
        ctl.clear_join_code();
        let proc = spawn_live(driver, &ctx, &agent.executor, &ctl).await?;
        tracing::info!(exec = %dir.execution_id, pid = proc.pid(), "game process spawned");
        agent.note_exec_pgid(&dir.execution_id, proc.pid());
        spawn_join_code_watcher(&agent, &ctl, proc.clone(), game_id.clone());
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
        let mut exit_status = None;
        let probe_deadline = Instant::now() + Duration::from_secs(120);
        while Instant::now() < probe_deadline && !exit_watch.is_finished() && exit_status.is_none()
        {
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
                        st = &mut exit_watch => { exit_status = Some(st); }
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
        if !running {
            let status = startup_exit(&mut exit_watch, exit_status.take()).await;
            if let Some(Ok(st)) = status {
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
            // reconcile freeze state ahead of snapshot barriers: a migration
            // snapshot under `tick freeze` is drift-free.
            let want = ctl.want_frozen.load(Ordering::SeqCst);
            if want != ctl.is_frozen() {
                if want {
                    match driver.freeze(&ctx, &*proc).await {
                        Ok(true) => ctl.frozen.store(true, Ordering::SeqCst),
                        Ok(false) => ctl.frozen.store(false, Ordering::SeqCst),
                        Err(e) => {
                            tracing::warn!(exec = %ctl.dir.execution_id, error = %format!("{e:#}"), "freeze failed")
                        }
                    }
                } else {
                    let _ = driver.unfreeze(&ctx, &*proc).await;
                    ctl.frozen.store(false, Ordering::SeqCst);
                }
            }
            // periodic + requested snapshots
            let due_req = ctl.next_snapshot_req();
            let due_time = last_snap.elapsed() >= snap_interval;
            if let Some((req_id, reason)) = due_req {
                let _ =
                    do_snapshot(&agent, &ctl, driver, &ctx, &proc, &reason, Some(&req_id)).await;
                last_snap = Instant::now();
            } else if due_time {
                let _ = do_snapshot(&agent, &ctl, driver, &ctx, &proc, "scheduled", None).await;
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
    // Warm migration flip: routes already point at the replacement; hand
    // connected clients off to the stable service address before stopping.
    issue_handoff(&ctl, driver, &ctx, &proc).await;
    graceful_stop_and_snapshot(&agent, &ctl, driver, &ctx, &proc).await;

    ctl.set_phase(Phase::Stopped);
    ctl.mark_finished();
    report(&agent, &ctl).await;
    agent.sync_hosted_services().await;
    Ok(())
}

/// How long a shutting-down host waits for the control plane to start a
/// warm migration (seen as a freeze on the execution) before stopping.
const SHUTDOWN_HANDOFF_ACK: Duration = Duration::from_secs(10);

/// What the shutdown drain does next with one handoff-capable execution.
#[derive(Debug, PartialEq, Eq)]
pub enum DrainStep {
    /// the execution finished or the flip arrived; nothing left to decide
    Done,
    /// the warm migration is pending or in progress
    Wait,
    /// fall back to the graceful stop (final save + replication hold)
    Stop,
}

/// `acked`: the CP froze this execution for a warm migration at some point.
pub fn drain_step(
    ctl: &ExecCtl,
    acked: bool,
    now: Instant,
    ack_by: Instant,
    handoff_by: Instant,
) -> DrainStep {
    if ctl.is_finished() || ctl.stop_reason().as_deref() == Some("handoff") {
        return DrainStep::Done;
    }
    let aborted = acked && !ctl.want_frozen();
    if aborted || now >= handoff_by || (!acked && now >= ack_by) {
        return DrainStep::Stop;
    }
    DrainStep::Wait
}

fn exec_game_id(dir: &ExecutionDirective) -> &str {
    dir.deployment
        .as_ref()
        .map(|d| d.game_id.as_str())
        .unwrap_or("testgame")
}

/// Clean-shutdown drain. Running servers whose driver can hand players off
/// stay up while the control plane warm-migrates them (players are
/// transferred, not disconnected); everything else, and any handoff that is
/// not acknowledged, aborts or overruns `shutdown_handoff_timeout_s`, takes
/// the graceful stop. Returns once every execution finished or the overall
/// budget ran out.
pub async fn drain_for_shutdown(agent: &Agent) {
    let start = Instant::now();
    let handoff_budget = Duration::from_secs(agent.cfg.shutdown_handoff_timeout_s.max(0) as u64);
    let ack_by = start + SHUTDOWN_HANDOFF_ACK.min(handoff_budget);
    let handoff_by = start + handoff_budget;
    let deadline = handoff_by
        + Duration::from_secs(agent.cfg.shutdown_replication_timeout_s.max(5) as u64 + 30);

    let execs: Vec<Arc<ExecCtl>> = agent.execs.lock().unwrap().values().cloned().collect();
    let mut waiting: Vec<(Arc<ExecCtl>, bool)> = Vec::new();
    for c in execs {
        let can_handoff = agent
            .drivers
            .get(exec_game_id(&c.dir))
            .is_some_and(|d| d.can_handoff());
        if can_handoff && c.phase() == Phase::Running && !handoff_budget.is_zero() {
            waiting.push((c, false));
        } else {
            c.request_stop(StopKind::Graceful);
        }
    }
    if !waiting.is_empty() {
        tracing::info!(
            servers = waiting.len(),
            "shutting down: waiting for the control plane to move servers"
        );
    }
    loop {
        let now = Instant::now();
        waiting.retain_mut(|(c, acked)| {
            *acked |= c.want_frozen();
            match drain_step(c, *acked, now, ack_by, handoff_by) {
                DrainStep::Done => false,
                DrainStep::Wait => true,
                DrainStep::Stop => {
                    tracing::info!(
                        exec = %c.dir.execution_id,
                        acked = *acked,
                        "shutdown handoff did not complete; stopping"
                    );
                    c.request_stop(StopKind::Graceful);
                    false
                }
            }
        });
        let all_finished = agent
            .execs
            .lock()
            .unwrap()
            .values()
            .all(|c| c.is_finished());
        if all_finished || now >= deadline {
            return;
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

/// Issue the driver's client handoff when this execution is stopping for a
/// warm migration flip: routes already point at the replacement, so clients
/// are sent the stable service address and land on it.
async fn issue_handoff(
    ctl: &ExecCtl,
    driver: &dyn GameDriver,
    ctx: &DriverContext<'_>,
    proc: &Arc<dyn ProcessHandle>,
) {
    if ctl.stop_reason().as_deref() != Some("handoff") {
        return;
    }
    let Some(svc) = &ctl.dir.service else {
        return;
    };
    let port = svc
        .ports
        .iter()
        .find(|p| p.protocol.eq_ignore_ascii_case("tcp"))
        .map(|p| p.port)
        .or_else(|| svc.ports.first().map(|p| p.port));
    let Some(port) = port else { return };
    match driver
        .handoff(ctx, &**proc, &svc.loopback_ip, port as u16)
        .await
    {
        Ok(true) => tracing::info!(exec = %ctl.dir.execution_id, "client handoff issued"),
        Ok(false) => {
            tracing::info!(exec = %ctl.dir.execution_id, "driver has no handoff; clients rejoin manually")
        }
        Err(e) => {
            tracing::warn!(exec = %ctl.dir.execution_id, error = %format!("{e:#}"), "handoff failed")
        }
    }
}

/// Graceful stop with the driver's snapshot barrier honoured: a `Live`
/// barrier snapshots while the game runs, then resumes and stops it; a
/// `RequiresStop` barrier stops first and snapshots the save written at exit
/// only after a clean exit and if the execution isn't fenced. Ends with the
/// replication hold for whichever final snapshot was produced.
async fn graceful_stop_and_snapshot(
    agent: &Agent,
    ctl: &ExecCtl,
    driver: &dyn GameDriver,
    ctx: &DriverContext<'_>,
    proc: &Arc<dyn ProcessHandle>,
) {
    let mut final_snap = None;
    let mut requires_stop = driver.snapshot_after_stop();
    if !requires_stop && !ctl.is_fenced() {
        let (final_snap, requires_stop) = (&mut final_snap, &mut requires_stop);
        // snapshot failures are logged here, so only a failed resume is
        // returned — and it never discards a snapshot already taken
        let resumed = with_snapshot_barrier(
            &ctl.dir.execution_id,
            driver,
            ctx,
            proc,
            |barrier| async move {
                match barrier {
                    Ok(SnapshotBarrier::Live) => {
                        match snapshot_files(agent, ctl, driver, ctx, "final", None).await {
                            Ok(info) => *final_snap = Some(info),
                            Err(e) => {
                                if let Some(path) = is_changed_during_snapshot(&e) {
                                    *requires_stop = true;
                                    tracing::warn!(
                                        exec = %ctl.dir.execution_id,
                                        path,
                                        "live final snapshot changed; falling back to a post-stop snapshot"
                                    );
                                } else {
                                    tracing::warn!(exec = %ctl.dir.execution_id, error = %format!("{e:#}"), "final snapshot failed")
                                }
                            }
                        }
                    }
                    Ok(SnapshotBarrier::RequiresStop) => *requires_stop = true,
                    Err(e) => {
                        tracing::warn!(exec = %ctl.dir.execution_id, error = %format!("{e:#}"), "final snapshot failed")
                    }
                }
                Ok(())
            },
        )
        .await;
        if let Err(e) = resumed {
            tracing::warn!("resume after snapshot: {e}");
        }
    }

    let _ = driver
        .graceful_stop(ctx, &**proc)
        .await
        .map_err(|e| tracing::warn!("graceful stop: {e}"));
    let exit_status = tokio::time::timeout(Duration::from_secs(30), proc.wait()).await;
    let exited_cleanly = matches!(&exit_status, Ok(Ok(status)) if status.success());
    let _ = proc.kill().await;

    if final_snap.is_none() && requires_stop {
        if !exited_cleanly {
            tracing::warn!(
                status = ?exit_status,
                "game did not exit cleanly; skipping final snapshot"
            );
        } else if !ctl.is_fenced() {
            match snapshot_files(agent, ctl, driver, ctx, "final", None).await {
                Ok(info) => final_snap = Some(info),
                Err(e) => {
                    tracing::warn!(exec = %ctl.dir.execution_id, error = %format!("{e:#}"), "final snapshot failed")
                }
            }
        }
    }

    if let Some(info) = final_snap {
        let _ = hold_for_replication(agent, &info.id).await;
    }
}

/// Barrier → Store::snapshot → resume → POST /v1/agent/snapshots. Returns the
/// info on success; marks the manifest invalid locally on 409 stale_epoch.
async fn do_snapshot(
    agent: &Agent,
    ctl: &ExecCtl,
    driver: &dyn GameDriver,
    ctx: &DriverContext<'_>,
    proc: &Arc<dyn ProcessHandle>,
    reason: &str,
    request_id: Option<&str>,
) -> Result<snapshot_store::SnapshotInfo> {
    if ctl.is_fenced() {
        bail!("fenced execution never uploads snapshots");
    }
    retry_changed_snapshot(&ctl.dir.execution_id, move |_| async move {
        if ctl.is_fenced() {
            bail!("fenced execution never uploads snapshots");
        }
        with_snapshot_barrier(
            &ctl.dir.execution_id,
            driver,
            ctx,
            proc,
            |barrier| async move {
                match barrier? {
                    SnapshotBarrier::Live => {
                        snapshot_files(agent, ctl, driver, ctx, reason, request_id).await
                    }
                    SnapshotBarrier::RequiresStop => {
                        bail!("driver requires stop for snapshot")
                    }
                }
            },
        )
        .await
    })
    .await
}

/// Run `body` inside the driver's snapshot barrier and always resume after
/// it. Drivers that pause autosave may have paused it even when the barrier
/// then reports `RequiresStop` or errors, so `body` gets the barrier result
/// and resume runs whatever it was. A failed resume is the result only when
/// `body` succeeded; otherwise it is logged and `body`'s error wins.
async fn with_snapshot_barrier<T, F, Fut>(
    exec: &str,
    driver: &dyn GameDriver,
    ctx: &DriverContext<'_>,
    proc: &Arc<dyn ProcessHandle>,
    body: F,
) -> Result<T>
where
    F: FnOnce(Result<SnapshotBarrier>) -> Fut,
    Fut: std::future::Future<Output = Result<T>>,
{
    let barrier = driver.prepare_snapshot(ctx, &**proc).await.map_err(dyn_err);
    let res = body(barrier).await;
    let resumed = driver
        .resume_after_snapshot(ctx, &**proc)
        .await
        .map_err(dyn_err);
    match (res, resumed) {
        (Ok(v), Ok(())) => Ok(v),
        (Ok(_), Err(re)) => Err(re),
        (Err(e), Err(re)) => {
            tracing::warn!(exec = %exec, error = %re, "resume after snapshot");
            Err(e)
        }
        (Err(e), Ok(())) => Err(e),
    }
}

async fn retry_changed_snapshot<T, Attempt, Fut>(exec: &str, mut attempt: Attempt) -> Result<T>
where
    Attempt: FnMut(u8) -> Fut,
    Fut: std::future::Future<Output = Result<T>>,
{
    for attempt_number in 1..=3 {
        match attempt(attempt_number).await {
            Ok(value) => return Ok(value),
            Err(error) => {
                let Some(path) = is_changed_during_snapshot(&error) else {
                    return Err(error);
                };
                if attempt_number == 3 {
                    return Err(error);
                }
                tracing::warn!(
                    exec = %exec,
                    attempt = attempt_number,
                    path,
                    "save files changed during snapshot; retrying"
                );
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
        }
    }
    unreachable!("snapshot retry loop always returns on the third attempt")
}

fn is_changed_during_snapshot(error: &anyhow::Error) -> Option<&str> {
    for cause in error.chain() {
        if let Some(snapshot_store::Error::ChangedDuringSnapshot(path)) =
            cause.downcast_ref::<snapshot_store::Error>()
        {
            return Some(path);
        }
    }
    None
}

/// Store::snapshot → POST /v1/agent/snapshots, with no barrier and no resume —
/// the caller arranges those around it. Used both inside `do_snapshot` (after
/// a Live barrier) and by the graceful-stop path for drivers that require the
/// process to have exited before their save can be read.
async fn snapshot_files(
    agent: &Agent,
    ctl: &ExecCtl,
    driver: &dyn GameDriver,
    ctx: &DriverContext<'_>,
    reason: &str,
    request_id: Option<&str>,
) -> Result<snapshot_store::SnapshotInfo> {
    let reason = reason.to_string();
    let reason2 = reason.clone();
    if ctl.is_fenced() {
        bail!("fenced execution never uploads snapshots");
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
    let created_at = crate::now_ms();
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
                created_at_unix_ms: created_at,
                reason: reason2,
            },
        )
    })
    .await??;
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
        request_id: request_id.map(|s| s.to_string()),
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
        Ok(v) => {
            // the CP's created_at, the same clock as latest_safe_save_at;
            // without one, ours moved onto the CP's clock
            let at = v
                .get("created_at")
                .and_then(|t| t.as_i64())
                .unwrap_or_else(|| created_at + agent.cp_view.lock().unwrap().cp_clock_offset_ms);
            ctl.latest_save_ms.store(at, Ordering::SeqCst);
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

/// Bail if the execution has been fenced or a stop was requested — a
/// fenced exec must never progress to spawn.
fn ensure_live(ctl: &ExecCtl) -> Result<()> {
    if ctl.is_fenced() {
        bail!("fenced");
    }
    if ctl.stop.borrow().is_some() {
        bail!("stop requested");
    }
    if ctl
        .deadline
        .lock()
        .unwrap()
        .is_some_and(|deadline| Instant::now() >= deadline)
    {
        bail!("lease deadline expired");
    }
    Ok(())
}

struct LiveExecutor<'a> {
    executor: &'a dyn Executor,
    ctl: &'a ExecCtl,
}

#[async_trait::async_trait]
impl Executor for LiveExecutor<'_> {
    fn kind(&self) -> &'static str {
        self.executor.kind()
    }

    async fn spawn(
        &self,
        spec: &executor_api::ProcessSpec,
    ) -> executor_api::Result<Box<dyn ProcessHandle>> {
        ensure_live(self.ctl).map_err(|error| error.to_string())?;
        self.executor.spawn(spec).await
    }
}

async fn spawn_live(
    driver: &dyn GameDriver,
    ctx: &DriverContext<'_>,
    executor: &dyn Executor,
    ctl: &ExecCtl,
) -> Result<Arc<dyn ProcessHandle>> {
    ensure_live(ctl)?;
    let executor = LiveExecutor { executor, ctl };
    let process = driver.spawn(ctx, &executor).await.map_err(dyn_err)?;
    let process: Arc<dyn ProcessHandle> = Arc::from(process);
    ctl.set_pgid(process.pid());
    ctl.set_proc(process.clone());
    if let Err(error) = ensure_live(ctl) {
        let _ = process.kill().await;
        let _ = process.wait().await;
        return Err(error);
    }
    Ok(process)
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
    // valid bases are 20000..=60000-span — a wider span has none
    if span > 40_000 {
        bail!("service ports span {span} ports; cannot allocate a contiguous block");
    }
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
    agent.chunk_tracker.pin(&id.0);
    while Instant::now() < deadline {
        if agent.chunk_tracker.fully_served(&id.0, &m) || agent.chunk_tracker.peer_ready(&id.0) {
            agent.chunk_tracker.unpin(&id.0);
            tracing::info!(snap = %id, "final snapshot fully fetched by a peer");
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
    agent.chunk_tracker.unpin(&id.0);
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
                        Err(tokio::sync::broadcast::error::RecvError::Lagged(n)) => {
                            // dropped lines are lost, but the pump must live
                            // on — exiting here would silence the rest
                            tracing::warn!(skipped = n, "log pump lagged");
                        }
                        Err(tokio::sync::broadcast::error::RecvError::Closed) => {
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

fn spawn_join_code_watcher(
    agent: &Arc<Agent>,
    ctl: &Arc<ExecCtl>,
    proc: Arc<dyn ProcessHandle>,
    game_id: String,
) {
    let mut rx = proc.output();
    let agent = agent.clone();
    let ctl = ctl.clone();
    tokio::spawn(async move {
        loop {
            let line = match rx.recv().await {
                Ok(line) => line,
                Err(tokio::sync::broadcast::error::RecvError::Lagged(_)) => continue,
                Err(tokio::sync::broadcast::error::RecvError::Closed) => return,
            };
            let code = agent
                .drivers
                .get(&game_id)
                .and_then(|driver| driver.join_code_from_log(&line.line));
            if let Some(code) = code {
                if ctl.set_join_code(code) {
                    report(&agent, &ctl).await;
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
mod snapshot_retry_tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn changed_error() -> anyhow::Error {
        anyhow::Error::new(snapshot_store::Error::ChangedDuringSnapshot(
            "saves/world.db".into(),
        ))
    }

    #[tokio::test]
    async fn retries_two_changes_then_succeeds() {
        let attempts = AtomicUsize::new(0);
        let result = retry_changed_snapshot("exec_t", |attempt_number| {
            attempts.fetch_add(1, Ordering::SeqCst);
            std::future::ready(if attempt_number < 3 {
                Err(changed_error())
            } else {
                Ok("snapshot")
            })
        })
        .await;

        assert_eq!(result.unwrap(), "snapshot");
        assert_eq!(attempts.load(Ordering::SeqCst), 3);
    }

    #[tokio::test]
    async fn returns_the_third_change_error() {
        let attempts = AtomicUsize::new(0);
        let result: Result<()> = retry_changed_snapshot("exec_t", |_| {
            attempts.fetch_add(1, Ordering::SeqCst);
            std::future::ready(Err(changed_error()))
        })
        .await;

        let error = result.unwrap_err();
        assert_eq!(is_changed_during_snapshot(&error), Some("saves/world.db"));
        assert_eq!(attempts.load(Ordering::SeqCst), 3);
    }

    #[tokio::test]
    async fn returns_non_change_errors_without_retrying() {
        let attempts = AtomicUsize::new(0);
        let result: Result<()> = retry_changed_snapshot("exec_t", |_| {
            attempts.fetch_add(1, Ordering::SeqCst);
            std::future::ready(Err(anyhow::anyhow!("other failure")))
        })
        .await;

        assert!(result.is_err());
        assert_eq!(attempts.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn finds_changed_snapshot_error_in_the_anyhow_chain() {
        let error = changed_error().context("snapshot failed");
        assert_eq!(is_changed_during_snapshot(&error), Some("saves/world.db"));
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
            latest_safe_save_at_unix_ms: None,
            latest_safe_save_on: vec![],
            latest_save_at_unix_ms: None,
            latest_save_on: vec![],
            freeze: None,
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
}

#[cfg(test)]
mod requires_stop_tests {
    use super::*;
    use executor_api::{ExitStatus, OutputLine, ProcessSpec, ResourceUsage};
    use game_driver_api::PathPattern;
    use snapshot_store::Store;
    use std::path::Path;
    use std::sync::atomic::AtomicUsize;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::sync::broadcast;

    #[tokio::test]
    async fn startup_exit_never_repolls_a_consumed_task() {
        for consumed in [false, true] {
            for failed in [false, true] {
                let mut watch = tokio::spawn(async move {
                    if failed {
                        return Err(std::io::Error::other("wait failed").into());
                    }
                    Ok(ExitStatus {
                        code: Some(1),
                        signal: None,
                    })
                });
                while !watch.is_finished() {
                    tokio::task::yield_now().await;
                }
                let observed = if consumed {
                    Some((&mut watch).await)
                } else {
                    None
                };
                let result = startup_exit(&mut watch, observed).await.unwrap().unwrap();
                assert_eq!(result.is_err(), failed);
            }
        }
        let mut watch = tokio::spawn(std::future::pending());
        assert!(startup_exit(&mut watch, None).await.is_none());
        watch.abort();
        let observed = (&mut watch).await;
        assert!(startup_exit(&mut watch, Some(observed))
            .await
            .unwrap()
            .is_err());
    }

    struct StopDriver {
        stopped: AtomicUsize,
        after_stop: bool,
        prepare_calls: AtomicUsize,
        live_barrier: bool,
        handoff_target: std::sync::Mutex<Option<(String, u16)>>,
        freeze_calls: AtomicUsize,
        handoff: bool,
        /// prepare_snapshot errors after its (pretend) autosave pause
        fail_prepare: bool,
        resume_calls: AtomicUsize,
        spawn_gate: Option<Arc<tokio::sync::Notify>>,
    }

    impl StopDriver {
        fn new(live_barrier: bool) -> Self {
            Self {
                stopped: AtomicUsize::new(0),
                after_stop: false,
                prepare_calls: AtomicUsize::new(0),
                live_barrier,
                handoff_target: std::sync::Mutex::new(None),
                freeze_calls: AtomicUsize::new(0),
                handoff: false,
                fail_prepare: false,
                resume_calls: AtomicUsize::new(0),
                spawn_gate: None,
            }
        }
    }

    #[async_trait::async_trait]
    impl game_driver_api::GameDriver for StopDriver {
        fn id(&self) -> &'static str {
            "stopgame"
        }
        fn can_handoff(&self) -> bool {
            self.handoff
        }
        fn ports(&self, _c: &serde_json::Value) -> Vec<PortSpec> {
            vec![]
        }
        fn persistent_paths(&self, _c: &serde_json::Value) -> Vec<PathPattern> {
            vec![PathPattern::new("saves/")]
        }
        fn validate(&self, _c: &serde_json::Value) -> game_driver_api::Result<()> {
            Ok(())
        }
        fn join_code_from_log(&self, line: &str) -> Option<String> {
            let code = line.strip_prefix("join code ")?;
            (!code.is_empty() && code.bytes().all(|byte| byte.is_ascii_digit()))
                .then(|| code.to_string())
        }
        async fn prepare(&self, _c: &DriverContext<'_>) -> game_driver_api::Result<()> {
            Ok(())
        }
        async fn configure(&self, _c: &DriverContext<'_>) -> game_driver_api::Result<()> {
            Ok(())
        }
        fn process_spec(&self, _c: &DriverContext<'_>) -> game_driver_api::Result<ProcessSpec> {
            unreachable!()
        }
        async fn spawn(
            &self,
            ctx: &DriverContext<'_>,
            executor: &dyn Executor,
        ) -> game_driver_api::Result<Box<dyn ProcessHandle>> {
            if let Some(gate) = &self.spawn_gate {
                gate.notified().await;
            }
            executor
                .spawn(&ProcessSpec {
                    program: "unused".into(),
                    args: vec![],
                    env: vec![],
                    cwd: ctx.deployment_dir.into(),
                    stdin: false,
                })
                .await
        }
        async fn freeze(
            &self,
            _c: &DriverContext<'_>,
            _p: &dyn ProcessHandle,
        ) -> game_driver_api::Result<bool> {
            self.freeze_calls.fetch_add(1, Ordering::SeqCst);
            Ok(true)
        }
        async fn unfreeze(
            &self,
            _c: &DriverContext<'_>,
            _p: &dyn ProcessHandle,
        ) -> game_driver_api::Result<()> {
            Ok(())
        }
        async fn handoff(
            &self,
            _c: &DriverContext<'_>,
            _p: &dyn ProcessHandle,
            addr: &str,
            port: u16,
        ) -> game_driver_api::Result<bool> {
            *self.handoff_target.lock().unwrap() = Some((addr.to_string(), port));
            Ok(true)
        }
        async fn probe(
            &self,
            _c: &DriverContext<'_>,
            _p: &dyn ProcessHandle,
        ) -> game_driver_api::Result<GameHealth> {
            Ok(GameHealth::Healthy)
        }
        fn snapshot_after_stop(&self) -> bool {
            self.after_stop
        }
        async fn prepare_snapshot(
            &self,
            ctx: &DriverContext<'_>,
            _p: &dyn ProcessHandle,
        ) -> game_driver_api::Result<SnapshotBarrier> {
            self.prepare_calls.fetch_add(1, Ordering::SeqCst);
            if self.fail_prepare {
                return Err("save-all write failed".into());
            }
            if self.live_barrier {
                std::fs::create_dir_all(ctx.server_dir.join("saves"))?;
                std::fs::write(ctx.server_dir.join("saves/world.txt"), b"barrier")?;
            }
            Ok(if self.live_barrier {
                SnapshotBarrier::Live
            } else {
                SnapshotBarrier::RequiresStop
            })
        }
        async fn resume_after_snapshot(
            &self,
            _c: &DriverContext<'_>,
            _p: &dyn ProcessHandle,
        ) -> game_driver_api::Result<()> {
            self.resume_calls.fetch_add(1, Ordering::SeqCst);
            Ok(())
        }
        async fn graceful_stop(
            &self,
            ctx: &DriverContext<'_>,
            p: &dyn ProcessHandle,
        ) -> game_driver_api::Result<()> {
            self.stopped.fetch_add(1, Ordering::SeqCst);
            // the game writes its save as it exits
            std::fs::create_dir_all(ctx.server_dir.join("saves")).unwrap();
            std::fs::write(ctx.server_dir.join("saves/world.txt"), b"final").unwrap();
            let _ = p.terminate().await;
            Ok(())
        }
    }

    struct FakeProc {
        tx: broadcast::Sender<OutputLine>,
        exited: AtomicBool,
        exit_status: ExitStatus,
    }

    #[async_trait::async_trait]
    impl ProcessHandle for FakeProc {
        fn pid(&self) -> u32 {
            1
        }
        async fn write_stdin(&self, _l: &str) -> executor_api::Result<()> {
            Ok(())
        }
        fn output(&self) -> broadcast::Receiver<OutputLine> {
            self.tx.subscribe()
        }
        fn output_tail(&self, _n: usize) -> Vec<OutputLine> {
            vec![]
        }
        async fn wait(&self) -> executor_api::Result<ExitStatus> {
            for _ in 0..500 {
                if self.exited.load(Ordering::SeqCst) {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            Ok(self.exit_status.clone())
        }
        async fn terminate(&self) -> executor_api::Result<()> {
            self.exited.store(true, Ordering::SeqCst);
            Ok(())
        }
        async fn interrupt(&self) -> executor_api::Result<()> {
            self.terminate().await
        }
        async fn kill(&self) -> executor_api::Result<()> {
            self.exited.store(true, Ordering::SeqCst);
            Ok(())
        }
        fn resource_usage(&self) -> Option<ResourceUsage> {
            None
        }
    }

    /// Stub CP: accept HTTP posts and answer 200 {}.
    async fn stub_cp() -> String {
        let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = l.local_addr().unwrap().port();
        tokio::spawn(async move {
            loop {
                let Ok((mut s, _)) = l.accept().await else {
                    break;
                };
                tokio::spawn(async move {
                    let mut buf = vec![0u8; 64 * 1024];
                    let _ = s.read(&mut buf).await;
                    let _ = s
                        .write_all(
                            b"HTTP/1.1 200 OK\r\ncontent-length: 2\r\nconnection: close\r\n\r\n{}",
                        )
                        .await;
                });
            }
        });
        format!("http://127.0.0.1:{port}")
    }

    struct RecordingExecutor {
        calls: AtomicUsize,
        fence_during_spawn: Option<Arc<ExecCtl>>,
    }

    #[async_trait::async_trait]
    impl Executor for RecordingExecutor {
        fn kind(&self) -> &'static str {
            "test"
        }
        async fn spawn(&self, _: &ProcessSpec) -> executor_api::Result<Box<dyn ProcessHandle>> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            if let Some(ctl) = &self.fence_during_spawn {
                ctl.mark_fenced();
            }
            Ok(Box::new(FakeProc {
                tx: broadcast::channel(1).0,
                exited: AtomicBool::new(false),
                exit_status: ExitStatus::default(),
            }))
        }
    }

    #[tokio::test]
    async fn spawn_rechecks_authorization_after_driver_wait() {
        for cancellation in [
            "none",
            "fenced",
            "hard stop",
            "graceful stop",
            "expired lease",
            "during spawn",
        ] {
            let tmp = tempfile::tempdir().unwrap();
            let runtimes = runtimes::HttpRuntimes::new(tmp.path().join("runtimes"));
            let config = serde_json::json!({});
            let deployment = DeploymentSpec::parse(&config);
            let ctx = DriverContext {
                server_id: "s",
                server_dir: tmp.path(),
                deployment_dir: tmp.path(),
                runtimes: &runtimes,
                deployment: &deployment,
                config: &config,
                ports: &[],
                memory_mb: 0,
            };
            let gate = Arc::new(tokio::sync::Notify::new());
            let driver = StopDriver {
                spawn_gate: Some(gate.clone()),
                ..StopDriver::new(false)
            };
            let ctl = test_ctl();
            let executor = RecordingExecutor {
                calls: AtomicUsize::new(0),
                fence_during_spawn: (cancellation == "during spawn").then(|| ctl.clone()),
            };
            let (result, ()) = tokio::join!(spawn_live(&driver, &ctx, &executor, &ctl), async {
                tokio::task::yield_now().await;
                match cancellation {
                    "fenced" => ctl.mark_fenced(),
                    "hard stop" => ctl.request_stop(StopKind::Hard),
                    "graceful stop" => ctl.request_stop(StopKind::Graceful),
                    "expired lease" => *ctl.deadline.lock().unwrap() = Some(Instant::now()),
                    _ => (),
                }
                gate.notify_one();
            });
            if cancellation == "none" {
                let process = result.unwrap();
                process.kill().await.unwrap();
                assert_eq!(executor.calls.load(Ordering::SeqCst), 1);
            } else if cancellation == "during spawn" {
                assert!(result.is_err());
                assert_eq!(executor.calls.load(Ordering::SeqCst), 1);
                let process = ctl
                    .proc()
                    .expect("handle registered before final fence check");
                tokio::time::timeout(Duration::from_millis(5), process.wait())
                    .await
                    .unwrap()
                    .unwrap();
            } else {
                assert!(result.is_err(), "{cancellation}");
                assert_eq!(executor.calls.load(Ordering::SeqCst), 0, "{cancellation}");
                assert!(ctl.proc().is_none());
            }
        }
    }

    fn test_agent(cp: String, tmp: &Path) -> Agent {
        let (directives_digest, _) = tokio::sync::watch::channel(None);
        let (heartbeat_sequence, _) = tokio::sync::watch::channel(0);
        Agent {
            cfg: crate::config::Config {
                control_plane_url: cp.clone(),
                node_id: "node_t".into(),
                group_id: "grp_t".into(),
                control_plane_public_key: String::new(),
                anchor: false,
                mesh_bin: None,
                testgame_bin: None,
                loopback_prefix: None,
                lease_profile: "standard".into(),
                fence_margin_ms: Some(5000),
                shutdown_replication_timeout_s: 1,
                shutdown_handoff_timeout_s: 45,
                force_relay: false,
                mesh_listen_port: 0,
                update: Default::default(),
            },
            data_dir: tmp.to_path_buf(),
            key: ed25519_dalek::SigningKey::generate(&mut rand::rngs::OsRng),
            key_path: tmp.join("key.pem"),
            cp: cp_api::CpClient::new(&cp).unwrap(),
            store: Arc::new(Store::open(tmp.join("store")).unwrap()),
            drivers: game_driver_api::DriverRegistry::new(),
            runtimes: Arc::new(runtimes::HttpRuntimes::new(tmp.join("runtimes"))),
            executor: executor_native::NativeExecutor,
            mesh: Arc::new(crate::mesh_ctl::MeshCtl::new("ipc".into())),
            execs: Mutex::new(std::collections::HashMap::new()),
            state: Mutex::new(crate::state::ExecState::load(tmp)),
            pending_delete_acks: Mutex::new(std::collections::BTreeSet::new()),
            chunk_tracker: Arc::new(chunks::ServeTracker::default()),
            started_at_ms: 0,
            repl: Arc::new(Mutex::new(crate::agent::ReplQueue::default())),
            repl_notify: Arc::new(tokio::sync::Notify::new()),
            last_heartbeat_ok: Mutex::new(None),
            directives_digest,
            heartbeat_sequence,
            heartbeat_now: tokio::sync::Notify::new(),
            shutting_down: AtomicBool::new(false),
            prepare_locks: Mutex::new(Default::default()),
            standby_inflight: Mutex::new(Default::default()),
            cp_view: Mutex::new(Default::default()),
            update: Mutex::new(crate::update::UpdateState::default()),
        }
    }

    fn running_ctl(game_id: &str) -> Arc<ExecCtl> {
        let mut dir = test_ctl().dir.clone();
        dir.deployment = Some(cp_api::Deployment {
            id: "dep_t".into(),
            game_id: game_id.into(),
            spec: serde_json::json!({}),
        });
        let ctl = ExecCtl::new(dir);
        ctl.set_phase(Phase::Running);
        ctl
    }

    fn stop_requested(ctl: &ExecCtl) -> bool {
        ctl.stop_rx().borrow().is_some()
    }

    #[test]
    fn drain_step_waits_for_ack_then_flip() {
        let ctl = test_ctl();
        let t0 = Instant::now();
        let (ack_by, handoff_by) = (t0 + Duration::from_secs(10), t0 + Duration::from_secs(45));
        assert_eq!(
            drain_step(&ctl, false, t0, ack_by, handoff_by),
            DrainStep::Wait
        );
        // no ack in time: fall back
        assert_eq!(
            drain_step(&ctl, false, ack_by, ack_by, handoff_by),
            DrainStep::Stop
        );
        // acked (frozen): keeps waiting past the ack window
        ctl.set_want_frozen(true);
        assert_eq!(
            drain_step(&ctl, true, ack_by, ack_by, handoff_by),
            DrainStep::Wait
        );
        // overran the handoff budget
        assert_eq!(
            drain_step(&ctl, true, handoff_by, ack_by, handoff_by),
            DrainStep::Stop
        );
        // flip: control loop sets the handoff stop reason before unfreezing
        ctl.set_stop_reason(Some("handoff".into()));
        ctl.set_want_frozen(false);
        assert_eq!(
            drain_step(&ctl, true, t0, ack_by, handoff_by),
            DrainStep::Done
        );
    }

    #[test]
    fn drain_step_stops_when_migration_aborts() {
        let ctl = test_ctl();
        let t0 = Instant::now();
        let later = t0 + Duration::from_secs(60);
        // frozen, then unfrozen without a handoff stop = CP aborted the move
        assert_eq!(drain_step(&ctl, true, t0, later, later), DrainStep::Stop);
        ctl.mark_finished();
        assert_eq!(drain_step(&ctl, true, t0, later, later), DrainStep::Done);
    }

    #[tokio::test]
    async fn shutdown_drain_stops_servers_without_handoff_at_once() {
        let tmp = tempfile::tempdir().unwrap();
        let mut agent = test_agent(stub_cp().await, tmp.path());
        agent.drivers.register(Box::new(StopDriver::new(true)));
        let ctl = running_ctl("stopgame");
        agent
            .execs
            .lock()
            .unwrap()
            .insert("exec_t".into(), ctl.clone());
        let finisher = {
            let ctl = ctl.clone();
            tokio::spawn(async move {
                while !stop_requested(&ctl) {
                    tokio::time::sleep(Duration::from_millis(20)).await;
                }
                ctl.mark_finished();
            })
        };
        let t0 = Instant::now();
        drain_for_shutdown(&agent).await;
        finisher.await.unwrap();
        assert!(
            t0.elapsed() < Duration::from_secs(2),
            "non-handoff server waited"
        );
    }

    #[tokio::test]
    async fn shutdown_drain_waits_for_handoff_flip() {
        let tmp = tempfile::tempdir().unwrap();
        let mut agent = test_agent(stub_cp().await, tmp.path());
        let mut driver = StopDriver::new(true);
        driver.handoff = true;
        agent.drivers.register(Box::new(driver));
        let ctl = running_ctl("stopgame");
        agent
            .execs
            .lock()
            .unwrap()
            .insert("exec_t".into(), ctl.clone());
        // the CP freezes (ack), then flips: handoff stop reason, unfreeze,
        // stop — the drain must not stop the server itself in between
        let cp = {
            let ctl = ctl.clone();
            tokio::spawn(async move {
                tokio::time::sleep(Duration::from_millis(300)).await;
                ctl.set_want_frozen(true);
                tokio::time::sleep(Duration::from_millis(600)).await;
                assert!(!stop_requested(&ctl), "drain stopped a server mid-handoff");
                ctl.set_stop_reason(Some("handoff".into()));
                ctl.set_want_frozen(false);
                ctl.request_stop(StopKind::Graceful);
                tokio::time::sleep(Duration::from_millis(100)).await;
                ctl.mark_finished();
            })
        };
        drain_for_shutdown(&agent).await;
        cp.await.unwrap();
        assert_eq!(ctl.stop_reason().as_deref(), Some("handoff"));
    }

    #[tokio::test]
    async fn shutdown_drain_falls_back_when_migration_aborts() {
        let tmp = tempfile::tempdir().unwrap();
        let mut agent = test_agent(stub_cp().await, tmp.path());
        let mut driver = StopDriver::new(true);
        driver.handoff = true;
        agent.drivers.register(Box::new(driver));
        let ctl = running_ctl("stopgame");
        agent
            .execs
            .lock()
            .unwrap()
            .insert("exec_t".into(), ctl.clone());
        let cp = {
            let ctl = ctl.clone();
            tokio::spawn(async move {
                ctl.set_want_frozen(true);
                tokio::time::sleep(Duration::from_millis(300)).await;
                ctl.set_want_frozen(false); // aborted: no handoff stop
                while !stop_requested(&ctl) {
                    tokio::time::sleep(Duration::from_millis(20)).await;
                }
                ctl.mark_finished();
            })
        };
        let t0 = Instant::now();
        drain_for_shutdown(&agent).await;
        cp.await.unwrap();
        assert!(
            t0.elapsed() < Duration::from_secs(3),
            "abort was not noticed"
        );
        assert_eq!(ctl.stop_reason(), None);
    }

    #[tokio::test]
    async fn standby_pre_installs_each_deployment_once() {
        let tmp = tempfile::tempdir().unwrap();
        let mut agent = test_agent(stub_cp().await, tmp.path());
        agent.drivers.register(Box::new(StopDriver::new(true)));
        let agent = Arc::new(agent);
        let sd = cp_api::StandbyDirective {
            server_id: "srv_s".into(),
            deployment: cp_api::Deployment {
                id: "dep_s".into(),
                game_id: "stopgame".into(),
                spec: serde_json::json!({}),
            },
            config: Some(serde_json::json!({"version": "1.21.4"})),
        };
        crate::standby::apply(&agent, &[sd.clone(), sd.clone()]);
        crate::standby::apply(&agent, &[sd]);
        let deadline = Instant::now() + Duration::from_secs(5);
        while crate::standby::prepared_deployments(tmp.path()).is_empty() {
            assert!(Instant::now() < deadline, "standby never prepared dep_s");
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        assert_eq!(
            crate::standby::prepared_deployments(tmp.path()),
            vec!["dep_s"]
        );
        assert_eq!(agent.standby_inflight.lock().unwrap().len(), 1);
    }

    fn test_ctl() -> Arc<ExecCtl> {
        ExecCtl::new(ExecutionDirective {
            execution_id: "exec_t".into(),
            server_id: "srv_t".into(),
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
            latest_safe_save_at_unix_ms: None,
            latest_safe_save_on: vec![],
            latest_save_at_unix_ms: None,
            latest_save_on: vec![],
            freeze: None,
        })
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn join_code_watcher_updates_execution_state() {
        let tmp = tempfile::tempdir().unwrap();
        let mut agent = test_agent(stub_cp().await, tmp.path());
        agent.drivers.register(Box::new(StopDriver::new(true)));
        let agent = Arc::new(agent);
        let ctl = test_ctl();
        agent
            .execs
            .lock()
            .unwrap()
            .insert(ctl.dir.execution_id.clone(), ctl.clone());
        let (tx, _rx) = broadcast::channel(8);
        let proc: Arc<dyn ProcessHandle> = Arc::new(FakeProc {
            tx: tx.clone(),
            exited: AtomicBool::new(false),
            exit_status: ExitStatus {
                code: Some(0),
                signal: None,
            },
        });
        spawn_join_code_watcher(&agent, &ctl, proc, "stopgame".into());
        tx.send(OutputLine {
            at_unix_ms: 1,
            stream: OutputStream::Stdout,
            line: Arc::from("join code 124841"),
        })
        .unwrap();

        tokio::time::timeout(Duration::from_secs(1), async {
            while ctl.join_code().as_deref() != Some("124841") {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("join-code watcher did not update the execution");
        assert_eq!(ctl.join_code().as_deref(), Some("124841"));
        assert_eq!(agent.exec_reports()[0].join_code.as_deref(), Some("124841"));
        ctl.clear_join_code();
        assert_eq!(ctl.join_code(), None);
        assert!(ctl.set_join_code("589208".into()));
        assert_eq!(ctl.join_code().as_deref(), Some("589208"));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn requires_stop_snapshots_files_written_at_exit() {
        snapshots_capture_barrier_or_exit_files(false).await;
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn live_snapshot_captures_barrier_files_before_stop() {
        snapshots_capture_barrier_or_exit_files(true).await;
    }

    async fn snapshots_capture_barrier_or_exit_files(live: bool) {
        let tmp = tempfile::tempdir().unwrap();
        let agent = test_agent(stub_cp().await, tmp.path());
        let ctl = test_ctl();
        let (tx, _rx) = broadcast::channel(8);
        let proc: Arc<dyn ProcessHandle> = Arc::new(FakeProc {
            tx,
            exited: AtomicBool::new(false),
            exit_status: ExitStatus {
                code: Some(0),
                signal: None,
            },
        });
        ctl.set_proc(proc.clone());
        let server_dir = tmp.path().join("servers").join("srv_t");
        let deployment_dir = tmp.path().join("deployments").join("dep_t");
        std::fs::create_dir_all(&server_dir).unwrap();
        std::fs::create_dir_all(&deployment_dir).unwrap();
        let deployment = DeploymentSpec::parse(&serde_json::json!({}));
        let config = serde_json::json!({});
        let bindings: Vec<PortBinding> = vec![];
        let ctx = DriverContext {
            server_id: "srv_t",
            server_dir: &server_dir,
            deployment_dir: &deployment_dir,
            runtimes: &*agent.runtimes,
            deployment: &deployment,
            config: &config,
            ports: &bindings,
            memory_mb: 0,
        };
        let driver = StopDriver::new(live);
        graceful_stop_and_snapshot(&agent, &ctl, &driver, &ctx, &proc).await;
        assert_eq!(driver.stopped.load(Ordering::SeqCst), 1);
        assert_eq!(driver.prepare_calls.load(Ordering::SeqCst), 1);
        // the save written at exit must be in the store's snapshot
        let snaps = agent.store.list_snapshots().unwrap();
        assert_eq!(snaps.len(), 1, "expected one final snapshot");
        let m = agent.store.manifest(&snaps[0]).unwrap();
        let files: Vec<String> = m.files.iter().map(|f| f.path.clone()).collect();
        assert!(
            files.iter().any(|p| p == "saves/world.txt"),
            "manifest missing exit-time save: {files:?}"
        );
        let restored = tmp.path().join("restored");
        agent
            .store
            .restore(
                &snaps[0],
                &restored,
                &[snapshot_store::PathPattern::new("saves/")],
            )
            .unwrap();
        assert_eq!(
            std::fs::read(restored.join("saves/world.txt")).unwrap(),
            if live {
                b"barrier".as_slice()
            } else {
                b"final".as_slice()
            }
        );
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn snapshot_after_stop_skips_barrier_and_captures_exit_save() {
        let tmp = tempfile::tempdir().unwrap();
        let agent = test_agent(stub_cp().await, tmp.path());
        let ctl = test_ctl();
        let (tx, _rx) = broadcast::channel(8);
        let proc: Arc<dyn ProcessHandle> = Arc::new(FakeProc {
            tx,
            exited: AtomicBool::new(false),
            exit_status: ExitStatus {
                code: Some(0),
                signal: None,
            },
        });
        ctl.set_proc(proc.clone());
        let server_dir = tmp.path().join("servers").join("srv_t");
        let deployment_dir = tmp.path().join("deployments").join("dep_t");
        std::fs::create_dir_all(&server_dir).unwrap();
        std::fs::create_dir_all(&deployment_dir).unwrap();
        let deployment = DeploymentSpec::parse(&serde_json::json!({}));
        let config = serde_json::json!({});
        let bindings: Vec<PortBinding> = vec![];
        let ctx = DriverContext {
            server_id: "srv_t",
            server_dir: &server_dir,
            deployment_dir: &deployment_dir,
            runtimes: &*agent.runtimes,
            deployment: &deployment,
            config: &config,
            ports: &bindings,
            memory_mb: 0,
        };
        let driver = {
            let mut d = StopDriver::new(true);
            d.after_stop = true;
            d
        };
        graceful_stop_and_snapshot(&agent, &ctl, &driver, &ctx, &proc).await;
        assert_eq!(driver.stopped.load(Ordering::SeqCst), 1);
        assert_eq!(driver.prepare_calls.load(Ordering::SeqCst), 0);

        let snaps = agent.store.list_snapshots().unwrap();
        assert_eq!(snaps.len(), 1, "expected one final snapshot");
        let manifest = agent.store.manifest(&snaps[0]).unwrap();
        let files: Vec<String> = manifest
            .files
            .iter()
            .map(|file| file.path.clone())
            .collect();
        assert!(
            files.iter().any(|path| path == "saves/world.txt"),
            "manifest missing exit-time save: {files:?}"
        );
        let restored = tmp.path().join("restored");
        agent
            .store
            .restore(
                &snaps[0],
                &restored,
                &[snapshot_store::PathPattern::new("saves/")],
            )
            .unwrap();
        assert_eq!(
            std::fs::read(restored.join("saves/world.txt")).unwrap(),
            b"final"
        );
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn post_stop_snapshot_skips_non_clean_exit() {
        for exit_status in [
            ExitStatus {
                code: Some(1),
                signal: None,
            },
            ExitStatus {
                code: None,
                signal: Some(9),
            },
        ] {
            let tmp = tempfile::tempdir().unwrap();
            let agent = test_agent(stub_cp().await, tmp.path());
            let ctl = test_ctl();
            let (tx, _rx) = broadcast::channel(8);
            let proc: Arc<dyn ProcessHandle> = Arc::new(FakeProc {
                tx,
                exited: AtomicBool::new(false),
                exit_status,
            });
            ctl.set_proc(proc.clone());
            let server_dir = tmp.path().join("servers").join("srv_t");
            let deployment_dir = tmp.path().join("deployments").join("dep_t");
            std::fs::create_dir_all(&server_dir).unwrap();
            std::fs::create_dir_all(&deployment_dir).unwrap();
            let deployment = DeploymentSpec::parse(&serde_json::json!({}));
            let config = serde_json::json!({});
            let bindings: Vec<PortBinding> = vec![];
            let ctx = DriverContext {
                server_id: "srv_t",
                server_dir: &server_dir,
                deployment_dir: &deployment_dir,
                runtimes: &*agent.runtimes,
                deployment: &deployment,
                config: &config,
                ports: &bindings,
                memory_mb: 0,
            };
            let driver = {
                let mut d = StopDriver::new(true);
                d.after_stop = true;
                d
            };
            graceful_stop_and_snapshot(&agent, &ctl, &driver, &ctx, &proc).await;
            assert!(agent.store.list_snapshots().unwrap().is_empty());
            assert_eq!(driver.prepare_calls.load(Ordering::SeqCst), 0);
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn handoff_issued_only_for_warm_migration_stop() {
        let tmp = tempfile::tempdir().unwrap();
        let agent = test_agent(stub_cp().await, tmp.path());
        let (tx, _rx) = broadcast::channel(8);
        let proc: Arc<dyn ProcessHandle> = Arc::new(FakeProc {
            tx,
            exited: AtomicBool::new(false),
            exit_status: ExitStatus {
                code: Some(0),
                signal: None,
            },
        });
        let server_dir = tmp.path().join("servers").join("srv_t");
        let deployment_dir = tmp.path().join("deployments").join("dep_t");
        std::fs::create_dir_all(&server_dir).unwrap();
        std::fs::create_dir_all(&deployment_dir).unwrap();
        let deployment = DeploymentSpec::parse(&serde_json::json!({}));
        let config = serde_json::json!({});
        let bindings: Vec<PortBinding> = vec![];
        let ctx = DriverContext {
            server_id: "srv_t",
            server_dir: &server_dir,
            deployment_dir: &deployment_dir,
            runtimes: &*agent.runtimes,
            deployment: &deployment,
            config: &config,
            ports: &bindings,
            memory_mb: 0,
        };
        let mk_ctl = |stop_reason: Option<String>| {
            let d = test_ctl();
            let mut dir = ExecutionDirective { ..d.dir.clone() };
            dir.stop_reason = stop_reason;
            dir.service = Some(cp_api::Service {
                service_id: "svc_t".into(),
                loopback_ip: "127.77.0.42".into(),
                ports: vec![
                    cp_api::GamePort {
                        port: 27002,
                        protocol: "udp".into(),
                    },
                    cp_api::GamePort {
                        port: 27001,
                        protocol: "tcp".into(),
                    },
                ],
            });
            ExecCtl::new(dir)
        };
        let driver = StopDriver::new(false);

        // ordinary stop: no handoff
        issue_handoff(&mk_ctl(None), &driver, &ctx, &proc).await;
        assert!(driver.handoff_target.lock().unwrap().is_none());

        // warm-migration stop: handoff targets the service's TCP port
        issue_handoff(&mk_ctl(Some("handoff".into())), &driver, &ctx, &proc).await;
        assert_eq!(
            *driver.handoff_target.lock().unwrap(),
            Some(("127.77.0.42".to_string(), 27001))
        );
    }

    /// Every do_snapshot exit after prepare_snapshot resumes the game — a
    /// driver that paused autosave must never be left paused.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn do_snapshot_always_resumes_after_barrier() {
        // (live barrier, prepare fails, CP reachable) → expect success
        for (live, fail_prepare, cp_up, ok) in [
            (false, false, true, false), // RequiresStop
            (true, true, true, false),   // barrier errored after pausing
            (true, false, false, false), // snapshot taken, upload failed
            (true, false, true, true),   // happy path
        ] {
            let tmp = tempfile::tempdir().unwrap();
            // nothing listens on port 1: the snapshot POST fails at once
            let cp = if cp_up {
                stub_cp().await
            } else {
                "http://127.0.0.1:1".to_string()
            };
            let agent = test_agent(cp, tmp.path());
            let ctl = test_ctl();
            let (tx, _rx) = broadcast::channel(8);
            let proc: Arc<dyn ProcessHandle> = Arc::new(FakeProc {
                tx,
                exited: AtomicBool::new(false),
                exit_status: ExitStatus {
                    code: Some(0),
                    signal: None,
                },
            });
            let server_dir = tmp.path().join("servers").join("srv_t");
            let deployment_dir = tmp.path().join("deployments").join("dep_t");
            std::fs::create_dir_all(server_dir.join("saves")).unwrap();
            std::fs::write(server_dir.join("saves/world.txt"), b"live").unwrap();
            std::fs::create_dir_all(&deployment_dir).unwrap();
            let deployment = DeploymentSpec::parse(&serde_json::json!({}));
            let config = serde_json::json!({});
            let bindings: Vec<PortBinding> = vec![];
            let ctx = DriverContext {
                server_id: "srv_t",
                server_dir: &server_dir,
                deployment_dir: &deployment_dir,
                runtimes: &*agent.runtimes,
                deployment: &deployment,
                config: &config,
                ports: &bindings,
                memory_mb: 0,
            };
            let mut driver = StopDriver::new(live);
            driver.fail_prepare = fail_prepare;
            let res = do_snapshot(&agent, &ctl, &driver, &ctx, &proc, "scheduled", None).await;
            assert_eq!(
                res.is_ok(),
                ok,
                "live={live} fail_prepare={fail_prepare} cp_up={cp_up}"
            );
            assert_eq!(driver.prepare_calls.load(Ordering::SeqCst), 1);
            assert_eq!(
                driver.resume_calls.load(Ordering::SeqCst),
                1,
                "live={live} fail_prepare={fail_prepare} cp_up={cp_up}: not resumed"
            );
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn graceful_stop_resumes_after_requires_stop_barrier() {
        let tmp = tempfile::tempdir().unwrap();
        let agent = test_agent(stub_cp().await, tmp.path());
        let ctl = test_ctl();
        let (tx, _rx) = broadcast::channel(8);
        let proc: Arc<dyn ProcessHandle> = Arc::new(FakeProc {
            tx,
            exited: AtomicBool::new(false),
            exit_status: ExitStatus {
                code: Some(0),
                signal: None,
            },
        });
        let server_dir = tmp.path().join("servers").join("srv_t");
        let deployment_dir = tmp.path().join("deployments").join("dep_t");
        std::fs::create_dir_all(&server_dir).unwrap();
        std::fs::create_dir_all(&deployment_dir).unwrap();
        let deployment = DeploymentSpec::parse(&serde_json::json!({}));
        let config = serde_json::json!({});
        let bindings: Vec<PortBinding> = vec![];
        let ctx = DriverContext {
            server_id: "srv_t",
            server_dir: &server_dir,
            deployment_dir: &deployment_dir,
            runtimes: &*agent.runtimes,
            deployment: &deployment,
            config: &config,
            ports: &bindings,
            memory_mb: 0,
        };
        let driver = StopDriver::new(false);
        graceful_stop_and_snapshot(&agent, &ctl, &driver, &ctx, &proc).await;
        assert_eq!(driver.prepare_calls.load(Ordering::SeqCst), 1);
        assert_eq!(driver.resume_calls.load(Ordering::SeqCst), 1);
    }
}
