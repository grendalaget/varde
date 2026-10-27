use std::ffi::OsString;
use std::path::{Path, PathBuf};
use std::time::Duration;

use async_trait::async_trait;
use executor_api::{Executor, ProcessHandle, ProcessSpec};
use game_driver_api::*;
use serde_json::Value;

#[cfg(target_os = "linux")]
mod confinement;
mod install_lock;
mod rcon;
mod shared_config;
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

    fn addon_mode(ctx: &DriverContext<'_>) -> bool {
        ctx.addons.iter().any(|addon| addon.target == "game")
    }

    fn valid_server_id(server_id: &str) -> bool {
        !server_id.is_empty()
            && server_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
    }

    fn addon_root(ctx: &DriverContext<'_>) -> Result<PathBuf> {
        if !Self::valid_server_id(ctx.server_id) {
            return Err(
                "cs2: server id must contain only letters, digits, underscores and hyphens".into(),
            );
        }
        Ok(ctx.deployment_dir.join("cs2-servers").join(ctx.server_id))
    }

    #[cfg(target_os = "linux")]
    fn gameinfo_search_paths(ctx: &DriverContext<'_>) -> Result<Vec<String>> {
        let mut paths = Vec::new();
        for addon in ctx.addons.iter().filter(|addon| addon.target == "game") {
            let label = format!("{}/{}", addon.plugin_id, addon.addon_id);
            let invalid =
                |message: &str| format!("cs2: invalid driver_options for addon {label}: {message}");
            let options = addon
                .driver_options
                .as_object()
                .ok_or_else(|| invalid("must be an object"))?;
            if options.keys().any(|key| key != "gameinfo_search_paths") {
                return Err(invalid("contains an unsupported option").into());
            }
            let Some(value) = options.get("gameinfo_search_paths") else {
                continue;
            };
            let entries = value
                .as_array()
                .ok_or_else(|| invalid("gameinfo_search_paths must be an array"))?;
            if entries.len() > 8 {
                return Err(invalid("gameinfo_search_paths accepts at most eight entries").into());
            }
            for entry in entries {
                let path = entry
                    .as_str()
                    .ok_or_else(|| invalid("gameinfo_search_paths entries must be strings"))?;
                let Some(relative) = path.strip_prefix("csgo/") else {
                    return Err(invalid("search paths must start with csgo/").into());
                };
                if relative.is_empty()
                    || relative.split('/').any(|component| {
                        component.is_empty()
                            || matches!(component, "." | "..")
                            || !component.bytes().all(|byte| {
                                byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-')
                            })
                    })
                {
                    return Err(invalid("search paths contain an invalid path component").into());
                }
                paths.push(path.to_owned());
            }
        }
        Ok(paths)
    }

    #[cfg(target_os = "linux")]
    fn insert_gameinfo_search_paths(path: &Path, search_paths: &[String]) -> Result<()> {
        if search_paths.is_empty() {
            return Ok(());
        }
        let contents = std::fs::read_to_string(path)?;
        let mut info = vdf::parse(&contents)?;
        let Some(filesystem) = info
            .get_mut("GameInfo")
            .and_then(|value| value.get_mut("FileSystem"))
        else {
            return Ok(());
        };
        let Some(vdf::Value::Object(entries)) = filesystem.get_mut("SearchPaths") else {
            return Ok(());
        };
        let Some(mut insert_at) = entries.iter().position(|(key, value)| {
            key.eq_ignore_ascii_case("Game") && value.text() == Some("csgo")
        }) else {
            return Ok(());
        };
        let mut present: Vec<String> = entries
            .iter()
            .filter(|(key, _)| key.eq_ignore_ascii_case("Game"))
            .filter_map(|(_, value)| value.text().map(str::to_owned))
            .collect();
        let first_game_csgo = insert_at;
        for search_path in search_paths {
            if present.iter().any(|existing| existing == search_path) {
                continue;
            }
            entries.insert(
                insert_at,
                ("Game".to_owned(), vdf::Value::Text(search_path.clone())),
            );
            insert_at += 1;
            present.push(search_path.clone());
        }
        if insert_at != first_game_csgo {
            std::fs::write(path, info.render())?;
        }
        Ok(())
    }

    #[cfg(target_os = "linux")]
    fn copy_file(source: &Path, destination: &Path) -> Result<()> {
        std::fs::copy(source, destination)?;
        std::fs::set_permissions(destination, std::fs::metadata(source)?.permissions())?;
        Ok(())
    }

    #[cfg(target_os = "linux")]
    fn symlink(source: &Path, destination: &Path) -> Result<()> {
        std::os::unix::fs::symlink(source, destination)?;
        Ok(())
    }

    #[cfg(target_os = "linux")]
    fn rebuild_addon_root(ctx: &DriverContext<'_>, search_paths: &[String]) -> Result<()> {
        let root = Self::addon_root(ctx)?;
        let source =
            Self::selected(ctx)?.map_or_else(|| Self::install_dir(ctx), |install| install.dir);
        let source_game = source.join("game");
        let root_game = root.join("game");

        match std::fs::remove_dir_all(&root) {
            Ok(()) => (),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => (),
            Err(error) => return Err(error.into()),
        }
        std::fs::create_dir_all(&root_game)?;

        for entry in std::fs::read_dir(&source_game)? {
            let entry = entry?;
            let name = entry.file_name();
            if name == "bin" || name == "csgo" {
                continue;
            }
            Self::symlink(&entry.path(), &root_game.join(name))?;
        }

        let source_bin = source_game.join("bin");
        let root_bin = root_game.join("bin");
        std::fs::create_dir_all(&root_bin)?;
        for entry in std::fs::read_dir(&source_bin)? {
            let entry = entry?;
            let name = entry.file_name();
            if name == "linuxsteamrt64" {
                continue;
            }
            Self::symlink(&entry.path(), &root_bin.join(name))?;
        }

        let source_runtime = source_bin.join("linuxsteamrt64");
        let root_runtime = root_bin.join("linuxsteamrt64");
        std::fs::create_dir_all(&root_runtime)?;
        for entry in std::fs::read_dir(&source_runtime)? {
            let entry = entry?;
            let name = entry.file_name();
            let destination = root_runtime.join(&name);
            if name == "cs2" {
                Self::copy_file(&entry.path(), &destination)?;
            } else {
                Self::symlink(&entry.path(), &destination)?;
            }
        }

        let source_csgo = source_game.join("csgo");
        let root_csgo = root_game.join("csgo");
        std::fs::create_dir_all(&root_csgo)?;
        for entry in std::fs::read_dir(&source_csgo)? {
            let entry = entry?;
            let name = entry.file_name();
            let destination = root_csgo.join(&name);
            if name == "cfg" {
                continue;
            }
            if name == "gameinfo.gi" || name == "gameinfo_branchspecific.gi" {
                Self::copy_file(&entry.path(), &destination)?;
                Self::insert_gameinfo_search_paths(&destination, search_paths)?;
            } else {
                Self::symlink(&entry.path(), &destination)?;
            }
        }

        let source_cfg = source_csgo.join("cfg");
        if !std::fs::symlink_metadata(&source_cfg)?.is_dir() {
            return Err("cs2: install cfg path is not a directory".into());
        }
        let root_cfg = root_csgo.join("cfg");
        std::fs::create_dir_all(&root_cfg)?;
        for entry in std::fs::read_dir(&source_cfg)? {
            let entry = entry?;
            let destination = root_cfg.join(entry.file_name());
            let kind = entry.file_type()?;
            if kind.is_file() {
                Self::copy_file(&entry.path(), &destination)?;
            } else if kind.is_dir() || kind.is_symlink() {
                Self::symlink(&entry.path(), &destination)?;
            } else {
                return Err(
                    format!("cs2: unsupported cfg entry {}", entry.path().display()).into(),
                );
            }
        }

        for addon in ctx.addons.iter().filter(|addon| addon.target == "game") {
            addon.install_into(&root_csgo)?;
        }
        Ok(())
    }

    fn remove_addon_root(ctx: &DriverContext<'_>) -> Result<()> {
        let root = Self::addon_root(ctx)?;
        match std::fs::remove_dir_all(root) {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(error.into()),
        }
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
        let lock = install_lock::open(dir)?;
        match fs2::FileExt::try_lock_exclusive(&lock) {
            Ok(()) => (),
            Err(e) if e.raw_os_error() == fs2::lock_contended_error().raw_os_error() => {
                if steam_app_installed(APP_ID, dir) && steam::binary(dir).is_file() {
                    #[cfg(target_os = "linux")]
                    if !Self::steamclient_ready(dir) {
                        return Err("cs2: in-use install is missing steamclient.so".into());
                    }
                    tracing::info!(install = %dir.display(), "CS2 install in use; skipping update");
                    return Ok(());
                }
                return Err("cs2: incomplete install is locked by another process".into());
            }
            Err(e) => return Err(e.into()),
        }
        check_install_space(dir, fs2::available_space(dir)?)
            .map_err(|e| format!("{e} at {}", dir.display()))?;
        let _updated = match ctx.runtimes.steam_app_install(APP_ID, dir).await {
            Ok(()) => true,
            Err(error) => {
                if !steam_app_installed(APP_ID, dir) || !steam::binary(dir).is_file() {
                    return Err(error);
                }
                tracing::warn!(install = %dir.display(), %error, "CS2 update failed; using complete install");
                false
            }
        };
        if !steam::binary(dir).is_file() {
            return Err("cs2: installed server binary is missing".into());
        }
        #[cfg(target_os = "linux")]
        if _updated || !Self::steamclient_ready(dir) {
            let steamcmd = ctx.runtimes.steamcmd_path().await?;
            let library = steamcmd
                .parent()
                .ok_or("cs2: steamcmd path has no parent")?
                .join("linux64/steamclient.so");
            if tokio::fs::metadata(&library).await?.len() == 0 {
                return Err("cs2: SteamCMD steamclient.so is empty".into());
            }
            tokio::fs::copy(library, steam::binary(dir).with_file_name("steamclient.so")).await?;
        }
        Ok(())
    }

    #[cfg(target_os = "linux")]
    fn steamclient_ready(dir: &Path) -> bool {
        std::fs::metadata(steam::binary(dir).with_file_name("steamclient.so"))
            .is_ok_and(|metadata| metadata.is_file() && metadata.len() > 0)
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
        Ok(if Self::addon_mode(ctx) {
            Self::addon_root(ctx)?.join("game/csgo")
        } else if Self::selected(ctx)?.is_some() {
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
        let value = match cfg.get(key).filter(|v| !v.is_null()) {
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
        let n = match cfg.get(key).filter(|v| !v.is_null()) {
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
            "-nominidumps".into(),
            "-usercon".into(),
            "-ip".into(),
            "127.0.0.1".into(),
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

    fn shared_config(ctx: &DriverContext<'_>) -> Result<Option<(PathBuf, String)>> {
        let Some(install) = Self::selected(ctx)? else {
            return Ok(None);
        };
        if ctx.server_id.is_empty()
            || !ctx
                .server_id
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'-'))
        {
            return Err(
                "cs2: server id must contain only letters, digits, underscores and hyphens".into(),
            );
        }
        let name = format!("varde_{}", ctx.server_id);
        let path = install
            .dir
            .join("game/csgo/cfg")
            .join(format!("{name}.cfg"));
        Ok(Some((path, name)))
    }

    fn startup_command(ctx: &DriverContext<'_>) -> Result<String> {
        let name = if Self::addon_mode(ctx) {
            "varde.cfg".to_string()
        } else {
            Self::shared_config(ctx)?.map_or_else(
                || "varde.cfg".to_string(),
                |(_, name)| format!("{name}.cfg"),
            )
        };
        let bots = Self::number(ctx.config, "bots", 0, 0, 64)?;
        let kick = if bots == 0 { "bot_kick; " } else { "" };
        Ok(format!("exec {name}; {kick}bot_quota {bots}"))
    }

    fn create_shared_config(
        ctx: &DriverContext<'_>,
    ) -> Result<Option<shared_config::SharedConfig>> {
        let Some((path, _)) = Self::shared_config(ctx)? else {
            return Ok(None);
        };
        let dir = Self::game_dir(ctx)?.join("cfg");
        if !dir.join("varde.cfg").is_file() {
            return Ok(None);
        }
        let mut cfg = std::fs::read_to_string(dir.join("varde.cfg"))?;
        for name in BANS {
            let contents = match std::fs::read_to_string(dir.join(name)) {
                Ok(contents) => contents,
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => String::new(),
                Err(e) => return Err(e.into()),
            };
            cfg = cfg.replace(&format!("exec {name}\n"), &format!("{contents}\n"));
        }
        std::fs::create_dir_all(path.parent().ok_or("cs2: config path has no parent")?)?;
        Ok(Some(shared_config::SharedConfig::write(path, &cfg)?))
    }

    async fn remove_shared_config(ctx: &DriverContext<'_>) -> Result<()> {
        let Some((path, _)) = Self::shared_config(ctx)? else {
            return Ok(());
        };
        match tokio::fs::remove_file(path).await {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(e.into()),
        }
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
    fn addon_targets(&self) -> &'static [&'static str] {
        &["game"]
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
        let addon_mode = Self::addon_mode(ctx);
        #[cfg(not(target_os = "linux"))]
        if addon_mode {
            return Err("cs2: server addons are only supported on Linux hosts".into());
        }
        self.validate(ctx.config)?;
        if addon_mode {
            #[cfg(target_os = "linux")]
            Self::rebuild_addon_root(ctx, &Self::gameinfo_search_paths(ctx)?)?;
        } else {
            if Self::valid_server_id(ctx.server_id) {
                Self::remove_addon_root(ctx)?;
            }
            Self::shared_config(ctx)?;
        }
        match tokio::fs::remove_file(ctx.deployment_dir.join("cs2-configured")).await {
            Ok(()) => (),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => (),
            Err(e) => return Err(e.into()),
        }
        Self::sync_bans(ctx, true).await?;
        let cfg_dir = Self::game_dir(ctx)?.join("cfg");
        let path = cfg_dir.join("varde.cfg");
        let mut cfg = Self::cfg(ctx.config)?;
        let password: String = rand::random::<[u8; 32]>()
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect();
        cfg.push_str(&format!("rcon_password \"{password}\"\n"));
        for name in BANS {
            if cfg_dir.join(name).is_file() {
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
        tokio::fs::write(&path, cfg).await?;
        Ok(())
    }
    fn process_spec(&self, ctx: &DriverContext<'_>) -> Result<ProcessSpec> {
        let mut env = Vec::new();
        let addon_mode = Self::addon_mode(ctx);
        let selected = Self::selected(ctx)?;
        let program = if addon_mode {
            #[cfg(target_os = "linux")]
            {
                Self::addon_root(ctx)?.join("game/bin/linuxsteamrt64/cs2")
            }
            #[cfg(not(target_os = "linux"))]
            {
                return Err("cs2: server addons are only supported on Linux hosts".into());
            }
        } else {
            Self::binary(ctx)?
        };
        let mut args = Self::args(ctx.config, ctx.local_port(PORT))?;
        match std::fs::remove_file(ctx.deployment_dir.join("cs2-configured")) {
            Ok(()) => (),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => (),
            Err(e) => return Err(e.into()),
        }
        if !addon_mode {
            if let Some((_, name)) = Self::shared_config(ctx)? {
                let index = args
                    .iter()
                    .position(|arg| arg == "+exec")
                    .ok_or("cs2: missing +exec argument")?;
                args[index + 1] = name.into();
            }
        }
        let cwd = if addon_mode {
            Self::addon_root(ctx)?
        } else if selected.is_some() {
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
        #[cfg(target_os = "linux")]
        env.extend(confinement::environment(u16::try_from(
            ctx.local_port(PORT),
        )?)?);
        if cfg!(windows) {
            if let Some(library) = selected
                .as_ref()
                .and_then(|s| s.steamclient.as_ref())
                .and_then(|p| p.parent())
            {
                let mut paths = vec![library.to_path_buf()];
                if let Some(old) = std::env::var_os("PATH") {
                    paths.extend(std::env::split_paths(&old));
                }
                env.push(("PATH".into(), std::env::join_paths(paths)?));
            }
        }
        Ok(ProcessSpec {
            program,
            args,
            env,
            cwd,
            stdin: !cfg!(windows),
        })
    }
    async fn spawn(
        &self,
        ctx: &DriverContext<'_>,
        executor: &dyn Executor,
    ) -> Result<Box<dyn ProcessHandle>> {
        let selected = Self::selected(ctx)?;
        let managed = !selected.as_ref().is_some_and(|install| !install.managed);
        let dir = selected.map_or_else(|| Self::install_dir(ctx), |install| install.dir);
        let lock = if managed {
            Some(install_lock::shared(dir.clone()).await?)
        } else {
            None
        };
        if managed && (!steam_app_installed(APP_ID, &dir) || !steam::binary(&dir).is_file()) {
            return Err("cs2: install became incomplete before launch".into());
        }
        let spec = self.process_spec(ctx)?;
        let config = if Self::addon_mode(ctx) {
            None
        } else {
            shared_config::SharedConfig::sweep(&dir.join("game/csgo/cfg"))?;
            Self::create_shared_config(ctx)?
        };
        install_lock::spawn(executor, &spec, lock, config).await
    }
    async fn probe(&self, ctx: &DriverContext<'_>, _: &dyn ProcessHandle) -> Result<GameHealth> {
        let map = Self::map(ctx.config)?;
        let port = u16::try_from(ctx.local_port(PORT))?;
        let ready = matches!(
            tokio::time::timeout(Duration::from_millis(500), a2s_ready(port, map)).await,
            Ok(Ok(true))
        );
        if ready {
            let marker = ctx.deployment_dir.join("cs2-configured");
            if !marker.is_file() {
                let cfg =
                    tokio::fs::read_to_string(Self::game_dir(ctx)?.join("cfg/varde.cfg")).await?;
                let password = cfg
                    .lines()
                    .find_map(|line| {
                        line.strip_prefix("rcon_password \"")
                            .and_then(|s| s.strip_suffix('"'))
                    })
                    .ok_or("cs2: missing startup RCON credential")?;
                let command = Self::startup_command(ctx)?;
                match tokio::time::timeout(
                    Duration::from_secs(2),
                    rcon::exec(port, password, &command),
                )
                .await
                {
                    Ok(result) => result?,
                    Err(_) => return Ok(GameHealth::Starting),
                }
                tokio::fs::write(marker, "").await?;
            }
            if !Self::addon_mode(ctx) {
                Self::remove_shared_config(ctx).await?;
            }
            Ok(GameHealth::Healthy)
        } else {
            Ok(GameHealth::Starting)
        }
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
    async fn graceful_stop(&self, _ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<()> {
        let _cleanup = if Self::addon_mode(_ctx) {
            None
        } else {
            Self::shared_config(_ctx)?
                .map(|(path, _)| shared_config::SharedConfig::remove_on_drop(path))
        };
        #[cfg(windows)]
        {
            p.kill().await?;
            p.wait().await?;
        }
        #[cfg(not(windows))]
        {
            if p.write_stdin("quit\n").await.is_err() {
                p.interrupt().await?;
            } else {
                match tokio::time::timeout(Duration::from_secs(5), p.wait()).await {
                    Ok(result) => {
                        result?;
                    }
                    Err(_) => p.interrupt().await?,
                }
            }
        }
        if !Self::addon_mode(_ctx) {
            Self::remove_shared_config(_ctx).await?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests;
