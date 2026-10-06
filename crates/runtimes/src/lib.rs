//! RuntimeProvider implementation (agent.md): every runtime download a game
//! driver needs — Temurin JRE, Mojang server jar, steamcmd — goes through
//! `HttpRuntimes` so drivers never do raw HTTP. Artifacts land in
//! `<root>/<kind>/<id>/` behind a `.complete` marker; downloads go to a
//! sibling staging dir and are renamed into place (atomic), checksums
//! (sha256 and/or sha1) are verified before the cache is populated.

use std::collections::VecDeque;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, OnceLock};

use async_trait::async_trait;
use game_driver_api::{ArchiveKind, FetchSpec, RuntimeProvider};
use http_body_util::BodyExt;
use hyper::header::{HeaderValue, HOST, LOCATION, USER_AGENT};
use hyper::{Request, Uri};
use hyper_util::rt::TokioIo;
use sha2::Digest;
use tokio::io::{AsyncBufReadExt, AsyncRead, BufReader};

const UA: &str = concat!("varde-agent/", env!("CARGO_PKG_VERSION"));

/// Runtime ids the provider can fetch on this host. Reported in node
/// capabilities so the scheduler can place games whose runtime isn't
/// installed yet — `HttpRuntimes` downloads it on demand.
pub fn provisionable() -> Vec<&'static str> {
    let mut out = Vec::new();
    if cfg!(any(
        all(target_os = "linux", target_arch = "x86_64"),
        all(target_os = "linux", target_arch = "aarch64"),
        all(target_os = "windows", target_arch = "x86_64"),
        all(target_os = "windows", target_arch = "aarch64"),
    )) {
        out.push("java");
    }
    if cfg!(any(
        all(target_os = "linux", target_arch = "x86_64"),
        all(target_os = "windows", target_arch = "x86_64"),
    )) {
        out.push("steamcmd");
    }
    out
}

/// Shared runtime cache rooted at `<data>/runtimes`.
pub struct HttpRuntimes {
    root: PathBuf,
    /// serializes concurrent fetches for the same destination
    locks: std::sync::Mutex<std::collections::HashMap<PathBuf, Arc<tokio::sync::Mutex<()>>>>,
}

impl HttpRuntimes {
    pub fn new(root: PathBuf) -> Self {
        Self {
            root,
            locks: std::sync::Mutex::new(std::collections::HashMap::new()),
        }
    }

    fn dest(&self, kind: &str, id: &str) -> PathBuf {
        self.root.join(kind).join(id)
    }

    fn done(path: &Path) -> bool {
        path.join(".complete").is_file()
    }

    fn dest_lock(&self, dest: &Path) -> Arc<tokio::sync::Mutex<()>> {
        self.locks
            .lock()
            .unwrap()
            .entry(dest.to_path_buf())
            .or_insert_with(|| Arc::new(tokio::sync::Mutex::new(())))
            .clone()
    }
}

#[async_trait]
impl RuntimeProvider for HttpRuntimes {
    fn runtime_path(&self, kind: &str, id: &str) -> Option<PathBuf> {
        let p = self.dest(kind, id);
        Self::done(&p).then_some(p)
    }

    async fn get_json(&self, url: &str) -> game_driver_api::Result<serde_json::Value> {
        let body = http_get(url, 8).await?;
        Ok(serde_json::from_slice(&body)?)
    }

    async fn fetch(
        &self,
        kind: &str,
        id: &str,
        spec: &FetchSpec,
    ) -> game_driver_api::Result<PathBuf> {
        let dest = self.dest(kind, id);
        if Self::done(&dest) {
            return Ok(dest);
        }
        // one fetcher per destination; re-check after acquiring so the
        // second caller of a concurrent pair returns the populated cache
        let lock = self.dest_lock(&dest);
        let _guard = lock.lock().await;
        if Self::done(&dest) {
            return Ok(dest);
        }
        let parent = dest.parent().unwrap().to_path_buf();
        // unique staging dir: guards against a stale staging left by a
        // killed run, and against a second agent sharing the cache
        let staging = parent.join(format!(
            "{}.staging-{}-{}",
            dest.file_name().unwrap().to_string_lossy(),
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_nanos()
        ));
        let staging2 = staging.clone();
        spawn_fs(move || {
            std::fs::create_dir_all(&parent)?;
            std::fs::create_dir_all(&staging2)?;
            Ok(())
        })
        .await?;
        let result = fetch_into(&staging, spec).await;
        if let Err(e) = result {
            let s = staging.clone();
            let _ = spawn_fs(move || {
                let _ = std::fs::remove_dir_all(&s);
                Ok(())
            })
            .await;
            return Err(e);
        }
        // publish atomically; fs ops stay off the async worker
        let st = staging.clone();
        let d = dest.clone();
        spawn_fs(move || {
            std::fs::write(st.join(".complete"), b"ok")?;
            std::fs::rename(&st, &d)?;
            Ok(())
        })
        .await?;
        Ok(dest)
    }

    async fn steam_app_install(&self, app_id: u32, dest: &Path) -> game_driver_api::Result<()> {
        let cmd = steamcmd(self).await?;
        let destination = dest.to_path_buf();
        spawn_fs(move || {
            std::fs::create_dir_all(destination)?;
            Ok(())
        })
        .await?;

        let mut retried_after_self_update = false;
        loop {
            let mut child = tokio::process::Command::new(&cmd)
                .arg("+force_install_dir")
                .arg(dest)
                .arg("+login")
                .arg("anonymous")
                .arg("+app_update")
                .arg(app_id.to_string())
                .arg("validate")
                .arg("+quit")
                .stdout(std::process::Stdio::piped())
                .stderr(std::process::Stdio::piped())
                .kill_on_drop(true)
                .spawn()?;
            let captured = Arc::new(Mutex::new(VecDeque::with_capacity(40)));
            let stdout = child
                .stdout
                .take()
                .map(|pipe| capture_output(pipe, captured.clone()));
            let stderr = child
                .stderr
                .take()
                .map(|pipe| capture_output(pipe, captured.clone()));
            let st = child.wait().await?;
            if let Some(stdout) = stdout {
                let _ = stdout.await;
            }
            if let Some(stderr) = stderr {
                let _ = stderr.await;
            }
            let lines = captured.lock().unwrap().iter().cloned().collect::<Vec<_>>();
            if st.success() {
                return Ok(());
            }
            if !retried_after_self_update && steamcmd_self_updated(&lines) {
                tracing::warn!(
                    status = %st,
                    "steamcmd exited non-zero after self-update; retrying once"
                );
                retried_after_self_update = true;
                continue;
            }
            return Err(format!("steamcmd exited {st}\nlast output:\n{}", lines.join("\n")).into());
        }
    }
}

fn steamcmd_self_updated(output: &[String]) -> bool {
    output.iter().any(|line| {
        let line = line.to_ascii_lowercase();
        line.contains("update complete, launching steamcmd")
            || line.contains("restarting steamcmd by request")
    })
}

fn capture_output<R>(pipe: R, captured: Arc<Mutex<VecDeque<String>>>) -> tokio::task::JoinHandle<()>
where
    R: AsyncRead + Unpin + Send + 'static,
{
    tokio::spawn(async move {
        let mut lines = BufReader::new(pipe).lines();
        while let Ok(Some(line)) = lines.next_line().await {
            let mut captured = captured.lock().unwrap();
            if captured.len() == 40 {
                captured.pop_front();
            }
            captured.push_back(line);
        }
    })
}

/// Path to the steamcmd binary, bootstrapping the tool itself via fetch().
/// Valve publishes no checksum for these artifacts, so we pin the sha256 we
/// observed (see pinned consts) and document it in docs/architecture/agent.md.
async fn steamcmd(rt: &HttpRuntimes) -> game_driver_api::Result<PathBuf> {
    #[cfg(windows)]
    {
        let dir = rt
            .fetch(
                "steamcmd",
                "windows",
                &FetchSpec {
                    url: "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip".into(),
                    file_name: "steamcmd.zip".into(),
                    sha256: Some(STEAMCMD_ZIP_SHA256.into()),
                    sha1: None,
                    archive: ArchiveKind::Zip,
                },
            )
            .await?;
        Ok(dir.join("steamcmd.exe"))
    }
    #[cfg(not(windows))]
    {
        let dir = rt
            .fetch(
                "steamcmd",
                "linux",
                &FetchSpec {
                    url: "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz"
                        .into(),
                    file_name: "steamcmd_linux.tar.gz".into(),
                    sha256: Some(STEAMCMD_TGZ_SHA256.into()),
                    sha1: None,
                    archive: ArchiveKind::Tgz,
                },
            )
            .await?;
        Ok(dir.join("steamcmd.sh"))
    }
}

/// sha256 of steamcmd_linux.tar.gz as served by steamcdn-a.akamaihd.net
/// (Valve publishes no checksum — pinned on first verified download).
pub const STEAMCMD_TGZ_SHA256: &str =
    "cebf0046bfd08cf45da6bc094ae47aa39ebf4155e5ede41373b579b8f1071e7c";
/// sha256 of steamcmd.zip (pinned; Valve publishes no checksum).
pub const STEAMCMD_ZIP_SHA256: &str =
    "7669b170dee42db8ee2273775ed7dfb2d173bdba1b849f70d2c7b379290bce13";

// ---------- minimal https GET with redirect following ----------

fn tls_config() -> game_driver_api::Result<Arc<rustls::ClientConfig>> {
    static CFG: OnceLock<Arc<rustls::ClientConfig>> = OnceLock::new();
    if let Some(c) = CFG.get() {
        return Ok(c.clone());
    }
    let roots = webpki_roots::TLS_SERVER_ROOTS
        .iter()
        .cloned()
        .collect::<rustls::RootCertStore>();
    let cfg = rustls::ClientConfig::builder_with_provider(
        rustls::crypto::ring::default_provider().into(),
    )
    .with_safe_default_protocol_versions()
    .map_err(|e| format!("tls: {e}"))?
    .with_root_certificates(roots)
    .with_no_client_auth();
    let cfg = Arc::new(cfg);
    let _ = CFG.set(cfg.clone());
    Ok(cfg)
}

/// GET `url` following ≤ `redirs` redirects; returns the final body bytes.
async fn http_get(url: &str, redirs: u32) -> game_driver_api::Result<Vec<u8>> {
    let uri: Uri = url.parse()?;
    let host = uri.host().ok_or("url has no host")?.to_string();
    let https = uri.scheme_str() == Some("https");
    let port = uri.port_u16().unwrap_or(if https { 443 } else { 80 });
    let stream = tokio::net::TcpStream::connect((host.as_str(), port)).await?;
    let path = uri.path_and_query().map(|p| p.as_str()).unwrap_or("/");
    let req = Request::get(path)
        .header(HOST, HeaderValue::from_str(&host)?)
        .header(USER_AGENT, UA)
        .body(http_body_util::Full::new(hyper::body::Bytes::new()))?;
    let resp = if https {
        let name = rustls::pki_types::ServerName::try_from(host.clone())?;
        let tls = tokio_rustls::TlsConnector::from(tls_config()?)
            .connect(name, stream)
            .await?;
        let (mut sender, conn) = hyper::client::conn::http1::handshake(TokioIo::new(tls)).await?;
        tokio::spawn(async move {
            let _ = conn.await;
        });
        sender.send_request(req).await?
    } else {
        let (mut sender, conn) =
            hyper::client::conn::http1::handshake(TokioIo::new(stream)).await?;
        tokio::spawn(async move {
            let _ = conn.await;
        });
        sender.send_request(req).await?
    };
    let status = resp.status().as_u16();
    if (300..400).contains(&status) {
        if redirs == 0 {
            return Err("too many redirects".into());
        }
        let loc = resp
            .headers()
            .get(LOCATION)
            .and_then(|v| v.to_str().ok())
            .ok_or("redirect without location")?;
        let next = if loc.starts_with('/') {
            format!(
                "{}://{}:{}{}",
                uri.scheme_str().unwrap_or("https"),
                host,
                port,
                loc
            )
        } else {
            loc.to_string()
        };
        return Box::pin(http_get(&next, redirs - 1)).await;
    }
    if !(200..300).contains(&status) {
        return Err(format!("GET {url} -> {status}").into());
    }
    Ok(resp.into_body().collect().await?.to_bytes().to_vec())
}

/// Blocking filesystem/CPU work (hashing, archive unpack, writes, renames)
/// must not run on the async worker — it stalls the heartbeat loop and
/// expires execution leases on big downloads (cold-start fencing bug).
async fn spawn_fs<T: Send + 'static>(
    f: impl FnOnce() -> game_driver_api::Result<T> + Send + 'static,
) -> game_driver_api::Result<T> {
    tokio::task::spawn_blocking(f)
        .await
        .map_err(|e| format!("blocking task: {e}"))?
}

/// Download + unpack `spec` into an existing staging dir. Network stays on
/// the async worker; hashing, writes and archive extraction are blocking.
async fn fetch_into(staging: &Path, spec: &FetchSpec) -> game_driver_api::Result<()> {
    let blob = staging.join(&spec.file_name);
    download_verified(
        &spec.url,
        &blob,
        spec.sha256.as_deref(),
        spec.sha1.as_deref(),
    )
    .await?;
    match spec.archive {
        ArchiveKind::File => {}
        ArchiveKind::Tgz => {
            let (b, s) = (blob.clone(), staging.to_path_buf());
            spawn_fs(move || {
                let f = std::fs::File::open(&b)?;
                tar::Archive::new(flate2::read::GzDecoder::new(f)).unpack(&s)?;
                let _ = std::fs::remove_file(&b);
                Ok(())
            })
            .await?;
        }
        ArchiveKind::Zip => {
            let (b, s) = (blob.clone(), staging.to_path_buf());
            spawn_fs(move || {
                let f = std::fs::File::open(&b)?;
                zip::ZipArchive::new(f)?.extract(&s)?;
                let _ = std::fs::remove_file(&b);
                Ok(())
            })
            .await?;
        }
    }
    Ok(())
}

/// Download `url` to `file` then verify sha256/sha1 (whichever is provided).
async fn download_verified(
    url: &str,
    file: &Path,
    sha256: Option<&str>,
    sha1: Option<&str>,
) -> game_driver_api::Result<()> {
    tracing::info!(url, dest = %file.display(), "runtime download");
    let body = http_get(url, 8).await?;
    let (s256, s1) = (sha256.map(String::from), sha1.map(String::from));
    let f = file.to_path_buf();
    let u = url.to_string();
    spawn_fs(move || {
        if let Some(want) = s256 {
            let got = hex::encode(sha2::Sha256::digest(&body));
            if !got.eq_ignore_ascii_case(&want) {
                return Err(format!("sha256 mismatch for {u}: got {got} want {want}").into());
            }
        }
        if let Some(want) = s1 {
            let got = hex::encode(sha1::Sha1::digest(&body));
            if !got.eq_ignore_ascii_case(&want) {
                return Err(format!("sha1 mismatch for {u}: got {got} want {want}").into());
            }
        }
        std::fs::write(&f, &body)?;
        Ok(())
    })
    .await
}

/// Find `bin/java` (or `bin/java.exe`) under a fetched runtime dir; Temurin
/// archives nest a `jdk-*/` or `jdk*/` top dir.
pub fn find_java(dir: &Path) -> Option<PathBuf> {
    let exe = if cfg!(windows) { "java.exe" } else { "java" };
    let direct = dir.join("bin").join(exe);
    if direct.is_file() {
        return Some(direct);
    }
    let rd = std::fs::read_dir(dir).ok()?;
    for e in rd.flatten() {
        if e.file_type().ok()?.is_dir() {
            let p = e.path().join("bin").join(exe);
            if p.is_file() {
                return Some(p);
            }
        }
    }
    None
}

/// Parse a `java -version` output line for the major version number.
pub fn java_major(out: &str) -> Option<u32> {
    let q = out.find("version \"")? + 9;
    let rest = &out[q..];
    let end = rest.find('"')?;
    let ver = &rest[..end];
    let major = ver.split('.').next()?;
    let n: u32 = major.parse().ok()?;
    // "1.8.x" style
    if n == 1 {
        ver.split('.').nth(1)?.parse().ok()
    } else {
        Some(n)
    }
}

/// `java` on PATH with major >= `min`, else None. `candidates` are extra
/// binaries to try first (e.g. a previously fetched JRE).
pub async fn system_java(min: u32, candidates: &[PathBuf]) -> Option<PathBuf> {
    for p in candidates.iter().cloned().chain([PathBuf::from("java")]) {
        let out = tokio::process::Command::new(&p)
            .arg("-version")
            .output()
            .await;
        if let Ok(o) = out {
            let text = format!(
                "{}{}",
                String::from_utf8_lossy(&o.stdout),
                String::from_utf8_lossy(&o.stderr)
            );
            if let Some(m) = java_major(&text) {
                if m >= min {
                    return Some(p);
                }
            }
        }
    }
    None
}

#[cfg(all(test, target_os = "linux", target_arch = "x86_64"))]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering as AOrd};

    #[test]
    fn provisionable_linux_x64() {
        let p = super::provisionable();
        assert!(p.contains(&"java"));
        assert!(p.contains(&"steamcmd"));
    }

    #[test]
    fn steamcmd_self_update_is_retryable_but_app_errors_alone_are_not() {
        assert!(super::steamcmd_self_updated(&[
            "Update complete, launching Steamcmd...".into(),
            "ERROR! Failed to install app '896660' (Missing configuration)".into(),
        ]));
        assert!(!super::steamcmd_self_updated(&[
            "ERROR! Failed to install app '896660' (Missing configuration)".into(),
        ]));
    }

    // minimal http/1.0 server serving one fixed body, counting requests
    async fn serve_once(
        count: Arc<AtomicUsize>,
        body: Vec<u8>,
    ) -> (String, tokio::task::JoinHandle<()>) {
        let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = l.local_addr().unwrap();
        let h = tokio::spawn(async move {
            while let Ok((mut s, _)) = l.accept().await {
                count.fetch_add(1, AOrd::SeqCst);
                use tokio::io::{AsyncReadExt, AsyncWriteExt};
                let mut buf = [0u8; 4096];
                let _ = s.read(&mut buf).await;
                let resp = format!("HTTP/1.0 200 OK\r\nContent-Length: {}\r\n\r\n", body.len());
                let _ = s.write_all(resp.as_bytes()).await;
                let _ = s.write_all(&body).await;
            }
        });
        (format!("http://{addr}/blob.tgz"), h)
    }

    fn make_tgz(payload: &[u8]) -> Vec<u8> {
        let mut b = tar::Builder::new(Vec::new());
        let mut h = tar::Header::new_gnu();
        h.set_size(payload.len() as u64);
        h.set_cksum();
        b.append_data(&mut h, "file.txt", payload).unwrap();
        let tgz = b.into_inner().unwrap();
        let mut e = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::fast());
        use std::io::Write;
        e.write_all(&tgz).unwrap();
        e.finish().unwrap()
    }

    #[tokio::test]
    async fn concurrent_fetch_same_dest() {
        let tmp = tempfile::tempdir().unwrap();
        let rt = Arc::new(HttpRuntimes::new(tmp.path().join("runtimes")));
        let count = Arc::new(AtomicUsize::new(0));
        let tgz = make_tgz(b"hello runtime");
        let (url, _srv) = serve_once(count.clone(), tgz).await;
        let spec = FetchSpec {
            url,
            file_name: "b.tgz".into(),
            sha256: None,
            sha1: None,
            archive: ArchiveKind::Tgz,
        };
        let (a, b) = (rt.clone(), rt.clone());
        let (r1, r2) = tokio::join!(a.fetch("kind", "id", &spec), b.fetch("kind", "id", &spec));
        let p1 = r1.unwrap();
        let p2 = r2.unwrap();
        assert_eq!(p1, p2);
        assert_eq!(count.load(AOrd::SeqCst), 1, "exactly one GET expected");
        let got = std::fs::read_to_string(p1.join("file.txt")).unwrap();
        assert_eq!(got, "hello runtime");
        assert!(p1.join(".complete").is_file());
    }

    #[tokio::test]
    async fn fetch_extracts_deflated_zip() {
        let mut w = zip::ZipWriter::new(std::io::Cursor::new(Vec::new()));
        let opts = zip::write::SimpleFileOptions::default()
            .compression_method(zip::CompressionMethod::Deflated);
        w.start_file("bin/file.txt", opts).unwrap();
        use std::io::Write;
        w.write_all(b"hello zip runtime").unwrap();
        let body = w.finish().unwrap().into_inner();
        let tmp = tempfile::tempdir().unwrap();
        let rt = HttpRuntimes::new(tmp.path().join("runtimes"));
        let (url, _srv) = serve_once(Arc::new(AtomicUsize::new(0)), body).await;
        let spec = FetchSpec {
            url,
            file_name: "b.zip".into(),
            sha256: None,
            sha1: None,
            archive: ArchiveKind::Zip,
        };
        let p = rt.fetch("kind", "zip", &spec).await.unwrap();
        let got = std::fs::read_to_string(p.join("bin").join("file.txt")).unwrap();
        assert_eq!(got, "hello zip runtime");
    }
}
