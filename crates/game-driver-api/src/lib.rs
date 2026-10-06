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

/// A service port mapped to the agent-allocated local port the game
/// actually binds. The mesh service listener owns `<loopback_ip>:<service_port>`
/// on every node (including the host), so the game must listen on
/// `127.0.0.1:<local_port>` — wildcard binds collide with the listener.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PortBinding {
    pub service_port: u32,
    pub local_port: u32,
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

/// How a fetched artifact lands in the runtime cache.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ArchiveKind {
    /// Single file, placed as <id>/<name>.
    File,
    /// .tar.gz / .tgz extracted under <id>/.
    Tgz,
    /// .zip extracted under <id>/.
    Zip,
}

/// Artifact a driver needs downloaded through the agent's RuntimeProvider
/// (drivers never do raw HTTP). At least one checksum should be set; the
/// provider refuses to use a downloaded artifact that fails verification.
#[derive(Debug, Clone)]
pub struct FetchSpec {
    pub url: String,
    pub file_name: String,
    pub sha256: Option<String>,
    pub sha1: Option<String>,
    pub archive: ArchiveKind,
}

/// Runtimes drivers may request (java, steamcmd); implemented by the agent.
/// All HTTP goes through the provider; drivers only see paths.
#[async_trait]
pub trait RuntimeProvider: Send + Sync {
    fn runtime_path(&self, kind: &str, id: &str) -> Option<PathBuf>;
    /// GET a JSON document (version manifests, runtime APIs).
    async fn get_json(&self, _url: &str) -> Result<serde_json::Value> {
        Err("runtime provider cannot fetch".into())
    }
    /// Download `spec` into the shared cache at `<runtimes>/<kind>/<id>`,
    /// atomically and checksum-verified; repeated calls hit the cache.
    async fn fetch(&self, _kind: &str, _id: &str, _spec: &FetchSpec) -> Result<PathBuf> {
        Err("runtime provider cannot fetch".into())
    }
    /// Install/update a Steam dedicated app into `dest` via steamcmd
    /// (`+force_install_dir +login anonymous +app_update <app> validate +quit`).
    async fn steam_app_install(&self, _app_id: u32, _dest: &Path) -> Result<()> {
        Err("runtime provider cannot fetch".into())
    }
}

pub struct DriverContext<'a> {
    pub server_dir: &'a Path,
    pub deployment_dir: &'a Path,
    pub runtimes: &'a dyn RuntimeProvider,
    pub deployment: &'a DeploymentSpec,
    pub config: &'a serde_json::Value,
    /// Service ports mapped to the agent-allocated local ports this
    /// execution must bind.
    pub ports: &'a [PortBinding],
    pub memory_mb: u32,
}

impl DriverContext<'_> {
    /// Local port the game should bind for `service_port`, or the service
    /// port itself when there is no mapping.
    pub fn local_port(&self, service_port: u32) -> u32 {
        self.ports
            .iter()
            .find(|b| b.service_port == service_port)
            .map(|b| b.local_port)
            .unwrap_or(service_port)
    }
}

#[async_trait]
pub trait GameDriver: Send + Sync {
    fn id(&self) -> &'static str;
    /// Ports the game listens on. Service ports map to agent-allocated
    /// local targets (see `DriverContext::local_port`).
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
