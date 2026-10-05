//! Valheim dedicated server driver: SteamCMD app 896660 via the
//! RuntimeProvider, periodic saves and a 60-second in-progress-save barrier.
//! Completed saves are write-then-rename; the final snapshot follows graceful
//! shutdown. Linux requires glibc >= 2.29 and Valheim's shared libraries.

#[cfg(all(target_os = "linux", target_env = "gnu"))]
use std::ffi::CStr;
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

    fn preset(config: &serde_json::Value) -> Result<Option<&'static str>> {
        let Some(value) = config.get("modifiers") else {
            return Ok(None);
        };
        let value = value
            .as_str()
            .ok_or("valheim: config modifiers must be a string")?;
        let name = match value {
            "normal" => "Normal",
            "casual" => "Casual",
            "easy" => "Easy",
            "hard" => "Hard",
            "hardcore" => "Hardcore",
            "immersive" => "Immersive",
            "hammer" => "Hammer",
            _ => return Err(format!("valheim: unknown modifiers preset {value:?}").into()),
        };
        Ok(Some(name))
    }

    fn save_interval_s(config: &serde_json::Value) -> Result<i64> {
        let Some(value) = config.get("save_interval_s") else {
            return Ok(300);
        };
        let seconds = value
            .as_i64()
            .ok_or("valheim: config save_interval_s must be an integer")?;
        if !(60..=3600).contains(&seconds) {
            return Err("valheim: save_interval_s must be between 60 and 3600".into());
        }
        Ok(seconds)
    }

    fn crossplay(config: &serde_json::Value) -> Result<bool> {
        Ok(config
            .get("crossplay")
            .map(|value| {
                value
                    .as_bool()
                    .ok_or("valheim: config crossplay must be a boolean")
            })
            .transpose()?
            .unwrap_or(false))
    }

    fn instance_id(server_id: &str) -> String {
        let sanitized: String = server_id
            .chars()
            .map(|character| {
                if character.is_ascii_alphanumeric() || matches!(character, '-' | '_') {
                    character
                } else {
                    '_'
                }
            })
            .collect();
        format!("varde-{sanitized}")
    }

    fn valid_world_name(world: &str) -> bool {
        !world.is_empty() && !world.chars().any(|c| matches!(c, '/' | '\\' | '*' | '?'))
    }

    /// argv after the binary, per agent.md.
    pub fn args(
        config: &serde_json::Value,
        server_id: &str,
        server_dir: &Path,
        port: u32,
    ) -> Result<Vec<OsString>> {
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
        let save_interval_s = Self::save_interval_s(config)?;
        let savedir = server_dir.join("saves");
        let mut args = vec![
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
            OsString::from("-saveinterval"),
            OsString::from(save_interval_s.to_string()),
            OsString::from("-backups"),
            OsString::from("0"),
        ];
        if let Some(preset) = Self::preset(config)? {
            args.extend([OsString::from("-preset"), OsString::from(preset)]);
        }
        if Self::crossplay(config)? {
            args.extend([
                OsString::from("-crossplay"),
                OsString::from("-instanceid"),
                OsString::from(Self::instance_id(server_id)),
            ]);
        }
        Ok(args)
    }

    fn parse_join_code(line: &str) -> Option<String> {
        let code = line.split_once("join code ")?.1.split_whitespace().next()?;
        if !code.is_empty() && code.bytes().all(|byte| byte.is_ascii_digit()) {
            Some(code.to_string())
        } else {
            None
        }
    }

    /// Valheim marks a completed world save with `World save (5/5) done`.
    pub fn is_world_saved(line: &str) -> bool {
        line.contains("World saved") || line.contains("World save (5/5) done")
    }
    pub fn is_world_save_started(line: &str) -> bool {
        line.contains("World save (") && !Self::is_world_saved(line)
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

    fn persistent_paths(&self, config: &serde_json::Value) -> Vec<PathPattern> {
        let mut paths = Vec::new();
        if let Some(world) = config.get("world_name").and_then(|v| v.as_str()) {
            if Self::valid_world_name(world) {
                let world_dir = format!("saves/worlds_local/{world}");
                paths.extend([
                    PathPattern::new(format!("saves/worlds_local/{world}.db")),
                    PathPattern::new(format!("saves/worlds_local/{world}.fwl")),
                    PathPattern::new(format!("{world_dir}/_main.*.db2")),
                    PathPattern::new(format!("{world_dir}/_main.*.fwl2")),
                    PathPattern::new(format!("{world_dir}/_main.*.chunks")),
                    PathPattern::new(format!("{world_dir}/_main.*.ok")),
                    PathPattern::new(format!("{world_dir}/*.chunk")),
                ]);
            }
        }
        paths.push(PathPattern::new("saves/*.txt"));
        paths
    }

    fn validate(&self, config: &serde_json::Value) -> Result<()> {
        Self::cfg_str(config, "server_name")?;
        let world = Self::cfg_str(config, "world_name")?;
        if !Self::valid_world_name(world) {
            return Err(
                "valheim: world_name must be non-empty and cannot contain /, \\, * or ?".into(),
            );
        }
        let pw = Self::cfg_str(config, "password")?;
        if pw.len() < 5 {
            return Err("valheim: password must be at least 5 characters".into());
        }
        Self::preset(config)?;
        Self::save_interval_s(config)?;
        Self::crossplay(config)?;
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
        #[cfg(target_os = "linux")]
        linux_preflight(&Self::binary(ctx), &Self::server_dir_dep(ctx)).await?;
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
            args: Self::args(
                ctx.config,
                ctx.server_id,
                ctx.server_dir,
                ctx.local_port(PORT),
            )?,
            env,
            cwd: dep,
            stdin: false,
        })
    }

    fn join_code_from_log(&self, line: &str) -> Option<String> {
        Self::parse_join_code(line)
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
        // Files are consistent after the fifth save phase. Only block
        // snapshots when the output tail shows an unfinished save.
        let mut rx = p.output();
        let tail = p.output_tail(200);
        let last_saved = tail.iter().rposition(|l| Self::is_world_saved(&l.line));
        let save_in_progress = tail.iter().enumerate().any(|(i, l)| {
            Self::is_world_save_started(&l.line) && last_saved.is_none_or(|saved| i > saved)
        });
        if !save_in_progress {
            return Ok(SnapshotBarrier::Live);
        }

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

    fn snapshot_after_stop(&self) -> bool {
        true
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

#[cfg(target_os = "linux")]
async fn linux_preflight(binary: &Path, server_dir: &Path) -> Result<()> {
    #[cfg(target_env = "gnu")]
    {
        let version = unsafe { CStr::from_ptr(libc::gnu_get_libc_version()) }
            .to_string_lossy()
            .into_owned();
        let mut components = version.split('.').filter_map(|n| n.parse::<u32>().ok());
        let major = components.next().unwrap_or(0);
        let minor = components.next().unwrap_or(0);
        if (major, minor) < (2, 29) {
            return Err(format!("Valheim requires glibc >= 2.29; found {version}").into());
        }
    }

    warn_missing_runtime_libraries().await;

    let output = match tokio::process::Command::new("ldd")
        .arg(binary)
        .env("LD_LIBRARY_PATH", server_dir.join("linux64"))
        .output()
        .await
    {
        Ok(output) => output,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e.into()),
    };
    let text = format!(
        "{}\n{}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    let missing: Vec<&str> = text
        .lines()
        .filter(|line| line.contains("=> not found"))
        .filter_map(|line| line.split_whitespace().next())
        .collect();
    if !missing.is_empty() {
        return Err(format!(
            "Valheim server is missing shared libraries: {}. Install libatomic1 and libpulse0 on Debian/Ubuntu.",
            missing.join(", ")
        )
        .into());
    }
    if !output.status.success() {
        return Err(format!("ldd could not inspect Valheim server: {text}").into());
    }
    Ok(())
}

#[cfg(target_os = "linux")]
async fn warn_missing_runtime_libraries() {
    let mut available = None;
    for command in ["ldconfig", "/sbin/ldconfig", "/usr/sbin/ldconfig"] {
        match tokio::process::Command::new(command)
            .arg("-p")
            .output()
            .await
        {
            Ok(output) if output.status.success() => {
                available = Some(output);
                break;
            }
            Ok(_) | Err(_) => {}
        }
    }
    let Some(output) = available else {
        return;
    };
    let output = String::from_utf8_lossy(&output.stdout);
    let missing: Vec<&str> = ["libatomic.so.1", "libpulse.so.0"]
        .into_iter()
        .filter(|library| !output.contains(library))
        .collect();
    if !missing.is_empty() {
        tracing::warn!(
            libraries = %missing.join(", "),
            packages = "libatomic1 libpulse0",
            "Valheim runtime libraries are not listed by ldconfig"
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use executor_api::{ExitStatus, OutputLine, OutputStream, ResourceUsage};
    use serde_json::json;
    use std::sync::Arc;
    use tokio::sync::broadcast;

    fn cfg() -> serde_json::Value {
        json!({
            "server_name": "My Server",
            "world_name": "Dedicated",
            "password": "hunter22",
            "modifiers": "normal",
        })
    }

    #[test]
    fn args_shape() {
        let a = ValheimDriver::args(&cfg(), "srv-123", Path::new("/srv"), 2456).unwrap();
        let s: Vec<String> = a.iter().map(|o| o.to_string_lossy().into()).collect();
        assert!(s.contains(&"-nographics".to_string()));
        assert!(s.contains(&"-batchmode".to_string()));
        let port_i = s.iter().position(|x| x == "-port").unwrap();
        assert_eq!(s[port_i + 1], "2456");
        // agent-allocated local port is used verbatim
        let a2 = ValheimDriver::args(&cfg(), "srv-123", Path::new("/srv"), 32101).unwrap();
        let s2: Vec<String> = a2.iter().map(|o| o.to_string_lossy().into()).collect();
        let pi2 = s2.iter().position(|x| x == "-port").unwrap();
        assert_eq!(s2[pi2 + 1], "32101");
        assert!(s.contains(&"-savedir".to_string()));
        assert!(s.contains(&"1".to_string()) || s.contains(&"0".to_string()));
        let pub_i = s.iter().position(|x| x == "-public").unwrap();
        assert_eq!(s[pub_i + 1], "0");
        let interval_i = s.iter().position(|x| x == "-saveinterval").unwrap();
        assert_eq!(s[interval_i + 1], "300");
        let backups_i = s.iter().position(|x| x == "-backups").unwrap();
        assert_eq!(s[backups_i + 1], "0");
        let preset_i = s.iter().position(|x| x == "-preset").unwrap();
        assert_eq!(s[preset_i + 1], "Normal");
        assert!(!s
            .iter()
            .any(|arg| arg == "-crossplay" || arg == "-instanceid"));
    }

    #[test]
    fn crossplay_args_use_a_sanitized_server_instance_id() {
        let mut config = cfg();
        config["crossplay"] = json!(true);
        let args = ValheimDriver::args(&config, "srv /123", Path::new("/srv"), 2456).unwrap();
        let args: Vec<String> = args
            .iter()
            .map(|arg| arg.to_string_lossy().into())
            .collect();
        assert!(args.contains(&"-crossplay".to_string()));
        let instance_id = args.iter().position(|arg| arg == "-instanceid").unwrap();
        assert_eq!(args[instance_id + 1], "varde-srv__123");
        assert!(ValheimDriver::new().validate(&config).is_ok());

        config["crossplay"] = json!("true");
        assert!(ValheimDriver::new().validate(&config).is_err());
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
        for world in ["", "nested/world", "bad\\world", "glob*", "glob?"] {
            let mut bad_world = cfg();
            bad_world["world_name"] = json!(world);
            assert!(
                ValheimDriver::new().validate(&bad_world).is_err(),
                "{world:?}"
            );
        }
        for preset in ["", "extreme"] {
            let mut bad_preset = cfg();
            bad_preset["modifiers"] = json!(preset);
            assert!(ValheimDriver::new().validate(&bad_preset).is_err());
        }
        let mut bad_crossplay = cfg();
        bad_crossplay["crossplay"] = json!("true");
        assert!(ValheimDriver::new().validate(&bad_crossplay).is_err());
    }

    #[test]
    fn log_parsing() {
        assert!(ValheimDriver::is_world_saved(
            "01/01/2026 12:00:00: World saved ( 12.3ms )"
        ));
        assert!(ValheimDriver::is_world_saved(
            "10/05/2026 22:07:59: World save (5/5) done. Total time [30ms]"
        ));
        assert!(!ValheimDriver::is_world_saved("World saving"));
        assert!(ValheimDriver::is_listening(
            "01/01/2026 12:00:00: Game server connected"
        ));
        assert!(!ValheimDriver::is_listening("DungeonDB Start"));
        assert!(ValheimDriver::is_world_save_started(
            "10/05/2026 22:07:59: World save (1/5) Cloud & Backup checks done [0ms]"
        ));
        assert!(ValheimDriver::is_world_save_started(
            "10/05/2026 22:07:59: World save (3/5) DB2 writing done [15ms]"
        ));
        assert!(!ValheimDriver::is_world_save_started("World saved"));
        assert!(!ValheimDriver::is_world_save_started(
            "10/05/2026 22:07:59: World save (5/5) done. Total time [30ms]"
        ));
    }

    #[test]
    fn join_code_parsing_ignores_timestamps_and_empty_codes() {
        let driver = ValheimDriver::new();
        assert_eq!(
            driver.join_code_from_log(
                "10/05/2026 22:08:01: Session \"Varde XP Test\" registered with join code 124841"
            ),
            Some("124841".into())
        );
        assert_eq!(
            driver.join_code_from_log(
                "10/05/2026 22:08:02: Session \"Varde XP Test\" with join code 124841 and IP 140.232.64.3:24560 is active with 0 player(s)"
            ),
            Some("124841".into())
        );
        assert_eq!(
            driver.join_code_from_log(
                "10/05/2026 22:08:00: New session server \"Varde XP Test\" that has join code , now 0 player(s)"
            ),
            None
        );
        assert_eq!(
            driver.join_code_from_log("10/05/2026 22:08:03: Game server connected"),
            None
        );
    }

    #[test]
    fn preset_mapping_and_save_interval_bounds() {
        for (config_value, expected) in [
            ("normal", "Normal"),
            ("casual", "Casual"),
            ("easy", "Easy"),
            ("hard", "Hard"),
            ("hardcore", "Hardcore"),
            ("immersive", "Immersive"),
            ("hammer", "Hammer"),
        ] {
            let mut config = cfg();
            config["modifiers"] = json!(config_value);
            assert_eq!(ValheimDriver::preset(&config).unwrap(), Some(expected));
            let args = ValheimDriver::args(&config, "srv-123", Path::new("/srv"), 2456).unwrap();
            let args: Vec<String> = args
                .iter()
                .map(|arg| arg.to_string_lossy().into())
                .collect();
            let preset = args.iter().position(|arg| arg == "-preset").unwrap();
            assert_eq!(args[preset + 1], expected);
        }

        let mut without_preset = cfg();
        without_preset.as_object_mut().unwrap().remove("modifiers");
        let args =
            ValheimDriver::args(&without_preset, "srv-123", Path::new("/srv"), 2456).unwrap();
        assert!(!args.iter().any(|arg| arg == "-preset"));
        assert_eq!(
            ValheimDriver::save_interval_s(&without_preset).unwrap(),
            300
        );

        for seconds in [60, 3600] {
            let mut valid = cfg();
            valid["save_interval_s"] = json!(seconds);
            assert!(ValheimDriver::new().validate(&valid).is_ok());
        }
        for seconds in [59, 3601] {
            let mut invalid = cfg();
            invalid["save_interval_s"] = json!(seconds);
            assert!(ValheimDriver::new().validate(&invalid).is_err());
        }
    }

    #[test]
    fn persistent_paths_capture_only_named_world_files() {
        let mut config = cfg();
        config["world_name"] = json!("Varde Test");
        let paths = ValheimDriver::new().persistent_paths(&config);
        assert_eq!(
            paths,
            vec![
                PathPattern::new("saves/worlds_local/Varde Test.db"),
                PathPattern::new("saves/worlds_local/Varde Test.fwl"),
                PathPattern::new("saves/worlds_local/Varde Test/_main.*.db2"),
                PathPattern::new("saves/worlds_local/Varde Test/_main.*.fwl2"),
                PathPattern::new("saves/worlds_local/Varde Test/_main.*.chunks"),
                PathPattern::new("saves/worlds_local/Varde Test/_main.*.ok"),
                PathPattern::new("saves/worlds_local/Varde Test/*.chunk"),
                PathPattern::new("saves/*.txt"),
            ]
        );
        for (pattern, path) in paths.iter().zip([
            "saves/worlds_local/Varde Test.db",
            "saves/worlds_local/Varde Test.fwl",
            "saves/worlds_local/Varde Test/_main.4.db2",
            "saves/worlds_local/Varde Test/_main.4.fwl2",
            "saves/worlds_local/Varde Test/_main.4.chunks",
            "saves/worlds_local/Varde Test/_main.4.ok",
            "saves/worlds_local/Varde Test/00_00__0_2.chunk",
        ]) {
            assert!(
                pattern.matches(path, true),
                "{pattern:?} does not match {path}"
            );
        }
        assert!(paths
            .iter()
            .all(|pattern| !pattern.matches("saves/worlds_local/Other World/_main.4.db2", true)));
        assert!(paths.iter().all(|pattern| {
            !pattern.matches("saves/worlds_local/Other World.db", true)
                && !pattern.matches("saves/worlds_local/Other World.fwl", true)
        }));
        assert!(ValheimDriver::new().snapshot_after_stop());
    }

    struct NoRuntimes;

    #[async_trait]
    impl RuntimeProvider for NoRuntimes {
        fn runtime_path(&self, _kind: &str, _id: &str) -> Option<PathBuf> {
            None
        }
    }

    struct TailProcess {
        tail: Vec<OutputLine>,
        output: broadcast::Sender<OutputLine>,
        complete_on_subscribe: bool,
    }

    #[async_trait]
    impl ProcessHandle for TailProcess {
        fn pid(&self) -> u32 {
            1
        }
        async fn write_stdin(&self, _line: &str) -> executor_api::Result<()> {
            Ok(())
        }
        fn output(&self) -> broadcast::Receiver<OutputLine> {
            let receiver = self.output.subscribe();
            if self.complete_on_subscribe {
                let _ = self.output.send(OutputLine {
                    at_unix_ms: 300,
                    stream: OutputStream::Stdout,
                    line: "World save (5/5) done. Total time [30ms]".into(),
                });
            }
            receiver
        }
        fn output_tail(&self, _n: usize) -> Vec<OutputLine> {
            self.tail.clone()
        }
        async fn wait(&self) -> executor_api::Result<ExitStatus> {
            Ok(ExitStatus::default())
        }
        async fn terminate(&self) -> executor_api::Result<()> {
            Ok(())
        }
        async fn interrupt(&self) -> executor_api::Result<()> {
            Ok(())
        }
        async fn kill(&self) -> executor_api::Result<()> {
            Ok(())
        }
        fn resource_usage(&self) -> Option<ResourceUsage> {
            None
        }
    }

    fn output_line(at_unix_ms: i64, line: &str) -> OutputLine {
        OutputLine {
            at_unix_ms,
            stream: OutputStream::Stdout,
            line: Arc::from(line),
        }
    }

    #[tokio::test]
    async fn barrier_waits_only_when_tail_shows_an_unfinished_save() {
        let driver = ValheimDriver::new();
        let runtimes = NoRuntimes;
        let deployment = DeploymentSpec::parse(&json!({}));
        let config = cfg();
        let ports = [];
        let ctx = DriverContext {
            server_id: "valheim-test",
            server_dir: Path::new("/srv"),
            deployment_dir: Path::new("/dep"),
            runtimes: &runtimes,
            deployment: &deployment,
            config: &config,
            ports: &ports,
            memory_mb: 2048,
        };
        let (output, _) = broadcast::channel(8);
        let in_progress = TailProcess {
            tail: vec![
                output_line(100, "World save (5/5) done. Total time [30ms]"),
                output_line(200, "World save (1/5) Cloud & Backup checks done [0ms]"),
            ],
            output,
            complete_on_subscribe: true,
        };
        assert_eq!(
            driver.prepare_snapshot(&ctx, &in_progress).await.unwrap(),
            SnapshotBarrier::Live
        );

        let (output, _) = broadcast::channel(8);
        let idle = TailProcess {
            tail: vec![
                output_line(100, "World save (5/5) done. Total time [30ms]"),
                output_line(200, "World save (1/5) Cloud & Backup checks done [0ms]"),
                output_line(300, "World save (5/5) done. Total time [30ms]"),
            ],
            output,
            complete_on_subscribe: false,
        };
        assert_eq!(
            driver.prepare_snapshot(&ctx, &idle).await.unwrap(),
            SnapshotBarrier::Live
        );
    }
}
