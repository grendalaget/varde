//! Executor contract (docs/architecture/agent.md): drivers ask for a runtime
//! process; the agent supervises it. Programs run directly with argv — never
//! through a shell (I9).

use std::ffi::OsString;
use std::fmt;
use std::io;
use std::path::{Path, PathBuf};
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

/// Local filesystem context that can be reported independently of error text.
#[derive(Debug)]
pub struct LocalPathError {
    operation: String,
    path: PathBuf,
    detail: String,
}

impl LocalPathError {
    pub fn new(operation: impl Into<String>, path: &Path, detail: impl fmt::Display) -> Self {
        Self {
            operation: operation.into(),
            path: path.to_owned(),
            detail: detail.to_string(),
        }
    }

    pub fn io(operation: impl Into<String>, path: &Path, source: io::Error) -> io::Error {
        io::Error::new(source.kind(), Self::new(operation, path, source))
    }

    pub fn path(&self) -> &Path {
        &self.path
    }
}

impl fmt::Display for LocalPathError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "{} {}: {}",
            self.operation,
            self.path.display(),
            self.detail
        )
    }
}

impl std::error::Error for LocalPathError {}

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
