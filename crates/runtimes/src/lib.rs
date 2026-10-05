//! RuntimeProvider implementation (agent.md): every runtime download a game
//! driver needs — Temurin JRE, Mojang server jar, steamcmd — goes through
//! `HttpRuntimes` so drivers never do raw HTTP. Artifacts land in
//! `<root>/<kind>/<id>/` behind a `.complete` marker; downloads go to a
//! sibling staging dir and are renamed into place (atomic), checksums
//! (sha256 and/or sha1) are verified before the cache is populated.

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::OnceLock;

use async_trait::async_trait;
use game_driver_api::{ArchiveKind, FetchSpec, RuntimeProvider};
use http_body_util::BodyExt;
use hyper::header::{HeaderValue, HOST, LOCATION, USER_AGENT};
use hyper::{Request, Uri};
use hyper_util::rt::TokioIo;
use sha2::Digest;

const UA: &str = concat!("varde-agent/", env!("CARGO_PKG_VERSION"));

/// Shared runtime cache rooted at `<data>/runtimes`.
pub struct HttpRuntimes {
    root: PathBuf,
}

impl HttpRuntimes {
    pub fn new(root: PathBuf) -> Self {
        Self { root }
    }

    fn dest(&self, kind: &str, id: &str) -> PathBuf {
        self.root.join(kind).join(id)
    }

    fn done(path: &Path) -> bool {
        path.join(".complete").is_file()
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
        std::fs::create_dir_all(dest.parent().unwrap())?;
        let staging = dest.with_extension("staging");
        let _ = std::fs::remove_dir_all(&staging);
        std::fs::create_dir_all(&staging)?;
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
                let f = std::fs::File::open(&blob)?;
                tar::Archive::new(flate2::read::GzDecoder::new(f)).unpack(&staging)?;
                let _ = std::fs::remove_file(&blob);
            }
            ArchiveKind::Zip => {
                let f = std::fs::File::open(&blob)?;
                zip::ZipArchive::new(f)?.extract(&staging)?;
                let _ = std::fs::remove_file(&blob);
            }
        }
        if dest.exists() {
            // another exec won the race; drop our staging copy
            let _ = std::fs::remove_dir_all(&staging);
            return Ok(dest);
        }
        std::fs::write(staging.join(".complete"), b"ok")?;
        std::fs::rename(&staging, &dest)?;
        Ok(dest)
    }

    async fn steam_app_install(&self, app_id: u32, dest: &Path) -> game_driver_api::Result<()> {
        let cmd = steamcmd(self).await?;
        std::fs::create_dir_all(dest)?;
        let st = tokio::process::Command::new(cmd)
            .arg("+force_install_dir")
            .arg(dest)
            .arg("+login")
            .arg("anonymous")
            .arg("+app_update")
            .arg(app_id.to_string())
            .arg("validate")
            .arg("+quit")
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()
            .await?;
        if !st.success() {
            return Err(format!("steamcmd exited {st}").into());
        }
        Ok(())
    }
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

/// Download `url` to `file` then verify sha256/sha1 (whichever is provided).
async fn download_verified(
    url: &str,
    file: &Path,
    sha256: Option<&str>,
    sha1: Option<&str>,
) -> game_driver_api::Result<()> {
    tracing::info!(url, dest = %file.display(), "runtime download");
    let body = http_get(url, 8).await?;
    if let Some(want) = sha256 {
        let got = hex::encode(sha2::Sha256::digest(&body));
        if !got.eq_ignore_ascii_case(want) {
            return Err(format!("sha256 mismatch for {url}: got {got} want {want}").into());
        }
    }
    if let Some(want) = sha1 {
        let got = hex::encode(sha1::Sha1::digest(&body));
        if !got.eq_ignore_ascii_case(want) {
            return Err(format!("sha1 mismatch for {url}: got {got} want {want}").into());
        }
    }
    std::fs::write(file, &body)?;
    Ok(())
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
