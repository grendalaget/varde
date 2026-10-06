//! minecraft (Java edition) driver, per docs/architecture/agent.md:
//! Java >= 21 on PATH or a downloaded Temurin 21 JRE (Adoptium API, sha256
//! verified via the RuntimeProvider), server jar from Mojang's version
//! manifest (sha1 verified, cached under runtimes/minecraft/<ver>-<sha1>),
//! `eula_accepted` gates eula.txt, owned server.properties keys merged
//! without touching the rest, `-Xmx`/`-Xms` from memory_mb, and the
//! save-off/save-all/save-on barrier on stdin.

use std::ffi::OsString;
use std::net::TcpStream;
use std::path::PathBuf;
use std::time::{Duration, Instant};

use async_trait::async_trait;
use executor_api::{ProcessHandle, ProcessSpec};
use game_driver_api::*;

const MANIFEST_URL: &str = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json";
pub const PORT: u32 = 25565;
const JAVA_MIN: u32 = 21;
/// Feature release downloaded when PATH has no suitable java. Pinned to
/// the current LTS rather than JAVA_MIN: recent server jars are compiled
/// for newer class-file versions (e.g. 69 = Java 25).
const JAVA_DOWNLOAD: u32 = 25;

#[derive(Default)]
pub struct MinecraftDriver;

impl MinecraftDriver {
    pub fn new() -> Self {
        Self
    }

    /// Resolved install: java binary + server jar, recorded in
    /// `deployment_dir/install.json` by prepare() so process_spec() can
    /// rebuild the launch line without another network round trip.
    fn install(ctx: &DriverContext<'_>) -> Result<(PathBuf, PathBuf)> {
        let p = ctx.deployment_dir.join("install.json");
        let v: serde_json::Value = serde_json::from_str(
            &std::fs::read_to_string(&p)
                .map_err(|e| format!("read {} (prepare not run?): {e}", p.display()))?,
        )?;
        let get = |k: &str| -> Result<PathBuf> {
            Ok(PathBuf::from(
                v.get(k)
                    .and_then(|x| x.as_str())
                    .ok_or(format!("install.json missing {k}"))?,
            ))
        };
        Ok((get("java")?, get("jar")?))
    }

    async fn ensure_java(ctx: &DriverContext<'_>) -> Result<PathBuf> {
        // runtimes cache: <runtimes>/java/temurin-<ver>-<os>-<arch>/
        let id = format!("temurin-{}-{}-{}", JAVA_DOWNLOAD, host_os(), host_arch());
        let cached = ctx
            .runtimes
            .runtime_path("java", &id)
            .and_then(|d| find_java(&d));
        if let Some(j) = system_java(JAVA_MIN, &cached.map(|p| vec![p]).unwrap_or_default()).await {
            return Ok(j);
        }
        // Adoptium assets API gives the download link + sha256 checksum.
        let api = format!(
            "https://api.adoptium.net/v3/assets/latest/{JAVA_DOWNLOAD}/hotspot?architecture={}&image_type=jre&os={}&vendor=eclipse",
            adoptium_arch(),
            adoptium_os()
        );
        let assets = ctx.runtimes.get_json(&api).await?;
        let pkg = assets
            .as_array()
            .and_then(|a| a.first())
            .and_then(|a| a.get("binary"))
            .and_then(|b| b.get("package"))
            .ok_or("adoptium: no package in response")?;
        let url = pkg
            .get("link")
            .and_then(|v| v.as_str())
            .ok_or("adoptium: no link")?
            .to_string();
        let checksum = pkg
            .get("checksum")
            .and_then(|v| v.as_str())
            .ok_or("adoptium: no checksum")?
            .to_string();
        let archive = if url.ends_with(".zip") {
            ArchiveKind::Zip
        } else {
            ArchiveKind::Tgz
        };
        let dir = ctx
            .runtimes
            .fetch(
                "java",
                &id,
                &FetchSpec {
                    url: url.clone(),
                    file_name: url.rsplit('/').next().unwrap_or("jre.bin").into(),
                    sha256: Some(checksum),
                    sha1: None,
                    archive,
                },
            )
            .await?;
        find_java(&dir).ok_or_else(|| format!("no java binary under {}", dir.display()).into())
    }

    /// Pick the version (config `version` or latest.release) and fetch the
    /// server jar into the cache keyed by version+sha1.
    async fn ensure_jar(ctx: &DriverContext<'_>) -> Result<PathBuf> {
        let manifest = ctx.runtimes.get_json(MANIFEST_URL).await?;
        let want = ctx
            .config
            .get("version")
            .and_then(|v| v.as_str())
            .map(str::to_string)
            .or_else(|| {
                manifest
                    .get("latest")
                    .and_then(|l| l.get("release"))
                    .and_then(|v| v.as_str())
                    .map(str::to_string)
            })
            .ok_or("minecraft: cannot determine server version")?;
        let ver_url = manifest
            .get("versions")
            .and_then(|vs| vs.as_array())
            .and_then(|vs| {
                vs.iter()
                    .find(|v| v.get("id").and_then(|i| i.as_str()) == Some(&want))
            })
            .and_then(|v| v.get("url"))
            .and_then(|u| u.as_str())
            .ok_or(format!("minecraft: version {want} not in manifest"))?
            .to_string();
        let vmeta = ctx.runtimes.get_json(&ver_url).await?;
        let srv = vmeta
            .get("downloads")
            .and_then(|d| d.get("server"))
            .ok_or("minecraft: no server download in version metadata")?;
        let url = srv
            .get("url")
            .and_then(|v| v.as_str())
            .ok_or("minecraft: no server url")?
            .to_string();
        let sha1 = srv
            .get("sha1")
            .and_then(|v| v.as_str())
            .ok_or("minecraft: no server sha1")?
            .to_string();
        // install cache keyed by version + sha1
        let id = format!("{want}-{sha1}");
        let dir = ctx
            .runtimes
            .fetch(
                "minecraft",
                &id,
                &FetchSpec {
                    url,
                    file_name: "server.jar".into(),
                    sha256: None,
                    sha1: Some(sha1),
                    archive: ArchiveKind::File,
                },
            )
            .await?;
        Ok(dir.join("server.jar"))
    }
}

#[async_trait]
impl GameDriver for MinecraftDriver {
    fn id(&self) -> &'static str {
        "minecraft"
    }

    fn ports(&self, _config: &serde_json::Value) -> Vec<PortSpec> {
        vec![PortSpec {
            port: PORT,
            protocol: GameProtocol::Tcp,
        }]
    }

    fn persistent_paths(&self, _config: &serde_json::Value) -> Vec<PathPattern> {
        vec![
            PathPattern::new("world*/"),
            PathPattern::new("server.properties"),
            PathPattern::new("ops.json"),
            PathPattern::new("whitelist.json"),
            PathPattern::new("banned-*.json"),
        ]
    }

    fn validate(&self, config: &serde_json::Value) -> Result<()> {
        if config
            .get("eula_accepted")
            .and_then(|v| v.as_bool())
            .unwrap_or(false)
        {
            Ok(())
        } else {
            Err("minecraft: config eula_accepted must be true (you must accept the Minecraft EULA to run a server)".into())
        }
    }

    async fn prepare(&self, ctx: &DriverContext<'_>) -> Result<()> {
        let java = Self::ensure_java(ctx).await?;
        let jar = Self::ensure_jar(ctx).await?;
        std::fs::write(
            ctx.deployment_dir.join("install.json"),
            serde_json::json!({"java": java, "jar": jar}).to_string(),
        )?;
        Ok(())
    }

    async fn configure(&self, ctx: &DriverContext<'_>) -> Result<()> {
        // eula gate
        std::fs::write(ctx.server_dir.join("eula.txt"), "eula=true\n")?;
        // server.properties: merge only the keys we own
        let cfg = ctx.config;
        let s = |k: &str| cfg.get(k).and_then(|v| v.as_str()).map(str::to_string);
        let owned = [
            ("server-port", ctx.local_port(PORT).to_string()),
            // the mesh service listener owns <loopback>:PORT on every node;
            // bind loopback only so a wildcard bind can't collide with it
            // (or expose the offline-mode server on the LAN)
            ("server-ip", "127.0.0.1".into()),
            (
                "online-mode",
                cfg.get("online_mode")
                    .and_then(|v| v.as_bool())
                    .map(|b| b.to_string())
                    .unwrap_or_else(|| "false".into()),
            ),
            ("motd", s("motd").unwrap_or_else(|| "A Varde server".into())),
            (
                "max-players",
                cfg.get("max_players")
                    .and_then(|v| v.as_i64())
                    .map(|n| n.to_string())
                    .unwrap_or_else(|| "20".into()),
            ),
            (
                "difficulty",
                s("difficulty").unwrap_or_else(|| "normal".into()),
            ),
            (
                "gamemode",
                s("gamemode").unwrap_or_else(|| "survival".into()),
            ),
            (
                "view-distance",
                cfg.get("view_distance")
                    .and_then(|v| v.as_i64())
                    .map(|n| n.to_string())
                    .unwrap_or_else(|| "10".into()),
            ),
            ("level-seed", s("seed").unwrap_or_default()),
        ];
        let path = ctx.server_dir.join("server.properties");
        let existing = std::fs::read_to_string(&path).unwrap_or_default();
        std::fs::write(&path, merge_properties(&existing, &owned))?;
        Ok(())
    }

    fn process_spec(&self, ctx: &DriverContext<'_>) -> Result<ProcessSpec> {
        let (java, jar) = Self::install(ctx)?;
        let mb = ctx.memory_mb.max(1024);
        Ok(ProcessSpec {
            program: java,
            args: vec![
                OsString::from(format!("-Xmx{mb}m")),
                OsString::from(format!("-Xms{mb}m")),
                OsString::from("-jar"),
                jar.into_os_string(),
                OsString::from("nogui"),
            ],
            env: vec![],
            cwd: ctx.server_dir.to_path_buf(),
            stdin: true,
        })
    }

    async fn probe(&self, _ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<GameHealth> {
        // `Done (` marks end of server bootstrap; then TCP 25565 accepts.
        let done = p.output_tail(200).iter().any(|l| l.line.contains("Done ("));
        if done && TcpStream::connect(("127.0.0.1", _ctx.local_port(PORT) as u16)).is_ok() {
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
        let mut rx = p.output();
        p.write_stdin("save-off").await?;
        p.write_stdin("save-all flush").await?;
        let deadline = Instant::now() + Duration::from_secs(60);
        loop {
            match tokio::time::timeout(Duration::from_millis(250), rx.recv()).await {
                Ok(Ok(l)) if l.line.contains("Saved the game") => return Ok(SnapshotBarrier::Live),
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
        p: &dyn ProcessHandle,
    ) -> Result<()> {
        p.write_stdin("save-on").await
    }

    async fn graceful_stop(&self, _ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<()> {
        p.write_stdin("stop").await?;
        Ok(())
    }
}

/// True for server log lines marking end of bootstrap: `…Done (3.042s)!…`.
pub fn is_done_line(line: &str) -> bool {
    line.contains("Done (")
}
/// True for `…Saved the game` (save-all / save-all flush output).
pub fn is_saved_line(line: &str) -> bool {
    line.contains("Saved the game")
}

/// Merge `owned` key=value pairs into a server.properties body: owned keys
/// are replaced in place or appended; every other line (comments, unknown
/// keys, ordering) is preserved byte for byte.
pub fn merge_properties(existing: &str, owned: &[(&str, String)]) -> String {
    let mut remaining: Vec<(&str, String)> = owned.to_vec();
    let mut out = String::new();
    for line in existing.lines() {
        let t = line.trim();
        if t.is_empty() || t.starts_with('#') || t.starts_with('!') || !t.contains('=') {
            out.push_str(line);
            out.push('\n');
            continue;
        }
        let key = t.split('=').next().unwrap().trim();
        if let Some(i) = remaining.iter().position(|(k, _)| *k == key) {
            let (_, v) = remaining.remove(i);
            out.push_str(&format!("{key}={v}\n"));
        } else {
            out.push_str(line);
            out.push('\n');
        }
    }
    for (k, v) in remaining {
        out.push_str(&format!("{k}={v}\n"));
    }
    out
}

fn host_os() -> &'static str {
    if cfg!(windows) {
        "windows"
    } else if cfg!(target_os = "macos") {
        "mac"
    } else {
        "linux"
    }
}
fn host_arch() -> &'static str {
    if cfg!(target_arch = "aarch64") {
        "aarch64"
    } else {
        "x86_64"
    }
}
fn adoptium_os() -> &'static str {
    host_os()
}
fn adoptium_arch() -> &'static str {
    if cfg!(target_arch = "aarch64") {
        "aarch64"
    } else {
        "x64"
    }
}

// java resolution helpers live in the runtimes crate (shared with agent)
use runtimes::{find_java, system_java};

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn properties_merge_replaces_owned_only() {
        let existing = "#comment\nserver-port=25565\ncustom-key=keep\nmotd=old\n";
        let owned = [
            ("server-port", "25565".to_string()),
            ("motd", "hi".to_string()),
            ("new-key", "added".to_string()),
        ];
        let merged = merge_properties(existing, &owned);
        assert!(merged.contains("#comment"));
        assert!(merged.contains("custom-key=keep"));
        assert!(merged.contains("server-port=25565"));
        assert!(merged.contains("motd=hi"));
        assert!(merged.contains("new-key=added"));
        assert!(!merged.contains("motd=old"));
    }

    struct NullRuntimes;
    #[async_trait]
    impl RuntimeProvider for NullRuntimes {
        fn runtime_path(&self, _k: &str, _id: &str) -> Option<PathBuf> {
            None
        }
    }

    #[tokio::test]
    async fn local_port_and_ip_in_properties() {
        let tmp = tempfile::tempdir().unwrap();
        let server_dir = tmp.path().join("srv");
        let dep_dir = tmp.path().join("dep");
        std::fs::create_dir_all(&server_dir).unwrap();
        std::fs::create_dir_all(&dep_dir).unwrap();
        let rt = NullRuntimes;
        let cfg = serde_json::json!({"eula_accepted": true});
        let bindings = [PortBinding {
            service_port: PORT,
            local_port: 31234,
            protocol: GameProtocol::Tcp,
        }];
        let dep = DeploymentSpec::parse(&serde_json::json!({}));
        let ctx = DriverContext {
            server_dir: &server_dir,
            deployment_dir: &dep_dir,
            runtimes: &rt,
            deployment: &dep,
            config: &cfg,
            ports: &bindings,
            memory_mb: 2048,
        };
        MinecraftDriver::new().configure(&ctx).await.unwrap();
        let props = std::fs::read_to_string(server_dir.join("server.properties")).unwrap();
        assert!(props.contains("server-port=31234"), "{props}");
        assert!(props.contains("server-ip=127.0.0.1"), "{props}");
        assert_eq!(ctx.local_port(PORT), 31234);
        assert_eq!(ctx.local_port(12345), 12345, "unmapped port unchanged");
    }

    #[test]
    fn eula_gate() {
        let d = MinecraftDriver::new();
        assert!(d.validate(&serde_json::json!({})).is_err());
        assert!(d
            .validate(&serde_json::json!({"eula_accepted": false}))
            .is_err());
        assert!(d
            .validate(&serde_json::json!({"eula_accepted": true}))
            .is_ok());
    }

    #[test]
    fn log_line_parsing() {
        assert!(is_done_line(
            "[12:00:00] [Server thread/INFO]: Done (2.531s)! For help, type \"help\""
        ));
        assert!(!is_done_line("[12:00:00] Done starting"));
        assert!(is_saved_line("[Server thread/INFO]: Saved the game"));
        assert!(!is_saved_line("[Server thread/INFO]: Saving the game"));
    }

    #[test]
    fn manifest_parsing_fixture() {
        let m: serde_json::Value =
            serde_json::from_str(include_str!("../testdata/version_manifest.json")).unwrap();
        let latest = m["latest"]["release"].as_str().unwrap();
        assert_eq!(latest, "9.9.9");
        let v = m["versions"]
            .as_array()
            .unwrap()
            .iter()
            .find(|v| v["id"] == "9.9.9")
            .unwrap();
        assert!(v["url"].as_str().unwrap().starts_with("https://"));
    }
}
