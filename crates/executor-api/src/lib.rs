//! Executor contract (docs/architecture/agent.md): drivers ask for a runtime
//! process; the agent supervises it. Programs run directly with argv — never
//! through a shell (I9).

use std::ffi::OsString;
use std::path::PathBuf;
use std::sync::Arc;

use async_trait::async_trait;
use tokio::sync::broadcast;

#[derive(Debug, Clone)]
pub struct ProcessSpec {
    pub program: PathBuf,
    pub args: Vec<OsString>,
    pub env: Vec<(OsString, OsString)>,
    pub cwd: PathBuf,
    pub stdin: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum OutputStream {
    Stdout,
    Stderr,
    Agent,
}

#[derive(Debug, Clone)]
pub struct OutputLine {
    pub at_unix_ms: i64,
    pub stream: OutputStream,
    pub line: Arc<str>,
}

#[derive(Debug, Clone, Default)]
pub struct ExitStatus {
    pub code: Option<i32>,
    pub signal: Option<i32>,
}

impl ExitStatus {
    pub fn success(&self) -> bool {
        self.code == Some(0) && self.signal.is_none()
    }
}

#[derive(Debug, Clone, Default)]
pub struct ResourceUsage {
    pub cpu_percent: f64,
    pub rss_bytes: u64,
}

pub type DynError = Box<dyn std::error::Error + Send + Sync>;
pub type Result<T> = std::result::Result<T, DynError>;

/// Number of output lines retained in the ring buffer.
pub const OUTPUT_RING_LINES: usize = 1000;

#[async_trait]
pub trait Executor: Send + Sync {
    /// "native" (and later "wine", "flatpak", …)
    fn kind(&self) -> &'static str;
    async fn spawn(&self, spec: &ProcessSpec) -> Result<Box<dyn ProcessHandle>>;
}

#[async_trait]
pub trait ProcessHandle: Send + Sync {
    fn pid(&self) -> u32;
    async fn write_stdin(&self, line: &str) -> Result<()>;
    /// Live stdout/stderr lines; the handle also keeps a ring buffer.
    fn output(&self) -> broadcast::Receiver<OutputLine>;
    /// Last N buffered lines (newest last).
    fn output_tail(&self, n: usize) -> Vec<OutputLine>;
    /// Waits for the process to exit; safe to call from several tasks.
    async fn wait(&self) -> Result<ExitStatus>;
    /// Graceful: SIGTERM to the process group / CTRL_BREAK (Windows).
    async fn terminate(&self) -> Result<()>;
    /// Interrupt-style graceful stop: SIGINT to the process group /
    /// CTRL_BREAK (Windows). Games that only honor SIGINT (valheim).
    async fn interrupt(&self) -> Result<()>;
    /// Hard kill of the whole group / job object.
    async fn kill(&self) -> Result<()>;
    fn resource_usage(&self) -> Option<ResourceUsage>;
}
