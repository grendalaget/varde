//! testgame driver (games/testgame): barrier `Live` via the Minecraft-style
//! stdin protocol — `save-off`, `save-all`, wait for `Saved the game`,
//! `save-on`. Binary is found through a `DeploymentSpec` of kind
//! `local_binary` (path from agent config `testgame_bin`, or next to the
//! agent executable). Catalog id: `testgame`.

use std::ffi::OsString;
use std::path::PathBuf;
use std::time::Duration;

use async_trait::async_trait;
use executor_api::{ProcessHandle, ProcessSpec};
use game_driver_api::*;

pub const PORT: u32 = 7777;

#[derive(Default)]
pub struct TestgameDriver {
    /// Explicit binary path override (agent config `testgame_bin`).
    pub binary: Option<PathBuf>,
}

impl TestgameDriver {
    pub fn new() -> Self {
        Self::default()
    }
    pub fn with_binary(binary: PathBuf) -> Self {
        Self {
            binary: Some(binary),
        }
    }

    fn binary_path(&self, ctx: &DriverContext<'_>) -> Result<PathBuf> {
        if ctx.deployment.kind() != "local_binary" {
            return Err(format!(
                "testgame: unsupported deployment kind {}",
                ctx.deployment.kind()
            )
            .into());
        }
        // spec path > driver override > next to agent exe > PATH
        if let Some(p) = ctx.deployment.get("path").and_then(|v| v.as_str()) {
            let p = PathBuf::from(p);
            if p.exists() {
                return Ok(p);
            }
        }
        if let Some(b) = &self.binary {
            if b.exists() {
                return Ok(b.clone());
            }
        }
        let name = if cfg!(windows) {
            "varde-testgame.exe"
        } else {
            "varde-testgame"
        };
        if let Ok(exe) = std::env::current_exe() {
            let sib = exe.with_file_name(name);
            if sib.exists() {
                return Ok(sib);
            }
        }
        Ok(PathBuf::from(name)) // PATH lookup
    }
}

#[async_trait]
impl GameDriver for TestgameDriver {
    fn id(&self) -> &'static str {
        "testgame"
    }

    fn ports(&self, _config: &serde_json::Value) -> Vec<PortSpec> {
        vec![
            PortSpec {
                port: PORT,
                protocol: GameProtocol::Tcp,
            },
            PortSpec {
                port: PORT,
                protocol: GameProtocol::Udp,
            },
        ]
    }

    fn persistent_paths(&self, _config: &serde_json::Value) -> Vec<PathPattern> {
        vec![PathPattern::new("data/")]
    }

    fn addon_targets(&self) -> &'static [&'static str] {
        &["server"]
    }

    fn validate(&self, _config: &serde_json::Value) -> Result<()> {
        Ok(())
    }

    async fn prepare(&self, ctx: &DriverContext<'_>) -> Result<()> {
        // local_binary: verify the binary resolves; nothing to install
        self.binary_path(ctx).map(|_| ())
    }

    async fn configure(&self, ctx: &DriverContext<'_>) -> Result<()> {
        for addon in ctx.addons.iter().filter(|addon| addon.target == "server") {
            let destination = ctx
                .server_dir
                .join("addons")
                .join(&addon.plugin_id)
                .join(&addon.addon_id);
            if destination.exists() {
                std::fs::remove_dir_all(&destination)?;
            }
            addon.install_into(&destination)?;
        }
        Ok(())
    }

    fn process_spec(&self, ctx: &DriverContext<'_>) -> Result<ProcessSpec> {
        Ok(ProcessSpec {
            program: self.binary_path(ctx)?,
            args: vec![
                OsString::from("--port"),
                OsString::from(ctx.local_port(PORT).to_string()),
            ],
            env: vec![],
            cwd: ctx.server_dir.to_path_buf(),
            stdin: true,
        })
    }

    async fn probe(&self, _ctx: &DriverContext<'_>, _p: &dyn ProcessHandle) -> Result<GameHealth> {
        match std::net::TcpStream::connect(("127.0.0.1", _ctx.local_port(PORT) as u16)) {
            Ok(_) => Ok(GameHealth::Healthy),
            Err(_) => Ok(GameHealth::Starting),
        }
    }

    async fn prepare_snapshot(
        &self,
        _ctx: &DriverContext<'_>,
        p: &dyn ProcessHandle,
    ) -> Result<SnapshotBarrier> {
        let mut rx = p.output();
        p.write_stdin("save-off").await?;
        p.write_stdin("save-all").await?;
        // wait for "Saved the game" (bounded)
        let deadline = std::time::Instant::now() + Duration::from_secs(30);
        loop {
            match tokio::time::timeout(Duration::from_millis(200), rx.recv()).await {
                Ok(Ok(l)) if l.line.contains("Saved the game") => return Ok(SnapshotBarrier::Live),
                Ok(_) => {}
                Err(_) => {
                    if std::time::Instant::now() > deadline {
                        return Ok(SnapshotBarrier::RequiresStop);
                    }
                }
            }
        }
    }

    async fn resume_after_snapshot(
        &self,
        _ctx: &DriverContext<'_>,
        p: &dyn ProcessHandle,
    ) -> Result<()> {
        p.write_stdin("save-on").await
    }

    async fn graceful_stop(&self, _ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<()> {
        p.write_stdin("stop").await?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct AddonRuntime {
        root: PathBuf,
    }

    struct NullRuntimes;
    #[async_trait]
    impl RuntimeProvider for NullRuntimes {
        fn runtime_path(&self, _k: &str, _id: &str) -> Option<PathBuf> {
            None
        }
    }

    #[async_trait]
    impl RuntimeProvider for AddonRuntime {
        fn runtime_path(&self, _k: &str, _id: &str) -> Option<PathBuf> {
            None
        }

        async fn fetch(&self, kind: &str, _id: &str, spec: &FetchSpec) -> Result<PathBuf> {
            assert_eq!(kind, "addons");
            assert_eq!(spec.file_name, "addon.zip");
            Ok(self.root.clone())
        }
    }

    #[test]
    fn spec_uses_local_port() {
        let tmp = tempfile::tempdir().unwrap();
        let rt = NullRuntimes;
        let cfg = serde_json::json!({});
        let bindings = [PortBinding {
            service_port: PORT,
            local_port: 29876,
            protocol: GameProtocol::Tcp,
        }];
        let dep = DeploymentSpec::parse(&serde_json::json!({}));
        let ctx = DriverContext {
            server_id: "testgame-test",
            server_dir: tmp.path(),
            deployment_dir: tmp.path(),
            runtimes: &rt,
            deployment: &dep,
            config: &cfg,
            ports: &bindings,
            memory_mb: 64,
            addons: &[],
        };
        let spec = TestgameDriver::default().process_spec(&ctx).unwrap();
        let args: Vec<String> = spec
            .args
            .iter()
            .map(|a| a.to_string_lossy().into())
            .collect();
        let i = args.iter().position(|a| a == "--port").unwrap();
        assert_eq!(args[i + 1], "29876");
    }

    #[tokio::test]
    async fn configure_replaces_and_installs_server_addons() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("fetched-addon");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::write(root.join("archive.txt"), "archive").unwrap();
        let server_dir = tmp.path().join("server");
        let destination = server_dir.join("addons/plugin/addon");
        std::fs::create_dir_all(&destination).unwrap();
        std::fs::write(destination.join("stale.txt"), "stale").unwrap();
        let rt = AddonRuntime { root: root.clone() };
        let fetched_root = rt
            .fetch(
                "addons",
                "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                &FetchSpec {
                    url: "https://example.test/addon.zip".into(),
                    file_name: "addon.zip".into(),
                    sha256: Some(
                        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa".into(),
                    ),
                    sha1: None,
                    archive: ArchiveKind::Zip,
                    untrusted: false,
                },
            )
            .await
            .unwrap();
        let cfg = serde_json::json!({});
        let dep = DeploymentSpec::parse(&serde_json::json!({}));
        let addons = [PreparedAddon {
            plugin_id: "plugin".into(),
            addon_id: "addon".into(),
            root: fetched_root,
            target: "server".into(),
            driver_options: serde_json::json!({"flag": true}),
            files: vec![("cfg/settings.ini".into(), "configured".into())],
        }];
        let ctx = DriverContext {
            server_id: "testgame-addon-test",
            server_dir: &server_dir,
            deployment_dir: tmp.path(),
            runtimes: &rt,
            deployment: &dep,
            config: &cfg,
            ports: &[],
            memory_mb: 64,
            addons: &addons,
        };
        TestgameDriver::new().configure(&ctx).await.unwrap();
        assert!(destination.join("archive.txt").exists());
        assert_eq!(
            std::fs::read_to_string(destination.join("cfg/settings.ini")).unwrap(),
            "configured"
        );
        assert!(!destination.join("stale.txt").exists());
    }
}
