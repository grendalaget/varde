//! Native executor: Linux `setsid` process groups (SIGTERM/SIGKILL to the
//! whole group), Windows Job Objects (KILL_ON_JOB_CLOSE + CREATE_NEW_PROCESS_
//! GROUP, CTRL_BREAK for terminate). Consoleless agents (Windows services)
//! first allocate a hidden console so CTRL_BREAK can reach the child.
//! Never a shell.

use std::collections::VecDeque;
use std::io;
use std::process::Stdio;
use std::sync::{Arc, Mutex};
use std::time::{SystemTime, UNIX_EPOCH};

use async_trait::async_trait;
use executor_api::*;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStdin, Command};
use tokio::sync::{broadcast, watch, Mutex as AsyncMutex};

pub struct NativeExecutor;

#[async_trait]
impl Executor for NativeExecutor {
    fn kind(&self) -> &'static str {
        "native"
    }

    async fn spawn(&self, spec: &ProcessSpec) -> Result<Box<dyn ProcessHandle>> {
        NativeHandle::spawn(spec).await.map(|h| Box::new(h) as _)
    }
}

struct Shared {
    tx: broadcast::Sender<OutputLine>,
    ring: Mutex<VecDeque<OutputLine>>,
    exit: watch::Sender<Option<ExitStatus>>,
    stdin: AsyncMutex<Option<ChildStdin>>,
}

pub struct NativeHandle {
    pid: u32,
    shared: Arc<Shared>,
    /// kept so the wait task owns the child; callers poll `exit` instead
    child: AsyncMutex<Option<Child>>,
    #[cfg(windows)]
    job: Arc<Job>,
}

fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

fn push_line(shared: &Shared, stream: OutputStream, line: String) {
    let l = OutputLine {
        at_unix_ms: now_ms(),
        stream,
        line: line.into(),
    };
    {
        let mut ring = shared.ring.lock().unwrap();
        if ring.len() >= OUTPUT_RING_LINES {
            ring.pop_front();
        }
        ring.push_back(l.clone());
    }
    let _ = shared.tx.send(l); // fine if no subscribers
}

impl NativeHandle {
    async fn spawn(spec: &ProcessSpec) -> Result<Self> {
        let mut cmd = Command::new(&spec.program);
        cmd.args(&spec.args)
            .envs(spec.env.iter().map(|(k, v)| (k, v)))
            .current_dir(&spec.cwd)
            .stdin(if spec.stdin {
                Stdio::piped()
            } else {
                Stdio::null()
            })
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);
        platform_configure(&mut cmd);

        let mut child = cmd.spawn().map_err(to_err)?;
        let pid = child.id().unwrap_or(0);

        let (tx, _rx) = broadcast::channel(512);
        let (exit, _erx) = watch::channel(None::<ExitStatus>);
        let shared = Arc::new(Shared {
            tx,
            ring: Mutex::new(VecDeque::with_capacity(OUTPUT_RING_LINES)),
            exit,
            stdin: AsyncMutex::new(child.stdin.take()),
        });

        let pipes: [(
            Option<Box<dyn tokio::io::AsyncRead + Unpin + Send>>,
            OutputStream,
        ); 2] = [
            (
                child.stdout.take().map(|s| Box::new(s) as _),
                OutputStream::Stdout,
            ),
            (
                child.stderr.take().map(|s| Box::new(s) as _),
                OutputStream::Stderr,
            ),
        ];
        for (pipe, stream) in pipes {
            let Some(pipe) = pipe else { continue };
            let sh = shared.clone();
            tokio::spawn(async move {
                let mut lines = BufReader::new(pipe).lines();
                while let Ok(Some(line)) = lines.next_line().await {
                    push_line(&sh, stream, line);
                }
            });
        }

        #[cfg(windows)]
        let job = {
            let job = Job::new()?;
            // assign via raw handle while we still have the Child
            job.assign(&child)?;
            Arc::new(job)
        };

        Ok(NativeHandle {
            pid,
            shared,
            child: AsyncMutex::new(Some(child)),
            #[cfg(windows)]
            job,
        })
    }
}

#[async_trait]
impl ProcessHandle for NativeHandle {
    fn pid(&self) -> u32 {
        self.pid
    }

    async fn write_stdin(&self, line: &str) -> Result<()> {
        let mut g = self.shared.stdin.lock().await;
        let Some(stdin) = g.as_mut() else {
            return Err("stdin not piped".into());
        };
        stdin.write_all(line.as_bytes()).await.map_err(to_err)?;
        stdin.write_all(b"\n").await.map_err(to_err)?;
        stdin.flush().await.map_err(to_err)?;
        Ok(())
    }

    fn output(&self) -> broadcast::Receiver<OutputLine> {
        self.shared.tx.subscribe()
    }

    fn output_tail(&self, n: usize) -> Vec<OutputLine> {
        let ring = self.shared.ring.lock().unwrap();
        ring.iter().rev().take(n).rev().cloned().collect()
    }

    async fn wait(&self) -> Result<ExitStatus> {
        let mut rx = self.shared.exit.subscribe();
        if let Some(st) = rx.borrow().clone() {
            return Ok(st);
        }
        // only one task actually waits on the child; others subscribe
        let mut g = self.child.lock().await;
        if let Some(mut child) = g.take() {
            let st = child.wait().await.map_err(to_err)?;
            let status = ExitStatus {
                code: st.code(),
                signal: exit_signal(&st),
            };
            let _ = self.shared.exit.send(Some(status.clone()));
            Ok(status)
        } else {
            drop(g);
            loop {
                if rx.changed().await.is_err() {
                    break;
                }
                if let Some(st) = rx.borrow().clone() {
                    return Ok(st);
                }
            }
            Err("waiter channel closed".into())
        }
    }

    async fn terminate(&self) -> Result<()> {
        platform_terminate(self)
    }

    async fn interrupt(&self) -> Result<()> {
        platform_interrupt(self)
    }

    async fn kill(&self) -> Result<()> {
        platform_kill(self)
    }

    fn resource_usage(&self) -> Option<ResourceUsage> {
        platform_rusage(self.pid)
    }
}

fn to_err(e: io::Error) -> DynError {
    Box::new(e)
}

#[cfg(unix)]
fn exit_signal(st: &std::process::ExitStatus) -> Option<i32> {
    use std::os::unix::process::ExitStatusExt;
    st.signal()
}
#[cfg(not(unix))]
fn exit_signal(_st: &std::process::ExitStatus) -> Option<i32> {
    None
}

// ---------- unix ----------

#[cfg(unix)]
fn platform_configure(cmd: &mut Command) {
    // tokio exposes pre_exec inherently on unix
    unsafe {
        cmd.pre_exec(|| {
            if libc::setsid() == -1 {
                return Err(io::Error::last_os_error());
            }
            Ok(())
        });
    }
}

#[cfg(unix)]
fn platform_terminate(h: &NativeHandle) -> Result<()> {
    // negative pid ⇒ the whole process group (child is group leader)
    if unsafe { libc::kill(-(h.pid as i32), libc::SIGTERM) } == -1 {
        let e = io::Error::last_os_error();
        if e.raw_os_error() != Some(libc::ESRCH) {
            return Err(e.into());
        }
    }
    Ok(())
}

#[cfg(unix)]
fn platform_interrupt(h: &NativeHandle) -> Result<()> {
    if unsafe { libc::kill(-(h.pid as i32), libc::SIGINT) } == -1 {
        let e = io::Error::last_os_error();
        if e.raw_os_error() != Some(libc::ESRCH) {
            return Err(e.into());
        }
    }
    Ok(())
}

#[cfg(unix)]
fn platform_kill(h: &NativeHandle) -> Result<()> {
    if unsafe { libc::kill(-(h.pid as i32), libc::SIGKILL) } == -1 {
        let e = io::Error::last_os_error();
        if e.raw_os_error() != Some(libc::ESRCH) {
            return Err(e.into());
        }
    }
    Ok(())
}

#[cfg(unix)]
fn platform_rusage(pid: u32) -> Option<ResourceUsage> {
    let stat = std::fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
    let status = std::fs::read_to_string(format!("/proc/{pid}/status")).ok()?;
    // fields after comm (which may contain spaces) start at state
    let after = stat.rsplit(')').next()?;
    let fields: Vec<&str> = after.split_whitespace().collect();
    // utime=idx 11, stime=idx 12 of remaining list (state is idx 0)
    let ticks = |i: usize| fields.get(i)?.parse::<u64>().ok();
    let utime = ticks(11)?;
    let stime = ticks(12)?;
    let uptime_s = std::fs::read_to_string("/proc/uptime")
        .ok()?
        .split_whitespace()
        .next()?
        .parse::<f64>()
        .ok()?;
    let starttime = ticks(19)? as f64;
    let hz = 100.0f64; // USER_HZ on linux
    let elapsed = (uptime_s - starttime / hz).max(0.001);
    let cpu = ((utime + stime) as f64 / hz) / elapsed * 100.0;
    let rss = status
        .lines()
        .find(|l| l.starts_with("VmRSS:"))
        .and_then(|l| l.split_whitespace().nth(1)?.parse::<u64>().ok())
        .unwrap_or(0)
        * 1024;
    Some(ResourceUsage {
        cpu_percent: cpu,
        rss_bytes: rss,
    })
}

// ---------- windows ----------

#[cfg(windows)]
fn platform_configure(cmd: &mut Command) {
    ensure_console();
    const CREATE_NEW_PROCESS_GROUP: u32 = 0x00000200;
    cmd.creation_flags(CREATE_NEW_PROCESS_GROUP);
}

/// Services run with no console, and `GenerateConsoleCtrlEvent` only reaches
/// processes attached to one. On the first spawn, give a consoleless agent a
/// hidden console so children inherit it and CTRL_BREAK reaches their group.
#[cfg(windows)]
fn ensure_console() {
    use windows_sys::Win32::System::Console::*;
    use windows_sys::Win32::UI::WindowsAndMessaging::*;

    unsafe extern "system" fn handler(ctrl: u32) -> i32 {
        // SCM STOP/PRESHUTDOWN drive service shutdown; don't let these reach
        // the default handler, which would ExitProcess.
        match ctrl {
            CTRL_LOGOFF_EVENT | CTRL_SHUTDOWN_EVENT => 1,
            _ => 0,
        }
    }

    static ONCE: std::sync::Once = std::sync::Once::new();
    ONCE.call_once(|| unsafe {
        // Fails (ERROR_ACCESS_DENIED) when the process already has a console.
        if AllocConsole() == 0 {
            return;
        }
        let hwnd = GetConsoleWindow();
        if !hwnd.is_null() {
            ShowWindow(hwnd, SW_HIDE);
        }
        SetConsoleCtrlHandler(Some(handler), 1);
    });
}

#[cfg(windows)]
struct Job(windows_sys::Win32::Foundation::HANDLE);
#[cfg(windows)]
unsafe impl Send for Job {}
#[cfg(windows)]
unsafe impl Sync for Job {}

#[cfg(windows)]
impl Job {
    fn new() -> Result<Job> {
        use windows_sys::Win32::System::JobObjects::*;
        unsafe {
            let job = CreateJobObjectW(std::ptr::null(), std::ptr::null());
            if job.is_null() {
                return Err(io::Error::last_os_error().into());
            }
            let mut info: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = std::mem::zeroed();
            info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
            if SetInformationJobObject(
                job,
                JobObjectExtendedLimitInformation,
                &info as *const _ as *const _,
                std::mem::size_of_val(&info) as u32,
            ) == 0
            {
                windows_sys::Win32::Foundation::CloseHandle(job);
                return Err(io::Error::last_os_error().into());
            }
            Ok(Job(job))
        }
    }

    fn assign(&self, child: &Child) -> Result<()> {
        use windows_sys::Win32::System::JobObjects::AssignProcessToJobObject;
        use windows_sys::Win32::System::Threading::*;
        unsafe {
            let proc = OpenProcess(PROCESS_ALL_ACCESS, 0, child.id().unwrap_or(0));
            if proc.is_null() {
                return Err(io::Error::last_os_error().into());
            }
            let ok = AssignProcessToJobObject(self.0, proc);
            windows_sys::Win32::Foundation::CloseHandle(proc);
            if ok == 0 {
                return Err(io::Error::last_os_error().into());
            }
            Ok(())
        }
    }

    fn terminate_job(&self) -> Result<()> {
        unsafe {
            if windows_sys::Win32::System::JobObjects::TerminateJobObject(self.0, 1) == 0 {
                return Err(io::Error::last_os_error().into());
            }
        }
        Ok(())
    }
}

#[cfg(windows)]
impl Drop for Job {
    fn drop(&mut self) {
        unsafe {
            windows_sys::Win32::Foundation::CloseHandle(self.0);
        }
    }
}

#[cfg(windows)]
fn platform_terminate(h: &NativeHandle) -> Result<()> {
    // CTRL_BREAK to the process group (process started with
    // CREATE_NEW_PROCESS_GROUP). Falls back to the job object if the process
    // has no console attached.
    use windows_sys::Win32::System::Console::*;
    unsafe {
        if GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT, h.pid) == 0 {
            let e = io::Error::last_os_error();
            // ERROR_INVALID_HANDLE = no console; kill the job instead
            if e.raw_os_error() == Some(6) {
                return h.job.terminate_job();
            }
            return Err(e.into());
        }
    }
    Ok(())
}

#[cfg(windows)]
fn platform_interrupt(h: &NativeHandle) -> Result<()> {
    // CTRL_BREAK is the closest interrupt signal available
    platform_terminate(h)
}

#[cfg(windows)]
fn platform_kill(h: &NativeHandle) -> Result<()> {
    h.job.terminate_job()
}

#[cfg(windows)]
fn platform_rusage(_pid: u32) -> Option<ResourceUsage> {
    None
}
