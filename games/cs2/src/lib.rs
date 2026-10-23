use std::ffi::OsString;
use std::path::{Path, PathBuf};
use std::time::Duration;

use async_trait::async_trait;
use executor_api::{ProcessHandle, ProcessSpec};
use game_driver_api::*;
use serde_json::Value;

pub mod steam;
mod vdf;

pub const APP_ID: u32 = 730;
pub const PORT: u32 = 27015;
const MIN_DISK_BYTES: u64 = 75_000_000_000;
const BANS: [&str; 2] = ["banned_user.cfg", "banned_ip.cfg"];
const INFO_REQUEST: &[u8] = b"\xff\xff\xff\xffTSource Engine Query\0";

fn check_install_space(dir: &Path, available: u64) -> Result<()> {
    if steam_app_installed(APP_ID, dir) {
        return Ok(());
    }
    let installed = allocated_install_bytes(dir)?;
    if available.saturating_add(installed) < MIN_DISK_BYTES {
        return Err(format!(
            "cs2: incomplete install requires a 75 GB disk budget ({} bytes free, {} bytes already installed)",
            available, installed
        )
        .into());
    }
    Ok(())
}

fn allocated_install_bytes(dir: &Path) -> Result<u64> {
    let mut dirs = vec![dir.to_path_buf()];
    let mut total = 0u64;
    while let Some(dir) = dirs.pop() {
        for entry in std::fs::read_dir(dir)? {
            let entry = entry?;
            let kind = entry.file_type()?;
            if kind.is_dir() {
                dirs.push(entry.path());
            } else if kind.is_file() {
                let file = std::fs::File::open(entry.path())?;
                total = total.saturating_add(fs2::FileExt::allocated_size(&file)?);
                if total >= MIN_DISK_BYTES {
                    return Ok(total);
                }
            }
        }
    }
    Ok(total)
}

#[derive(Default)]
pub struct Cs2Driver;

impl Cs2Driver {
    pub fn new() -> Self {
        Self
    }

    fn install_dir(ctx: &DriverContext<'_>) -> PathBuf {
        ctx.deployment_dir.join("server")
    }

    async fn prepare_install(
        ctx: &DriverContext<'_>,
        client: Option<steam::Install>,
    ) -> Result<()> {
        if let Some(install) = client {
            return Self::prepare_shared(ctx, &install);
        }
        if let Some(install) = ctx.deployment_dir.parent().and_then(steam::in_deployments) {
            Self::update_install(ctx, &install.dir).await?;
            return Self::prepare_shared(ctx, &install);
        }
        match tokio::fs::remove_file(ctx.deployment_dir.join("cs2-install.json")).await {
            Ok(()) => (),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => (),
            Err(e) => return Err(e.into()),
        }
        Self::update_install(ctx, &Self::install_dir(ctx)).await
    }

    async fn update_install(ctx: &DriverContext<'_>, dir: &Path) -> Result<()> {
        tokio::fs::create_dir_all(dir).await?;
        check_install_space(dir, fs2::available_space(dir)?)
            .map_err(|e| format!("{e} at {}", dir.display()))?;
        ctx.runtimes.steam_app_install(APP_ID, dir).await?;
        if !steam::binary(dir).is_file() {
            return Err("cs2: installed server binary is missing".into());
        }
        #[cfg(target_os = "linux")]
        {
            let steamcmd = ctx.runtimes.steamcmd_path().await?;
            let library = steamcmd
                .parent()
                .ok_or("cs2: steamcmd path has no parent")?
                .join("linux64/steamclient.so");
            tokio::fs::copy(library, steam::binary(dir).with_file_name("steamclient.so")).await?;
        }
        Ok(())
    }

    fn selected(ctx: &DriverContext<'_>) -> Result<Option<steam::Install>> {
        match std::fs::read(ctx.deployment_dir.join("cs2-install.json")) {
            Ok(contents) => Ok(Some(serde_json::from_slice(&contents)?)),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    fn binary(ctx: &DriverContext<'_>) -> Result<PathBuf> {
        Ok(steam::binary(
            &Self::selected(ctx)?.map_or_else(|| Self::install_dir(ctx), |s| s.dir),
        ))
    }

    fn game_dir(ctx: &DriverContext<'_>) -> Result<PathBuf> {
        Ok(if Self::selected(ctx)?.is_some() {
            ctx.deployment_dir.join("cs2-runtime/csgo")
        } else {
            Self::install_dir(ctx).join("game/csgo")
        })
    }

    fn prepare_shared(ctx: &DriverContext<'_>, install: &steam::Install) -> Result<()> {
        let game = ctx.deployment_dir.join("cs2-runtime/csgo");
        std::fs::create_dir_all(game.join("cfg"))?;
        let mut info = vdf::parse(&std::fs::read_to_string(
            install.dir.join("game/csgo/gameinfo.gi"),
        )?)?;
        let paths = info
            .get_mut("GameInfo")
            .and_then(|v| v.get_mut("FileSystem"))
            .and_then(|v| v.get_mut("SearchPaths"))
            .ok_or("cs2: gameinfo has no SearchPaths")?;
        let Some(entries) = paths.entries() else {
            return Err("cs2: invalid SearchPaths".into());
        };
        fn engine_path(path: &Path) -> String {
            path.to_string_lossy()
                .strip_prefix(r"\\?\")
                .unwrap_or(&path.to_string_lossy())
                .replace('\\', "/")
        }
        let mut isolated = vec![("Game".to_owned(), vdf::Value::Text(engine_path(&game)))];
        for (key, value) in entries {
            let path = value.text().ok_or("cs2: invalid game search path")?;
            let path = if key.eq_ignore_ascii_case("Mod") {
                game.clone()
            } else {
                install.dir.join("game").join(path)
            };
            isolated.push((key.clone(), vdf::Value::Text(engine_path(&path))));
        }
        *paths = vdf::Value::Object(isolated);
        std::fs::write(game.join("gameinfo.gi"), info.render())?;
        for name in ["autoexec.cfg", "server.cfg"] {
            std::fs::write(game.join("cfg").join(name), "")?;
        }
        std::fs::write(
            ctx.deployment_dir.join("cs2-install.json"),
            serde_json::to_vec(install)?,
        )?;
        Ok(())
    }

    fn string<'a>(cfg: &'a Value, key: &str, default: &'a str) -> Result<&'a str> {
        let value = match cfg.get(key) {
            Some(v) => v
                .as_str()
                .ok_or_else(|| format!("cs2: {key} must be a string"))?,
            None => default,
        };
        if value
            .chars()
            .any(|c| c.is_control() || matches!(c, '"' | '\\' | ';'))
        {
            return Err(format!(
                "cs2: {key} must not contain controls, quotes, backslashes or semicolons"
            )
            .into());
        }
        Ok(value)
    }

    fn number(cfg: &Value, key: &str, default: u64, min: u64, max: u64) -> Result<u64> {
        let n = match cfg.get(key) {
            Some(v) => v
                .as_u64()
                .ok_or_else(|| format!("cs2: {key} must be an integer"))?,
            None => default,
        };
        if !(min..=max).contains(&n) {
            return Err(format!("cs2: {key} must be {min}–{max}").into());
        }
        Ok(n)
    }

    fn mode(cfg: &Value) -> Result<(u8, u8)> {
        match Self::string(cfg, "game_mode", "competitive")? {
            "casual" => Ok((0, 0)),
            "competitive" => Ok((0, 1)),
            "wingman" => Ok((0, 2)),
            "arms_race" => Ok((1, 0)),
            "deathmatch" => Ok((1, 2)),
            _ => Err("cs2: unsupported game_mode".into()),
        }
    }

    fn map(cfg: &Value) -> Result<&str> {
        let map = Self::string(cfg, "map", "de_dust2")?;
        if map.is_empty() || !map.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_') {
            return Err("cs2: map must contain only letters, digits and underscores".into());
        }
        Ok(map)
    }

    fn cfg(cfg: &Value) -> Result<String> {
        Ok(format!(
            "hostname \"{}\"\nsv_password \"{}\"\nbot_quota_mode normal\nbot_quota {}\n",
            Self::string(cfg, "server_name", "")?,
            Self::string(cfg, "password", "")?,
            Self::number(cfg, "bots", 0, 0, 64)?
        ))
    }

    fn args(cfg: &Value, port: u32) -> Result<Vec<OsString>> {
        let (kind, mode) = Self::mode(cfg)?;
        let mut args: Vec<OsString> = vec![
            "-dedicated".into(),
            "-port".into(),
            port.to_string().into(),
            "-maxplayers".into(),
            Self::number(cfg, "max_players", 10, 2, 64)?
                .to_string()
                .into(),
            "+game_type".into(),
            kind.to_string().into(),
            "+game_mode".into(),
            mode.to_string().into(),
            "+exec".into(),
            "varde.cfg".into(),
            "+map".into(),
            Self::map(cfg)?.into(),
        ];
        let token = Self::string(cfg, "gslt", "")?;
        if !token.is_empty() {
            args.extend(["+sv_setsteamaccount".into(), token.into()]);
        }
        Ok(args)
    }

    async fn sync_bans(ctx: &DriverContext<'_>, restoring: bool) -> Result<()> {
        let state = ctx.server_dir.join("game/csgo/cfg");
        let installed = Self::game_dir(ctx)?.join("cfg");
        let (from, to) = if restoring {
            (state, installed)
        } else {
            (installed, state)
        };
        tokio::fs::create_dir_all(&to).await?;
        for name in BANS {
            match tokio::fs::copy(from.join(name), to.join(name)).await {
                Ok(_) => (),
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                    match tokio::fs::remove_file(to.join(name)).await {
                        Ok(()) => (),
                        Err(e) if e.kind() == std::io::ErrorKind::NotFound => (),
                        Err(e) => return Err(e.into()),
                    }
                }
                Err(e) => return Err(e.into()),
            }
        }
        Ok(())
    }
}

#[derive(Debug, PartialEq, Eq)]
enum QueryReply<'a> {
    Challenge([u8; 4]),
    Info { map: &'a str, max_players: u8 },
}

fn cstring<'a>(data: &mut &'a [u8]) -> Option<&'a str> {
    let end = data.iter().position(|b| *b == 0)?;
    let s = std::str::from_utf8(&data[..end]).ok()?;
    *data = &data[end + 1..];
    Some(s)
}

fn parse_reply(data: &[u8]) -> Option<QueryReply<'_>> {
    if data.get(..4)? != [255; 4] {
        return None;
    }
    match *data.get(4)? {
        0x41 if data.len() == 9 => Some(QueryReply::Challenge(data[5..9].try_into().ok()?)),
        0x49 => {
            let mut rest = data.get(6..)?;
            cstring(&mut rest)?;
            let map = cstring(&mut rest)?;
            cstring(&mut rest)?;
            cstring(&mut rest)?;
            let max_players = *rest.get(3)?;
            // Fixed INFO fields through visibility/VAC, then version.
            let mut version = rest.get(9..)?;
            cstring(&mut version)?;
            Some(QueryReply::Info { map, max_players })
        }
        _ => None,
    }
}

async fn a2s_ready(port: u16, map: &str) -> Result<bool> {
    let socket = tokio::net::UdpSocket::bind("127.0.0.1:0").await?;
    socket
        .connect((std::net::Ipv4Addr::LOCALHOST, port))
        .await?;
    socket.send(INFO_REQUEST).await?;
    let mut buf = [0; 2048];
    for _ in 0..2 {
        let n = socket.recv(&mut buf).await?;
        match parse_reply(&buf[..n]) {
            Some(QueryReply::Challenge(challenge)) => {
                let mut request = INFO_REQUEST.to_vec();
                request.extend(challenge);
                socket.send(&request).await?;
            }
            Some(QueryReply::Info {
                map: actual,
                max_players,
            }) => return Ok(actual == map && max_players > 0),
            None => return Ok(false),
        }
    }
    Ok(false)
}

#[async_trait]
impl GameDriver for Cs2Driver {
    fn id(&self) -> &'static str {
        "cs2"
    }
    fn ports(&self, _: &Value) -> Vec<PortSpec> {
        vec![PortSpec {
            port: PORT,
            protocol: GameProtocol::Udp,
        }]
    }
    fn persistent_paths(&self, _: &Value) -> Vec<PathPattern> {
        BANS.iter()
            .map(|name| PathPattern::new(format!("game/csgo/cfg/{name}")))
            .collect()
    }
    fn validate(&self, cfg: &Value) -> Result<()> {
        if Self::string(cfg, "server_name", "")?.trim().is_empty() {
            return Err("cs2: server_name is required".into());
        }
        Self::cfg(cfg)?;
        Self::args(cfg, PORT)?;
        let token = Self::string(cfg, "gslt", "")?;
        if !token.is_empty() && (token.len() != 32 || !token.bytes().all(|b| b.is_ascii_hexdigit()))
        {
            return Err("cs2: gslt must be a 32-character hexadecimal token".into());
        }
        Ok(())
    }
    async fn prepare(&self, ctx: &DriverContext<'_>) -> Result<()> {
        Self::prepare_install(ctx, steam::detect_client()).await
    }
    async fn configure(&self, ctx: &DriverContext<'_>) -> Result<()> {
        self.validate(ctx.config)?;
        Self::sync_bans(ctx, true).await?;
        let path = Self::game_dir(ctx)?.join("cfg/varde.cfg");
        let mut cfg = Self::cfg(ctx.config)?;
        for name in BANS {
            if path.with_file_name(name).is_file() {
                cfg.push_str(&format!("exec {name}\n"));
            }
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
            let mut file = std::fs::OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o600)
                .open(&path)?;
            file.set_permissions(std::fs::Permissions::from_mode(0o600))?;
            use std::io::Write;
            file.write_all(cfg.as_bytes())?;
        }
        #[cfg(not(unix))]
        tokio::fs::write(path, cfg).await?;
        Ok(())
    }
    fn process_spec(&self, ctx: &DriverContext<'_>) -> Result<ProcessSpec> {
        let mut env = Vec::new();
        let selected = Self::selected(ctx)?;
        let program = Self::binary(ctx)?;
        let mut args = Self::args(ctx.config, ctx.local_port(PORT))?;
        let cwd = if selected.is_some() {
            let game = Self::game_dir(ctx)?;
            args.splice(1..1, [OsString::from("-game"), game.into_os_string()]);
            ctx.deployment_dir.join("cs2-runtime")
        } else {
            Self::install_dir(ctx)
        };
        if cfg!(target_os = "linux") {
            let dir = program.parent().unwrap().to_path_buf();
            let mut paths = vec![dir];
            if let Some(library) = selected
                .as_ref()
                .and_then(|s| s.steamclient.as_ref())
                .and_then(|p| p.parent())
            {
                paths.push(library.to_path_buf());
            }
            if let Some(old) = std::env::var_os("LD_LIBRARY_PATH") {
                paths.extend(std::env::split_paths(&old));
            }
            env.push(("LD_LIBRARY_PATH".into(), std::env::join_paths(paths)?));
        }
        Ok(ProcessSpec {
            program,
            args,
            env,
            cwd,
            stdin: true,
        })
    }
    async fn probe(&self, ctx: &DriverContext<'_>, _: &dyn ProcessHandle) -> Result<GameHealth> {
        let map = Self::map(ctx.config)?;
        let port = u16::try_from(ctx.local_port(PORT))?;
        let ready = matches!(
            tokio::time::timeout(Duration::from_millis(500), a2s_ready(port, map)).await,
            Ok(Ok(true))
        );
        Ok(if ready {
            GameHealth::Healthy
        } else {
            GameHealth::Starting
        })
    }
    async fn prepare_snapshot(
        &self,
        ctx: &DriverContext<'_>,
        _: &dyn ProcessHandle,
    ) -> Result<SnapshotBarrier> {
        Self::sync_bans(ctx, false).await?;
        Ok(SnapshotBarrier::Live)
    }
    async fn resume_after_snapshot(
        &self,
        _: &DriverContext<'_>,
        _: &dyn ProcessHandle,
    ) -> Result<()> {
        Ok(())
    }
    async fn graceful_stop(&self, _: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<()> {
        if p.write_stdin("quit\n").await.is_err() {
            return p.interrupt().await;
        }
        match tokio::time::timeout(Duration::from_secs(5), p.wait()).await {
            Ok(result) => {
                result?;
            }
            Err(_) => p.interrupt().await?,
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests;
