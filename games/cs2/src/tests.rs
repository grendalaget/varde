use super::*;
use executor_api::{ExitStatus, OutputLine, ResourceUsage};
use serde_json::json;
use std::sync::atomic::{AtomicUsize, Ordering};

// Captured by /home/ubuntu/cs2-spike/probe.py (A2S_INFO and S2C_CHALLENGE).
const INFO_HEX: &str = "ffffffff4911436f756e7465722d537472696b6520320064655f6475737432006373676f00436f756e7465722d537472696b65203200da02024002646c0001312e34312e392e3200b1eb691a0808ce64cb4001656d7074792c696e73656375726500da02000000000000";
const CHALLENGE: &[u8] = b"\xff\xff\xff\xff\x41\xc9\xa4\x2b\x90";

fn info() -> Vec<u8> {
    (0..INFO_HEX.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&INFO_HEX[i..i + 2], 16).unwrap())
        .collect()
}

struct NoRuntimes;
#[async_trait]
impl RuntimeProvider for NoRuntimes {
    fn runtime_path(&self, _: &str, _: &str) -> Option<PathBuf> {
        None
    }
}

fn context<'a>(
    root: &'a std::path::Path,
    cfg: &'a Value,
    deployment: &'a DeploymentSpec,
) -> DriverContext<'a> {
    DriverContext {
        server_id: "cs2-test",
        server_dir: root,
        deployment_dir: root,
        config: cfg,
        deployment,
        runtimes: &NoRuntimes,
        ports: &[],
        memory_mb: 4096,
    }
}

#[derive(Default)]
struct Process {
    fail_stdin: bool,
    stall: bool,
    interrupts: AtomicUsize,
}
#[async_trait]
impl ProcessHandle for Process {
    fn pid(&self) -> u32 {
        0
    }
    async fn write_stdin(&self, line: &str) -> Result<()> {
        assert_eq!(line, "quit\n");
        if self.fail_stdin {
            Err("closed stdin".into())
        } else {
            Ok(())
        }
    }
    fn output(&self) -> tokio::sync::broadcast::Receiver<OutputLine> {
        tokio::sync::broadcast::channel(1).1
    }
    fn output_tail(&self, _: usize) -> Vec<OutputLine> {
        vec![]
    }
    async fn wait(&self) -> Result<ExitStatus> {
        if self.stall {
            std::future::pending().await
        } else {
            Ok(ExitStatus {
                code: Some(0),
                signal: None,
            })
        }
    }
    async fn terminate(&self) -> Result<()> {
        Ok(())
    }
    async fn interrupt(&self) -> Result<()> {
        self.interrupts.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }
    async fn kill(&self) -> Result<()> {
        Ok(())
    }
    fn resource_usage(&self) -> Option<ResourceUsage> {
        None
    }
}

#[test]
fn args_cfg_and_modes() {
    let mut cfg =
        json!({"server_name":"Varde CS2", "password":"secret", "max_players":12, "bots":3});
    let expected =
        "hostname \"Varde CS2\"\nsv_password \"secret\"\nbot_quota_mode normal\nbot_quota 3\n";
    assert_eq!(Cs2Driver::cfg(&cfg).unwrap(), expected);
    for (name, kind, mode) in [
        ("casual", 0, 0),
        ("competitive", 0, 1),
        ("wingman", 0, 2),
        ("arms_race", 1, 0),
        ("deathmatch", 1, 2),
    ] {
        cfg["game_mode"] = json!(name);
        assert_eq!(Cs2Driver::mode(&cfg).unwrap(), (kind, mode));
        let args = Cs2Driver::args(&cfg, 32123).unwrap();
        let expected: Vec<OsString> = [
            "-dedicated",
            "-port",
            "32123",
            "-maxplayers",
            "12",
            "+game_type",
            &kind.to_string(),
            "+game_mode",
            &mode.to_string(),
            "+exec",
            "varde.cfg",
            "+map",
            "de_dust2",
        ]
        .iter()
        .map(OsString::from)
        .collect();
        assert_eq!(args, expected);
    }
    cfg["gslt"] = json!("0123456789abcdef0123456789abcdef");
    let args = Cs2Driver::args(&cfg, PORT).unwrap();
    assert_eq!(
        &args[args.len() - 2..],
        &[
            OsString::from("+sv_setsteamaccount"),
            OsString::from("0123456789abcdef0123456789abcdef")
        ]
    );
    assert!(!Cs2Driver::cfg(&cfg).unwrap().contains("0123456789"));
}

#[test]
fn validation_defaults_bounds_and_injection() {
    let driver = Cs2Driver::new();
    let cfg = json!({"server_name":"Varde"});
    driver.validate(&cfg).unwrap();
    assert_eq!(Cs2Driver::mode(&cfg).unwrap(), (0, 1));
    assert_eq!(
        Cs2Driver::cfg(&cfg).unwrap(),
        "hostname \"Varde\"\nsv_password \"\"\nbot_quota_mode normal\nbot_quota 0\n"
    );
    assert!(driver.validate(&json!({})).is_err());
    for (key, value) in [
        ("server_name", json!("")),
        ("server_name", json!(" \t")),
        ("server_name", json!(4)),
        ("password", json!("x\";quit")),
        ("password", json!("x\nquit")),
        ("password", json!(true)),
        ("map", json!("")),
        ("map", json!("../de_dust2")),
        ("map", json!("+quit")),
        ("max_players", json!(1)),
        ("max_players", json!(65)),
        ("max_players", json!(2.5)),
        ("bots", json!(-1)),
        ("bots", json!(65)),
        ("bots", json!(true)),
        ("game_mode", json!("unknown")),
        ("gslt", json!("+quit")),
        ("gslt", json!(false)),
    ] {
        let mut invalid = cfg.clone();
        invalid[key] = value;
        assert!(driver.validate(&invalid).is_err(), "accepted {invalid}");
    }
    for n in [2, 64] {
        let mut cfg = cfg.clone();
        cfg["max_players"] = json!(n);
        driver.validate(&cfg).unwrap();
    }
    for n in [0, 64] {
        let mut cfg = cfg.clone();
        cfg["bots"] = json!(n);
        driver.validate(&cfg).unwrap();
    }
}

#[test]
fn incomplete_install_disk_budget() {
    let root = tempfile::tempdir().unwrap();
    std::fs::create_dir_all(root.path().join("steamapps")).unwrap();
    let manifest = root.path().join("steamapps/appmanifest_730.acf");
    for state in [None, Some("incomplete"), Some("0"), Some("1026")] {
        if let Some(state) = state {
            std::fs::write(&manifest, format!("\"StateFlags\" \"{state}\"")).unwrap();
        }
        assert!(check_install_space(root.path(), 0).is_err(), "{state:?}");
    }
    std::fs::create_dir_all(root.path().join("game")).unwrap();
    std::fs::write(root.path().join("game/partial.bin"), vec![1; 65536]).unwrap();
    let installed = allocated_install_bytes(root.path()).unwrap();
    assert!(installed > 0);
    check_install_space(root.path(), MIN_DISK_BYTES - installed).unwrap();
    assert!(check_install_space(root.path(), MIN_DISK_BYTES - installed - 1).is_err());
    std::fs::write(&manifest, "\"StateFlags\" \"4\"").unwrap();
    check_install_space(root.path(), 0).unwrap();
}

#[cfg(unix)]
#[test]
fn install_disk_budget_ignores_sparse_bytes_and_symlinks() {
    let root = tempfile::tempdir().unwrap();
    let sparse = std::fs::File::create(root.path().join("partial.bin")).unwrap();
    sparse.set_len(MIN_DISK_BYTES).unwrap();
    assert!(allocated_install_bytes(root.path()).unwrap() < MIN_DISK_BYTES);
    assert!(check_install_space(root.path(), 0).is_err());
    std::os::unix::fs::symlink(root.path(), root.path().join("loop")).unwrap();
    assert!(check_install_space(root.path(), 0).is_err());
}

#[test]
fn disk_and_persistence() {
    let root = tempfile::tempdir().unwrap();
    assert!(check_install_space(root.path(), 0).is_err());
    assert!(check_install_space(root.path(), MIN_DISK_BYTES - 1)
        .unwrap_err()
        .to_string()
        .contains("75 GB"));
    check_install_space(root.path(), MIN_DISK_BYTES).unwrap();
    let driver = Cs2Driver::new();
    assert_eq!(driver.id(), "cs2");
    assert_eq!(
        driver.ports(&json!({})),
        vec![PortSpec {
            port: 27015,
            protocol: GameProtocol::Udp
        }]
    );
    let paths = driver.persistent_paths(&json!({}));
    assert_eq!(
        paths,
        vec![
            PathPattern::new("game/csgo/cfg/banned_user.cfg"),
            PathPattern::new("game/csgo/cfg/banned_ip.cfg")
        ]
    );
    assert!(!paths
        .iter()
        .any(|p| p.matches("game/csgo/cfg/varde.cfg", true)));
    assert!(!driver.snapshot_after_stop());
}

#[test]
fn captured_a2s_and_malformed_packets() {
    let packet = info();
    assert_eq!(
        parse_reply(CHALLENGE),
        Some(QueryReply::Challenge([0xc9, 0xa4, 0x2b, 0x90]))
    );
    assert_eq!(
        parse_reply(&packet),
        Some(QueryReply::Info {
            map: "de_dust2",
            max_players: 64
        })
    );
    for n in 0..70 {
        assert_eq!(parse_reply(&packet[..n]), None, "prefix {n}");
    }
    for n in 0..9 {
        assert_eq!(parse_reply(&CHALLENGE[..n]), None);
    }
    assert_eq!(parse_reply(b""), None);
    assert_eq!(parse_reply(b"\xff\xff\xff\xfeI"), None);
    assert_eq!(parse_reply(b"\xff\xff\xff\xffZ"), None);
    assert_eq!(parse_reply(b"\xfe\xff\xff\xffI"), None);
}

#[tokio::test]
async fn udp_challenge_readiness_and_rejections() {
    for (map, zero_capacity, malformed, expected) in [
        ("de_dust2", false, false, true),
        ("de_inferno", false, false, false),
        ("de_dust2", true, false, false),
        ("de_dust2", false, true, false),
    ] {
        let socket = tokio::net::UdpSocket::bind("127.0.0.1:0").await.unwrap();
        let port = socket.local_addr().unwrap().port();
        let task = tokio::spawn(async move {
            let mut buf = [0; 256];
            let (n, addr) = socket.recv_from(&mut buf).await.unwrap();
            assert_eq!(&buf[..n], INFO_REQUEST);
            socket.send_to(CHALLENGE, addr).await.unwrap();
            let (n, addr) = socket.recv_from(&mut buf).await.unwrap();
            assert_eq!(&buf[..n], [INFO_REQUEST, &CHALLENGE[5..]].concat());
            let mut packet = info();
            if zero_capacity {
                packet[57] = 0;
            }
            if malformed {
                packet.truncate(7);
            }
            socket.send_to(&packet, addr).await.unwrap();
        });
        assert_eq!(a2s_ready(port, map).await.unwrap(), expected);
        task.await.unwrap();
    }
}

#[tokio::test(start_paused = true)]
async fn lifecycle_configs_bans_and_stop_fallback() {
    let root = tempfile::tempdir().unwrap();
    let cfg = json!({"server_name":"Varde"});
    let deployment = DeploymentSpec::parse(&json!({}));
    let mut ctx = context(root.path(), &cfg, &deployment);
    let bindings = [PortBinding {
        service_port: PORT,
        local_port: 32123,
        protocol: GameProtocol::Udp,
    }];
    ctx.ports = &bindings;
    let driver = Cs2Driver::new();
    driver.configure(&ctx).await.unwrap();
    let spec = driver.process_spec(&ctx).unwrap();
    assert!(spec.stdin);
    assert_eq!(spec.args[2], "32123");
    assert_eq!(spec.cwd, root.path().join("server"));
    #[cfg(target_os = "linux")]
    assert!(spec.env.iter().any(|(key, value)| key == "LD_LIBRARY_PATH"
        && std::env::split_paths(value).next().unwrap() == spec.program.parent().unwrap()));
    let bans = root.path().join("server/game/csgo/cfg/banned_user.cfg");
    tokio::fs::write(&bans, "banid 0 STEAM_0:1:123\n")
        .await
        .unwrap();
    let p = Process::default();
    assert_eq!(
        driver.prepare_snapshot(&ctx, &p).await.unwrap(),
        SnapshotBarrier::Live
    );
    let persisted = root.path().join("game/csgo/cfg/banned_user.cfg");
    assert_eq!(
        tokio::fs::read(&persisted).await.unwrap(),
        tokio::fs::read(&bans).await.unwrap()
    );
    tokio::fs::remove_file(&bans).await.unwrap();
    driver.configure(&ctx).await.unwrap();
    assert!(bans.is_file());
    let config = tokio::fs::read_to_string(bans.with_file_name("varde.cfg"))
        .await
        .unwrap();
    assert!(config.contains("exec banned_user.cfg\n"));
    tokio::fs::remove_file(&bans).await.unwrap();
    driver.prepare_snapshot(&ctx, &p).await.unwrap();
    assert!(!persisted.exists());
    driver.resume_after_snapshot(&ctx, &p).await.unwrap();
    for (fail_stdin, stall, interrupts) in [(false, false, 0), (true, false, 1), (false, true, 1)] {
        let p = Process {
            fail_stdin,
            stall,
            ..Default::default()
        };
        driver.graceful_stop(&ctx, &p).await.unwrap();
        assert_eq!(p.interrupts.load(Ordering::SeqCst), interrupts);
    }
}

#[tokio::test]
async fn probe_handles_closed_port_and_missing_install() {
    let root = tempfile::tempdir().unwrap();
    let cfg = json!({"server_name":"Varde"});
    let deployment = DeploymentSpec::parse(&json!({}));
    let mut ctx = context(root.path(), &cfg, &deployment);
    let socket = std::net::UdpSocket::bind("127.0.0.1:0").unwrap();
    let bindings = [PortBinding {
        service_port: PORT,
        local_port: socket.local_addr().unwrap().port().into(),
        protocol: GameProtocol::Udp,
    }];
    ctx.ports = &bindings;
    drop(socket);
    assert_eq!(
        Cs2Driver.probe(&ctx, &Process::default()).await.unwrap(),
        GameHealth::Starting
    );
    tokio::fs::create_dir_all(root.path().join("server/steamapps"))
        .await
        .unwrap();
    tokio::fs::write(
        root.path().join("server/steamapps/appmanifest_730.acf"),
        "\"StateFlags\" \"4\"",
    )
    .await
    .unwrap();
    assert_eq!(
        Cs2Driver.prepare(&ctx).await.unwrap_err().to_string(),
        "runtime provider cannot fetch"
    );
}
