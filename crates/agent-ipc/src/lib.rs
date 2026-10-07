//! Local IPC between the agent (server) and desktop clients such as the tray.
//!
//! Transport: the named pipe `\\.\pipe\varde-agent` on Windows,
//! `<data>/run/agent.sock` on Unix. Never TCP. The protocol is a fixed set of
//! typed operations (status, link) — not a command channel (I9).
#![allow(clippy::result_large_err)] // generated tonic code returns big Status errs

use std::io;
use std::path::Path;

use tonic::transport::{Channel, Endpoint};

pub mod pb {
    tonic::include_proto!("varde.agent.v1");
}

#[cfg(feature = "server")]
pub mod server;

pub use pb::local_service_client::LocalServiceClient;

/// The agent service's pipe. One agent service per Windows machine.
pub const PIPE_NAME: &str = r"\\.\pipe\varde-agent";

/// The hosted Varde every install points at unless the user picks another
/// server (`server.url`, `--link`, installer /CPURL, `enroll --server`).
pub const DEFAULT_CP_URL: &str = "https://varde.games";

/// IPC endpoint for an agent whose data dir is `data_dir`.
pub fn endpoint(data_dir: &Path) -> String {
    #[cfg(windows)]
    {
        let _ = data_dir;
        PIPE_NAME.to_string()
    }
    #[cfg(not(windows))]
    {
        data_dir
            .join("run")
            .join("agent.sock")
            .display()
            .to_string()
    }
}

/// Connects to the agent. On Windows the pipe must be served from session 0
/// (i.e. by a service), so a user process can't impersonate the agent;
/// `VARDE_DEV_ALLOW_USER_AGENT=1` lifts that for a foreground dev agent.
pub async fn connect(endpoint: &str) -> io::Result<LocalServiceClient<Channel>> {
    let endpoint = endpoint.to_string();
    let channel = Endpoint::try_from("http://[::]:50051")
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e.to_string()))?
        .connect_with_connector(tower::service_fn(move |_: tonic::transport::Uri| {
            let endpoint = endpoint.clone();
            async move { platform_connect(&endpoint).await }
        }))
        .await
        .map_err(|e| {
            // tonic's top-level message is just "transport error"
            let mut msg = e.to_string();
            let mut src = std::error::Error::source(&e);
            while let Some(s) = src {
                msg = format!("{msg}: {s}");
                src = s.source();
            }
            io::Error::new(io::ErrorKind::ConnectionRefused, msg)
        })?;
    Ok(LocalServiceClient::new(channel))
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
    use windows_sys::Win32::Foundation::ERROR_PIPE_BUSY;
    let mut tries = 0;
    let client = loop {
        match win::open_client(path) {
            Ok(c) => break c,
            Err(e) if e.raw_os_error() == Some(ERROR_PIPE_BUSY as i32) && tries < 20 => {
                tries += 1;
                tokio::time::sleep(std::time::Duration::from_millis(50)).await;
            }
            Err(e) => return Err(e),
        }
    };
    if std::env::var_os("VARDE_DEV_ALLOW_USER_AGENT").is_none()
        && win::server_session(&client)? != 0
    {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "agent pipe is not served by the Varde service",
        ));
    }
    Ok(hyper_util::rt::TokioIo::new(client))
}

#[cfg(windows)]
mod win {
    use std::io;
    use std::os::windows::io::{AsRawHandle, RawHandle};

    use tokio::net::windows::named_pipe::NamedPipeClient;
    use windows_sys::Win32::Foundation::INVALID_HANDLE_VALUE;
    use windows_sys::Win32::Storage::FileSystem::{
        CreateFileW, FILE_FLAG_OVERLAPPED, FILE_GENERIC_READ, FILE_WRITE_DATA, OPEN_EXISTING,
        SECURITY_IDENTIFICATION, SECURITY_SQOS_PRESENT,
    };
    use windows_sys::Win32::System::Pipes::GetNamedPipeServerSessionId;

    /// Read + write data only: the pipe ACL grants interactive users exactly
    /// this, withholding FILE_CREATE_PIPE_INSTANCE so they can't add rogue
    /// instances. Identification level: the agent may inspect, never act as us.
    pub fn open_client(path: &str) -> io::Result<NamedPipeClient> {
        let wide: Vec<u16> = path.encode_utf16().chain(std::iter::once(0)).collect();
        // SAFETY: valid NUL-terminated path; the handle is owned by the
        // NamedPipeClient on success.
        unsafe {
            let h = CreateFileW(
                wide.as_ptr(),
                FILE_GENERIC_READ | FILE_WRITE_DATA,
                0,
                std::ptr::null(),
                OPEN_EXISTING,
                FILE_FLAG_OVERLAPPED | SECURITY_SQOS_PRESENT | SECURITY_IDENTIFICATION,
                std::ptr::null_mut(),
            );
            if h == INVALID_HANDLE_VALUE {
                return Err(io::Error::last_os_error());
            }
            NamedPipeClient::from_raw_handle(h as RawHandle)
        }
    }

    pub fn server_session(c: &NamedPipeClient) -> io::Result<u32> {
        let mut id = u32::MAX;
        // SAFETY: valid pipe handle and out pointer.
        if unsafe { GetNamedPipeServerSessionId(c.as_raw_handle() as _, &mut id) } == 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(id)
    }
}
