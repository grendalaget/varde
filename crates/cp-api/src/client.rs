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
    pub async fn call(
        &self,
        method: &str,
        path: &str,
        body: Option<&[u8]>,
    ) -> Result<(u16, Vec<u8>)> {
        let uri: Uri = format!("{}{}", self.base, path)
            .parse()
            .map_err(|e| Error::Url(format!("{path}: {e}")))?;
        let https = uri.scheme_str() == Some("https");
        let host = uri
            .host()
            .ok_or_else(|| Error::Url(uri.to_string()))?
            .to_string();
        let port = uri.port_u16().unwrap_or(if https { 443 } else { 80 });

        let body_bytes = body.map(|b| b.to_vec()).unwrap_or_default();
        let mut req = Request::builder()
            .method(Method::from_bytes(method.as_bytes()).map_err(|e| Error::Http(e.to_string()))?)
            .uri(&uri)
            .header("host", host.clone())
            .header("content-type", "application/json")
            .header("content-length", body_bytes.len().to_string());
        if let Some(tok) = &self.bearer {
            req = req.header("authorization", format!("Bearer {tok}"));
        }
        if let (Some(key), Some(node)) = (&self.node_key, &self.node_id) {
            let ts = now_ms();
            let sig = sign::sign(key, method, path, ts, &body_bytes);
            req = req
                .header(sign::HDR_NODE, node.as_str())
                .header(sign::HDR_TIMESTAMP, ts.to_string())
                .header(sign::HDR_SIGNATURE, sig);
        }
        let req = req
            .body(Full::new(Bytes::from(body_bytes)))
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
        let bytes = resp
            .into_body()
            .collect()
            .await
            .map_err(|e| Error::Http(e.to_string()))?
            .to_bytes()
            .to_vec();
        Ok((status, bytes))
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
