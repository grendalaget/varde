//! Mesh supervisor: spawn the Go mesh child, keep the gRPC client connected,
//! and re-apply the full desired state (Configure/Set*) after every restart.

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

/// Handle to the supervised mesh. All `set_*` calls are full-replacement and
/// safe to re-issue; they are replayed automatically on reconnect.
pub struct MeshCtl {
    desired: Mutex<Desired>,
    client_tx: watch::Sender<Option<MeshServiceClient<tonic::transport::Channel>>>,
    ipc: String,
}

impl MeshCtl {
    fn current(&self) -> Option<MeshServiceClient<tonic::transport::Channel>> {
        self.client_tx.borrow().clone()
    }

    fn set_client(&self, c: Option<MeshServiceClient<tonic::transport::Channel>>) {
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
            let (mut child, _spec) = mesh_child::spawn(mesh_bin.clone(), ipc.clone());
            // connect + apply desired state
            if let Err(e) = self.connect_apply().await {
                tracing::warn!(error = %e, "mesh connect/apply failed");
            }
            tokio::select! {
                status = child.wait() => {
                    tracing::warn!(status = ?status, "mesh child died; restarting");
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

    pub async fn bind_internal_forward(&self, peer: &str, name: &str) -> Result<Option<String>> {
        let Some(mut c) = self.current() else {
            return Ok(None);
        };
        let r = c
            .bind_internal_forward(pb::BindInternalForwardRequest {
                peer_node_id: peer.into(),
                name: name.into(),
            })
            .await?;
        Ok(Some(r.into_inner().local_addr))
    }

    pub async fn unbind_internal_forward(&self, forward_id: &str) -> Result<()> {
        if let Some(mut c) = self.current() {
            let _ = c
                .unbind_internal_forward(pb::UnbindInternalForwardRequest {
                    forward_id: forward_id.into(),
                })
                .await;
        }
        Ok(())
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
            ipc,
        }
    }
}
