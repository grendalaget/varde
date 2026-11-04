//! Native, pinned Auto Tournament stack. Only logical database dumps and DATA_DIR migrate.
use std::ffi::OsString;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use executor_api::{Executor, ProcessHandle, ProcessSpec};
use game_driver_api::*;
use rand::RngCore;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

mod process;
#[cfg(test)]
mod tests;

pub const PORT: u32 = 3069;
pub const IMAGE: &str = "sha256:a8b38b53b5d81a2325466947b3a5a95e5bf7cb9d1253e7d48b2a40e3ff377f7d";
pub struct AutoTournamentDriver;

#[derive(Serialize, Deserialize)]
struct Installation {
    node: PathBuf,
    postgres: PathBuf,
    caddy: PathBuf,
    app: PathBuf,
}

#[derive(Serialize, Deserialize)]
struct Settings {
    api_port: u16,
    pg_port: u16,
    password: String,
}

fn reserve_ports(
    api_port: u16,
    pg_port: u16,
) -> Result<(std::net::TcpListener, std::net::TcpListener)> {
    let api = std::net::TcpListener::bind(("127.0.0.1", api_port))?;
    let pg = std::net::TcpListener::bind(("127.0.0.1", pg_port))?;
    Ok((api, pg))
}

fn secret() -> String {
    let mut bytes = [0; 32];
    rand::thread_rng().fill_bytes(&mut bytes);
    hex::encode(bytes)
}

fn text<'a>(config: &'a serde_json::Value, key: &str) -> Result<Option<&'a str>> {
    match config.get(key).filter(|v| !v.is_null()) {
        None => Ok(None),
        Some(v) => v
            .as_str()
            .map(Some)
            .ok_or_else(|| format!("autotournament: {key} must be a string").into()),
    }
}

fn private_write(path: &Path, bytes: &[u8]) -> Result<()> {
    use std::io::Write;
    let mut opts = std::fs::OpenOptions::new();
    opts.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    opts.open(path)?.write_all(bytes)?;
    Ok(())
}

fn installation(ctx: &DriverContext<'_>) -> Result<Installation> {
    Ok(serde_json::from_slice(&std::fs::read(
        ctx.deployment_dir.join("installation.json"),
    )?)?)
}
fn settings(ctx: &DriverContext<'_>) -> Result<Settings> {
    Ok(serde_json::from_slice(&std::fs::read(
        ctx.server_dir.join("settings.json"),
    )?)?)
}
fn spec(program: &Path, cwd: &Path, args: &[&str]) -> ProcessSpec {
    ProcessSpec {
        program: program.into(),
        cwd: cwd.into(),
        args: args.iter().map(OsString::from).collect(),
        env: vec![],
        stdin: false,
    }
}
async fn command(mut command: tokio::process::Command) -> Result<()> {
    command.kill_on_drop(true);
    let result = tokio::time::timeout(Duration::from_secs(120), command.output()).await??;
    if !result.status.success() {
        return Err(format!(
            "autotournament tool failed: {}",
            String::from_utf8_lossy(&result.stderr)
        )
        .into());
    }
    Ok(())
}
fn pg_command(ctx: &DriverContext<'_>, name: &str) -> Result<tokio::process::Command> {
    let install = installation(ctx)?;
    let settings = settings(ctx)?;
    let mut cmd = tokio::process::Command::new(install.postgres.join("bin").join(name));
    cmd.env("PGHOST", "127.0.0.1")
        .env("PGPORT", settings.pg_port.to_string())
        .env("PGUSER", "autotournament")
        .env("PGDATABASE", "autotournament")
        .env("PGPASSWORD", settings.password)
        .env("LD_LIBRARY_PATH", install.postgres.join("lib"));
    Ok(cmd)
}
async fn dump(ctx: &DriverContext<'_>) -> Result<()> {
    let tmp = ctx.server_dir.join("database.dump.partial");
    let mut cmd = pg_command(ctx, "pg_dump")?;
    cmd.arg("--format=custom").arg("--file").arg(&tmp);
    command(cmd).await?;
    publish_dump(ctx.server_dir).await?;
    Ok(())
}

#[derive(Serialize, Deserialize)]
struct DumpIdentity {
    id: String,
    sha256: String,
}

fn dump_hash(path: &Path) -> Result<Option<String>> {
    use std::io::Read;
    let mut file = match std::fs::File::open(path) {
        Ok(file) => file,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e.into()),
    };
    let mut hash = Sha256::new();
    let mut buffer = [0; 65536];
    loop {
        let n = file.read(&mut buffer)?;
        if n == 0 {
            break;
        }
        hash.update(&buffer[..n]);
    }
    Ok(Some(hex::encode(hash.finalize())))
}

async fn publish_dump(server: &Path) -> Result<Vec<u8>> {
    let server = server.to_path_buf();
    tokio::task::spawn_blocking(move || {
        let identity = stage_dump(&server)?;
        let marker = server.join("postgres/.varde-dump");
        if server.join("postgres/.varde-ready").exists() {
            let current = read_dump(&server)?
                .map(|dump| serde_json::from_slice::<serde_json::Value>(&dump.identity))
                .transpose()?;
            write_atomic(
                &marker,
                &serde_json::to_vec(&serde_json::json!({
                    "current": current,
                    "pending": serde_json::from_slice::<serde_json::Value>(&identity)?,
                }))?,
            )?;
        }
        // This rename is the commit point: the referenced immutable dump is complete.
        write_atomic(&server.join("database.manifest.json"), &identity)?;
        if server.join("postgres/.varde-ready").exists() {
            write_atomic(&marker, &identity)?;
        }
        let selected: DumpIdentity = serde_json::from_slice(&identity)?;
        if let Ok(entries) = std::fs::read_dir(server.join("database-dumps")) {
            for entry in entries.flatten() {
                if entry.path() != dump_file(&server, &selected) {
                    let _ = std::fs::remove_file(entry.path());
                }
            }
        }
        let _ = std::fs::remove_file(server.join("database.dump"));
        let _ = std::fs::remove_file(server.join("database.dump.id"));
        Ok(identity)
    })
    .await?
}

fn dump_file(server: &Path, identity: &DumpIdentity) -> PathBuf {
    server
        .join("database-dumps")
        .join(format!("{}.dump", identity.id))
}

fn valid_dump_id(id: &str) -> bool {
    id.len() == 64 && hex::decode(id).is_ok()
}

fn sync_directory(path: &Path) -> Result<()> {
    #[cfg(unix)]
    std::fs::File::open(path)?.sync_all()?;
    #[cfg(not(unix))]
    let _ = path;
    Ok(())
}

fn write_atomic(path: &Path, bytes: &[u8]) -> Result<()> {
    use std::io::Write;
    let partial = path.with_extension("partial");
    {
        let mut file = std::fs::File::create(&partial)?;
        file.write_all(bytes)?;
        file.sync_all()?;
    }
    std::fs::rename(partial, path)?;
    sync_directory(path.parent().unwrap())
}

fn stage_dump(server: &Path) -> Result<Vec<u8>> {
    let partial = server.join("database.dump.partial");
    let identity = DumpIdentity {
        id: secret(),
        sha256: dump_hash(&partial)?.ok_or("autotournament: pending database dump missing")?,
    };
    let file = dump_file(server, &identity);
    std::fs::create_dir_all(file.parent().unwrap())?;
    std::fs::rename(partial, &file)?;
    std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open(&file)?
        .sync_all()?;
    sync_directory(file.parent().unwrap())?;
    Ok(serde_json::to_vec(&identity)?)
}

struct DatabaseDump {
    path: PathBuf,
    identity: Vec<u8>,
}

fn read_dump(server: &Path) -> Result<Option<DatabaseDump>> {
    match std::fs::read(server.join("database.manifest.json")) {
        Ok(bytes) => {
            let identity: DumpIdentity = serde_json::from_slice(&bytes)?;
            if !valid_dump_id(&identity.id) {
                return Err("autotournament: invalid database dump id".into());
            }
            let path = dump_file(server, &identity);
            if dump_hash(&path)?.as_deref() != Some(&identity.sha256) {
                return Err("autotournament: database dump identity mismatch".into());
            }
            return Ok(Some(DatabaseDump {
                path,
                identity: serde_json::to_vec(&identity)?,
            }));
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => return Err(e.into()),
    }
    let path = server.join("database.dump");
    let Some(sha256) = dump_hash(&path)? else {
        return Ok(None);
    };
    // Legacy two-file publication could be interrupted. The dump itself was
    // renamed whole; repair its digest, retaining any usable ownership ID.
    let identity = match std::fs::read(server.join("database.dump.id")) {
        Ok(bytes) => serde_json::from_slice::<DumpIdentity>(&bytes).ok(),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => None,
        Err(e) => return Err(e.into()),
    };
    let identity = match identity.filter(|identity| valid_dump_id(&identity.id)) {
        Some(mut identity) => {
            identity.sha256 = sha256;
            serde_json::to_vec(&identity)?
        }
        None => serde_json::to_vec(&serde_json::json!({"legacy": true, "sha256": sha256}))?,
    };
    Ok(Some(DatabaseDump { path, identity }))
}

async fn load_dump(server: PathBuf) -> Result<Option<DatabaseDump>> {
    tokio::task::spawn_blocking(move || read_dump(&server)).await?
}

fn needs_restore(cluster: &Path, identity: &[u8]) -> bool {
    // The current own dump means this cluster is equal or newer, even after a crash
    // or a restore of that same ID. Only a foreign or older dump replaces it.
    fn key(value: &serde_json::Value) -> Option<&str> {
        value
            .get("id")
            .or_else(|| value.get("sha256"))
            .and_then(|v| v.as_str())
    }
    let owned = || -> Option<bool> {
        let wanted: serde_json::Value = serde_json::from_slice(identity).ok()?;
        let marker: serde_json::Value =
            serde_json::from_slice(&std::fs::read(cluster.join(".varde-dump")).ok()?).ok()?;
        let id = key(&wanted)?;
        Some(
            key(&marker) == Some(id)
                || key(&marker["current"]) == Some(id)
                || key(&marker["pending"]) == Some(id),
        )
    };
    !cluster.join(".varde-ready").exists() || (!identity.is_empty() && owned() != Some(true))
}

async fn remember_dump(cluster: &Path, identity: &[u8]) -> Result<()> {
    let path = cluster.join(".varde-dump");
    let bytes = identity.to_vec();
    tokio::task::spawn_blocking(move || write_atomic(&path, &bytes)).await?
}

async fn health(port: u32) -> Result<bool> {
    use http_body_util::BodyExt;
    let socket = tokio::net::TcpStream::connect(("127.0.0.1", port as u16)).await?;
    let (mut sender, conn) =
        hyper::client::conn::http1::handshake(hyper_util::rt::TokioIo::new(socket)).await?;
    tokio::spawn(async move {
        let _ = conn.await;
    });
    let response = sender
        .send_request(
            hyper::Request::get("/health")
                .header("host", format!("127.0.0.1:{port}"))
                .body(http_body_util::Empty::<hyper::body::Bytes>::new())?,
        )
        .await?;
    if response.status() != hyper::StatusCode::OK {
        return Ok(false);
    }
    let body = http_body_util::Limited::new(response.into_body(), 4096)
        .collect()
        .await?
        .to_bytes();
    let value: serde_json::Value = serde_json::from_slice(&body)?;
    Ok(value.get("status").and_then(|s| s.as_str()) == Some("ok"))
}

#[async_trait]
impl GameDriver for AutoTournamentDriver {
    fn id(&self) -> &'static str {
        "autotournament"
    }
    fn ports(&self, _: &serde_json::Value) -> Vec<PortSpec> {
        vec![PortSpec {
            port: PORT,
            protocol: GameProtocol::Tcp,
        }]
    }
    fn persistent_paths(&self, _: &serde_json::Value) -> Vec<PathPattern> {
        vec![
            PathPattern::new("database.dump"),
            PathPattern::new("database.dump.id"),
            PathPattern::new("database.manifest.json"),
            PathPattern::new("database-dumps/"),
            PathPattern::new("data/"),
        ]
    }
    fn validate(&self, config: &serde_json::Value) -> Result<()> {
        if !matches!(
            text(config, "licence")?,
            Some("noncommercial" | "commercial")
        ) {
            return Err(
                "autotournament: licence must be explicitly noncommercial or commercial".into(),
            );
        }
        let url = text(config, "public_url")?.ok_or("autotournament: public_url required")?;
        let uri: hyper::Uri = url.parse()?;
        if !matches!(uri.scheme_str(), Some("http" | "https"))
            || uri.host().is_none()
            || uri.authority().is_some_and(|a| a.as_str().contains('@'))
            || uri.query().is_some()
            || uri.path() != "/"
        {
            return Err(
                "autotournament: public_url must be an http(s) origin without a path prefix".into(),
            );
        }
        let host = uri
            .host()
            .unwrap()
            .trim_start_matches('[')
            .trim_end_matches(']');
        let loopback = host.eq_ignore_ascii_case("localhost")
            || host
                .parse::<std::net::IpAddr>()
                .is_ok_and(|ip| ip.is_loopback());
        if uri.scheme_str() == Some("http") && !loopback {
            return Err("autotournament: public_url requires HTTPS outside loopback".into());
        }
        if let Some(token) = text(config, "api_token")? {
            if token.len() < 16
                || !token
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b"_-".contains(&b))
            {
                return Err("autotournament: api_token must have >=16 URL-safe characters".into());
            }
        }
        text(config, "steam_web_api_key")?;
        Ok(())
    }
    async fn prepare(&self, ctx: &DriverContext<'_>) -> Result<()> {
        if !cfg!(all(target_os = "linux", target_arch = "x86_64")) {
            return Err("autotournament supports Linux x86_64 only".into());
        }
        self.validate(ctx.config)?;
        let fetch = |url: &str, name: &str, hash: &str, archive| FetchSpec {
            url: url.into(),
            file_name: name.into(),
            sha256: Some(hash.into()),
            sha1: None,
            archive,
            untrusted: false,
        };
        let node = ctx
            .runtimes
            .fetch(
                "node",
                "24.22.2-linux-x64",
                &fetch(
                    "https://nodejs.org/dist/v24.22.2/node-v24.22.2-linux-x64.tar.xz",
                    "node.tar.xz",
                    "48cc0e21e048790e54c0cf5749c2725005313d652651c40b7a21feb9697aaf75",
                    ArchiveKind::TarXz,
                ),
            )
            .await?;
        let pg = ctx.runtimes.fetch("postgres", "theseus-16.9.0-linux-amd64", &fetch("https://github.com/theseus-rs/postgresql-binaries/releases/download/16.9.0/postgresql-16.9.0-x86_64-unknown-linux-gnu.tar.gz", "postgres.tar.gz", "ecfd3be18704dd9c5f1db8c3dc8d1f3de52df270ca47ad5f7cd07a5e9f288a7f",ArchiveKind::Tgz)).await?;
        let caddy = ctx.runtimes.fetch("caddy", "2.10.2-linux-amd64", &fetch("https://github.com/caddyserver/caddy/releases/download/v2.10.2/caddy_2.10.2_linux_amd64.tar.gz", "caddy.tar.gz", "5c218bc34c9197369263da7e9317a83acdbd80ef45d94dca5eff76e727c67cdd",ArchiveKind::Tgz)).await?;
        let app = ctx
            .runtimes
            .fetch_oci(&OciSpec {
                registry: "ghcr.io".into(),
                repository: "auto-tournament/auto-tournament".into(),
                digest: IMAGE.into(),
                os: "linux".into(),
                arch: "amd64".into(),
                paths: vec!["/app".into()],
            })
            .await?
            .join("app");
        tokio::fs::create_dir_all(ctx.deployment_dir).await?;
        let postgres = pg.join("postgresql-16.9.0-x86_64-unknown-linux-gnu");
        let install = Installation {
            node: node.join("node-v24.22.2-linux-x64/bin/node"),
            postgres,
            caddy: caddy.join("caddy"),
            app,
        };
        tokio::fs::write(
            ctx.deployment_dir.join("installation.json"),
            serde_json::to_vec(&install)?,
        )
        .await?;
        Ok(())
    }
    async fn configure(&self, ctx: &DriverContext<'_>) -> Result<()> {
        self.validate(ctx.config)?;
        tokio::fs::create_dir_all(ctx.server_dir.join("data")).await?;
        let token = ctx.server_dir.join("data/varde-api-token");
        if let Some(value) = text(ctx.config, "api_token")? {
            private_write(&token, value.as_bytes())?;
        } else if !token.exists() {
            private_write(&token, secret().as_bytes())?;
        }
        let password = ctx.server_dir.join("postgres-password");
        if !password.exists() {
            private_write(&password, secret().as_bytes())?;
        }
        let (api, pg) = reserve_ports(0, 0)?;
        let settings = Settings {
            api_port: api.local_addr()?.port(),
            pg_port: pg.local_addr()?.port(),
            password: std::fs::read_to_string(password)?,
        };
        private_write(
            &ctx.server_dir.join("settings.json"),
            &serde_json::to_vec(&settings)?,
        )?;
        let install = installation(ctx)?;
        let caddy = std::fs::read_to_string(install.app.join("Caddyfile"))?
            .replace("{\n\tservers", "{\n\tadmin off\n\tservers")
            .replace(
                "trusted_proxies static {$TRUSTED_PROXIES:private_ranges}",
                "trusted_proxies static 127.0.0.1/32 ::1/128",
            )
            .replace(
                ":3069 {",
                &format!("http://:{} {{\n\tbind 127.0.0.1", ctx.local_port(PORT)),
            )
            .replace(
                "localhost:3000",
                &format!("127.0.0.1:{}", settings.api_port),
            )
            .replace(
                "/app/public",
                &format!("\"{}\"", install.app.join("public").display()),
            );
        tokio::fs::write(ctx.server_dir.join("Caddyfile"), caddy).await?;
        Ok(())
    }
    fn process_spec(&self, ctx: &DriverContext<'_>) -> Result<ProcessSpec> {
        let install = installation(ctx)?;
        let settings = settings(ctx)?;
        // Upstream listens on all interfaces; constrain its HTTP server without modifying AT.
        let mut node = spec(&install.node,&install.app,&["--import","data:text/javascript,import net from 'node:net';const listen=net.Server.prototype.listen;net.Server.prototype.listen=function(port,...args){return listen.call(this,port,'127.0.0.1',...args)};","dist/index.js"]);
        let values = [
            ("NODE_ENV", "production".into()),
            ("PORT", settings.api_port.to_string()),
            (
                "DATABASE_URL",
                format!(
                    "postgresql://autotournament:{}@127.0.0.1:{}/autotournament",
                    settings.password, settings.pg_port
                ),
            ),
            (
                "DATA_DIR",
                ctx.server_dir.join("data").to_string_lossy().into_owned(),
            ),
            (
                "FRONTEND_BASE_URL",
                text(ctx.config, "public_url")?
                    .unwrap()
                    .trim_end_matches('/')
                    .into(),
            ),
            (
                "AT_ACCEPT_LICENSE",
                text(ctx.config, "licence")?.unwrap().into(),
            ),
            (
                "API_TOKENS",
                std::fs::read_to_string(ctx.server_dir.join("data/varde-api-token"))?,
            ),
            (
                "STEAM_API_KEY",
                text(ctx.config, "steam_web_api_key")?.unwrap_or("").into(),
            ),
        ];
        node.env = values
            .into_iter()
            .map(|(k, v)| (k.into(), v.into()))
            .collect();
        Ok(node)
    }
    async fn spawn(
        &self,
        ctx: &DriverContext<'_>,
        executor: &dyn Executor,
    ) -> Result<Box<dyn ProcessHandle>> {
        let install = installation(ctx)?;
        let settings = settings(ctx)?;
        let (api_reservation, pg_reservation) = reserve_ports(settings.api_port, settings.pg_port)?;
        let cluster = ctx.server_dir.join("postgres");
        let dump = load_dump(ctx.server_dir.to_path_buf()).await?;
        let identity = dump
            .as_ref()
            .map(|dump| dump.identity.as_slice())
            .unwrap_or_default();
        let fresh = needs_restore(&cluster, identity);
        if fresh {
            if cluster.exists() {
                tokio::fs::remove_dir_all(&cluster).await?;
            }
            let mut cmd = tokio::process::Command::new(install.postgres.join("bin/initdb"));
            cmd.arg("-D")
                .arg(&cluster)
                .args([
                    "--username=autotournament",
                    "--auth-local=trust",
                    "--auth-host=scram-sha-256",
                    "--encoding=UTF8",
                    "--no-locale",
                    "--pwfile",
                ])
                .arg(ctx.server_dir.join("postgres-password"));
            cmd.env("LD_LIBRARY_PATH", install.postgres.join("lib"));
            command(cmd).await?;
        }
        let mut pg = spec(&install.postgres.join("bin/postgres"), ctx.server_dir, &[]);
        pg.args = vec![
            "-D".into(),
            cluster.as_os_str().into(),
            "-h".into(),
            "127.0.0.1".into(),
            "-p".into(),
            settings.pg_port.to_string().into(),
            "-k".into(),
            "".into(),
        ];
        pg.env = vec![(
            "LD_LIBRARY_PATH".into(),
            install.postgres.join("lib").into_os_string(),
        )];
        drop(pg_reservation);
        let pg: Arc<dyn ProcessHandle> = executor.spawn(&pg).await?.into();
        let result = async {
            let mut ready = false;
            for _ in 0..100 {
                let mut check = pg_command(ctx, "pg_isready")?;
                if check.kill_on_drop(true).output().await?.status.success() {
                    ready = true;
                    break;
                }
                tokio::time::sleep(Duration::from_millis(100)).await;
            }
            if !ready {
                return Err("autotournament: PostgreSQL readiness timed out".into());
            }
            if fresh {
                let mut create = pg_command(ctx, "createdb")?;
                create.arg("autotournament");
                command(create).await?;
                if let Some(dump) = &dump {
                    let mut restore = pg_command(ctx, "pg_restore")?;
                    restore
                        .args(["--exit-on-error", "--no-owner", "--dbname=autotournament"])
                        .arg(&dump.path);
                    command(restore).await?;
                }
            }
            remember_dump(&cluster, identity).await?;
            if fresh {
                tokio::fs::write(cluster.join(".varde-ready"), b"ok").await?;
            }
            drop(api_reservation);
            let node: Arc<dyn ProcessHandle> =
                executor.spawn(&self.process_spec(ctx)?).await?.into();
            let mut caddy = spec(
                &install.caddy,
                ctx.server_dir,
                &["run", "--config", "Caddyfile", "--adapter", "caddyfile"],
            );
            caddy.env = vec![
                (
                    "FRONTEND_BASE_URL".into(),
                    text(ctx.config, "public_url")?.unwrap().into(),
                ),
                (
                    "XDG_DATA_HOME".into(),
                    ctx.server_dir.join("caddy-data").into_os_string(),
                ),
                (
                    "XDG_CONFIG_HOME".into(),
                    ctx.server_dir.join("caddy-config").into_os_string(),
                ),
            ];
            match executor.spawn(&caddy).await {
                Ok(caddy) => Ok(
                    Box::new(process::Stack::new(vec![pg.clone(), node, caddy.into()]))
                        as Box<dyn ProcessHandle>,
                ),
                Err(e) => {
                    let _ = node.kill().await;
                    let _ = node.wait().await;
                    Err(e)
                }
            }
        }
        .await;
        if result.is_err() {
            let _ = pg.kill().await;
            let _ = pg.wait().await;
        }
        result
    }
    async fn probe(&self, ctx: &DriverContext<'_>, _: &dyn ProcessHandle) -> Result<GameHealth> {
        Ok(
            match tokio::time::timeout(Duration::from_secs(3), health(ctx.local_port(PORT))).await {
                Ok(Ok(true)) => GameHealth::Healthy,
                _ => GameHealth::Starting,
            },
        )
    }
    async fn prepare_snapshot(
        &self,
        ctx: &DriverContext<'_>,
        p: &dyn ProcessHandle,
    ) -> Result<SnapshotBarrier> {
        p.write_stdin("pause").await?;
        dump(ctx).await?;
        Ok(SnapshotBarrier::Live)
    }
    async fn resume_after_snapshot(
        &self,
        _: &DriverContext<'_>,
        p: &dyn ProcessHandle,
    ) -> Result<()> {
        p.write_stdin("resume").await
    }
    fn snapshot_after_stop(&self) -> bool {
        true
    }
    async fn graceful_stop(&self, ctx: &DriverContext<'_>, p: &dyn ProcessHandle) -> Result<()> {
        let front = p.write_stdin("frontend-stop").await;
        let dumped = if front.is_ok() {
            dump(ctx).await
        } else {
            front
        };
        let stopped = if dumped.is_ok() {
            p.terminate().await
        } else {
            p.kill().await
        };
        dumped.and(stopped)
    }
    fn join_code_from_log(&self, line: &str) -> Option<String> {
        let candidate = line.split_once("enter this code: ")?.1;
        let code: String = candidate
            .chars()
            .take_while(|c| c.is_ascii_uppercase() || c.is_ascii_digit() || *c == '-')
            .collect();
        (code.len() == 34
            && code.split('-').all(|s| {
                s.len() == 4
                    && s.bytes()
                        .all(|b| b"0123456789ABCDEFGHJKMNPQRSTVWXYZ".contains(&b))
            }))
        .then_some(code)
    }
}
