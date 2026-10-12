//! Minimal HTTP(S) client for the control plane: hyper 1 / http1, rustls
//! with the ring provider for `https://` URLs, plain TCP for `http://`.

use std::sync::Arc;
use std::time::Duration;

use http_body_util::{BodyExt, Full};
use hyper::body::{Bytes, Incoming};
use hyper::{Method, Request, Response, Uri};
use hyper_util::rt::TokioIo;

use crate::sign;
use crate::types::ErrorBody;

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("bad url: {0}")]
    Url(String),
    #[error("http: {0}")]
    Http(String),
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    #[error("json: {0}")]
    Json(#[from] serde_json::Error),
    /// server returned an error status with a structured body
    #[error("api {status} {code}: {message}")]
    Api {
        status: u16,
        code: String,
        message: String,
        details: Option<serde_json::Value>,
    },
}

impl Error {
    pub fn api_code(&self) -> Option<&str> {
        match self {
            Error::Api { code, .. } => Some(code),
            _ => None,
        }
    }
    pub fn status(&self) -> Option<u16> {
        match self {
            Error::Api { status, .. } => Some(*status),
            _ => None,
        }
    }
}

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Clone)]
pub struct CpClient {
    base: String,
    bearer: Option<String>,
    node_key: Option<ed25519_dalek::SigningKey>,
    node_id: Option<String>,
}

impl CpClient {
    pub fn new(base: &str) -> Result<CpClient> {
        let base = base.trim_end_matches('/').to_string();
        if !base.starts_with("http://") && !base.starts_with("https://") {
            return Err(Error::Url(base));
        }
        Ok(CpClient {
            base,
            bearer: None,
            node_key: None,
            node_id: None,
        })
    }

    pub fn with_bearer(mut self, token: impl Into<String>) -> CpClient {
        self.bearer = Some(token.into());
        self
    }

    /// Sign requests with the node key (agent identity after enrollment).
    pub fn with_node_key(
        mut self,
        key: ed25519_dalek::SigningKey,
        node_id: impl Into<String>,
    ) -> CpClient {
        self.node_key = Some(key);
        self.node_id = Some(node_id.into());
        self
    }

    /// Raw call: returns (status, body bytes). Signed when a node key is set.
    /// Follows 301/302/303/307/308 redirects (bounded): 301/302/303 replay as
    /// a bare GET, 307/308 keep method + body. Authorization and node-sign
    /// headers are only forwarded while the redirect stays on the original
    /// origin — a CP that redirects cross-origin gets an unsigned retry.
    pub async fn call(
        &self,
        method: &str,
        path: &str,
        body: Option<&[u8]>,
    ) -> Result<(u16, Vec<u8>)> {
        let mut method =
            Method::from_bytes(method.as_bytes()).map_err(|e| Error::Http(e.to_string()))?;
        let mut url = format!("{}{}", self.base, path);
        let base_origin = origin(&url)?;
        let mut body_bytes = body.map(|b| b.to_vec()).unwrap_or_default();
        let mut send_auth = true;
        for _ in 0..MAX_REDIRECTS {
            let (status, location, bytes) = self
                .call_once(&method, &url, &body_bytes, send_auth)
                .await?;
            if !matches!(status, 301 | 302 | 303 | 307 | 308) {
                return Ok((status, bytes));
            }
            let loc = location
                .ok_or_else(|| Error::Http(format!("{method} {url}: redirect without location")))?;
            let next = resolve_location(&url, &loc)?;
            if origin(&next)? != base_origin {
                send_auth = false;
            }
            if status != 307 && status != 308 && method != Method::GET && method != Method::HEAD {
                method = Method::GET;
                body_bytes.clear();
            }
            url = next;
        }
        Err(Error::Http("too many redirects".into()))
    }

    /// One request, no redirect handling: (status, location header, body).
    async fn call_once(
        &self,
        method: &Method,
        url: &str,
        body_bytes: &[u8],
        send_auth: bool,
    ) -> Result<(u16, Option<String>, Vec<u8>)> {
        let uri: Uri = url.parse().map_err(|e| Error::Url(format!("{url}: {e}")))?;
        let https = uri.scheme_str() == Some("https");
        let host = uri
            .host()
            .ok_or_else(|| Error::Url(uri.to_string()))?
            .to_string();
        let port = uri.port_u16().unwrap_or(if https { 443 } else { 80 });
        let req_path = uri
            .path_and_query()
            .map(|pq| pq.as_str())
            .unwrap_or("/")
            .to_string();

        let mut req = Request::builder()
            .method(method)
            .uri(&uri)
            .header("host", host.clone())
            .header("content-type", "application/json")
            .header("content-length", body_bytes.len().to_string());
        if send_auth {
            if let Some(tok) = &self.bearer {
                req = req.header("authorization", format!("Bearer {tok}"));
            }
            if let (Some(key), Some(node)) = (&self.node_key, &self.node_id) {
                let ts = now_ms();
                let sig = sign::sign(key, method.as_str(), &req_path, ts, body_bytes);
                req = req
                    .header(sign::HDR_NODE, node.as_str())
                    .header(sign::HDR_TIMESTAMP, ts.to_string())
                    .header(sign::HDR_SIGNATURE, sig);
            }
        }
        let req = req
            .body(Full::new(Bytes::from(body_bytes.to_vec())))
            .map_err(|e| Error::Http(e.to_string()))?;

        let stream = tokio::time::timeout(
            Duration::from_secs(15),
            tokio::net::TcpStream::connect((host.as_str(), port)),
        )
        .await
        .map_err(|_| {
            Error::Io(std::io::Error::new(
                std::io::ErrorKind::TimedOut,
                "connect timeout",
            ))
        })?
        .map_err(Error::Io)?;

        let resp: Response<Incoming> = if https {
            let cfg = rustls_client_config()?;
            let server_name = rustls::pki_types::ServerName::try_from(host.clone())
                .map_err(|e| Error::Url(format!("{host}: {e}")))?;
            let tls = tokio_rustls::TlsConnector::from(cfg)
                .connect(server_name, stream)
                .await
                .map_err(Error::Io)?;
            let (mut sender, conn) = hyper::client::conn::http1::handshake(TokioIo::new(tls))
                .await
                .map_err(|e| Error::Http(e.to_string()))?;
            tokio::spawn(async move {
                let _ = conn.await;
            });
            sender
                .send_request(req)
                .await
                .map_err(|e| Error::Http(e.to_string()))?
        } else {
            let (mut sender, conn) = hyper::client::conn::http1::handshake(TokioIo::new(stream))
                .await
                .map_err(|e| Error::Http(e.to_string()))?;
            tokio::spawn(async move {
                let _ = conn.await;
            });
            sender
                .send_request(req)
                .await
                .map_err(|e| Error::Http(e.to_string()))?
        };

        let status = resp.status().as_u16();
        let location = resp
            .headers()
            .get("location")
            .and_then(|v| v.to_str().ok())
            .map(|s| s.to_string());
        let bytes = resp
            .into_body()
            .collect()
            .await
            .map_err(|e| Error::Http(e.to_string()))?
            .to_bytes()
            .to_vec();
        Ok((status, location, bytes))
    }

    /// JSON call that maps error statuses into `Error::Api`.
    pub async fn json<B: serde::Serialize, R: serde::de::DeserializeOwned>(
        &self,
        method: &str,
        path: &str,
        body: Option<&B>,
    ) -> Result<R> {
        let bytes = match body {
            Some(b) => Some(serde_json::to_vec(b)?),
            None => None,
        };
        let (status, out) = self.call(method, path, bytes.as_deref()).await?;
        if !(200..300).contains(&status) {
            return Err(api_error(status, &out));
        }
        if out.is_empty() {
            return serde_json::from_str("null").map_err(Error::Json);
        }
        Ok(serde_json::from_slice(&out)?)
    }

    /// JSON call returning the raw status (endpoints with several 2xx codes).
    pub async fn json_status<B: serde::Serialize>(
        &self,
        method: &str,
        path: &str,
        body: Option<&B>,
    ) -> Result<(u16, Vec<u8>)> {
        let bytes = match body {
            Some(b) => Some(serde_json::to_vec(b)?),
            None => None,
        };
        self.call(method, path, bytes.as_deref()).await
    }
}

const MAX_REDIRECTS: usize = 5;

/// scheme://authority of an http(s) URL — the auth-forwarding boundary for
/// redirects.
fn origin(url: &str) -> Result<String> {
    let uri: Uri = url.parse().map_err(|_| Error::Url(url.to_string()))?;
    let scheme = uri
        .scheme_str()
        .ok_or_else(|| Error::Url(url.to_string()))?;
    if scheme != "http" && scheme != "https" {
        return Err(Error::Url(url.to_string()));
    }
    let authority = uri.authority().ok_or_else(|| Error::Url(url.to_string()))?;
    Ok(format!("{scheme}://{authority}"))
}

/// Resolve a Location header against the current URL: absolute URLs pass
/// through, root-relative and scheme-relative resolve against the current
/// origin/scheme. Only http(s) targets are allowed.
fn resolve_location(cur: &str, loc: &str) -> Result<String> {
    let next = if loc.starts_with('/') && !loc.starts_with("//") {
        format!("{}{}", origin(cur)?, loc)
    } else if let Some(rest) = loc.strip_prefix("//") {
        let scheme = cur.split(':').next().unwrap_or("https");
        format!("{scheme}://{rest}")
    } else {
        loc.to_string()
    };
    origin(&next)?;
    Ok(next)
}

/// GET an arbitrary http(s) URL, following redirects (GitHub release assets
/// 302 to the objects CDN). Not part of CpClient: no base, no signing.
/// `max_bytes` bounds the response body (a stalled or hostile feed can't
/// hang the caller or exhaust memory).
pub async fn fetch_url(url: &str, max_bytes: usize) -> Result<Vec<u8>> {
    let mut url = url.to_string();
    for _ in 0..5 {
        let (status, headers, bytes) =
            tokio::time::timeout(Duration::from_secs(30), get_once(&url, max_bytes))
                .await
                .map_err(|_| Error::Http(format!("GET {url}: timed out")))??;
        match status {
            200..=299 => return Ok(bytes),
            301 | 302 | 303 | 307 | 308 => {
                let loc = headers
                    .iter()
                    .find(|(k, _)| k.eq_ignore_ascii_case("location"))
                    .map(|(_, v)| v.clone())
                    .ok_or_else(|| Error::Http("redirect without location".into()))?;
                // relative locations resolve against the current URL's origin
                if loc.starts_with('/') && !loc.starts_with("//") {
                    let cur: Uri = url.parse().map_err(|_| Error::Url(url.to_string()))?;
                    let scheme = cur.scheme_str().unwrap_or("https");
                    let authority = cur.authority().ok_or_else(|| Error::Url(url.clone()))?;
                    url = format!("{scheme}://{authority}{loc}");
                } else {
                    url = loc;
                }
            }
            _ => return Err(Error::Http(format!("GET {url}: status {status}"))),
        }
    }
    Err(Error::Http("too many redirects".into()))
}

/// One bare GET: (status, response headers, body). Used by fetch_url.
async fn get_once(url: &str, max_bytes: usize) -> Result<(u16, Vec<(String, String)>, Vec<u8>)> {
    let uri: Uri = url.parse().map_err(|_| Error::Url(url.to_string()))?;
    let https = uri.scheme_str() == Some("https");
    if !https && uri.scheme_str() != Some("http") {
        return Err(Error::Url(url.to_string()));
    }
    let host = uri
        .host()
        .ok_or_else(|| Error::Url(url.to_string()))?
        .to_string();
    let port = uri.port_u16().unwrap_or(if https { 443 } else { 80 });
    let req = Request::builder()
        .method(Method::GET)
        .uri(&uri)
        .header("host", host.clone())
        .header(
            "user-agent",
            concat!("varde-agent/", env!("CARGO_PKG_VERSION")),
        )
        .body(Full::new(Bytes::new()))
        .map_err(|e| Error::Http(e.to_string()))?;
    let stream = tokio::time::timeout(
        Duration::from_secs(15),
        tokio::net::TcpStream::connect((host.as_str(), port)),
    )
    .await
    .map_err(|_| {
        Error::Io(std::io::Error::new(
            std::io::ErrorKind::TimedOut,
            "connect timeout",
        ))
    })?
    .map_err(Error::Io)?;
    let resp: Response<Incoming> = if https {
        let cfg = rustls_client_config()?;
        let server_name = rustls::pki_types::ServerName::try_from(host.clone())
            .map_err(|e| Error::Url(format!("{host}: {e}")))?;
        let tls = tokio_rustls::TlsConnector::from(cfg)
            .connect(server_name, stream)
            .await
            .map_err(Error::Io)?;
        let (mut sender, conn) = hyper::client::conn::http1::handshake(TokioIo::new(tls))
            .await
            .map_err(|e| Error::Http(e.to_string()))?;
        tokio::spawn(async move {
            let _ = conn.await;
        });
        sender
            .send_request(req)
            .await
            .map_err(|e| Error::Http(e.to_string()))?
    } else {
        let (mut sender, conn) = hyper::client::conn::http1::handshake(TokioIo::new(stream))
            .await
            .map_err(|e| Error::Http(e.to_string()))?;
        tokio::spawn(async move {
            let _ = conn.await;
        });
        sender
            .send_request(req)
            .await
            .map_err(|e| Error::Http(e.to_string()))?
    };
    let status = resp.status().as_u16();
    let headers: Vec<(String, String)> = resp
        .headers()
        .iter()
        .map(|(k, v)| (k.as_str().to_string(), v.to_str().unwrap_or("").to_string()))
        .collect();
    let bytes = http_body_util::Limited::new(resp.into_body(), max_bytes)
        .collect()
        .await
        .map_err(|e| Error::Http(format!("GET {url}: {e}")))?
        .to_bytes()
        .to_vec();
    Ok((status, headers, bytes))
}

pub fn api_error(status: u16, body: &[u8]) -> Error {
    match serde_json::from_slice::<ErrorBody>(body) {
        Ok(e) => Error::Api {
            status,
            code: e.code,
            message: e.message,
            details: e.details,
        },
        Err(_) => Error::Api {
            status,
            code: "http_error".into(),
            message: String::from_utf8_lossy(body).chars().take(200).collect(),
            details: None,
        },
    }
}

fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

fn rustls_client_config() -> Result<Arc<rustls::ClientConfig>> {
    use std::sync::OnceLock;
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
    .map_err(|e| Error::Http(e.to_string()))?
    .with_root_certificates(roots)
    .with_no_client_auth();
    let cfg = Arc::new(cfg);
    let _ = CFG.set(cfg.clone());
    Ok(cfg)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn root_relative_resolves_against_current_origin() {
        assert_eq!(
            resolve_location("https://a.example:8443/v1/x?y=1", "/v2/z").unwrap(),
            "https://a.example:8443/v2/z"
        );
    }

    #[test]
    fn scheme_relative_keeps_current_scheme() {
        assert_eq!(
            resolve_location("http://a.example/v1/x", "//b.example/q").unwrap(),
            "http://b.example/q"
        );
    }

    #[test]
    fn absolute_location_passes_through() {
        assert_eq!(
            resolve_location("https://a.example/x", "https://b.example/y?z=1").unwrap(),
            "https://b.example/y?z=1"
        );
    }

    #[test]
    fn non_http_targets_rejected() {
        for loc in [
            "file:///etc/passwd",
            "javascript:alert(1)",
            "gopher://b/1",
            "not a url",
        ] {
            assert!(
                resolve_location("https://a.example/x", loc).is_err(),
                "{loc}"
            );
        }
    }

    #[test]
    fn origin_includes_explicit_port() {
        assert_eq!(
            origin("https://a.example:8443/p").unwrap(),
            "https://a.example:8443"
        );
        assert_eq!(origin("http://a.example/p").unwrap(), "http://a.example");
        assert!(origin("ftp://a.example/p").is_err());
    }
}
