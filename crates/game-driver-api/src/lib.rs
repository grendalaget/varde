//! Trait surface every game driver (games/*) implements (agent.md):
//! lifecycle hooks, config validation, snapshot barriers. Drivers are
//! compiled into the agent and registered in a `DriverRegistry` by id; the
//! control plane's catalog lists the same ids.

use std::collections::HashMap;
use std::io::Write;
use std::path::{Component, Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use async_trait::async_trait;
use executor_api::{Executor, ProcessHandle, ProcessSpec};
pub use snapshot_store::PathPattern;

pub type DynError = Box<dyn std::error::Error + Send + Sync>;
pub type Result<T> = std::result::Result<T, DynError>;

/// Whether SteamCMD marks an app installation as fully installed.
pub fn steam_app_installed(app_id: u32, dest: &Path) -> bool {
    let manifest =
        std::fs::read_to_string(dest.join(format!("steamapps/appmanifest_{app_id}.acf")))
            .unwrap_or_default();
    let tokens: Vec<_> = manifest.split('"').collect();
    tokens
        .windows(3)
        .any(|w| w[0] == "StateFlags" && w[2] == "4")
}

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
    pub untrusted: bool,
}

#[derive(Debug, Clone)]
pub struct PreparedAddon {
    pub plugin_id: String,
    pub addon_id: String,
    pub root: PathBuf,
    pub target: String,
    pub driver_options: serde_json::Value,
    pub files: Vec<(String, String)>,
}

impl PreparedAddon {
    pub fn install_into(&self, dest: &Path) -> Result<Vec<PathBuf>> {
        let mut installed = Installed::default();
        self.install_recorded(dest, &mut installed)?;
        Ok(installed.files)
    }

    fn install_recorded(&self, dest: &Path, installed: &mut Installed) -> Result<()> {
        fn valid_key(key: &str) -> bool {
            !key.is_empty()
                && !key.starts_with('/')
                && !key.contains('\\')
                && !key.contains(':')
                && key
                    .split('/')
                    .all(|part| !part.is_empty() && part != "." && part != "..")
        }

        let dest = absolute_path(dest)?;
        ensure_directory(&dest, &dest, installed)?;
        copy_tree(&self.root, &dest, &dest, installed)?;
        for (key, contents) in &self.files {
            if !valid_key(key) {
                return Err(format!("invalid addon file key: {key}").into());
            }
            let path = dest.join(key);
            ensure_directory(
                &dest,
                path.parent().expect("validated file key has parent"),
                installed,
            )?;
            ensure_regular_target(&dest, &path)?;
            installed.files.push(path.clone());
            let mut options = std::fs::OpenOptions::new();
            options.write(true).create(true).truncate(true);
            #[cfg(unix)]
            {
                use std::os::unix::fs::OpenOptionsExt;
                options.mode(0o600);
            }
            options.open(&path)?.write_all(contents.as_bytes())?;
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))?;
            }
        }
        Ok(())
    }
}

fn absolute_path(path: &Path) -> Result<PathBuf> {
    let path = if path.is_absolute() {
        path.to_path_buf()
    } else {
        std::env::current_dir()?.join(path)
    };
    let mut absolute = PathBuf::new();
    for component in path.components() {
        match component {
            Component::Prefix(_) | Component::RootDir | Component::Normal(_) => {
                absolute.push(component.as_os_str());
            }
            Component::CurDir => {}
            Component::ParentDir => {
                if !absolute.pop() {
                    return Err(format!("path cannot be made absolute: {}", path.display()).into());
                }
            }
        }
    }
    if !absolute.is_absolute() {
        return Err(format!("path is not absolute: {}", absolute.display()).into());
    }
    Ok(absolute)
}

fn ensure_directory(root: &Path, path: &Path, installed: &mut Installed) -> Result<()> {
    let relative = path
        .strip_prefix(root)
        .map_err(|_| format!("addon path is outside its destination: {}", path.display()))?;
    ensure_directory_exists(root, installed)?;
    let mut current = root.to_path_buf();
    for component in relative.components() {
        let Component::Normal(name) = component else {
            return Err(format!("unsafe addon destination: {}", path.display()).into());
        };
        current.push(name);
        ensure_directory_exists(&current, installed)?;
    }
    Ok(())
}

fn ensure_directory_exists(path: &Path, installed: &mut Installed) -> Result<()> {
    let mut missing = Vec::new();
    let mut current = path.to_path_buf();
    loop {
        match std::fs::symlink_metadata(&current) {
            Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_dir() => {
                return Err(format!("unsafe addon destination: {}", current.display()).into());
            }
            Ok(_) => break,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                missing.push(current.clone());
                current = current
                    .parent()
                    .ok_or_else(|| {
                        format!(
                            "addon destination has no existing ancestor: {}",
                            path.display()
                        )
                    })?
                    .to_path_buf();
            }
            Err(error) => return Err(error.into()),
        }
    }

    for directory in missing.iter().rev() {
        match std::fs::create_dir(directory) {
            Ok(()) => installed.dirs.push(directory.clone()),
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {
                let metadata = std::fs::symlink_metadata(directory)?;
                if metadata.file_type().is_symlink() || !metadata.is_dir() {
                    return Err(format!("unsafe addon destination: {}", directory.display()).into());
                }
            }
            Err(error) => return Err(error.into()),
        }
    }
    Ok(())
}

fn ensure_regular_target(root: &Path, path: &Path) -> Result<()> {
    if !path.starts_with(root) {
        return Err(format!("addon file is outside its destination: {}", path.display()).into());
    }
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_file() => {
            Err(format!("unsafe addon file destination: {}", path.display()).into())
        }
        Ok(_) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error.into()),
    }
}

fn copy_tree(
    source: &Path,
    destination: &Path,
    root: &Path,
    installed: &mut Installed,
) -> Result<()> {
    let metadata = std::fs::symlink_metadata(source)?;
    if metadata.file_type().is_symlink() || !metadata.is_dir() {
        return Err(format!(
            "addon archive contains a non-directory path: {}",
            source.display()
        )
        .into());
    }
    ensure_directory(root, destination, installed)?;
    for entry in std::fs::read_dir(source)? {
        let entry = entry?;
        let source_path = entry.path();
        let target_path = destination.join(entry.file_name());
        let metadata = std::fs::symlink_metadata(&source_path)?;
        if metadata.file_type().is_symlink() {
            return Err(format!(
                "addon archive contains a symlink: {}",
                source_path.display()
            )
            .into());
        }
        if metadata.is_dir() {
            copy_tree(&source_path, &target_path, root, installed)?;
        } else if metadata.is_file() {
            ensure_directory(
                root,
                target_path.parent().expect("file has parent"),
                installed,
            )?;
            ensure_regular_target(root, &target_path)?;
            installed.files.push(target_path.clone());
            let mut options = std::fs::OpenOptions::new();
            options.write(true).create(true).truncate(true);
            #[cfg(unix)]
            {
                use std::os::unix::fs::OpenOptionsExt;
                options.mode(0o600);
            }
            let mut output = options.open(&target_path)?;
            std::io::copy(&mut std::fs::File::open(&source_path)?, &mut output)?;
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                std::fs::set_permissions(&target_path, std::fs::Permissions::from_mode(0o600))?;
            }
        } else {
            return Err(format!(
                "addon archive contains a non-regular file: {}",
                source_path.display()
            )
            .into());
        }
    }
    Ok(())
}

#[derive(Default, serde::Deserialize, serde::Serialize)]
struct AddonState {
    #[serde(default)]
    files: Vec<PathBuf>,
    #[serde(default)]
    dirs: Vec<PathBuf>,
}

#[derive(Default)]
struct Installed {
    files: Vec<PathBuf>,
    dirs: Vec<PathBuf>,
}

/// Installs `installs` and removes stale files and empty directories from prior calls.
pub fn sync_addons(state_file: &Path, installs: &[(&PreparedAddon, PathBuf)]) -> Result<()> {
    let state_file = absolute_path(state_file)?;
    let managed_root = state_file
        .parent()
        .ok_or_else(|| format!("state file has no parent: {}", state_file.display()))?;
    let managed_root = absolute_path(managed_root)?;
    let mut state_directories = Installed::default();
    ensure_directory(&managed_root, &managed_root, &mut state_directories)?;
    let previous = read_addon_state(&state_file)?;
    let mut destinations = Vec::new();
    for (addon, destination) in installs {
        let destination = absolute_path(destination)?;
        if destination == managed_root || !destination.starts_with(&managed_root) {
            return Err(format!(
                "addon destination is outside state directory: {}",
                destination.display()
            )
            .into());
        }
        destinations.push((*addon, destination));
    }

    for path in previous.files.iter().chain(&previous.dirs) {
        validate_addon_state_path(path, &managed_root)?;
    }
    for path in &previous.files {
        remove_stale_file(path, &managed_root)?;
    }
    let mut previous_dirs = previous.dirs;
    previous_dirs.sort_by_key(|path| std::cmp::Reverse(path.components().count()));
    for path in &previous_dirs {
        remove_stale_dir(path, &managed_root)?;
    }

    let mut installed = Installed::default();
    for path in &previous_dirs {
        match std::fs::symlink_metadata(path) {
            Ok(metadata) if !metadata.file_type().is_symlink() && metadata.is_dir() => {
                installed.dirs.push(path.clone());
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
    }
    let install_result: Result<()> = (|| {
        for (addon, destination) in destinations {
            ensure_directory(&managed_root, &destination, &mut installed)?;
            addon.install_recorded(&destination, &mut installed)?;
        }
        Ok(())
    })();
    installed.files.sort();
    installed.files.dedup();
    installed.dirs.sort();
    installed.dirs.dedup();
    let state_result = write_addon_state(
        &state_file,
        AddonState {
            files: installed.files,
            dirs: installed.dirs,
        },
    );
    install_result?;
    state_result
}

fn read_addon_state(path: &Path) -> Result<AddonState> {
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_file() => {
            Err(format!("unsafe addon state file: {}", path.display()).into())
        }
        Ok(_) => Ok(serde_json::from_slice(&std::fs::read(path)?)?),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(AddonState::default()),
        Err(error) => Err(error.into()),
    }
}

fn validate_addon_state_path(path: &Path, root: &Path) -> Result<()> {
    if !path.is_absolute() || absolute_path(path)? != path {
        return Err(format!("invalid addon state path: {}", path.display()).into());
    }
    if path == root || !path.starts_with(root) {
        return Err(format!(
            "addon state path is outside state directory: {}",
            path.display()
        )
        .into());
    }
    Ok(())
}

fn validate_addon_state_parents(path: &Path, root: &Path) -> Result<()> {
    let relative = path.strip_prefix(root).map_err(|_| {
        format!(
            "addon state path is outside state directory: {}",
            path.display()
        )
    })?;
    match std::fs::symlink_metadata(root) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_dir() => {
            return Err(format!("unsafe addon state parent: {}", root.display()).into());
        }
        Ok(_) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error.into()),
    }
    let mut current = root.to_path_buf();
    let mut components = relative.components().peekable();
    while let Some(component) = components.next() {
        let Component::Normal(name) = component else {
            return Err(format!("invalid addon state path: {}", path.display()).into());
        };
        if components.peek().is_none() {
            break;
        }
        current.push(name);
        match std::fs::symlink_metadata(&current) {
            Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_dir() => {
                return Err(format!("unsafe addon state parent: {}", current.display()).into());
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(error) => return Err(error.into()),
        }
    }
    Ok(())
}

fn remove_stale_file(path: &Path, root: &Path) -> Result<()> {
    validate_addon_state_parents(path, root)?;
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_file() => {
            return Err(format!(
                "refusing to remove non-regular addon file: {}",
                path.display()
            )
            .into());
        }
        Ok(_) => std::fs::remove_file(path)?,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => return Err(error.into()),
    }
    Ok(())
}

fn remove_stale_dir(path: &Path, root: &Path) -> Result<()> {
    validate_addon_state_parents(path, root)?;
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_dir() => {
            return Err(format!(
                "refusing to remove non-directory addon path: {}",
                path.display()
            )
            .into());
        }
        Ok(_) => {
            let mut entries = std::fs::read_dir(path)?;
            if entries.next().is_some() {
                return Ok(());
            }
            match std::fs::remove_dir(path) {
                Ok(()) => {}
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                Err(error) => return Err(error.into()),
            }
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => return Err(error.into()),
    }
    Ok(())
}

fn write_addon_state(path: &Path, state: AddonState) -> Result<()> {
    static NEXT_TEMP: AtomicU64 = AtomicU64::new(0);
    let parent = path
        .parent()
        .ok_or_else(|| format!("state file has no parent: {}", path.display()))?;
    std::fs::create_dir_all(parent)?;
    let name = path
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or("state");
    let temp = parent.join(format!(
        ".{name}.{}.{}.tmp",
        std::process::id(),
        NEXT_TEMP.fetch_add(1, Ordering::Relaxed)
    ));
    let result: Result<()> = (|| {
        let mut options = std::fs::OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut file = options.open(&temp)?;
        file.write_all(&serde_json::to_vec(&state)?)?;
        file.sync_all()?;
        std::fs::rename(&temp, path)?;
        Ok(())
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(temp);
    }
    result
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
    /// Validate missing/incomplete installations; routine updates skip validation.
    async fn steam_app_install(&self, _app_id: u32, _dest: &Path) -> Result<()> {
        Err("runtime provider cannot fetch".into())
    }
    /// Bootstrap SteamCMD and return its executable path.
    async fn steamcmd_path(&self) -> Result<PathBuf> {
        Err("runtime provider cannot supply steamcmd".into())
    }
}

pub struct DriverContext<'a> {
    pub server_id: &'a str,
    pub server_dir: &'a Path,
    pub deployment_dir: &'a Path,
    pub runtimes: &'a dyn RuntimeProvider,
    pub deployment: &'a DeploymentSpec,
    pub config: &'a serde_json::Value,
    /// Service ports mapped to the agent-allocated local ports this
    /// execution must bind.
    pub ports: &'a [PortBinding],
    pub memory_mb: u32,
    pub addons: &'a [PreparedAddon],
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
    fn addon_targets(&self) -> &'static [&'static str] {
        &[]
    }
    /// Install into `deployment_dir` (idempotent).
    async fn prepare(&self, ctx: &DriverContext<'_>) -> Result<()>;
    /// Write config files into `server_dir`.
    async fn configure(&self, ctx: &DriverContext<'_>) -> Result<()>;
    fn process_spec(&self, ctx: &DriverContext<'_>) -> Result<ProcessSpec>;
    async fn spawn(
        &self,
        ctx: &DriverContext<'_>,
        executor: &dyn Executor,
    ) -> Result<Box<dyn ProcessHandle>> {
        executor.spawn(&self.process_spec(ctx)?).await
    }
    async fn probe(&self, ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<GameHealth>;
    /// Enter the snapshot barrier; returns Live or RequiresStop.
    async fn prepare_snapshot(
        &self,
        ctx: &DriverContext<'_>,
        p: &dyn ProcessHandle,
    ) -> Result<SnapshotBarrier>;
    /// The game writes a complete save when it exits on graceful_stop, so the
    /// final snapshot is taken from disk after the process has exited.
    fn snapshot_after_stop(&self) -> bool {
        false
    }
    /// The driver's `handoff` can move connected clients to a replacement
    /// server, so a clean host shutdown waits for a warm migration instead of
    /// stopping straight away.
    fn can_handoff(&self) -> bool {
        false
    }
    fn join_code_from_log(&self, _line: &str) -> Option<String> {
        None
    }
    async fn resume_after_snapshot(
        &self,
        ctx: &DriverContext<'_>,
        p: &dyn ProcessHandle,
    ) -> Result<()>;
    /// Freeze the live world so a migration snapshot is drift-free
    /// (e.g. `tick freeze`). Returns true when the driver supports it.
    async fn freeze(&self, _ctx: &DriverContext<'_>, _p: &dyn ProcessHandle) -> Result<bool> {
        Ok(false)
    }
    /// Undo `freeze` after a migration aborts.
    async fn unfreeze(&self, _ctx: &DriverContext<'_>, _p: &dyn ProcessHandle) -> Result<()> {
        Ok(())
    }
    /// Ask connected clients to move to `addr:port`, which serves the same
    /// world (e.g. `/transfer`). Returns true when the handoff was issued.
    /// Drivers that can't transfer return false and clients rejoin manually.
    async fn handoff(
        &self,
        _ctx: &DriverContext<'_>,
        _p: &dyn ProcessHandle,
        _addr: &str,
        _port: u16,
    ) -> Result<bool> {
        Ok(false)
    }
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

#[cfg(test)]
mod addon_tests {
    use super::*;

    fn addon(root: PathBuf, files: Vec<(String, String)>) -> PreparedAddon {
        PreparedAddon {
            plugin_id: "plugin".into(),
            addon_id: "addon".into(),
            root,
            target: "server".into(),
            driver_options: serde_json::json!({}),
            files,
        }
    }

    #[test]
    fn install_copies_tree_and_overlay_files() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("root");
        std::fs::create_dir_all(root.join("tree/sub")).unwrap();
        std::fs::write(root.join("tree/sub/file.txt"), "archive").unwrap();
        let dest = tmp.path().join("dest");
        let written = addon(root, vec![("cfg/server.cfg".into(), "overlay".into())])
            .install_into(&dest)
            .unwrap();
        assert_eq!(written.len(), 2);
        assert!(written.contains(&dest.join("tree/sub/file.txt")));
        assert!(written.contains(&dest.join("cfg/server.cfg")));
        assert_eq!(
            std::fs::read_to_string(dest.join("tree/sub/file.txt")).unwrap(),
            "archive"
        );
        assert_eq!(
            std::fs::read_to_string(dest.join("cfg/server.cfg")).unwrap(),
            "overlay"
        );
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(
                std::fs::metadata(dest.join("cfg/server.cfg"))
                    .unwrap()
                    .permissions()
                    .mode()
                    & 0o777,
                0o600
            );
        }
    }

    #[cfg(unix)]
    #[test]
    fn install_rejects_symlinks() {
        use std::os::unix::fs::symlink;
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("root");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::write(tmp.path().join("outside"), "outside").unwrap();
        symlink(tmp.path().join("outside"), root.join("link")).unwrap();
        let err = addon(root, vec![])
            .install_into(&tmp.path().join("dest"))
            .unwrap_err();
        assert!(err.to_string().contains("symlink"), "{err}");
    }

    #[test]
    fn install_revalidates_file_keys() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("root");
        std::fs::create_dir_all(&root).unwrap();
        for key in ["../escape", "/absolute", "a//b", r"a\b", "a:b"] {
            let err = addon(root.clone(), vec![(key.into(), "x".into())])
                .install_into(&tmp.path().join("dest"))
                .unwrap_err();
            assert!(
                err.to_string().contains("invalid addon file key"),
                "{key}: {err}"
            );
        }
    }

    #[test]
    fn sync_addons_removes_files_for_deleted_addons() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("root");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::write(root.join("addon.cfg"), "configured").unwrap();
        let state_file = tmp.path().join("server/.varde-addons.json");
        let destination = tmp.path().join("server/addons/plugin/addon");
        let addon = addon(root, vec![]);
        sync_addons(&state_file, &[(&addon, destination.clone())]).unwrap();
        assert!(destination.join("addon.cfg").exists());
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(
                std::fs::metadata(&state_file).unwrap().permissions().mode() & 0o777,
                0o600
            );
        }

        sync_addons(&state_file, &[]).unwrap();
        assert!(!destination.join("addon.cfg").exists());
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&std::fs::read(state_file).unwrap())
                .unwrap()["files"],
            serde_json::json!([])
        );
    }

    #[test]
    fn sync_addons_removes_files_dropped_by_updated_addon() {
        let tmp = tempfile::tempdir().unwrap();
        let v1_root = tmp.path().join("v1");
        let v2_root = tmp.path().join("v2");
        std::fs::create_dir_all(&v1_root).unwrap();
        std::fs::create_dir_all(&v2_root).unwrap();
        let state_file = tmp.path().join("server/.varde-addons.json");
        let destination = tmp.path().join("server/addons/plugin/addon");
        let v1 = addon(
            v1_root,
            vec![
                ("cfg/x".into(), "v1".into()),
                ("cfg/y".into(), "old".into()),
            ],
        );
        sync_addons(&state_file, &[(&v1, destination.clone())]).unwrap();

        let v2 = addon(v2_root, vec![("cfg/x".into(), "v2".into())]);
        sync_addons(&state_file, &[(&v2, destination.clone())]).unwrap();
        assert_eq!(
            std::fs::read_to_string(destination.join("cfg/x")).unwrap(),
            "v2"
        );
        assert!(!destination.join("cfg/y").exists());
    }

    #[test]
    fn sync_addons_replaces_file_with_directory() {
        let tmp = tempfile::tempdir().unwrap();
        let v1_root = tmp.path().join("v1");
        let v2_root = tmp.path().join("v2");
        std::fs::create_dir_all(&v1_root).unwrap();
        std::fs::create_dir_all(&v2_root).unwrap();
        let state_file = tmp.path().join("server/.varde-addons.json");
        let destination = tmp.path().join("server/addons/plugin/addon");
        let v1 = addon(v1_root, vec![("cfg".into(), "file".into())]);
        sync_addons(&state_file, &[(&v1, destination.clone())]).unwrap();
        assert!(destination.join("cfg").is_file());

        let v2 = addon(v2_root, vec![("cfg/x.cfg".into(), "nested".into())]);
        sync_addons(&state_file, &[(&v2, destination.clone())]).unwrap();
        assert!(destination.join("cfg").is_dir());
        assert_eq!(
            std::fs::read_to_string(destination.join("cfg/x.cfg")).unwrap(),
            "nested"
        );
    }

    #[test]
    fn sync_addons_replaces_directory_with_file() {
        let tmp = tempfile::tempdir().unwrap();
        let v1_root = tmp.path().join("v1");
        let v2_root = tmp.path().join("v2");
        std::fs::create_dir_all(v1_root.join("cfg")).unwrap();
        std::fs::create_dir_all(&v2_root).unwrap();
        std::fs::write(v1_root.join("cfg/x.cfg"), "nested").unwrap();
        let state_file = tmp.path().join("server/.varde-addons.json");
        let destination = tmp.path().join("server/addons/plugin/addon");
        let v1 = addon(v1_root, vec![]);
        sync_addons(&state_file, &[(&v1, destination.clone())]).unwrap();
        assert!(destination.join("cfg").is_dir());

        let v2 = addon(v2_root, vec![("cfg".into(), "file".into())]);
        sync_addons(&state_file, &[(&v2, destination.clone())]).unwrap();
        assert!(destination.join("cfg").is_file());
        assert_eq!(
            std::fs::read_to_string(destination.join("cfg")).unwrap(),
            "file"
        );
    }

    #[test]
    fn sync_addons_records_partial_install_for_cleanup() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("root");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::write(root.join("a.txt"), "archive").unwrap();
        let state_file = tmp.path().join("server/.varde-addons.json");
        let destination = tmp.path().join("server/addons/plugin/addon");
        let addon = addon(root, vec![("../x".into(), "invalid".into())]);

        let error = sync_addons(&state_file, &[(&addon, destination.clone())]).unwrap_err();
        assert!(
            error.to_string().contains("invalid addon file key"),
            "{error}"
        );
        assert_eq!(
            std::fs::read_to_string(destination.join("a.txt")).unwrap(),
            "archive"
        );
        let state = read_addon_state(&state_file).unwrap();
        assert!(state.files.contains(&destination.join("a.txt")));

        sync_addons(&state_file, &[]).unwrap();
        assert!(!destination.join("a.txt").exists());
    }

    #[test]
    fn sync_addons_removes_empty_created_directories() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("root");
        std::fs::create_dir_all(root.join("maps")).unwrap();
        let state_file = tmp.path().join("server/.varde-addons.json");
        let destination = tmp.path().join("server/addons/p/a");
        let addon = addon(root, vec![]);
        assert!(!destination.exists());

        sync_addons(&state_file, &[(&addon, destination.clone())]).unwrap();
        assert!(destination.join("maps").is_dir());
        sync_addons(&state_file, &[]).unwrap();

        assert!(!destination.join("maps").exists());
        assert!(!destination.exists());
        assert!(!destination.parent().unwrap().exists());
        assert!(!destination.parent().unwrap().parent().unwrap().exists());
    }

    #[test]
    fn sync_addons_preserves_preexisting_directories() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("root");
        std::fs::create_dir_all(&root).unwrap();
        let state_file = tmp.path().join("server/.varde-addons.json");
        let destination = tmp.path().join("server/addons/p/a");
        let preexisting = destination.join("cfg");
        std::fs::create_dir_all(&preexisting).unwrap();
        let addon = addon(root, vec![("cfg/x.cfg".into(), "addon".into())]);

        sync_addons(&state_file, &[(&addon, destination.clone())]).unwrap();
        let state = read_addon_state(&state_file).unwrap();
        assert!(!state.dirs.contains(&destination));
        assert!(!state.dirs.contains(&preexisting));

        sync_addons(&state_file, &[]).unwrap();
        assert!(preexisting.is_dir());
        assert!(!preexisting.join("x.cfg").exists());
    }

    #[test]
    fn sync_addons_preserves_recorded_directories_with_user_files() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("root");
        std::fs::create_dir_all(root.join("maps")).unwrap();
        let state_file = tmp.path().join("server/.varde-addons.json");
        let destination = tmp.path().join("server/addons/p/a");
        let addon = addon(root, vec![]);

        sync_addons(&state_file, &[(&addon, destination.clone())]).unwrap();
        let user_file = destination.join("maps/user.txt");
        std::fs::write(&user_file, "user data").unwrap();
        sync_addons(&state_file, &[]).unwrap();

        assert!(destination.join("maps").is_dir());
        assert_eq!(std::fs::read_to_string(user_file).unwrap(), "user data");

        std::fs::remove_file(destination.join("maps/user.txt")).unwrap();
        sync_addons(&state_file, &[]).unwrap();

        assert!(!destination.join("maps").exists());
        assert!(!destination.exists());
    }

    #[cfg(unix)]
    #[test]
    fn sync_addons_refuses_recorded_symlink_files() {
        use std::os::unix::fs::symlink;

        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("root");
        std::fs::create_dir_all(&root).unwrap();
        let state_file = tmp.path().join("server/.varde-addons.json");
        let destination = tmp.path().join("server/addons/plugin/addon");
        let addon = addon(root, vec![("cfg/x".into(), "managed".into())]);
        sync_addons(&state_file, &[(&addon, destination.clone())]).unwrap();

        let outside = tmp.path().join("outside");
        std::fs::write(&outside, "untouched").unwrap();
        let managed = destination.join("cfg/x");
        std::fs::remove_file(&managed).unwrap();
        symlink(&outside, &managed).unwrap();

        let state_before = std::fs::read(&state_file).unwrap();
        let error = sync_addons(&state_file, &[]).unwrap_err();
        assert!(
            error.to_string().contains("non-regular addon file"),
            "{error}"
        );
        assert_eq!(std::fs::read_to_string(&outside).unwrap(), "untouched");
        assert_eq!(std::fs::read_link(&managed).unwrap(), outside);
        assert_eq!(std::fs::read(&state_file).unwrap(), state_before);
    }
}
