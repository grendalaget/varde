//! Generated client and IPC plumbing for the agent ↔ mesh local protocol.
//!
//! Transport: Unix domain socket on Unix, named pipe on Windows. Never TCP.

use std::io;
use std::path::Path;

use tonic::transport::{Channel, Endpoint};

pub mod pb {
    tonic::include_proto!("varde.mesh.v1");
}

pub use pb::mesh_service_client::MeshServiceClient;

/// Computes the IPC endpoint for a given data directory.
///
/// Unix: `<data>/run/mesh.sock`. Windows: `\\.\pipe\varde-mesh-<8 hex of
/// sha256(data dir)>` so multiple installs/users don't collide.
pub fn ipc_endpoint(data_dir: &Path) -> String {
    platform_ipc_endpoint(data_dir)
}

#[cfg(unix)]
fn platform_ipc_endpoint(data_dir: &Path) -> String {
    data_dir.join("run").join("mesh.sock").display().to_string()
}

#[cfg(windows)]
fn platform_ipc_endpoint(data_dir: &Path) -> String {
    use sha2::{Digest, Sha256};
    let hash = Sha256::digest(data_dir.to_string_lossy().as_bytes());
    format!("\\\\.\\pipe\\varde-mesh-{}", &hex::encode(hash)[..8])
}

/// Connects to the mesh daemon over the platform IPC transport.
pub async fn connect(endpoint: &str) -> io::Result<MeshServiceClient<Channel>> {
    let endpoint = endpoint.to_string();
    let channel = Endpoint::try_from("http://[::]:50051")
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e.to_string()))?
        .connect_with_connector(tower::service_fn(move |_: tonic::transport::Uri| {
            let endpoint = endpoint.clone();
            async move { platform_connect(&endpoint).await }
        }))
        .await
        .map_err(|e| io::Error::new(io::ErrorKind::ConnectionRefused, e.to_string()))?;
    Ok(MeshServiceClient::new(channel))
}

#[cfg(unix)]
async fn platform_connect(
    path: &str,
) -> io::Result<hyper_util::rt::TokioIo<tokio::net::UnixStream>> {
    let stream = tokio::net::UnixStream::connect(path).await?;
    Ok(hyper_util::rt::TokioIo::new(stream))
}

#[cfg(windows)]
async fn platform_connect(
    path: &str,
) -> io::Result<hyper_util::rt::TokioIo<tokio::net::windows::named_pipe::NamedPipeClient>> {
    let client = tokio::net::windows::named_pipe::ClientOptions::new().open(path)?;
    Ok(hyper_util::rt::TokioIo::new(client))
}
