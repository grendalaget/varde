//! Local status + linking for desktop clients over agent-ipc (named pipe /
//! Unix socket). Owns the not-linked state and the service-side device-code
//! flow: the tray runs as the logged-in user and can't write the data dir, so
//! the service links itself and the tray only asks for it.

// tonic::Status is the error type of every handler here
#![allow(clippy::result_large_err)]

use std::path::PathBuf;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use agent_ipc::pb;
use agent_ipc::server::PeerInfo;
use tokio::sync::{watch, Notify};
use tonic::{Request, Response, Status};
use tracing::{info, warn};

use crate::config::Config;
use crate::exec::Phase;
use crate::{identity, link, Agent};

pub struct Hub {
    data_dir: PathBuf,
    agent: Mutex<Option<Arc<Agent>>>,
    link: Mutex<LinkState>,
    shutting_down: AtomicBool,
    changed: watch::Sender<u64>,
    /// A link flow wrote config.toml.
    pub linked: Notify,
}

#[derive(Default)]
struct LinkState {
    cp_url: String,
    active: Option<pb::Link>,
    error: String,
    task: Option<tokio::task::JoinHandle<()>>,
}

const CODE_EXPIRED: &str = "The code expired. Start linking again.";

impl Hub {
    pub fn new(data_dir: PathBuf) -> Arc<Hub> {
        Arc::new(Hub {
            data_dir,
            agent: Mutex::new(None),
            link: Mutex::new(LinkState::default()),
            shutting_down: AtomicBool::new(false),
            changed: watch::channel(0).0,
            linked: Notify::new(),
        })
    }

    fn bump(&self) {
        self.changed.send_modify(|v| *v = v.wrapping_add(1));
    }

    pub fn attach(&self, a: Arc<Agent>) {
        *self.agent.lock().unwrap() = Some(a);
        self.bump();
    }

    pub fn detach(&self) {
        *self.agent.lock().unwrap() = None;
        self.bump();
    }

    pub fn set_shutting_down(&self) {
        self.shutting_down.store(true, Ordering::SeqCst);
        self.cancel_link();
        self.bump();
    }

    pub fn status(&self) -> pb::Status {
        let mut s = pb::Status {
            agent_version: env!("CARGO_PKG_VERSION").into(),
            ..Default::default()
        };
        {
            let l = self.link.lock().unwrap();
            s.link = l.active.clone();
            s.link_error = l.error.clone();
            s.control_plane_url = l.cp_url.clone();
        }
        let linking = s.link.is_some();
        let down = self.shutting_down.load(Ordering::SeqCst);
        let agent = self.agent.lock().unwrap().clone();
        let state = if let Some(a) = agent {
            if !linking {
                s.control_plane_url = a.cfg.control_plane_url.clone();
            }
            s.node_id = a.cfg.node_id.clone();
            s.group_id = a.cfg.group_id.clone();
            let view = a.cp_view.lock().unwrap().clone();
            s.node_name = view.node_name;
            s.group_name = view.group_name;
            s.last_contact_unix_ms = view.last_contact_unix_ms;
            s.hosting = hosting(&a);
            let fresh = Duration::from_millis((view.heartbeat_interval_ms * 3).max(15_000) as u64);
            match *a.last_heartbeat_ok.lock().unwrap() {
                _ if down => pb::State::ShuttingDown,
                _ if linking => pb::State::Linking,
                None => pb::State::Connecting,
                Some(t) if t.elapsed() <= fresh => pb::State::Online,
                Some(_) => pb::State::Offline,
            }
        } else if let Ok(Some(cfg)) = Config::load_opt(&self.data_dir) {
            // linked, agent (re)starting
            s.control_plane_url = cfg.control_plane_url;
            s.node_id = cfg.node_id;
            s.group_id = cfg.group_id;
            if down {
                pb::State::ShuttingDown
            } else {
                pb::State::Connecting
            }
        } else if down {
            pb::State::ShuttingDown
        } else if linking {
            pb::State::Linking
        } else {
            pb::State::NotLinked
        };
        s.state = state as i32;
        s
    }

    fn is_linked(&self, agent: &Option<Arc<Agent>>) -> bool {
        agent.is_some() || self.data_dir.join("config.toml").exists()
    }

    pub fn cancel_link(&self) {
        let mut l = self.link.lock().unwrap();
        if let Some(t) = l.task.take() {
            t.abort();
        }
        l.active = None;
        drop(l);
        self.bump();
    }

    pub async fn start_link(
        self: &Arc<Self>,
        url: &str,
        relink: bool,
        peer: &PeerInfo,
    ) -> Result<pb::Status, Status> {
        let url =
            link::normalize_url(url).map_err(|e| Status::invalid_argument(format!("{e:#}")))?;
        if self.shutting_down.load(Ordering::SeqCst) {
            return Err(Status::unavailable("Varde is shutting down"));
        }
        let agent = self.agent.lock().unwrap().clone();
        let linked = self.is_linked(&agent);
        let is_hosting = agent.as_ref().is_some_and(|a| !hosting(a).is_empty());
        check_link_allowed(linked, relink, peer.is_admin, is_hosting)?;

        // a re-link gets a new identity, written only once approved
        let key = if linked {
            identity::generate()
        } else {
            let p = link::key_path(&self.data_dir);
            identity::load_or_create(&p)
                .and_then(|_| identity::load(&p))
                .map_err(|e| Status::internal(format!("node key: {e:#}")))?
        };
        let client =
            cp_api::CpClient::new(&url).map_err(|e| Status::invalid_argument(format!("{e}")))?;
        self.cancel_link();
        let dev = match tokio::time::timeout(
            Duration::from_secs(15),
            link::start_device(&client, identity::public_key_b64(&key)),
        )
        .await
        {
            Ok(Ok(d)) => d,
            Ok(Err(e)) => {
                return Err(self.link_failed(&url, format!("could not reach {url}: {e:#}")))
            }
            Err(_) => return Err(self.link_failed(&url, format!("{url} did not answer"))),
        };
        let active = pb::Link {
            user_code: dev.user_code.clone(),
            link_url: link::link_url(&url, &dev),
            expires_at_unix_ms: crate::now_ms() + dev.expires_in * 1000,
        };
        info!(code = %dev.user_code, url = %active.link_url, relink, "linking: waiting for approval");
        let task = tokio::spawn(
            self.clone()
                .finish_link(client, dev, url.clone(), key, linked),
        );
        {
            let mut l = self.link.lock().unwrap();
            l.cp_url = url;
            l.active = Some(active);
            l.error.clear();
            l.task = Some(task);
        }
        self.bump();
        Ok(self.status())
    }

    fn link_failed(&self, url: &str, msg: String) -> Status {
        warn!(%url, error = %msg, "linking failed");
        {
            let mut l = self.link.lock().unwrap();
            l.cp_url = url.to_string();
            l.error = msg.clone();
        }
        self.bump();
        Status::unavailable(msg)
    }

    async fn finish_link(
        self: Arc<Self>,
        client: cp_api::CpClient,
        dev: cp_api::DeviceEnrollResponse,
        url: String,
        key: ed25519_dalek::SigningKey,
        relink: bool,
    ) {
        let deadline = Instant::now() + Duration::from_secs(dev.expires_in.max(1) as u64);
        let every = Duration::from_secs(dev.interval.max(1) as u64);
        let err = loop {
            if Instant::now() >= deadline {
                break CODE_EXPIRED.to_string();
            }
            tokio::time::sleep(every).await;
            match link::poll_device(&client, &dev.device_code).await {
                Ok(link::Poll::Pending) => {}
                Ok(link::Poll::Expired) => break CODE_EXPIRED.to_string(),
                Ok(link::Poll::Approved(r)) => match self.write_link(&url, &r, &key, relink) {
                    Ok(()) => {
                        info!(node_id = %r.node_id, group = %r.group_id, "linked; wrote config.toml");
                        {
                            let mut l = self.link.lock().unwrap();
                            l.active = None;
                            l.task = None;
                        }
                        self.bump();
                        self.linked.notify_one();
                        return;
                    }
                    Err(e) => break format!("could not save the link: {e:#}"),
                },
                // transient (offline, CP restarting): keep polling until expiry
                Err(e) => warn!(error = %format!("{e:#}"), "link poll failed; retrying"),
            }
        };
        warn!(error = %err, "linking stopped");
        {
            let mut l = self.link.lock().unwrap();
            l.active = None;
            l.error = err;
            l.task = None;
        }
        self.bump();
    }

    fn write_link(
        &self,
        url: &str,
        r: &cp_api::EnrollResult,
        key: &ed25519_dalek::SigningKey,
        relink: bool,
    ) -> anyhow::Result<()> {
        let prev = if relink {
            identity::save(&link::key_path(&self.data_dir), key)?;
            Config::load_opt(&self.data_dir).ok().flatten()
        } else {
            None
        };
        link::enrolled_config(url, r, prev).save(&self.data_dir)
    }
}

/// Executions on this machine that haven't ended, with their latest safe save.
fn hosting(a: &Agent) -> Vec<pb::Hosting> {
    let saves = a.cp_view.lock().unwrap().latest_safe_save.clone();
    let mut out: Vec<pb::Hosting> = a
        .execs
        .lock()
        .unwrap()
        .values()
        .filter(|c| !c.is_finished())
        .filter(|c| {
            matches!(
                c.phase(),
                Phase::Preparing
                    | Phase::Restoring
                    | Phase::Starting
                    | Phase::Running
                    | Phase::Stopping
            )
        })
        .map(|c| pb::Hosting {
            server_id: c.dir.server_id.clone(),
            server_name: c.dir.server_name.clone(),
            phase: c.phase().as_str().into(),
            latest_safe_save_at_unix_ms: saves.get(&c.dir.server_id).copied().unwrap_or(0),
        })
        .collect();
    out.sort_by(|a, b| a.server_name.cmp(&b.server_name));
    out
}

/// Linking a machine that isn't linked is open to any local user (that's
/// the installer flow). Re-linking needs an administrator and an idle machine.
fn check_link_allowed(
    linked: bool,
    relink: bool,
    is_admin: bool,
    hosting: bool,
) -> Result<(), Status> {
    if !linked {
        return Ok(());
    }
    if !relink {
        return Err(Status::failed_precondition("this PC is already linked"));
    }
    if !is_admin {
        return Err(Status::permission_denied(
            "re-linking this PC needs an administrator",
        ));
    }
    if hosting {
        return Err(Status::failed_precondition(
            "this PC is hosting a server; stop or move it first",
        ));
    }
    Ok(())
}

struct Svc(Arc<Hub>);

type StatusStream =
    Pin<Box<dyn tokio_stream::Stream<Item = Result<pb::WatchStatusResponse, Status>> + Send>>;

#[tonic::async_trait]
impl pb::local_service_server::LocalService for Svc {
    async fn get_status(
        &self,
        _req: Request<pb::GetStatusRequest>,
    ) -> Result<Response<pb::GetStatusResponse>, Status> {
        Ok(Response::new(pb::GetStatusResponse {
            status: Some(self.0.status()),
        }))
    }

    type WatchStatusStream = StatusStream;

    async fn watch_status(
        &self,
        _req: Request<pb::WatchStatusRequest>,
    ) -> Result<Response<StatusStream>, Status> {
        let hub = self.0.clone();
        let mut changed = hub.changed.subscribe();
        let (tx, rx) = tokio::sync::mpsc::channel(4);
        tokio::spawn(async move {
            let mut last: Option<pb::Status> = None;
            loop {
                let s = hub.status();
                if last.as_ref() != Some(&s) {
                    let msg = pb::WatchStatusResponse {
                        status: Some(s.clone()),
                    };
                    if tx.send(Ok(msg)).await.is_err() {
                        return;
                    }
                    last = Some(s);
                }
                // changes outside the hub (heartbeats, phases) show up within 1 s
                tokio::select! {
                    _ = changed.changed() => {}
                    _ = tokio::time::sleep(Duration::from_secs(1)) => {}
                    _ = tx.closed() => return,
                }
            }
        });
        Ok(Response::new(Box::pin(
            tokio_stream::wrappers::ReceiverStream::new(rx),
        )))
    }

    async fn start_link(
        &self,
        req: Request<pb::StartLinkRequest>,
    ) -> Result<Response<pb::StartLinkResponse>, Status> {
        let peer = req
            .extensions()
            .get::<PeerInfo>()
            .cloned()
            .unwrap_or_default();
        let r = req.into_inner();
        let status = self
            .0
            .start_link(&r.control_plane_url, r.relink, &peer)
            .await?;
        Ok(Response::new(pb::StartLinkResponse {
            status: Some(status),
        }))
    }

    async fn cancel_link(
        &self,
        _req: Request<pb::CancelLinkRequest>,
    ) -> Result<Response<pb::CancelLinkResponse>, Status> {
        self.0.cancel_link();
        Ok(Response::new(pb::CancelLinkResponse {
            status: Some(self.0.status()),
        }))
    }
}

/// Serves the local IPC endpoint for `hub` until the process exits.
pub fn serve(hub: Arc<Hub>) -> std::io::Result<String> {
    let ep = agent_ipc::endpoint(&hub.data_dir);
    let incoming = agent_ipc::server::incoming(&ep)?;
    let svc = pb::local_service_server::LocalServiceServer::new(Svc(hub));
    tokio::spawn(async move {
        if let Err(e) = tonic::transport::Server::builder()
            .add_service(svc)
            .serve_with_incoming(incoming)
            .await
        {
            warn!(error = %e, "local IPC server stopped");
        }
    });
    Ok(ep)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    #[test]
    fn link_policy() {
        assert!(check_link_allowed(false, false, false, false).is_ok());
        assert!(check_link_allowed(false, true, false, true).is_ok());
        let code = |r: Result<(), Status>| r.unwrap_err().code();
        assert_eq!(
            code(check_link_allowed(true, false, true, false)),
            tonic::Code::FailedPrecondition
        );
        assert_eq!(
            code(check_link_allowed(true, true, false, false)),
            tonic::Code::PermissionDenied
        );
        assert_eq!(
            code(check_link_allowed(true, true, true, true)),
            tonic::Code::FailedPrecondition
        );
        assert!(check_link_allowed(true, true, true, false).is_ok());
    }

    /// Minimal control plane: device enroll + poll (202 once, then 200).
    async fn fake_cp() -> String {
        let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let base = format!("http://{}", l.local_addr().unwrap());
        let polls = Arc::new(AtomicUsize::new(0));
        let b = base.clone();
        tokio::spawn(async move {
            loop {
                let (mut s, _) = l.accept().await.unwrap();
                let polls = polls.clone();
                let base = b.clone();
                tokio::spawn(async move {
                    let mut buf = vec![0u8; 16 * 1024];
                    let mut n = 0;
                    loop {
                        let m = s.read(&mut buf[n..]).await.unwrap();
                        n += m;
                        let text = String::from_utf8_lossy(&buf[..n]).to_string();
                        if let Some(h) = text.find("\r\n\r\n") {
                            let len = text
                                .lines()
                                .find_map(|l| {
                                    l.to_ascii_lowercase()
                                        .strip_prefix("content-length:")
                                        .map(|v| v.trim().parse::<usize>().unwrap())
                                })
                                .unwrap_or(0);
                            if n >= h + 4 + len || m == 0 {
                                break;
                            }
                        }
                        if m == 0 {
                            break;
                        }
                    }
                    let req = String::from_utf8_lossy(&buf[..n]).to_string();
                    // hyper may send an absolute-form target
                    let target = req.split(' ').nth(1).unwrap_or("");
                    let path = target.find("/v1/").map(|i| &target[i..]).unwrap_or(target);
                    let (code, body) = if path == "/v1/agent/enroll/device/poll" {
                        if polls.fetch_add(1, Ordering::SeqCst) == 0 {
                            (202, String::new())
                        } else {
                            (200, r#"{"node_id":"node_x","group_id":"grp_x","control_plane_public_key":"cpk"}"#.to_string())
                        }
                    } else if path == "/v1/agent/enroll/device" {
                        assert!(req.contains("\"hostname\""));
                        (
                            200,
                            format!(
                                r#"{{"device_code":"dev","user_code":"WXYZ-1234","verification_url":"{base}/link?code=WXYZ-1234","expires_in":60,"interval":1}}"#
                            ),
                        )
                    } else {
                        (404, String::new())
                    };
                    let resp = format!(
                        "HTTP/1.1 {code} X\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
                        body.len()
                    );
                    let _ = s.write_all(resp.as_bytes()).await;
                });
            }
        });
        base
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn not_linked_then_link_over_ipc() {
        let tmp = tempfile::tempdir().unwrap();
        let hub = Hub::new(tmp.path().to_path_buf());
        let ep = serve(hub.clone()).unwrap();
        let mut c = agent_ipc::connect(&ep).await.unwrap();

        let st = |r: pb::GetStatusResponse| r.status.unwrap();
        let s = st(c
            .get_status(pb::GetStatusRequest {})
            .await
            .unwrap()
            .into_inner());
        assert_eq!(s.state(), pb::State::NotLinked);

        let cp = fake_cp().await;
        let s = c
            .start_link(pb::StartLinkRequest {
                control_plane_url: format!("{cp}/"),
                relink: false,
            })
            .await
            .unwrap()
            .into_inner()
            .status
            .unwrap();
        assert_eq!(s.state(), pb::State::Linking);
        let l = s.link.unwrap();
        assert_eq!(l.user_code, "WXYZ-1234");
        assert_eq!(l.link_url, format!("{cp}/link?code=WXYZ-1234"));
        assert_eq!(s.control_plane_url, cp);

        // approval lands: config written, state moves on, run loop is woken
        let mut w = c
            .watch_status(pb::WatchStatusRequest {})
            .await
            .unwrap()
            .into_inner();
        tokio::time::timeout(Duration::from_secs(15), async {
            while let Some(m) = w.message().await.unwrap() {
                if m.status.unwrap().state() == pb::State::Connecting {
                    return;
                }
            }
        })
        .await
        .expect("never reached connecting");
        tokio::time::timeout(Duration::from_secs(1), hub.linked.notified())
            .await
            .expect("linked not signalled");
        let cfg = Config::load(tmp.path()).unwrap();
        assert_eq!(cfg.node_id, "node_x");
        assert_eq!(cfg.group_id, "grp_x");
        assert_eq!(cfg.control_plane_url, cp);
        assert!(link::key_path(tmp.path()).exists());

        // linking again without relink is refused
        let e = c
            .start_link(pb::StartLinkRequest {
                control_plane_url: cp.clone(),
                relink: false,
            })
            .await
            .unwrap_err();
        assert_eq!(e.code(), tonic::Code::FailedPrecondition);
    }

    #[tokio::test]
    async fn unreachable_control_plane_reports_error() {
        let tmp = tempfile::tempdir().unwrap();
        let hub = Hub::new(tmp.path().to_path_buf());
        // nothing listens on port 9 (discard) on loopback in CI
        let e = hub
            .start_link("http://127.0.0.1:9", false, &PeerInfo::default())
            .await
            .unwrap_err();
        assert_eq!(e.code(), tonic::Code::Unavailable);
        let s = hub.status();
        assert_eq!(s.state(), pb::State::NotLinked);
        assert!(s.link_error.contains("could not reach"), "{}", s.link_error);
    }
}
