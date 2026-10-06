//! The `Agent` — shared state for the control loop, supervisors, chunk server
//! and the replication worker.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::AtomicBool;
use std::sync::{Arc, Mutex};

use ed25519_dalek::SigningKey;
use game_driver_api::DriverRegistry;
use snapshot_store::Store;

use crate::chunks::{self, ServeTracker};
use crate::config::Config;
use crate::exec::ExecCtl;
use crate::mesh_ctl::MeshCtl;
use crate::state::ExecState;

pub struct Agent {
    pub cfg: Config,
    pub data_dir: PathBuf,
    pub key: SigningKey,
    pub key_path: PathBuf,
    pub cp: cp_api::CpClient,
    pub store: Arc<Store>,
    pub drivers: DriverRegistry,
    pub runtimes: Arc<runtimes::HttpRuntimes>,
    pub executor: executor_native::NativeExecutor,
    pub mesh: Arc<MeshCtl>,
    pub execs: Mutex<HashMap<String, Arc<ExecCtl>>>,
    pub state: Mutex<ExecState>,
    pub chunk_tracker: Arc<ServeTracker>,
    pub started_at_ms: i64,
    /// replication queue + in-flight set
    pub repl: Mutex<ReplQueue>,
    pub repl_notify: tokio::sync::Notify,
    /// last successful heartbeat apply; diagnostics for fencing stalls
    pub last_heartbeat_ok: Mutex<Option<std::time::Instant>>,
    /// shutdown requested: drain executions while heartbeats, the mesh, the
    /// chunk server and replication keep running; refuse new work
    pub shutting_down: AtomicBool,
}

#[derive(Default)]
pub struct ReplQueue {
    tasks: Vec<cp_api::ReplicationTask>,
    in_flight: std::collections::HashSet<String>,
}

impl Agent {
    pub fn exec_reports(&self) -> Vec<cp_api::ExecutionReport> {
        self.execs
            .lock()
            .unwrap()
            .values()
            .filter(|c| !c.is_finished())
            .map(|c| cp_api::ExecutionReport {
                execution_id: c.dir.execution_id.clone(),
                server_id: c.dir.server_id.clone(),
                epoch: c.dir.epoch,
                state: c.phase().as_str().into(),
                health: Some(c.health()),
                message: Some(c.message()),
                player_count: None,
                join_code: Some(c.join_code().unwrap_or_default()),
            })
            .collect()
    }

    /// Approximate: on-disk size of the chunk pool (manifests are tiny).
    pub fn snapshots_stored_bytes(&self) -> i64 {
        fn dir_size(p: &std::path::Path) -> u64 {
            std::fs::read_dir(p)
                .map(|it| {
                    it.flatten()
                        .map(|e| {
                            let m = e.metadata().ok();
                            match (e.file_type().ok().map(|t| t.is_dir()), m) {
                                (Some(true), _) => dir_size(&e.path()),
                                (_, Some(m)) if m.is_file() => m.len(),
                                _ => 0,
                            }
                        })
                        .sum()
                })
                .unwrap_or(0)
        }
        dir_size(&self.store.root().join("chunks")) as i64
    }

    pub fn note_exec_pgid(&self, execution_id: &str, pgid: u32) {
        // lock order: execs before state (same as everywhere else)
        let (server_id, epoch) = self
            .execs
            .lock()
            .unwrap()
            .get(execution_id)
            .map(|c| (c.dir.server_id.clone(), c.dir.epoch))
            .unwrap_or_default();
        self.state
            .lock()
            .unwrap()
            .upsert(crate::state::ExecRecord::new(
                execution_id.into(),
                server_id,
                epoch,
                pgid,
                false,
            ));
    }

    /// Push services for currently-hosting executions into the mesh.
    /// One HostedService per active execution's services (id = service_id).
    pub async fn sync_hosted_services(&self) {
        let services: Vec<mesh_ipc::pb::HostedService> = self
            .execs
            .lock()
            .unwrap()
            .values()
            .filter(|c| c.phase() == crate::exec::Phase::Running && !c.is_fenced())
            .flat_map(|c| {
                let bindings = c.port_bindings.lock().unwrap().clone();
                c.dir
                    .service
                    .iter()
                    .map(|s| mesh_ipc::pb::HostedService {
                        service_id: s.service_id.clone(),
                        epoch: c.dir.epoch as u64,
                        ports: s
                            .ports
                            .iter()
                            .map(|p| mesh_ipc::pb::HostedPort {
                                port: p.port.max(0) as u32,
                                protocol: proto_for(&p.protocol),
                                target_port: bindings
                                    .iter()
                                    .find(|b| b.service_port == p.port.max(0) as u32)
                                    .map(|b| b.local_port)
                                    .unwrap_or(p.port.max(0) as u32),
                            })
                            .collect(),
                    })
                    .collect::<Vec<_>>()
            })
            .collect();
        if let Err(e) = self.mesh.set_hosted(services).await {
            tracing::warn!(error = %e, "set_hosted_services failed");
        }
    }

    pub fn enqueue_replication(&self, t: cp_api::ReplicationTask) {
        let mut q = self.repl.lock().unwrap();
        if q.in_flight.contains(&t.snapshot_id)
            || q.tasks.iter().any(|x| x.snapshot_id == t.snapshot_id)
        {
            return; // dedup
        }
        q.tasks.push(t);
        self.repl_notify.notify_one();
    }

    /// Long-lived worker: fetch chunks, then POST replica ready. Idempotent —
    /// already-complete tasks are reported ready without refetching.
    pub async fn replication_worker(
        self: &Arc<Agent>,
        mut stop: tokio::sync::watch::Receiver<bool>,
    ) {
        loop {
            let task = {
                let mut q = self.repl.lock().unwrap();
                if let Some(t) = q.tasks.pop() {
                    q.in_flight.insert(t.snapshot_id.clone());
                    Some(t)
                } else {
                    None
                }
            };
            let Some(t) = task else {
                tokio::select! {
                    _ = self.repl_notify.notified() => continue,
                    _ = stop.changed() => return,
                }
            };
            if let Err(e) = self.replicate_one(&t).await {
                tracing::warn!(snap = %t.snapshot_id, error = %format!("{e:#}"), "replication failed");
            }
            self.repl.lock().unwrap().in_flight.remove(&t.snapshot_id);
        }
    }

    async fn replicate_one(&self, t: &cp_api::ReplicationTask) -> anyhow::Result<()> {
        let Some(sid) = snapshot_store::SnapshotId::parse(&t.snapshot_id) else {
            anyhow::bail!("bad snapshot id {}", t.snapshot_id);
        };
        let missing = match self.store.manifest(&sid) {
            Ok(m) => !self.store.missing_or_corrupt_chunks(&m)?.is_empty(),
            Err(_) => true,
        };
        if missing {
            let srcs: Vec<String> = t.source_node_ids.clone();
            chunks::fetch_snapshot(&self.mesh, &self.store, &sid, &t.manifest_digest, &srcs)
                .await?;
        }
        let body = serde_json::json!({"state": "ready"});
        self.cp
            .call(
                "POST",
                &format!("/v1/agent/snapshots/{}/replicas", t.snapshot_id),
                Some(body.to_string().as_bytes()),
            )
            .await?;
        tracing::info!(snap = %t.snapshot_id, "replica ready");
        // best-effort: tell each source so a graceful-stop hold releases even
        // when dedup meant we fetched no chunks from it
        for src in &t.source_node_ids {
            let _ = chunks::notify_ready(&self.mesh, src, &sid).await;
        }
        Ok(())
    }
}

fn proto_for(s: &str) -> i32 {
    match s {
        "tcp" => mesh_ipc::pb::Protocol::Tcp as i32,
        "udp" => mesh_ipc::pb::Protocol::Udp as i32,
        _ => 0,
    }
}
