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

    /// Exchange bytes over private IPC and the mesh's cached peer connection.
    pub async fn internal_request(
        &self,
        peer: &str,
        name: &str,
        request: &[u8],
    ) -> Result<Vec<u8>> {
        let mut c = self
            .current()
            .ok_or_else(|| anyhow::anyhow!("mesh not connected"))?;
        let mut response = c
            .exchange_internal(pb::ExchangeInternalRequest {
                peer_node_id: peer.into(),
                name: name.into(),
                request: request.to_vec(),
            })
            .await?
            .into_inner();
        let mut bytes = Vec::new();
        while let Some(part) = response.message().await? {
            bytes.extend_from_slice(&part.data);
        }
        Ok(bytes)
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

#[cfg(test)]
mod tests {
    use super::*;

    struct ExchangeService;

    impl tonic::server::ServerStreamingService<pb::ExchangeInternalRequest> for ExchangeService {
        type Response = pb::ExchangeInternalResponse;
        type ResponseStream = tokio_stream::Iter<
            std::vec::IntoIter<std::result::Result<Self::Response, tonic::Status>>,
        >;
        type Future =
            tonic::codegen::BoxFuture<tonic::Response<Self::ResponseStream>, tonic::Status>;

        fn call(&mut self, request: tonic::Request<pb::ExchangeInternalRequest>) -> Self::Future {
            Box::pin(async move {
                let request = request.into_inner();
                assert_eq!(request.peer_node_id, "node_b");
                assert_eq!(request.name, "agent.chunks");
                let mut messages: Vec<_> = request
                    .request
                    .chunks(3)
                    .map(|data| pb::ExchangeInternalResponse {
                        data: data.to_vec(),
                    })
                    .map(Ok)
                    .collect();
                if request.request == b"partial-error" {
                    messages.push(Err(tonic::Status::unavailable("peer disconnected")));
                }
                Ok(tonic::Response::new(tokio_stream::iter(messages)))
            })
        }
    }

    #[tokio::test]
    async fn internal_request_reuses_channel_and_reassembles_streams() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            hyper::server::conn::http2::Builder::new(hyper_util::rt::TokioExecutor::new())
                .serve_connection(
                    hyper_util::rt::TokioIo::new(stream),
                    hyper::service::service_fn(|request| async move {
                        assert_eq!(
                            request.uri().path(),
                            "/varde.mesh.v1.MeshService/ExchangeInternal"
                        );
                        let codec = tonic::codec::ProstCodec::default();
                        let response = tonic::server::Grpc::new(codec)
                            .server_streaming(ExchangeService, request)
                            .await;
                        Ok::<_, std::convert::Infallible>(response)
                    }),
                )
                .await
                .unwrap();
        });
        let channel = tonic::transport::Endpoint::from_shared(format!("http://{addr}"))
            .unwrap()
            .connect()
            .await
            .unwrap();
        let mesh = MeshCtl::new(String::new());
        mesh.set_client(Some(MeshServiceClient::new(channel)));
        for request in [b"first-transfer\x00".as_slice(), b"second-transfer", b""] {
            let response = tokio::time::timeout(
                Duration::from_secs(5),
                mesh.internal_request("node_b", "agent.chunks", request),
            )
            .await
            .unwrap()
            .unwrap();
            assert_eq!(response, request);
        }
        let error = mesh
            .internal_request("node_b", "agent.chunks", b"partial-error")
            .await
            .unwrap_err();
        assert!(error.to_string().contains("peer disconnected"));
        mesh.set_client(None);
        assert!(mesh
            .internal_request("node_b", "agent.chunks", b"request")
            .await
            .is_err());
        server.abort();
    }

    #[tokio::test]
    async fn internal_request_without_mesh_fails_closed() {
        let mesh = MeshCtl::new(String::new());
        let error = mesh
            .internal_request("node_b", "agent.chunks", b"GET / HTTP/1.1\r\n\r\n")
            .await
            .unwrap_err();
        assert_eq!(error.to_string(), "mesh not connected");
    }
}
