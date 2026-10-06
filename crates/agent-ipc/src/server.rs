//! Listener side: an incoming-connection stream for tonic that tags each
//! connection with [`PeerInfo`] (available in request extensions).

use std::io;
use std::pin::Pin;
use std::task::{Context, Poll};

use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
use tokio_stream::wrappers::ReceiverStream;

/// Who is on the other end of a connection.
#[derive(Clone, Debug, Default)]
pub struct PeerInfo {
    /// Windows: the client process token is in BUILTIN\Administrators
    /// (elevated). Unix: root or the agent's own user.
    pub is_admin: bool,
}

pub struct Conn<T> {
    io: T,
    peer: PeerInfo,
}

impl<T: AsyncRead + Unpin> AsyncRead for Conn<T> {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        Pin::new(&mut self.io).poll_read(cx, buf)
    }
}

impl<T: AsyncWrite + Unpin> AsyncWrite for Conn<T> {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.io).poll_write(cx, buf)
    }
    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.io).poll_flush(cx)
    }
    fn poll_shutdown(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.io).poll_shutdown(cx)
    }
    fn poll_write_vectored(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        bufs: &[io::IoSlice<'_>],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.io).poll_write_vectored(cx, bufs)
    }
    fn is_write_vectored(&self) -> bool {
        self.io.is_write_vectored()
    }
}

impl<T> tonic::transport::server::Connected for Conn<T> {
    type ConnectInfo = PeerInfo;
    fn connect_info(&self) -> PeerInfo {
        self.peer.clone()
    }
}

#[cfg(unix)]
pub type Io = tokio::net::UnixStream;
#[cfg(windows)]
pub type Io = tokio::net::windows::named_pipe::NamedPipeServer;

/// Binds `endpoint` and yields accepted connections. Must be called inside a
/// tokio runtime.
pub fn incoming(endpoint: &str) -> io::Result<ReceiverStream<io::Result<Conn<Io>>>> {
    let (tx, rx) = tokio::sync::mpsc::channel(16);
    platform_listen(endpoint, tx)?;
    Ok(ReceiverStream::new(rx))
}

type Tx = tokio::sync::mpsc::Sender<io::Result<Conn<Io>>>;

#[cfg(unix)]
fn platform_listen(path: &str, tx: Tx) -> io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    let p = std::path::Path::new(path);
    if let Some(dir) = p.parent() {
        std::fs::create_dir_all(dir)?;
    }
    let _ = std::fs::remove_file(p);
    let listener = tokio::net::UnixListener::bind(p)?;
    // any local user may read status; linking is checked per call
    std::fs::set_permissions(p, std::fs::Permissions::from_mode(0o666))?;
    // SAFETY: geteuid has no preconditions.
    let me = unsafe { libc::geteuid() };
    tokio::spawn(async move {
        loop {
            let res = listener.accept().await.map(|(s, _)| {
                let uid = s.peer_cred().map(|c| c.uid()).unwrap_or(u32::MAX);
                Conn {
                    io: s,
                    peer: PeerInfo {
                        is_admin: uid == 0 || uid == me,
                    },
                }
            });
            if tx.send(res).await.is_err() {
                return;
            }
        }
    });
    Ok(())
}

#[cfg(windows)]
fn platform_listen(path: &str, tx: Tx) -> io::Result<()> {
    let mut server = win::create(path, true)?;
    let path = path.to_string();
    tokio::spawn(async move {
        loop {
            if let Err(e) = server.connect().await {
                if tx.send(Err(e)).await.is_err() {
                    return;
                }
                continue;
            }
            let next = match win::create(&path, false) {
                Ok(n) => n,
                Err(e) => {
                    let _ = tx.send(Err(e)).await;
                    return;
                }
            };
            let io = std::mem::replace(&mut server, next);
            let peer = PeerInfo {
                is_admin: win::client_is_admin(&io),
            };
            if tx.send(Ok(Conn { io, peer })).await.is_err() {
                return;
            }
        }
    });
    Ok(())
}

#[cfg(windows)]
mod win {
    use std::io;
    use std::os::windows::io::AsRawHandle;

    use tokio::net::windows::named_pipe::{NamedPipeServer, ServerOptions};
    use windows_sys::Win32::Foundation::{CloseHandle, LocalFree, HANDLE};
    use windows_sys::Win32::Security::Authorization::{
        ConvertStringSecurityDescriptorToSecurityDescriptorW, SDDL_REVISION_1,
    };
    use windows_sys::Win32::Security::{
        CheckTokenMembership, CreateWellKnownSid, DuplicateToken, SecurityIdentification,
        WinBuiltinAdministratorsSid, PSECURITY_DESCRIPTOR, SECURITY_ATTRIBUTES,
        SECURITY_MAX_SID_SIZE, TOKEN_DUPLICATE, TOKEN_QUERY,
    };
    use windows_sys::Win32::System::Pipes::GetNamedPipeClientProcessId;
    use windows_sys::Win32::System::Threading::{
        OpenProcess, OpenProcessToken, PROCESS_QUERY_LIMITED_INFORMATION,
    };

    /// Protected DACL: SYSTEM and Administrators full control; interactive
    /// users FILE_GENERIC_READ | FILE_WRITE_DATA (0x12008b) — enough to talk,
    /// not FILE_CREATE_PIPE_INSTANCE. No network users (also rejected below).
    const PIPE_SDDL: &str = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x12008b;;;IU)";

    pub fn create(path: &str, first: bool) -> io::Result<NamedPipeServer> {
        let sddl: Vec<u16> = PIPE_SDDL.encode_utf16().chain(std::iter::once(0)).collect();
        let mut sd: PSECURITY_DESCRIPTOR = std::ptr::null_mut();
        // SAFETY: valid NUL-terminated SDDL and out pointer; sd is freed below.
        unsafe {
            if ConvertStringSecurityDescriptorToSecurityDescriptorW(
                sddl.as_ptr(),
                SDDL_REVISION_1,
                &mut sd,
                std::ptr::null_mut(),
            ) == 0
            {
                return Err(io::Error::last_os_error());
            }
            let mut sa = SECURITY_ATTRIBUTES {
                nLength: std::mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
                lpSecurityDescriptor: sd,
                bInheritHandle: 0,
            };
            let res = ServerOptions::new()
                // refuse to attach to a pipe someone else created first
                .first_pipe_instance(first)
                .reject_remote_clients(true)
                .create_with_security_attributes_raw(path, &mut sa as *mut _ as *mut _);
            LocalFree(sd as _);
            res
        }
    }

    struct Handle(HANDLE);
    impl Drop for Handle {
        fn drop(&mut self) {
            if !self.0.is_null() {
                // SAFETY: we own the handle.
                unsafe { CloseHandle(self.0) };
            }
        }
    }

    /// Whether the connected client's process token is a member of
    /// BUILTIN\Administrators. UAC-filtered (non-elevated) admin tokens carry
    /// the group as deny-only and so are not admins here.
    pub fn client_is_admin(pipe: &NamedPipeServer) -> bool {
        // SAFETY: plain Win32 calls on handles we own; failures yield false.
        unsafe {
            let mut pid = 0u32;
            if GetNamedPipeClientProcessId(pipe.as_raw_handle() as _, &mut pid) == 0 {
                return false;
            }
            let proc = Handle(OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid));
            if proc.0.is_null() {
                return false;
            }
            let mut tok = Handle(std::ptr::null_mut());
            if OpenProcessToken(proc.0, TOKEN_QUERY | TOKEN_DUPLICATE, &mut tok.0) == 0 {
                return false;
            }
            let mut imp = Handle(std::ptr::null_mut());
            if DuplicateToken(tok.0, SecurityIdentification, &mut imp.0) == 0 {
                return false;
            }
            let mut sid = [0u8; SECURITY_MAX_SID_SIZE as usize];
            let mut len = sid.len() as u32;
            if CreateWellKnownSid(
                WinBuiltinAdministratorsSid,
                std::ptr::null_mut(),
                sid.as_mut_ptr() as _,
                &mut len,
            ) == 0
            {
                return false;
            }
            let mut member = 0;
            CheckTokenMembership(imp.0, sid.as_mut_ptr() as _, &mut member) != 0 && member != 0
        }
    }
}
