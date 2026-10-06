//! valheim dedicated server driver (agent.md): steamcmd app 896660 via the
//! RuntimeProvider, `-nographics -batchmode -name -port 2456 -world
//! -password -public -savedir <server_dir>/saves`, env
//! LD_LIBRARY_PATH=<dep>/linux64 + SteamAppId=892970; password >= 5 chars;
//! barrier waits for a `World saved` log line bounded to 60 s else
//! RequiresStop; stop = SIGINT (Linux) / CTRL_BREAK (Windows).

use std::ffi::OsString;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use async_trait::async_trait;
use executor_api::{ProcessHandle, ProcessSpec};
use game_driver_api::*;

pub const APP_ID: u32 = 896660;
pub const STEAM_APP_ID: u32 = 892970;
pub const PORT: u32 = 2456;

#[derive(Default)]
pub struct ValheimDriver;

impl ValheimDriver {
    pub fn new() -> Self {
        Self
    }

    fn server_dir_dep(ctx: &DriverContext<'_>) -> PathBuf {
        ctx.deployment_dir.join("server")
    }

    fn binary(ctx: &DriverContext<'_>) -> PathBuf {
        let dir = Self::server_dir_dep(ctx);
        if cfg!(windows) {
            dir.join("valheim_server.exe")
        } else {
            dir.join("valheim_server.x86_64")
        }
    }

    fn cfg_str<'a>(config: &'a serde_json::Value, k: &str) -> Result<&'a str> {
        config
            .get(k)
            .and_then(|v| v.as_str())
            .ok_or(format!("valheim: config {k} required").into())
    }

    /// argv after the binary, per agent.md.
    pub fn args(config: &serde_json::Value, server_dir: &Path, port: u32) -> Result<Vec<OsString>> {
        let name = Self::cfg_str(config, "server_name")?;
        let world = Self::cfg_str(config, "world_name")?;
        let pw = Self::cfg_str(config, "password")?;
        if pw.len() < 5 {
            return Err("valheim: password must be at least 5 characters".into());
        }
        let public = config
            .get("public")
            .and_then(|v| v.as_bool())
            .unwrap_or(false);
        let savedir = server_dir.join("saves");
        Ok(vec![
            OsString::from("-nographics"),
            OsString::from("-batchmode"),
            OsString::from("-name"),
            OsString::from(name),
            OsString::from("-port"),
            OsString::from(port.to_string()),
            OsString::from("-world"),
            OsString::from(world),
            OsString::from("-password"),
            OsString::from(pw),
            OsString::from("-public"),
            OsString::from(if public { "1" } else { "0" }),
            OsString::from("-savedir"),
            savedir.into_os_string(),
        ])
    }

    /// `World saved` marks a completed write-then-rename world save.
    pub fn is_world_saved(line: &str) -> bool {
        line.contains("World saved")
    }
    /// Log line once the server is listening/registered.
    pub fn is_listening(line: &str) -> bool {
        line.contains("Game server connected")
    }
}

#[async_trait]
impl GameDriver for ValheimDriver {
    fn id(&self) -> &'static str {
        "valheim"
    }

    fn ports(&self, _config: &serde_json::Value) -> Vec<PortSpec> {
        vec![
            PortSpec {
                port: PORT,
                protocol: GameProtocol::Udp,
            },
            PortSpec {
                port: PORT + 1,
                protocol: GameProtocol::Udp,
            },
        ]
    }

    fn persistent_paths(&self, _config: &serde_json::Value) -> Vec<PathPattern> {
        vec![
            PathPattern::new("saves/worlds_local/"),
            PathPattern::new("saves/*.txt"),
        ]
    }

    fn validate(&self, config: &serde_json::Value) -> Result<()> {
        Self::cfg_str(config, "server_name")?;
        Self::cfg_str(config, "world_name")?;
        let pw = Self::cfg_str(config, "password")?;
        if pw.len() < 5 {
            return Err("valheim: password must be at least 5 characters".into());
        }
        Ok(())
    }

    async fn prepare(&self, ctx: &DriverContext<'_>) -> Result<()> {
        // steamcmd bootstrap + app 896660 validate, all via the provider
        ctx.runtimes
            .steam_app_install(APP_ID, &Self::server_dir_dep(ctx))
            .await?;
        if !Self::binary(ctx).exists() {
            return Err(format!(
                "valheim server binary missing at {}",
                Self::binary(ctx).display()
            )
            .into());
        }
        Ok(())
    }

    async fn configure(&self, _ctx: &DriverContext<'_>) -> Result<()> {
        Ok(())
    }

    fn process_spec(&self, ctx: &DriverContext<'_>) -> Result<ProcessSpec> {
        let dep = Self::server_dir_dep(ctx);
        let env = vec![
            (
                OsString::from("LD_LIBRARY_PATH"),
                dep.join("linux64").into_os_string(),
            ),
            (
                OsString::from("SteamAppId"),
                OsString::from(STEAM_APP_ID.to_string()),
            ),
            (
                OsString::from("SteamGameId"),
                OsString::from(STEAM_APP_ID.to_string()),
            ),
        ];
        Ok(ProcessSpec {
            program: Self::binary(ctx),
            args: Self::args(ctx.config, ctx.server_dir, ctx.local_port(PORT))?,
            env,
            cwd: dep,
            stdin: false,
        })
    }

    async fn probe(&self, _ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<GameHealth> {
        if p.output_tail(200)
            .iter()
            .any(|l| Self::is_listening(&l.line))
        {
            Ok(GameHealth::Healthy)
        } else {
            Ok(GameHealth::Starting)
        }
    }

    async fn prepare_snapshot(
        &self,
        _ctx: &DriverContext<'_>,
        p: &dyn ProcessHandle,
    ) -> Result<SnapshotBarrier> {
        // Valheim saves with write-then-rename; files are consistent right
        // after a `World saved` line. Autosaves are ~20 min apart, so the
        // 60 s bound usually falls through to RequiresStop unless one lands.
        let mut rx = p.output();
        let deadline = Instant::now() + Duration::from_secs(60);
        loop {
            match tokio::time::timeout(Duration::from_millis(500), rx.recv()).await {
                Ok(Ok(l)) if Self::is_world_saved(&l.line) => return Ok(SnapshotBarrier::Live),
                Ok(_) => {}
                Err(_) => {
                    if Instant::now() > deadline {
                        return Ok(SnapshotBarrier::RequiresStop);
                    }
                }
            }
        }
    }

    async fn resume_after_snapshot(
        &self,
        _ctx: &DriverContext<'_>,
        _p: &dyn ProcessHandle,
    ) -> Result<()> {
        Ok(())
    }

    async fn graceful_stop(&self, _ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<()> {
        p.interrupt().await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn cfg() -> serde_json::Value {
        json!({
            "server_name": "My Server",
            "world_name": "Dedicated",
            "password": "hunter22",
        })
    }

    #[test]
    fn args_shape() {
        let a = ValheimDriver::args(&cfg(), Path::new("/srv"), 2456).unwrap();
        let s: Vec<String> = a.iter().map(|o| o.to_string_lossy().into()).collect();
        assert!(s.contains(&"-nographics".to_string()));
        assert!(s.contains(&"-batchmode".to_string()));
        let port_i = s.iter().position(|x| x == "-port").unwrap();
        assert_eq!(s[port_i + 1], "2456");
        // agent-allocated local port is used verbatim
        let a2 = ValheimDriver::args(&cfg(), Path::new("/srv"), 32101).unwrap();
        let s2: Vec<String> = a2.iter().map(|o| o.to_string_lossy().into()).collect();
        let pi2 = s2.iter().position(|x| x == "-port").unwrap();
        assert_eq!(s2[pi2 + 1], "32101");
        assert!(s.contains(&"-savedir".to_string()));
        assert!(s.contains(&"1".to_string()) || s.contains(&"0".to_string()));
        let pub_i = s.iter().position(|x| x == "-public").unwrap();
        assert_eq!(s[pub_i + 1], "0");
    }

    #[test]
    fn validation() {
        assert!(ValheimDriver::new().validate(&cfg()).is_ok());
        let mut bad = cfg();
        bad["password"] = json!("abc");
        assert!(ValheimDriver::new().validate(&bad).is_err());
        let mut bad2 = cfg();
        bad2.as_object_mut().unwrap().remove("server_name");
        assert!(ValheimDriver::new().validate(&bad2).is_err());
    }

    #[test]
    fn log_parsing() {
        assert!(ValheimDriver::is_world_saved(
            "01/01/2026 12:00:00: World saved ( 12.3ms )"
        ));
        assert!(!ValheimDriver::is_world_saved("World saving"));
        assert!(ValheimDriver::is_listening(
            "01/01/2026 12:00:00: Game server connected"
        ));
        assert!(!ValheimDriver::is_listening("DungeonDB Start"));
    }
}
