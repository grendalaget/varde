//! VARDE_NET_TESTS=1 VARDE_CS2_TEST=1 enables the real Linux server test.
//! VARDE_STEAM_DIR points at a Steam root (or use VARDE_CS2_DIR directly).
#![cfg(target_os = "linux")]

use async_trait::async_trait;
use executor_api::Executor;
use game_cs2::steam;
use game_cs2::{Cs2Driver, PORT};
use game_driver_api::*;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

struct ExistingInstall;

struct IsolatedHome(PathBuf);

#[async_trait]
impl Executor for IsolatedHome {
    fn kind(&self) -> &'static str {
        "native"
    }
    async fn spawn(
        &self,
        spec: &executor_api::ProcessSpec,
    ) -> executor_api::Result<Box<dyn executor_api::ProcessHandle>> {
        let mut spec = spec.clone();
        spec.env
            .push(("HOME".into(), self.0.clone().into_os_string()));
        executor_native::NativeExecutor.spawn(&spec).await
    }
}

async fn settings(port: u16) -> Option<(String, u8)> {
    let socket = tokio::net::UdpSocket::bind("127.0.0.1:0").await.ok()?;
    socket
        .connect((std::net::Ipv4Addr::LOCALHOST, port))
        .await
        .ok()?;
    let mut request = b"\xff\xff\xff\xffTSource Engine Query\0".to_vec();
    let mut buf = [0; 2048];
    for _ in 0..2 {
        socket.send(&request).await.ok()?;
        let n = tokio::time::timeout(Duration::from_secs(1), socket.recv(&mut buf))
            .await
            .ok()?
            .ok()?;
        if buf.get(..5)? == b"\xff\xff\xff\xffA" && n == 9 {
            request.extend_from_slice(&buf[5..9]);
            continue;
        }
        if buf.get(..5)? != b"\xff\xff\xff\xffI" {
            return None;
        }
        let mut rest = buf.get(6..n)?;
        let mut name = String::new();
        for i in 0..4 {
            let end = rest.iter().position(|b| *b == 0)?;
            if i == 0 {
                name = std::str::from_utf8(&rest[..end]).ok()?.to_string();
            }
            rest = &rest[end + 1..];
        }
        return Some((name, *rest.get(4)?));
    }
    None
}
#[async_trait]
impl RuntimeProvider for ExistingInstall {
    fn runtime_path(&self, _: &str, _: &str) -> Option<PathBuf> {
        None
    }
    async fn steam_app_install(&self, _: u32, _: &Path) -> Result<()> {
        panic!("detected install must not call SteamCMD")
    }
    async fn steamcmd_path(&self) -> Result<PathBuf> {
        panic!("detected install must not require SteamCMD")
    }
}

#[tokio::test]
async fn real_cs2_detected_install_concurrent_start_a2s_and_quit() {
    if std::env::var("VARDE_NET_TESTS").as_deref() != Ok("1")
        || std::env::var("VARDE_CS2_TEST").as_deref() != Ok("1")
    {
        eprintln!("skipping; set VARDE_NET_TESTS=1 VARDE_CS2_TEST=1");
        return;
    }
    let install = steam::detect(None).expect("set VARDE_STEAM_DIR or VARDE_CS2_DIR");
    let marker = steam::binary(&install.dir).with_file_name("steam_appid.txt");
    let boot = install.dir.join("game/csgo/cfg/boot.vcfg");
    let original_boot = std::fs::read(&boot).ok();
    let seeded_boot = b"\"boot\"\n{\n\t\"setting.test\"\t\t\"1\"\n}\n";
    std::fs::write(&boot, seeded_boot).unwrap();
    println!(
        "CS2 boot before: {:?}",
        String::from_utf8_lossy(seeded_boot)
    );
    let mut before = installation_files(&install.dir);
    before.remove(&marker);
    let tmp = tempfile::tempdir().unwrap();
    let barrier = tokio::sync::Barrier::new(2);
    tokio::join!(
        run_instance(tmp.path(), "cs2-net-a", 0, &barrier),
        run_instance(tmp.path(), "cs2-net-b", 0, &barrier)
    );
    let mut after = installation_files(&install.dir);
    let final_boot = std::fs::read(&boot).unwrap();
    println!("CS2 boot after: {:?}", String::from_utf8_lossy(&final_boot));
    match original_boot {
        Some(bytes) => std::fs::write(&boot, bytes).unwrap(),
        None => std::fs::remove_file(&boot).unwrap(),
    }
    assert_eq!(
        final_boot, seeded_boot,
        "shared boot config content changed"
    );
    before.remove(&boot);
    after.remove(&boot);
    assert_eq!(
        std::fs::read_to_string(&marker)
            .expect("CS2 must create its app-ID marker")
            .trim_matches(|c: char| c.is_ascii_whitespace() || c == '\0'),
        "730",
        "unexpected CS2 app-ID marker content"
    );
    after.remove(&marker);
    for name in ["cs2-net-a", "cs2-net-b"] {
        let lock = install
            .dir
            .join("game/csgo/cfg")
            .join(format!(".varde_{name}.cfg.lock"));
        assert_eq!(after.remove(&lock).expect("config lock sidecar").0, 0);
        before.remove(&lock);
    }
    for (path, metadata) in &after {
        assert_eq!(
            before.get(path),
            Some(metadata),
            "shared installation file changed: {}",
            path.display()
        );
    }
    assert_eq!(
        before.len(),
        after.len(),
        "shared installation files removed"
    );
    println!(
        "CS2 shared installation unchanged: {} files; allowed steam_appid.txt=730, byte-identical boot.vcfg and empty config lock sidecars; SteamCMD calls=0",
        before.len()
    );
}

fn installation_files(
    dir: &Path,
) -> std::collections::BTreeMap<PathBuf, (u64, std::time::SystemTime)> {
    let mut files = std::collections::BTreeMap::new();
    let mut dirs = vec![dir.to_path_buf()];
    while let Some(dir) = dirs.pop() {
        for entry in std::fs::read_dir(dir).unwrap() {
            let entry = entry.unwrap();
            let kind = entry.file_type().unwrap();
            if kind.is_dir() {
                dirs.push(entry.path());
            } else if kind.is_file() {
                let meta = entry.metadata().unwrap();
                files.insert(entry.path(), (meta.len(), meta.modified().unwrap()));
            }
        }
    }
    files
}

async fn run_instance(root: &Path, name: &str, bots: u8, barrier: &tokio::sync::Barrier) {
    let deployment_dir = root.join("deployments").join(name);
    std::fs::create_dir_all(&deployment_dir).unwrap();
    let server_dir = root.join("servers").join(name);
    std::fs::create_dir_all(&server_dir).unwrap();
    let cfg = serde_json::json!({"server_name":name,"map":"de_dust2","max_players":10,"bots":bots,"password":"cs2-test-password"});
    let deployment = DeploymentSpec::parse(&serde_json::json!({}));
    let socket = std::net::UdpSocket::bind("127.0.0.1:0").unwrap();
    let port = socket.local_addr().unwrap().port();
    drop(socket);
    let bindings = [PortBinding {
        service_port: PORT,
        local_port: port.into(),
        protocol: GameProtocol::Udp,
    }];
    let ctx = DriverContext {
        server_id: name,
        server_dir: &server_dir,
        deployment_dir: &deployment_dir,
        runtimes: &ExistingInstall,
        deployment: &deployment,
        config: &cfg,
        ports: &bindings,
        memory_mb: 4096,
    };
    let driver = Cs2Driver::new();
    driver.validate(&cfg).unwrap();
    driver.prepare(&ctx).await.unwrap();
    driver.configure(&ctx).await.unwrap();
    let spec = driver.process_spec(&ctx).unwrap();
    assert_eq!(
        spec.program,
        steam::binary(&steam::detect(None).unwrap().dir)
    );
    assert!(spec.args.iter().any(|s| s == "-game"));
    let home = server_dir.join("empty-home");
    std::fs::create_dir_all(&home).unwrap();
    let start = Instant::now();
    let process = driver.spawn(&ctx, &IsolatedHome(home)).await.unwrap();
    let ready = tokio::time::timeout(Duration::from_secs(90), async {
        loop {
            if matches!(driver.probe(&ctx, &*process).await, Ok(GameHealth::Healthy)) {
                break;
            }
            tokio::time::sleep(Duration::from_millis(250)).await;
        }
    })
    .await
    .is_ok();
    println!(
        "CS2 detected-install {name} A2S ready={ready} elapsed={:.3}s port={port}",
        start.elapsed().as_secs_f64()
    );
    let together = tokio::time::timeout(Duration::from_secs(100), barrier.wait())
        .await
        .is_ok();
    let healthy_together = matches!(driver.probe(&ctx, &*process).await, Ok(GameHealth::Healthy));
    println!("CS2 concurrent {name} healthy={healthy_together}");
    let sockets = std::process::Command::new("ss")
        .args(["-H", "-l", "-u", "-n", "-p", "-t"])
        .output()
        .unwrap();
    assert!(sockets.status.success());
    let sockets = String::from_utf8(sockets.stdout).unwrap();
    let mut protocols = std::collections::BTreeSet::new();
    let mut exposed = Vec::new();
    for line in sockets.lines() {
        let fields: Vec<_> = line.split_whitespace().collect();
        let Some(local) = fields.get(4) else { continue };
        let Some((address, actual)) = local.rsplit_once(':') else {
            continue;
        };
        if actual != port.to_string() {
            continue;
        }
        println!("CS2 ss {name}: {line}");
        protocols.insert(fields[0].to_string());
        let address = address.trim_matches(['[', ']']).parse::<std::net::IpAddr>();
        let loopback = match address {
            Ok(std::net::IpAddr::V4(ip)) => ip.is_loopback(),
            Ok(std::net::IpAddr::V6(ip)) => {
                ip.is_loopback() || ip.to_ipv4_mapped().is_some_and(|ip| ip.is_loopback())
            }
            Err(_) => false,
        };
        if !loopback {
            exposed.push(line.to_string());
        }
    }
    let mut observed = None;
    if ready {
        process
            .write_stdin("sv_password\nbot_quota\nrcon_password\n")
            .await
            .unwrap();
        tokio::time::sleep(Duration::from_secs(8)).await;
        let _ = tokio::time::timeout(Duration::from_secs(30), async {
            loop {
                observed = settings(port).await;
                if observed == Some((name.to_string(), cfg["bots"].as_u64().unwrap() as u8)) {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(500)).await;
            }
        })
        .await;
    }
    if ready {
        assert_eq!(
            driver.prepare_snapshot(&ctx, &*process).await.unwrap(),
            SnapshotBarrier::Live
        );
    }
    let stopped = Instant::now();
    let stop = driver.graceful_stop(&ctx, &*process).await;
    let status = tokio::time::timeout(Duration::from_secs(15), process.wait()).await;
    if status.is_err() {
        process.kill().await.unwrap();
        process.wait().await.unwrap();
    }
    for line in process.output_tail(1000) {
        if line.line.contains("SteamAPI_Init")
            || line.line.contains("ss_loading -> ss_active")
            || line.line.contains("LAN only")
            || line.line.contains("varde.cfg")
            || line.line.contains("MainLoop returning")
            || line.line.contains("bot_quota =")
        {
            println!("CS2 LOG {name} {}", line.line);
        }
    }
    stop.unwrap();
    assert!(
        exposed.is_empty(),
        "non-loopback CS2 listeners: {exposed:?}"
    );
    assert!(protocols.contains("udp"), "game UDP listener missing");
    assert!(protocols.contains("tcp"), "RCON listener missing");
    let status = status.expect("server did not stop").unwrap();
    let password_stuck = process
        .output_tail(1000)
        .iter()
        .any(|l| l.line.contains("sv_password = cs2-test-password"));
    let rcon_disabled = process
        .output_tail(1000)
        .iter()
        .any(|l| l.line.trim() == "rcon_password =");
    println!("CS2 settings {name} A2S={observed:?} password_stuck={password_stuck} rcon_disabled={rcon_disabled}");
    println!(
        "CS2 quit {name} elapsed={:.3}s exit={status:?}",
        stopped.elapsed().as_secs_f64()
    );
    assert!(status.success());
    let (actual_name, bots) = observed.expect("missing settings A2S reply");
    assert_eq!(actual_name, cfg["server_name"].as_str().unwrap());
    assert_eq!(bots as u64, cfg["bots"].as_u64().unwrap());
    assert!(password_stuck);
    assert!(rcon_disabled);
    assert!(
        ready && together && healthy_together,
        "server never answered with the configured map and nonzero capacity"
    );
}
