//! Chunk server (storage.md §Replication): serves the local snapshot store to
//! peers over plain HTTP/1.1 on 127.0.0.1:<ephemeral>, registered with the
//! mesh as internal service `agent.chunks`. Also implements the pull side:
//! fetch a manifest + missing chunks from `source_node_ids`.

use std::collections::{HashMap, HashSet};
use std::io::Write as _;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

use anyhow::{bail, Context, Result};
use snapshot_store::{Manifest, SnapshotId, Store};

pub const SERVICE_NAME: &str = "agent.chunks";
const BATCH_INFLIGHT: usize = 4;

/// Tracks which chunks each snapshot served, so a graceful-stop hold can wait
/// until a peer has fetched everything for a snapshot.
#[derive(Default)]
struct Served {
    manifest: bool,
    chunks: HashSet<String>,
    peer_ready: bool,
}

#[derive(Default)]
pub struct ServeTracker {
    inner: Mutex<HashMap<String, Served>>, // snap_id → serving state
}

impl ServeTracker {
    fn record_manifest(&self, id: &str) {
        self.inner
            .lock()
            .unwrap()
            .entry(id.to_string())
            .or_default()
            .manifest = true;
    }
    /// A fetcher reports replica-ready after POSTing to the CP; with dedup a
    /// peer may need zero chunks, so served-count alone can't prove readiness.
    fn record_ready(&self, id: &str) {
        self.inner
            .lock()
            .unwrap()
            .entry(id.to_string())
            .or_default()
            .peer_ready = true;
    }
    pub fn peer_ready(&self, snap_id: &str) -> bool {
        self.inner
            .lock()
            .unwrap()
            .get(snap_id)
            .map(|s| s.peer_ready)
            .unwrap_or(false)
    }
    fn record_chunk(&self, snap_hints: &[String], chunk: &str) {
        let mut g = self.inner.lock().unwrap();
        for h in snap_hints {
            g.entry(h.clone())
                .or_default()
                .chunks
                .insert(chunk.to_string());
        }
    }
    /// True once some peer fetched the manifest and every chunk of `m`.
    pub fn fully_served(&self, snap_id: &str, m: &Manifest) -> bool {
        let g = self.inner.lock().unwrap();
        let Some(s) = g.get(snap_id) else {
            return false;
        };
        if !s.manifest {
            return false;
        }
        m.files
            .iter()
            .flat_map(|e| e.chunks.iter().flatten())
            .all(|c| s.chunks.contains(c))
    }
    /// All chunk ids fetched for `snap_id` (by any peer).
    pub fn chunks_served(&self, snap_id: &str) -> usize {
        self.inner
            .lock()
            .unwrap()
            .get(snap_id)
            .map(|s| s.chunks.len())
            .unwrap_or(0)
    }
}

pub struct ChunkServer {
    pub addr: SocketAddr,
    #[allow(dead_code)]
    pub tracker: Arc<ServeTracker>,
}

/// Serves the store until `stop` fires. Returns the bound address.
pub async fn serve(
    store: Arc<Store>,
    tracker: Arc<ServeTracker>,
    mut stop: tokio::sync::watch::Receiver<bool>,
) -> Result<ChunkServer> {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    let tracker2 = tracker.clone();
    tokio::spawn(async move {
        loop {
            tokio::select! {
                c = listener.accept() => {
                    let Ok((stream, _)) = c else { continue };
                    let store = store.clone();
                    let tracker = tracker2.clone();
                    tokio::spawn(async move {
                        let _ = handle(stream, store, tracker).await;
                    });
                }
                _ = stop.changed() => break,
            }
        }
    });
    Ok(ChunkServer { addr, tracker })
}

async fn handle(
    stream: tokio::net::TcpStream,
    store: Arc<Store>,
    tracker: Arc<ServeTracker>,
) -> Result<()> {
    let (mut rd, mut wr) = tokio::io::split(stream);
    loop {
        let mut head = Vec::new();
        let mut byte = [0u8; 1];
        while !head.ends_with(b"\r\n\r\n") {
            let n = tokio::io::AsyncReadExt::read(&mut rd, &mut byte).await?;
            if n == 0 {
                return Ok(());
            }
            head.push(byte[0]);
            if head.len() > 16 * 1024 {
                bail!("head too large");
            }
        }
        let head = String::from_utf8_lossy(&head).to_string();
        let mut lines = head.lines();
        let mut req_line = lines.next().unwrap_or("").split_whitespace();
        let (method, target) = (req_line.next().unwrap_or(""), req_line.next().unwrap_or(""));
        let mut content_len = 0usize;
        let mut close = false;
        for l in lines {
            let l = l.to_ascii_lowercase();
            if let Some(v) = l.strip_prefix("content-length:") {
                content_len = v.trim().parse().unwrap_or(0);
            }
            if l.starts_with("connection:") && l.contains("close") {
                close = true;
            }
        }
        let mut body = vec![0u8; content_len];
        tokio::io::AsyncReadExt::read_exact(&mut rd, &mut body).await?;
        let resp = route(method, target, &body, &store, &tracker);
        tokio::io::AsyncWriteExt::write_all(&mut wr, &resp).await?;
        if close {
            return Ok(());
        }
    }
}

fn route(
    method: &str,
    target: &str,
    body: &[u8],
    store: &Store,
    tracker: &ServeTracker,
) -> Vec<u8> {
    let (status, ctype, payload): (&str, &str, Vec<u8>) =
        match (method, target.split('/').collect::<Vec<_>>().as_slice()) {
            ("GET", ["", "v1", "manifests", id]) => match SnapshotId::parse(id) {
                Some(id) => match store.manifest_bytes(&id) {
                    Ok(b) => {
                        tracker.record_manifest(&id.0);
                        ("200 OK", "application/octet-stream", b)
                    }
                    Err(_) => ("404 Not Found", "text/plain", b"not found".to_vec()),
                },
                None => ("400 Bad Request", "text/plain", b"bad id".to_vec()),
            },
            ("GET", ["", "v1", "chunks", hash]) => {
                match store.read_chunk_compressed(&hash.to_string()) {
                    Ok(b) => {
                        tracker.record_chunk(&[], hash);
                        ("200 OK", "application/octet-stream", b)
                    }
                    Err(_) => ("404 Not Found", "text/plain", b"not found".to_vec()),
                }
            }
            ("POST", ["", "v1", "chunks", "batch"]) => {
                match serde_json::from_slice::<serde_json::Value>(body) {
                    Ok(v) => {
                        let mut frames = Vec::new();
                        let mut missing = Vec::new();
                        let snap_hints: Vec<String> = v
                            .get("snapshot_id")
                            .and_then(|s| s.as_str())
                            .map(|s| vec![s.to_string()])
                            .unwrap_or_default();
                        for h in v
                            .get("hashes")
                            .and_then(|h| h.as_array())
                            .cloned()
                            .unwrap_or_default()
                        {
                            let Some(h) = h.as_str() else { continue };
                            match store.read_chunk_compressed(&h.to_string()) {
                                Ok(z) => {
                                    frames.extend_from_slice(&(z.len() as u32).to_le_bytes());
                                    // chunk id is 64 lowercase hex chars = 32 bytes
                                    if let Ok(raw) = hex::decode(h) {
                                        frames.extend_from_slice(&raw[..32.min(raw.len())]);
                                    } else {
                                        frames.extend_from_slice(&[0u8; 32]);
                                    }
                                    frames.extend_from_slice(&z);
                                    tracker.record_chunk(&snap_hints, h);
                                }
                                Err(_) => missing.push(h.to_string()),
                            }
                        }
                        if !missing.is_empty() {
                            (
                                "404 Not Found",
                                "application/json",
                                serde_json::json!({"missing": missing})
                                    .to_string()
                                    .into_bytes(),
                            )
                        } else {
                            ("200 OK", "application/octet-stream", frames)
                        }
                    }
                    Err(e) => ("400 Bad Request", "text/plain", e.to_string().into_bytes()),
                }
            }
            ("POST", ["", "v1", "replicas", id]) => {
                tracker.record_ready(id);
                ("200 OK", "text/plain", b"ok".to_vec())
            }
            _ => ("404 Not Found", "text/plain", b"not found".to_vec()),
        };
    let mut r = Vec::new();
    let _ = write!(
        r,
        "HTTP/1.1 {}\r\ncontent-type: {}\r\ncontent-length: {}\r\n\r\n",
        status,
        ctype,
        payload.len()
    );
    r.extend_from_slice(&payload);
    r
}

// ---------------- pull side ----------------

/// Fetch `snapshot_id`'s manifest + all missing chunks from `source_node_ids`
/// via the mesh (`BindInternalForward` per source). Idempotent + resumable.
pub async fn fetch_snapshot(
    mesh: &crate::mesh_ctl::MeshCtl,
    store: &Store,
    snapshot_id: &SnapshotId,
    expected_digest: &str,
    source_node_ids: &[String],
) -> Result<()> {
    if source_node_ids.is_empty() {
        bail!("no source nodes for {snapshot_id}");
    }
    // manifest
    if store.manifest(snapshot_id).is_err() {
        let mut got = false;
        for src in source_node_ids {
            match http_get(mesh, src, &format!("/v1/manifests/{snapshot_id}")).await {
                Ok(bytes) => {
                    let id = store.put_manifest_bytes(&bytes)?;
                    if id != *snapshot_id {
                        store.delete_snapshot(&id).ok();
                        continue;
                    }
                    if !expected_digest.is_empty() && Manifest::digest(&bytes) != expected_digest {
                        store.delete_snapshot(&id).ok();
                        continue;
                    }
                    got = true;
                    break;
                }
                Err(e) => tracing::warn!(src, error = %e, "manifest fetch failed"),
            }
        }
        if !got {
            bail!("could not fetch manifest {snapshot_id}");
        }
    }
    let m = store.manifest(snapshot_id)?;
    let mut missing = store.missing_chunks(&m)?;
    let mut rr = 0usize;
    while !missing.is_empty() {
        let mut progress = false;
        for chunk_batch in missing.chunks(BATCH_INFLIGHT) {
            let src = &source_node_ids[rr % source_node_ids.len()];
            rr += 1;
            match batch_get(mesh, src, snapshot_id, chunk_batch).await {
                Ok(chunks) => {
                    for (id, z) in chunks {
                        store.put_chunk_compressed(&id, &z)?;
                        progress = true;
                    }
                }
                Err(e) => {
                    tracing::warn!(src, error = %e, "chunk batch fetch failed");
                }
            }
        }
        let still = store.missing_chunks(&m)?;
        if !progress && still == missing {
            bail!("no progress fetching chunks for {snapshot_id}");
        }
        missing = still;
    }
    Ok(())
}

async fn http_get(mesh: &crate::mesh_ctl::MeshCtl, peer: &str, path: &str) -> Result<Vec<u8>> {
    let Some(addr) = mesh.bind_internal_forward(peer, SERVICE_NAME).await? else {
        bail!("mesh not connected");
    };
    let mut stream = tokio::net::TcpStream::connect(&addr).await?;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    stream
        .write_all(
            format!(
                "GET {path} HTTP/1.1\r\nhost: x\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"
            )
            .as_bytes(),
        )
        .await?;
    let mut buf = Vec::new();
    stream.read_to_end(&mut buf).await?;
    let (status, bytes) = parse_http(&buf)?;
    if status != 200 {
        bail!("GET {path} → {status}");
    }
    Ok(bytes)
}

/// Tell a source node we reported replica-ready; lets its stop-hold release
/// even when dedup meant we fetched nothing from it.
pub async fn notify_ready(
    mesh: &crate::mesh_ctl::MeshCtl,
    peer: &str,
    snap_id: &SnapshotId,
) -> Result<()> {
    let Some(addr) = mesh.bind_internal_forward(peer, SERVICE_NAME).await? else {
        bail!("mesh not connected");
    };
    let mut stream = tokio::net::TcpStream::connect(&addr).await?;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    stream
        .write_all(
            format!("POST /v1/replicas/{snap_id} HTTP/1.1\r\nhost: x\r\ncontent-length: 0\r\nconnection: close\r\n\r\n")
                .as_bytes(),
        )
        .await?;
    let mut buf = Vec::new();
    stream.read_to_end(&mut buf).await?;
    let (status, _) = parse_http(&buf)?;
    if status != 200 {
        bail!("ready notify → {status}");
    }
    Ok(())
}

async fn batch_get(
    mesh: &crate::mesh_ctl::MeshCtl,
    peer: &str,
    snap_id: &SnapshotId,
    hashes: &[String],
) -> Result<Vec<(String, Vec<u8>)>> {
    let Some(addr) = mesh.bind_internal_forward(peer, SERVICE_NAME).await? else {
        bail!("mesh not connected");
    };
    let body = serde_json::json!({"snapshot_id": snap_id.0, "hashes": hashes}).to_string();
    let mut stream = tokio::net::TcpStream::connect(&addr).await?;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    stream
        .write_all(
            format!(
                "POST /v1/chunks/batch HTTP/1.1\r\nhost: x\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{}",
                body.len(),
                body
            )
            .as_bytes(),
        )
        .await?;
    let mut buf = Vec::new();
    stream.read_to_end(&mut buf).await?;
    let (status, bytes) = parse_http(&buf)?;
    if status != 200 {
        bail!("batch status {status}");
    }
    // frames: [u32 len][32-byte hash][zstd]
    let mut out = Vec::new();
    let mut off = 0usize;
    while off + 4 + 32 <= bytes.len() {
        let len = u32::from_le_bytes(bytes[off..off + 4].try_into().unwrap()) as usize;
        let hash = hex::encode(&bytes[off + 4..off + 4 + 32]);
        off += 4 + 32;
        if off + len > bytes.len() {
            break;
        }
        out.push((hash, bytes[off..off + len].to_vec()));
        off += len;
    }
    Ok(out)
}

/// Crude HTTP response parser: returns (status code, body).
fn parse_http(buf: &[u8]) -> Result<(u16, Vec<u8>)> {
    let sep = buf
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .context("no header end")?;
    let head = String::from_utf8_lossy(&buf[..sep]).to_string();
    let status: u16 = head
        .split_whitespace()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .unwrap_or(0);
    let mut body = buf[sep + 4..].to_vec();
    // honour content-length if we read more (pipelined junk) or less
    if let Some(cl) = head.to_ascii_lowercase().split("content-length:").nth(1) {
        if let Ok(n) = cl.lines().next().unwrap_or("").trim().parse::<usize>() {
            body.truncate(n);
        }
    }
    Ok((status, body))
}
