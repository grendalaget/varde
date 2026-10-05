//! Trait surface every game driver (games/*) implements (agent.md):
//! lifecycle hooks, config validation, snapshot barriers. Drivers are
//! compiled into the agent and registered in a `DriverRegistry` by id; the
//! control plane's catalog lists the same ids.

use std::collections::HashMap;
use std::path::{Path, PathBuf};

use async_trait::async_trait;
use executor_api::{ProcessHandle, ProcessSpec};
pub use snapshot_store::PathPattern;

pub type DynError = Box<dyn std::error::Error + Send + Sync>;
pub type Result<T> = std::result::Result<T, DynError>;

/// How the game runtime is obtained. `local_binary` = an already-installed
/// binary (path from agent config or next to the agent executable); other
/// kinds (steamcmd, mojang, …) are driver-defined.
#[derive(Debug, Clone, serde::Deserialize)]
pub struct DeploymentSpec {
    #[serde(default)]
    pub kind: Option<String>,
    /// Raw spec JSON from the deployment directive.
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
}

impl DeploymentSpec {
    pub fn parse(spec: &serde_json::Value) -> DeploymentSpec {
        serde_json::from_value(spec.clone()).unwrap_or_else(|_| DeploymentSpec {
            kind: None,
            extra: serde_json::Map::new(),
        })
    }
    pub fn kind(&self) -> &str {
        self.kind.as_deref().unwrap_or("local_binary")
    }
    pub fn get(&self, key: &str) -> Option<&serde_json::Value> {
        self.extra.get(key)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PortSpec {
    pub port: u32,
    pub protocol: GameProtocol,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GameProtocol {
    Tcp,
    Udp,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum GameHealth {
    Starting,
    Healthy,
    /// process exited or probe keeps failing
    Dead(String),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SnapshotBarrier {
    /// Snapshot may be taken while the game runs (the driver quiesced it).
    Live,
    /// The game must be stopped for a consistent snapshot.
    RequiresStop,
}

/// Runtimes drivers may request (java, steamcmd); implemented by the agent.
pub trait RuntimeProvider: Send + Sync {
    fn runtime_path(&self, kind: &str, id: &str) -> Option<PathBuf>;
}

pub struct DriverContext<'a> {
    pub server_dir: &'a Path,
    pub deployment_dir: &'a Path,
    pub runtimes: &'a dyn RuntimeProvider,
    pub deployment: &'a DeploymentSpec,
    pub config: &'a serde_json::Value,
    pub memory_mb: u32,
}

#[async_trait]
pub trait GameDriver: Send + Sync {
    fn id(&self) -> &'static str;
    /// Ports the game listens on (service ports map 1:1 to local targets).
    fn ports(&self, config: &serde_json::Value) -> Vec<PortSpec>;
    /// Server-dir-relative paths captured by snapshots.
    fn persistent_paths(&self, config: &serde_json::Value) -> Vec<PathPattern>;
    fn validate(&self, config: &serde_json::Value) -> Result<()>;
    /// Install into `deployment_dir` (idempotent).
    async fn prepare(&self, ctx: &DriverContext<'_>) -> Result<()>;
    /// Write config files into `server_dir`.
    async fn configure(&self, ctx: &DriverContext<'_>) -> Result<()>;
    fn process_spec(&self, ctx: &DriverContext<'_>) -> Result<ProcessSpec>;
    async fn probe(&self, ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<GameHealth>;
    /// Enter the snapshot barrier; returns Live or RequiresStop.
    async fn prepare_snapshot(
        &self,
        ctx: &DriverContext<'_>,
        p: &dyn ProcessHandle,
    ) -> Result<SnapshotBarrier>;
    async fn resume_after_snapshot(
        &self,
        ctx: &DriverContext<'_>,
        p: &dyn ProcessHandle,
    ) -> Result<()>;
    async fn graceful_stop(&self, ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<()>;
}

#[derive(Default)]
pub struct DriverRegistry {
    drivers: HashMap<&'static str, Box<dyn GameDriver>>,
}

impl DriverRegistry {
    pub fn new() -> Self {
        Self::default()
    }
    pub fn register(&mut self, d: Box<dyn GameDriver>) {
        self.drivers.insert(d.id(), d);
    }
    pub fn get(&self, id: &str) -> Option<&dyn GameDriver> {
        self.drivers.get(id).map(|d| &**d)
    }
    pub fn ids(&self) -> Vec<&'static str> {
        let mut v: Vec<_> = self.drivers.keys().copied().collect();
        v.sort();
        v
    }
}
