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
// Callers display RPC failures without taking a tonic dep.
pub use tonic::{Code as GrpcCode, Status as GrpcStatus};

/// The agent service's pipe. One agent service per Windows machine.
pub const PIPE_NAME: &str = r"\\.\pipe\varde-agent";

/// The hosted Varde every install points at unless the user picks another
/// server (`server.url`, `--link`, installer /CPURL, `enroll --server`).
pub const DEFAULT_CP_URL: &str = "https://cp.varde.games";

/// The agent's default data directory: `%ProgramData%\Varde` on Windows,
/// `~/Library/Application Support/Varde` on macOS (the per-user LaunchAgent
/// install — a root-run daemon still wants `--data-dir /var/lib/varde`),
/// `/var/lib/varde` on other Unix.
pub fn default_data_dir() -> std::path::PathBuf {
    #[cfg(windows)]
    {
        std::env::var_os("ProgramData")
            .map(std::path::PathBuf::from)
            .unwrap_or_else(|| std::path::PathBuf::from(r"C:\ProgramData"))
            .join("Varde")
    }
    #[cfg(target_os = "macos")]
    {
        // Agents installed before Varde.app ran with the Unix default — an
        // existing /var/lib/varde means an upgrade, and its config and node
        // identity live there. Only fresh installs get the per-user dir.
        let legacy = std::path::PathBuf::from("/var/lib/varde");
        if legacy.is_dir() {
            return legacy;
        }
        std::env::var_os("HOME")
            .map(std::path::PathBuf::from)
            .unwrap_or_else(|| std::path::PathBuf::from("/Users/Shared"))
            .join("Library/Application Support/Varde")
    }
    #[cfg(all(unix, not(target_os = "macos")))]
    {
        std::path::PathBuf::from("/var/lib/varde")
    }
}

/// IPC endpoint of an agent installed with its default data dir — where a
/// tray or CLI client looks without reading any config first.
pub fn default_endpoint() -> String {
    endpoint(&default_data_dir())
}

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
    // crash reports carry minidumps (~a few hundred KiB, capped at 8 MiB)
    Ok(LocalServiceClient::new(channel)
        .max_encoding_message_size(32 * 1024 * 1024)
        .max_decoding_message_size(32 * 1024 * 1024))
}

#[cfg(unix)]
async fn platform_connect(
    path: &str,
) -> io::Result<hyper_util::rt::TokioIo<tokio::net::UnixStream>> {
    let stream = tokio::net::UnixStream::connect(path).await?;
    #[cfg(target_os = "macos")]
    verify_peer_agent(&stream)?;
    Ok(hyper_util::rt::TokioIo::new(stream))
}

/// Reject an impostor agent: the socket lives under the user's own Library,
/// so any same-user process could bind the path while the agent is down and
/// feed the tray a fake dashboard or link URL. The Windows client gets the
/// equivalent from the pipe's session-0 requirement. Best-effort — a
/// determined same-user attacker can name a binary `varde-agent` too, but
/// passive socket squatting is no longer enough.
/// `VARDE_DEV_ALLOW_USER_AGENT=1` skips the check like on Windows.
#[cfg(target_os = "macos")]
fn verify_peer_agent(stream: &tokio::net::UnixStream) -> io::Result<()> {
    use std::os::unix::io::AsRawFd;
    // SOL_LOCAL/LOCAL_PEEREPID are stable Darwin ABI missing from libc.
    const SOL_LOCAL: i32 = 0;
    const LOCAL_PEEREPID: i32 = 0x002;
    let mut pid: i32 = -1;
    let mut len = std::mem::size_of::<i32>() as u32;
    // SAFETY: live socket fd; out pointer sized exactly for LOCAL_PEEREPID.
    let rc = unsafe {
        libc::getsockopt(
            stream.as_raw_fd(),
            SOL_LOCAL,
            LOCAL_PEEREPID,
            &mut pid as *mut i32 as *mut _,
            &mut len,
        )
    };
    if rc != 0 || pid <= 0 {
        return Err(io::Error::last_os_error());
    }
    // proc_pidpath is libproc, folded into libSystem on Darwin.
    extern "C" {
        fn proc_pidpath(pid: i32, buffer: *mut u8, buffersize: u32) -> i32;
    }
    let mut buf = [0u8; 4096]; // PROC_PIDPATHINFO_MAXSIZE
                               // SAFETY: buf is a writable 4096-byte buffer; the return is the length.
    let n = unsafe { proc_pidpath(pid, buf.as_mut_ptr(), buf.len() as u32) };
    if n <= 0 {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "could not identify the agent peer process",
        ));
    }
    let path = String::from_utf8_lossy(&buf[..n as usize]);
    let is_agent = std::path::Path::new(path.as_ref())
        .file_name()
        .is_some_and(|f| f == "varde-agent");
    if !is_agent && std::env::var_os("VARDE_DEV_ALLOW_USER_AGENT").is_none() {
        return Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            format!("IPC peer is {path}, not varde-agent"),
        ));
    }
    Ok(())
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
