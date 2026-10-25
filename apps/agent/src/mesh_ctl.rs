//! Mesh supervisor: spawn the Go mesh child, keep the gRPC client connected,
//! and re-apply the full desired state (Configure/Set*) after every restart.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Mutex;
use std::time::Duration;

use anyhow::Result;
use tokio::sync::watch;

use crate::mesh_child;

use mesh_ipc::pb;
pub use mesh_ipc::pb::MeshStatus;
use mesh_ipc::MeshServiceClient;

#[derive(Default)]
struct Desired {
    configure: Option<pb::ConfigureRequest>,
    peers: Vec<pb::Peer>,
    routes: Vec<pb::ServiceRoute>,
    hosted: Vec<pb::HostedService>,
    internal: Vec<pb::InternalService>,
}

/// A bound internal forward: a mesh-side 127.0.0.1 listener tunnelling to one
/// peer's internal service.
#[derive(Clone, Debug)]
pub struct Forward {
    pub id: String,
    pub addr: String,
    /// Came from the cache rather than a fresh bind — it may be stale.
    pub reused: bool,
}

type FwdKey = (String, String); // (peer node id, service name)

/// Forwards cached per (peer node, internal service). Each bind is a listener
/// plus goroutine in the mesh kept until unbound, so binding per request
/// leaks fds — and every open listener is reachable by any local user.
#[derive(Default)]
struct Forwards {
    /// Bumped on every mesh (re)connect: forwards die with the mesh process
    /// and ids may repeat in the next one, so entries never cross it.
    generation: u64,
    map: HashMap<FwdKey, Forward>,
}

impl Forwards {
    fn get(&self, key: &FwdKey) -> Option<Forward> {
        self.map.get(key).map(|f| Forward {
            reused: true,
            ..f.clone()
        })
    }

    /// Cache a forward bound at `generation`. Returns the forward the caller
    /// should unbind — the new one when a concurrent bind won the slot —
    /// plus the one to use. A bind from an earlier generation died with its
    /// mesh: nothing is cached and nothing needs unbinding.
    fn insert(&mut self, generation: u64, key: FwdKey, fwd: Forward) -> (Option<Forward>, Forward) {
        if self.generation != generation {
            return (None, fwd);
        }
        if let Some(theirs) = self.map.get(&key) {
            return (Some(fwd), theirs.clone());
        }
        self.map.insert(key, fwd.clone());
        (None, fwd)
    }

    /// Remove `key` if it still maps to `forward_id`; true = caller unbinds.
    fn retire(&mut self, key: &FwdKey, forward_id: &str) -> bool {
        if self.map.get(key).is_some_and(|f| f.id == forward_id) {
            self.map.remove(key);
            return true;
        }
        false
    }

    /// Drop forwards to peers outside `peers`; returns their ids to unbind.
    fn retain_peers(&mut self, peers: &[pb::Peer]) -> Vec<String> {
        let mut gone = vec![];
        self.map.retain(|(peer, _), f| {
            let keep = peers.iter().any(|p| &p.node_id == peer);
            if !keep {
                gone.push(f.id.clone());
            }
            keep
        });
        gone
    }

    /// New mesh process: every cached forward died with the old one.
    fn reset(&mut self) {
        self.generation += 1;
        self.map.clear();
    }
}

/// Handle to the supervised mesh. All `set_*` calls are full-replacement and
/// safe to re-issue; they are replayed automatically on reconnect.
pub struct MeshCtl {
    desired: Mutex<Desired>,
    client_tx: watch::Sender<Option<MeshServiceClient<tonic::transport::Channel>>>,
    forwards: Mutex<Forwards>,
    ipc: String,
}

impl MeshCtl {
    fn current(&self) -> Option<MeshServiceClient<tonic::transport::Channel>> {
        self.client_tx.borrow().clone()
    }

    fn set_client(&self, c: Option<MeshServiceClient<tonic::transport::Channel>>) {
        // a new (or no) mesh process: the old one's forwards are gone — drop
        // them without unbinding, their ids may name new forwards now. Under
        // the forwards lock so a bind sees client and generation together.
        let mut f = self.forwards.lock().unwrap();
        f.reset();
        // send_replace stores the value even with no receivers — `send` would
        // silently drop it (the watch channel is write-side only).
        self.client_tx.send_replace(c);
    }

    /// Runs the supervision loop until `stop` fires: respawns the mesh child
    /// with backoff and re-applies all Set* state on each reconnect.
    pub async fn supervise(
        self: std::sync::Arc<Self>,
        data_dir: PathBuf,
        mesh_bin: PathBuf,
        mut stop: watch::Receiver<bool>,
    ) {
        let ipc = mesh_ipc::ipc_endpoint(&data_dir);
        let mut backoff = Duration::from_millis(250);
        loop {
            let mut child = mesh_child::spawn(mesh_bin.clone(), ipc.clone());
            // connect + apply desired state
            if let Err(e) = self.connect_apply().await {
                tracing::warn!(error = %e, "mesh connect/apply failed");
            }
            tokio::select! {
                status = child.wait() => {
                    tracing::warn!(status = ?status, "mesh child died; restarting");
                    // a non-clean exit is a crash: record it with the stderr
                    // tail so the upload loop can report it. A clean exit
                    // (exit code 0) is not a crash.
                    if !status.map(|s| s.success()).unwrap_or_default() {
                        let status_s = status
                            .map(|s| s.to_string())
                            .unwrap_or_else(|| "unknown".into());
                        let exe = mesh_bin
                            .file_name()
                            .map(|f| f.to_string_lossy().into_owned())
                            .unwrap_or_else(|| "varde-mesh".into());
                        if let Err(e) = crash_report::record_process_exit(
                            &crate::crash::dir(&data_dir),
                            &exe,
                            env!("CARGO_PKG_VERSION"),
                            &status_s,
                            &child.stderr_tail(),
                        ) {
                            tracing::warn!(error = %e, "could not record mesh crash");
                        }
                    }
                    self.set_client(None);
                    tokio::select! {
                        _ = tokio::time::sleep(backoff) => {},
                        _ = stop.changed() => return,
                    }
                    backoff = (backoff * 2).min(Duration::from_secs(30));
                }
                _ = stop.changed() => {
                    child.kill();
                    return;
                }
            }
        }
    }

    async fn connect_apply(&self) -> Result<()> {
        for _ in 0..50 {
            if let Ok(mut c) = mesh_ipc::connect(&self.ipc).await {
                let (cfg, peers, routes, hosted, internal) = {
                    let d = self.desired.lock().unwrap();
                    (
                        d.configure.clone(),
                        d.peers.clone(),
                        d.routes.clone(),
                        d.hosted.clone(),
                        d.internal.clone(),
                    )
                };
                if let Some(cfg) = cfg {
                    c.configure(cfg).await?;
                }
                c.set_peers(pb::SetPeersRequest { peers }).await?;
                c.set_routes(pb::SetRoutesRequest { routes }).await?;
                c.set_hosted_services(pb::SetHostedServicesRequest { services: hosted })
                    .await?;
                c.set_internal_services(pb::SetInternalServicesRequest { services: internal })
                    .await?;
                self.set_client(Some(c));
                return Ok(());
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        anyhow::bail!("mesh IPC unreachable")
    }

    pub async fn configure(&self, req: pb::ConfigureRequest) -> Result<()> {
        {
            let mut d = self.desired.lock().unwrap();
            if d.configure.as_ref() == Some(&req) {
                return Ok(());
            }
            d.configure = Some(req.clone());
        }
        if let Some(mut c) = self.current() {
            c.configure(req).await?;
        }
        Ok(())
    }

    pub async fn set_peers(&self, peers: Vec<pb::Peer>) -> Result<()> {
        self.desired.lock().unwrap().peers = peers.clone();
        // forwards to peers that left the set would only ever fail
        let (client, gone) = {
            let mut f = self.forwards.lock().unwrap();
            (self.current(), f.retain_peers(&peers))
        };
        for id in gone {
            unbind_internal_forward(client.clone(), &id).await;
        }
        if let Some(mut c) = self.current() {
            c.set_peers(pb::SetPeersRequest { peers }).await?;
        }
        Ok(())
    }

    pub async fn set_routes(&self, routes: Vec<pb::ServiceRoute>) -> Result<()> {
        self.desired.lock().unwrap().routes = routes.clone();
        if let Some(mut c) = self.current() {
            c.set_routes(pb::SetRoutesRequest { routes }).await?;
        }
        Ok(())
    }

    pub async fn set_hosted(&self, services: Vec<pb::HostedService>) -> Result<()> {
        self.desired.lock().unwrap().hosted = services.clone();
        if let Some(mut c) = self.current() {
            c.set_hosted_services(pb::SetHostedServicesRequest { services })
                .await?;
        }
        Ok(())
    }

    pub async fn set_internal(&self, services: Vec<pb::InternalService>) -> Result<()> {
        self.desired.lock().unwrap().internal = services.clone();
        if let Some(mut c) = self.current() {
            c.set_internal_services(pb::SetInternalServicesRequest { services })
                .await?;
        }
        Ok(())
    }

    /// The cached forward to internal service `name` on `peer`, bound on
    /// first use. None = mesh not connected. Callers `retire_internal_forward`
    /// it when a connection through it fails.
    pub async fn internal_forward(&self, peer: &str, name: &str) -> Result<Option<Forward>> {
        let key = (peer.to_string(), name.to_string());
        let (client, generation) = {
            let f = self.forwards.lock().unwrap();
            if let Some(fwd) = f.get(&key) {
                return Ok(Some(fwd));
            }
            (self.current(), f.generation)
        };
        let Some(mut c) = client else {
            return Ok(None);
        };
        let r = c
            .bind_internal_forward(pb::BindInternalForwardRequest {
                peer_node_id: peer.into(),
                name: name.into(),
            })
            .await?
            .into_inner();
        let fwd = Forward {
            id: r.forward_id,
            addr: r.local_addr,
            reused: false,
        };
        let (surplus, fwd) = self.forwards.lock().unwrap().insert(generation, key, fwd);
        if let Some(s) = surplus {
            // a concurrent caller bound first: theirs is cached, ours goes
            unbind_internal_forward(Some(c), &s.id).await;
        }
        Ok(Some(fwd))
    }

    /// Drop a forward whose connection failed (stale peer path, peer
    /// re-added) and close its mesh listener. A no-op when the cache already
    /// moved on to another forward for that key.
    pub async fn retire_internal_forward(&self, peer: &str, name: &str, forward_id: &str) {
        let key = (peer.to_string(), name.to_string());
        let client = {
            let mut f = self.forwards.lock().unwrap();
            if !f.retire(&key, forward_id) {
                return;
            }
            self.current()
        };
        unbind_internal_forward(client, forward_id).await;
    }

    pub async fn status(&self) -> Option<MeshStatus> {
        let mut c = self.current()?;
        c.get_status(pb::GetStatusRequest {})
            .await
            .ok()
            .map(|r| r.into_inner())
    }

    pub async fn list_peers(&self) -> Vec<pb::PeerState> {
        let Some(mut c) = self.current() else {
            return vec![];
        };
        c.list_peers(pb::ListPeersRequest {})
            .await
            .map(|r| r.into_inner().peers)
            .unwrap_or_default()
    }

    pub fn new(ipc: String) -> MeshCtl {
        let (tx, _rx) = watch::channel(None);
        MeshCtl {
            desired: Mutex::new(Desired::default()),
            client_tx: tx,
            forwards: Mutex::new(Forwards::default()),
            ipc,
        }
    }
}

/// Best-effort unbind, on the client captured together with the id (same
/// mesh generation). Should a restart still slip in between, the worst case
/// is a closed fresh forward, which its next failed request retires and
/// re-binds.
async fn unbind_internal_forward(
    client: Option<MeshServiceClient<tonic::transport::Channel>>,
    forward_id: &str,
) {
    if let Some(mut c) = client {
        let _ = c
            .unbind_internal_forward(pb::UnbindInternalForwardRequest {
                forward_id: forward_id.into(),
            })
            .await;
    }
}

#[cfg(test)]
mod forward_cache_tests {
    use super::*;

    fn key(peer: &str) -> FwdKey {
        (peer.to_string(), "agent.chunks".to_string())
    }

    fn fwd(id: &str) -> Forward {
        Forward {
            id: id.into(),
            addr: format!("127.0.0.1:{}", 40000 + id.len()),
            reused: false,
        }
    }

    fn peer(id: &str) -> pb::Peer {
        pb::Peer {
            node_id: id.into(),
            ..Default::default()
        }
    }

    #[test]
    fn one_forward_per_peer_and_service_is_reused() {
        let mut f = Forwards::default();
        assert!(f.get(&key("node_a")).is_none());
        let (surplus, used) = f.insert(0, key("node_a"), fwd("fwd_1"));
        assert!(surplus.is_none());
        assert_eq!(used.id, "fwd_1");
        let hit = f.get(&key("node_a")).unwrap();
        assert_eq!(hit.id, "fwd_1");
        assert!(hit.reused);
        // a concurrent bind lost the race: it gets the cached one and must
        // unbind its own
        let (surplus, used) = f.insert(0, key("node_a"), fwd("fwd_2"));
        assert_eq!(surplus.unwrap().id, "fwd_2");
        assert_eq!(used.id, "fwd_1");
        assert_eq!(f.map.len(), 1);
    }

    #[test]
    fn retire_only_removes_the_named_forward() {
        let mut f = Forwards::default();
        f.insert(0, key("node_a"), fwd("fwd_1"));
        assert!(!f.retire(&key("node_a"), "fwd_old"), "already replaced");
        assert!(f.get(&key("node_a")).is_some());
        assert!(f.retire(&key("node_a"), "fwd_1"));
        assert!(f.get(&key("node_a")).is_none());
        assert!(!f.retire(&key("node_a"), "fwd_1"), "retired once");
    }

    #[test]
    fn departed_peers_lose_their_forwards() {
        let mut f = Forwards::default();
        f.insert(0, key("node_a"), fwd("fwd_1"));
        f.insert(0, key("node_b"), fwd("fwd_2"));
        let gone = f.retain_peers(&[peer("node_b")]);
        assert_eq!(gone, vec!["fwd_1".to_string()]);
        assert!(f.get(&key("node_a")).is_none());
        assert!(f.get(&key("node_b")).is_some());
    }

    #[test]
    fn mesh_restart_drops_the_cache_and_stale_binds() {
        let mut f = Forwards::default();
        f.insert(0, key("node_a"), fwd("fwd_1"));
        f.reset();
        assert!(f.get(&key("node_a")).is_none());
        // a bind that started before the restart is not cached (its
        // forward died with the old mesh) and needs no unbind
        let (surplus, used) = f.insert(0, key("node_a"), fwd("fwd_1"));
        assert!(surplus.is_none());
        assert_eq!(used.id, "fwd_1");
        assert!(f.map.is_empty());
    }
}
