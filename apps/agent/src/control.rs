//! Control loop: heartbeat = reconciliation. Reports observed state, applies
//! the complete desired state (peers/routes/hosted/relays/executions/
//! replication/deletions), computes fencing deadlines, and drives the
//! fencing watchdog (checked every 250 ms on a monotonic clock).

use std::collections::{BTreeSet, HashSet};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use anyhow::Result;
use base64::Engine;
use tracing::{debug, info, warn};

use crate::exec::{self, ExecCtl, Phase, StopKind};
use crate::Agent;

/// One heartbeat iteration: returns the interval to wait before the next.
pub async fn heartbeat_once(agent: &Arc<Agent>) -> Result<i64> {
    // capture send instant BEFORE the request — fencing math depends on it
    let sent = Instant::now();
    let deleted_snapshots = pending_delete_ack_ids(&agent.pending_delete_acks);
    let hb = cp_api::AgentHeartbeat {
        agent_version: Some(env!("CARGO_PKG_VERSION").into()),
        capabilities: Some(crate::sysinfo::capabilities(
            &agent.data_dir,
            &agent.drivers.ids(),
            agent.started_at_ms,
            agent
                .shutting_down
                .load(std::sync::atomic::Ordering::SeqCst),
        )),
        mesh: Some(mesh_report(agent).await),
        executions: Some(agent.exec_reports()),
        snapshots_stored_bytes: Some(agent.snapshots_stored_bytes()),
        deleted_snapshots: Some(deleted_snapshots.clone()),
    };
    let d: cp_api::AgentDirectives = agent
        .cp
        .json("POST", "/v1/agent/heartbeat", Some(&hb))
        .await?;
    acknowledge_delete_acks(&agent.pending_delete_acks, &deleted_snapshots);
    apply(agent, &d, sent).await?;
    let elapsed_ms = sent.elapsed().as_millis() as u64;
    if elapsed_ms > 1000 {
        warn!(elapsed_ms, "slow heartbeat");
    }
    *agent.last_heartbeat_ok.lock().unwrap() = Some(Instant::now());
    agent.cp_view.lock().unwrap().last_contact_unix_ms = crate::now_ms();
    Ok(d.heartbeat_interval_ms)
}

async fn mesh_report(agent: &Agent) -> cp_api::MeshReport {
    let mut r = cp_api::MeshReport::default();
    if let Some(st) = agent.mesh.status().await {
        r.observed_endpoints = Some(st.observed_endpoints);
        r.relay_ids = Some(
            st.relays
                .iter()
                .filter(|x| x.registered)
                .map(|x| x.relay_id.clone())
                .collect(),
        );
        let port = st
            .listen_addrs
            .first()
            .and_then(|a| a.rsplit(':').next()?.parse::<i64>().ok());
        r.listen_port = port;
        r.local_endpoints = Some(st.listen_addrs);
    }
    let peers = agent.mesh.list_peers().await;
    r.peers = Some(
        peers
            .iter()
            .map(|p| cp_api::MeshPeerReport {
                node_id: Some(p.node_id.clone()),
                path: Some(
                    match mesh_ipc::pb::PathKind::try_from(p.path) {
                        Ok(mesh_ipc::pb::PathKind::Direct) => "direct",
                        Ok(mesh_ipc::pb::PathKind::Relayed) => "relayed",
                        _ => "none",
                    }
                    .into(),
                ),
                rtt_us: Some(p.rtt_us),
            })
            .collect(),
    );
    r
}

fn pending_delete_ack_ids(pending: &Mutex<BTreeSet<String>>) -> Vec<String> {
    pending.lock().unwrap().iter().cloned().collect()
}

fn acknowledge_delete_acks(pending: &Mutex<BTreeSet<String>>, sent: &[String]) {
    let mut pending = pending.lock().unwrap();
    for id in sent {
        pending.remove(id);
    }
}

/// The CP stamps server_time about halfway through the heartbeat round trip.
fn cp_clock_offset_ms(server_time_ms: i64, now_ms: i64, round_trip: Duration) -> i64 {
    server_time_ms - (now_ms - round_trip.as_millis() as i64 / 2)
}

/// Apply the directives: mesh state, execution diff, replication, deletes.
async fn apply(agent: &Arc<Agent>, d: &cp_api::AgentDirectives, sent: Instant) -> Result<()> {
    {
        let mut v = agent.cp_view.lock().unwrap();
        v.node_name = d.node.name.clone();
        v.group_name = d.node.group_name.clone().unwrap_or_default();
        v.heartbeat_interval_ms = d.heartbeat_interval_ms;
        v.cp_clock_offset_ms =
            cp_clock_offset_ms(d.server_time_unix_ms, crate::now_ms(), sent.elapsed());
        v.latest_safe_save = d
            .executions
            .iter()
            .filter_map(|e| Some((e.server_id.clone(), e.latest_safe_save_at_unix_ms?)))
            .collect();
    }
    // ---- mesh: configure + peers + routes ----
    agent
        .mesh
        .configure(mesh_ipc::pb::ConfigureRequest {
            node_id: agent.cfg.node_id.clone(),
            identity_key_path: agent.key_path.display().to_string(),
            listen_port: agent.cfg.mesh_listen_port,
            relays: d
                .relays
                .iter()
                .map(|r| mesh_ipc::pb::Relay {
                    relay_id: r.relay_id.clone(),
                    addr: r.addr.clone(),
                    token: r.token.clone(),
                })
                .collect(),
            force_relay: agent.cfg.force_relay,
        })
        .await?;
    agent
        .mesh
        .set_peers(
            d.peers
                .iter()
                .filter_map(|p| {
                    let pk = base64::engine::general_purpose::STANDARD
                        .decode(&p.public_key)
                        .ok()?;
                    Some(mesh_ipc::pb::Peer {
                        node_id: p.node_id.clone(),
                        public_key: pk,
                        endpoints: p.endpoints.clone(),
                        relay_ids: p.relay_ids.clone(),
                    })
                })
                .collect(),
        )
        .await?;
    agent
        .mesh
        .set_routes(
            d.routes
                .iter()
                .map(|r| mesh_ipc::pb::ServiceRoute {
                    service_id: r.service_id.clone(),
                    loopback_ip: agent.cfg.translate_loopback(&r.loopback_ip),
                    ports: r
                        .ports
                        .iter()
                        .map(|p| mesh_ipc::pb::PortSpec {
                            port: p.port.max(0) as u32,
                            protocol: proto_for(&p.protocol),
                        })
                        .collect(),
                    host_node_id: r.host_node_id.clone(),
                    epoch: r.epoch.max(0) as u64,
                })
                .collect(),
        )
        .await?;

    // ---- executions ----
    let margin = Duration::from_millis(agent.cfg.fence_margin_ms.max(0) as u64);
    let mut wanted: HashSet<String> = HashSet::new();
    for e in &d.executions {
        wanted.insert(e.execution_id.clone());
        // fencing deadline: sent_instant + (lease − server_time) − margin
        let window_ms = e.lease_expires_at_unix_ms - d.server_time_unix_ms;
        let window = Duration::from_millis(window_ms.max(0) as u64);
        // saturating: a nearly-expired lease still yields a deadline <= sent
        let deadline = sent + window.saturating_sub(margin);
        let existing = agent.execs.lock().unwrap().get(&e.execution_id).cloned();
        match existing {
            Some(ctl) => {
                *ctl.deadline.lock().unwrap() = Some(deadline);
                for r in &e.snapshot_requests {
                    ctl.queue_snapshot(r.request_id.clone(), r.reason.clone());
                }
                if e.action == "stop" && !ctl.is_finished() {
                    ctl.request_stop(StopKind::Graceful);
                }
            }
            None => {
                if e.action == "stop" {
                    // already gone locally or never started: nothing to stop;
                    // report stopped so the CP can end it
                    report_absent_stopped(agent, e).await;
                    continue;
                }
                if agent
                    .shutting_down
                    .load(std::sync::atomic::Ordering::SeqCst)
                {
                    info!(exec = %e.execution_id, "shutting down; refusing new execution");
                    report_absent_stopped_msg(agent, e, "node shutting down").await;
                    continue;
                }
                let ctl = ExecCtl::new(e.clone());
                *ctl.deadline.lock().unwrap() = Some(deadline);
                for r in &e.snapshot_requests {
                    ctl.queue_snapshot(r.request_id.clone(), r.reason.clone());
                }
                agent
                    .execs
                    .lock()
                    .unwrap()
                    .insert(e.execution_id.clone(), ctl.clone());
                agent.note_exec_pgid(&e.execution_id, 0);
                exec::spawn_supervisor(agent.clone(), ctl);
            }
        }
    }
    // executions we run that are absent from directives → stop now (I2)
    let stale: Vec<Arc<ExecCtl>> = {
        let g = agent.execs.lock().unwrap();
        g.iter()
            .filter(|(id, _)| !wanted.contains(*id))
            .map(|(_, c)| c.clone())
            .collect()
    };
    for c in stale {
        warn!(exec = %c.dir.execution_id, "execution absent from directives; stopping");
        c.request_stop(StopKind::Hard);
    }

    agent.sync_hosted_services().await;

    // ---- replication + deletes ----
    for t in &d.replication_tasks {
        agent.enqueue_replication(t.clone());
    }
    for id in &d.delete_snapshots {
        if let Some(sid) = snapshot_store::SnapshotId::parse(id) {
            match agent.store.delete_snapshot(&sid) {
                Ok(true) => {
                    agent.pending_delete_acks.lock().unwrap().insert(id.clone());
                    info!(snap = %id, "deleted snapshot per CP directive");
                }
                Ok(false) => {
                    agent.pending_delete_acks.lock().unwrap().insert(id.clone());
                    debug!(snap = %id, "snapshot already absent per CP directive");
                }
                Err(error) => {
                    warn!(snap = %id, %error, "failed to delete snapshot per CP directive");
                }
            }
        } else {
            warn!(snap = %id, "invalid snapshot ID in delete directive");
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::{acknowledge_delete_acks, cp_clock_offset_ms, pending_delete_ack_ids};
    use crate::agent::CpView;
    use std::collections::BTreeSet;
    use std::sync::Mutex;
    use std::time::Duration;

    #[test]
    fn cp_timestamps_move_to_this_pcs_clock() {
        // CP 30 s ahead; heartbeat sent at PC 1_000_000, answered 200 ms later
        let offset = cp_clock_offset_ms(1_030_100, 1_000_200, Duration::from_millis(200));
        assert_eq!(offset, 30_000);
        let v = CpView {
            cp_clock_offset_ms: offset,
            ..Default::default()
        };
        assert_eq!(v.to_local_ms(1_025_000), 995_000);
        assert_eq!(v.to_local_ms(0), 0);
    }

    #[test]
    fn delete_acknowledgement_removes_only_sent_ids() {
        let pending = Mutex::new(BTreeSet::from(["snap_a".to_string(), "snap_b".to_string()]));
        let sent = pending_delete_ack_ids(&pending);
        assert_eq!(sent, vec!["snap_a".to_string(), "snap_b".to_string()]);

        pending.lock().unwrap().insert("snap_c".into());
        acknowledge_delete_acks(&pending, &sent);

        assert_eq!(pending_delete_ack_ids(&pending), vec!["snap_c".to_string()]);
    }
}

fn proto_for(s: &str) -> i32 {
    match s {
        "tcp" => mesh_ipc::pb::Protocol::Tcp as i32,
        "udp" => mesh_ipc::pb::Protocol::Udp as i32,
        _ => 0,
    }
}

async fn report_absent_stopped(agent: &Agent, e: &cp_api::ExecutionDirective) {
    report_absent_stopped_msg(agent, e, "not running locally").await
}

async fn report_absent_stopped_msg(agent: &Agent, e: &cp_api::ExecutionDirective, message: &str) {
    let body = cp_api::ExecutionStatusUpdate {
        server_id: e.server_id.clone(),
        epoch: e.epoch,
        state: "stopped".into(),
        health: None,
        message: Some(message.to_string()),
        join_code: None,
    };
    let path = format!("/v1/agent/executions/{}/status", e.execution_id);
    let _ = agent
        .cp
        .json::<_, serde_json::Value>("POST", &path, Some(&body))
        .await;
}

/// The control loop: heartbeat with 1 s → 5 s backoff on failures; fencing
/// never blocks on network.
pub async fn control_loop(agent: Arc<Agent>, mut stop: tokio::sync::watch::Receiver<bool>) {
    let mut interval = Duration::from_millis(1000);
    loop {
        match heartbeat_once(&agent).await {
            Ok(ms) => interval = Duration::from_millis(ms.clamp(100, 60_000) as u64),
            Err(e) => {
                warn!(error = %e, "heartbeat failed");
                interval = (interval * 2).min(Duration::from_secs(5));
            }
        }
        tokio::select! {
            _ = tokio::time::sleep(interval) => {},
            _ = stop.changed() => return,
        }
    }
}

/// Fencing watchdog: deadline expiry ⇒ graceful stop bounded to
/// fence_margin/2, then kill; mark fenced; drop hosted services; never upload.
pub async fn fence_watchdog(agent: Arc<Agent>, mut stop: tokio::sync::watch::Receiver<bool>) {
    let mut tick = tokio::time::interval(Duration::from_millis(250));
    loop {
        tokio::select! {
            _ = tick.tick() => {},
            _ = stop.changed() => return,
        }
        let now = Instant::now();
        let expired: Vec<Arc<ExecCtl>> = agent
            .execs
            .lock()
            .unwrap()
            .values()
            .filter(|c| {
                !c.is_fenced()
                    && !c.is_finished()
                    && matches!(
                        c.phase(),
                        Phase::Running
                            | Phase::Starting
                            | Phase::Stopping
                            | Phase::Restoring
                            | Phase::Preparing
                    )
                    && c.deadline
                        .lock()
                        .unwrap()
                        .map(|d| now >= d)
                        .unwrap_or(false)
            })
            .cloned()
            .collect();
        for c in expired {
            let since_last_heartbeat_ms = agent
                .last_heartbeat_ok
                .lock()
                .unwrap()
                .map(|t| now.duration_since(t).as_millis() as u64);
            warn!(exec = %c.dir.execution_id, since_last_heartbeat_ms, "lease deadline expired — fencing");
            c.mark_fenced();
            // graceful bounded to fence_margin/2, then kill
            let a = agent.clone();
            let c = c.clone();
            tokio::spawn(async move {
                let bound = Duration::from_millis((a.cfg.fence_margin_ms.max(0) as u64) / 2);
                if let Some(p) = c.proc() {
                    if bound > Duration::ZERO {
                        let _ = p.terminate().await;
                        let _ = tokio::time::timeout(bound, p.wait()).await;
                    }
                    let _ = p.kill().await;
                }
                c.mark_finished();
                c.request_stop(StopKind::Hard);
                a.state
                    .lock()
                    .unwrap()
                    .upsert(crate::state::ExecRecord::new(
                        c.dir.execution_id.clone(),
                        c.dir.server_id.clone(),
                        c.dir.epoch,
                        c.pgid(),
                        true,
                    ));
                a.sync_hosted_services().await;
            });
        }
    }
}
