use super::*;
use executor_api::{ExitStatus, OutputLine, ResourceUsage};
use serde_json::json;
use std::sync::atomic::{AtomicUsize, Ordering};
use tokio::io::{AsyncReadExt, AsyncWriteExt};

fn mock_rcon(
    listener: tokio::net::TcpListener,
    password: String,
    response: i32,
) -> tokio::task::JoinHandle<String> {
    tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.unwrap();
        let len = stream.read_i32_le().await.unwrap();
        let mut packet = vec![0; len as usize];
        stream.read_exact(&mut packet).await.unwrap();
        assert_eq!(&packet[8..packet.len() - 2], password.as_bytes());
        if response == -3 {
            return String::new();
        }
        if response == -2 {
            stream.write_all(&4i32.to_le_bytes()).await.unwrap();
            return String::new();
        }
        for (id, kind) in [(1i32, 0i32), (response, if response == 0 { 0 } else { 2 })] {
            let mut bytes = 10i32.to_le_bytes().to_vec();
            bytes.extend(id.to_le_bytes());
            bytes.extend(kind.to_le_bytes());
            bytes.extend([0, 0]);
            stream.write_all(&bytes).await.unwrap();
        }
        if response != 1 {
            return String::new();
        }
        let len = stream.read_i32_le().await.unwrap();
        let mut packet = vec![0; len as usize];
        stream.read_exact(&mut packet).await.unwrap();
        assert_eq!(&packet[..8], &[2, 0, 0, 0, 2, 0, 0, 0]);
        stream
            .write_all(&[10, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0])
            .await
            .unwrap();
        stream
            .write_all(&[10, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0])
            .await
            .unwrap();
        let command = String::from_utf8(packet[8..packet.len() - 2].to_vec()).unwrap();
        let len = stream.read_i32_le().await.unwrap();
        let mut packet = vec![0; len as usize];
        stream.read_exact(&mut packet).await.unwrap();
        assert_eq!(&packet[..8], &[3, 0, 0, 0, 2, 0, 0, 0]);
        assert_eq!(&packet[8..packet.len() - 2], b"rcon_password \"\"");
        command
    })
}

#[tokio::test]
async fn rcon_authentication_commands_and_bad_replies() {
    for response in [1, 0, -1, -2, -3] {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        let server = mock_rcon(listener, "private-password".into(), response);
        let result = rcon::exec(port, "private-password", "").await;
        assert_eq!(result.is_ok(), response == 1);
        assert_eq!(server.await.unwrap(), "");
    }
}

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

fn fake_steam_install(root: &Path) -> PathBuf {
    let dir = root.join("steamapps/common/Counter-Strike Global Offensive");
    std::fs::create_dir_all(steam::binary(&dir).parent().unwrap()).unwrap();
    std::fs::write(steam::binary(&dir), "binary").unwrap();
    std::fs::create_dir_all(dir.join("game/csgo/cfg")).unwrap();
    std::fs::write(
        dir.join("game/csgo/gameinfo.gi"),
        "GameInfo { FileSystem { SearchPaths { Game csgo Game core Mod csgo } } }",
    )
    .unwrap();
    std::fs::write(
        root.join("steamapps/appmanifest_730.acf"),
        "\"AppState\" { \"StateFlags\" \"4\" \"installdir\" \"Counter-Strike Global Offensive\" }",
    )
    .unwrap();
    dir
}

#[test]
fn steam_library_discovery_and_vdf() {
    let root = tempfile::tempdir().unwrap();
    assert!(steam::in_root(root.path()).is_none());
    let library = root.path().join("another library");
    let dir = fake_steam_install(&library);
    std::fs::create_dir_all(root.path().join("steamapps")).unwrap();
    let path = library.to_string_lossy().replace('\\', "\\\\");
    let fixture = include_str!("../tests/libraryfolders.vdf").replace("@LIBRARY@", &path);
    let empty_library = root
        .path()
        .join("empty library")
        .to_string_lossy()
        .replace('\\', "\\\\");
    let on_disk_fixture = fixture.replace(r#"C:\\Program Files (x86)\\Steam"#, &empty_library);
    // The on-disk copy must not point at the host's real Steam library.
    std::fs::write(
        root.path().join("steamapps/libraryfolders.vdf"),
        &on_disk_fixture,
    )
    .unwrap();
    let parsed = vdf::parse(&fixture).unwrap();
    assert_eq!(
        parsed
            .get("libraryfolders")
            .unwrap()
            .get("0")
            .unwrap()
            .get("path")
            .unwrap()
            .text(),
        Some(r"C:\Program Files (x86)\Steam")
    );
    assert_eq!(
        steam::in_root(root.path()).unwrap().dir,
        std::fs::canonicalize(&dir).unwrap()
    );
    let manifest = library.join("steamapps/appmanifest_730.acf");
    for flags in ["0", "6", "1026"] {
        std::fs::write(
            &manifest,
            format!(
                "AppState {{ StateFlags {flags} installdir \"Counter-Strike Global Offensive\" }}"
            ),
        )
        .unwrap();
        assert!(steam::in_root(root.path()).is_none());
    }
    for contents in [
        "AppState {",
        "AppState { StateFlags 4 }",
        "AppState { StateFlags 4 installdir ../escape }",
        "AppState { StateFlags 4 installdir \"\" }",
    ] {
        std::fs::write(&manifest, contents).unwrap();
        assert!(steam::in_root(root.path()).is_none());
    }
    for input in ["\"unterminated", "a { b", "}", "a", "a { b }"] {
        assert!(vdf::parse(input).is_err(), "{input}");
    }
    let info = vdf::parse(
        "GameInfo { game \"Counter Strike\" FileSystem { SearchPaths { Game csgo Game core } } }",
    )
    .unwrap();
    let roundtrip = vdf::parse(&info.render()).unwrap();
    assert_eq!(
        roundtrip
            .get("GameInfo")
            .unwrap()
            .get("FileSystem")
            .unwrap()
            .get("SearchPaths")
            .unwrap()
            .entries()
            .unwrap()
            .len(),
        2
    );
}

#[cfg(windows)]
#[test]
fn windows_steamclient_path_precedes_existing_path() {
    let root = tempfile::tempdir().unwrap();
    fake_steam_install(root.path());
    let steamclient = root.path().join("steamclient64.dll");
    std::fs::write(&steamclient, "steamclient").unwrap();

    let install = steam::in_root(root.path()).unwrap();
    assert_eq!(install.steamclient.as_deref(), Some(steamclient.as_path()));
    std::fs::write(
        root.path().join("cs2-install.json"),
        serde_json::to_vec(&install).unwrap(),
    )
    .unwrap();

    let cfg = json!({"server_name":"Varde"});
    let deployment = DeploymentSpec::parse(&json!({}));
    let ctx = context(root.path(), &cfg, &deployment);
    let spec = Cs2Driver.process_spec(&ctx).unwrap();
    let path = spec.env.iter().find(|(key, _)| key == "PATH").unwrap();
    let mut expected = vec![root.path().to_path_buf()];
    if let Some(old) = std::env::var_os("PATH") {
        expected.extend(std::env::split_paths(&old));
    }
    assert_eq!(std::env::split_paths(&path.1).collect::<Vec<_>>(), expected);
}

#[tokio::test]
async fn shared_install_config_and_bans_are_private() {
    let root = tempfile::tempdir().unwrap();
    let dir = fake_steam_install(root.path());
    let user_bans = dir.join("game/csgo/cfg/banned_user.cfg");
    std::fs::write(&user_bans, "user bans must not change").unwrap();
    let config = json!({"server_name":"Private server", "password":"secret"});
    let deployment = DeploymentSpec::parse(&json!({}));
    for name in ["first", "second"] {
        let private = root.path().join(name);
        std::fs::create_dir_all(&private).unwrap();
        let ctx = context(&private, &config, &deployment);
        let install = steam::in_root(root.path()).unwrap();
        Cs2Driver::prepare_shared(&ctx, &install).unwrap();
        assert_eq!(
            Cs2Driver::binary(&ctx).unwrap(),
            steam::binary(&std::fs::canonicalize(&dir).unwrap())
        );
        let state = private.join("game/csgo/cfg");
        std::fs::create_dir_all(&state).unwrap();
        std::fs::write(state.join("banned_user.cfg"), "banid 0 STEAM_0:1:123\n").unwrap();
        Cs2Driver.configure(&ctx).await.unwrap();
        let game = Cs2Driver::game_dir(&ctx).unwrap();
        assert_eq!(
            std::fs::read_to_string(game.join("cfg/banned_user.cfg")).unwrap(),
            "banid 0 STEAM_0:1:123\n"
        );
        let cfg = std::fs::read_to_string(game.join("cfg/varde.cfg")).unwrap();
        assert!(cfg.contains("exec banned_user.cfg"));
        assert!(cfg.contains("Private server"));
        assert!(std::fs::read_to_string(game.join("gameinfo.gi"))
            .unwrap()
            .contains("cs2-runtime"));
        let spec = Cs2Driver.process_spec(&ctx).unwrap();
        assert_eq!(
            &spec.args[..3],
            &[
                OsString::from("-dedicated"),
                OsString::from("-game"),
                game.into_os_string()
            ]
        );
        assert_eq!(
            Cs2Driver
                .prepare_snapshot(&ctx, &Process::default())
                .await
                .unwrap(),
            SnapshotBarrier::Live
        );
        assert_eq!(
            std::fs::read_to_string(&user_bans).unwrap(),
            "user bans must not change"
        );
    }
    assert!(!dir.join("game/csgo/cfg/varde.cfg").exists());
}

#[test]
fn complete_steamcmd_deployments_are_reusable() {
    let root = tempfile::tempdir().unwrap();
    let server = root.path().join("dep_previous/server");
    std::fs::create_dir_all(server.join("steamapps")).unwrap();
    std::fs::create_dir_all(steam::binary(&server).parent().unwrap()).unwrap();
    std::fs::write(steam::binary(&server), "binary").unwrap();
    let manifest = server.join("steamapps/appmanifest_730.acf");
    std::fs::write(&manifest, "\"AppState\" { \"StateFlags\" \"6\" }").unwrap();
    assert!(steam::in_deployments(root.path()).is_none());
    std::fs::write(&manifest, "\"AppState\" { \"StateFlags\" \"4\" }").unwrap();
    assert_eq!(
        steam::in_deployments(root.path()).unwrap().dir,
        std::fs::canonicalize(&server).unwrap()
    );
    std::fs::remove_file(steam::binary(&server)).unwrap();
    assert!(steam::in_deployments(root.path()).is_none());
}

struct UpdatingRuntimes {
    calls: std::sync::Mutex<Vec<PathBuf>>,
    steamcmd: PathBuf,
    fail: bool,
    invalidate: bool,
}

#[async_trait]
impl RuntimeProvider for UpdatingRuntimes {
    fn runtime_path(&self, _: &str, _: &str) -> Option<PathBuf> {
        None
    }

    async fn steam_app_install(&self, app_id: u32, dest: &Path) -> Result<()> {
        assert_eq!(app_id, APP_ID);
        assert!(game_driver_api::steam_app_installed(app_id, dest));
        self.calls.lock().unwrap().push(dest.to_path_buf());
        if self.fail {
            if self.invalidate {
                std::fs::write(
                    dest.join("steamapps/appmanifest_730.acf"),
                    "\"AppState\" { \"StateFlags\" \"6\" }",
                )?;
            }
            return Err("update failed".into());
        }
        Ok(())
    }

    async fn steamcmd_path(&self) -> Result<PathBuf> {
        Ok(self.steamcmd.clone())
    }
}

fn managed_install(root: &Path, owner: &str) -> (PathBuf, PathBuf, UpdatingRuntimes) {
    let client = fake_steam_install(&root.join("fixture"));
    let private = root.join("deployments/current");
    std::fs::create_dir_all(&private).unwrap();
    let server = root.join("deployments").join(owner).join("server");
    std::fs::create_dir_all(server.parent().unwrap()).unwrap();
    std::fs::rename(client, &server).unwrap();
    std::fs::create_dir_all(server.join("steamapps")).unwrap();
    std::fs::write(
        server.join("steamapps/appmanifest_730.acf"),
        "\"AppState\" { \"StateFlags\" \"4\" }",
    )
    .unwrap();
    let runtime = root.join("runtime");
    std::fs::create_dir_all(runtime.join("linux64")).unwrap();
    std::fs::write(runtime.join("linux64/steamclient.so"), "library").unwrap();
    let runtimes = UpdatingRuntimes {
        calls: Default::default(),
        steamcmd: runtime.join("steamcmd"),
        fail: false,
        invalidate: false,
    };
    (private, std::fs::canonicalize(server).unwrap(), runtimes)
}

#[tokio::test]
async fn prepare_updates_complete_own_and_previous_steamcmd_installs_each_time() {
    for (name, fail) in [
        ("current", false),
        ("previous", false),
        ("current", true),
        ("previous", true),
    ] {
        let root = tempfile::tempdir().unwrap();
        let (private, server, mut runtimes) = managed_install(root.path(), name);
        runtimes.fail = fail;
        let cfg = json!({});
        let deployment = DeploymentSpec::parse(&json!({}));
        let mut ctx = context(&private, &cfg, &deployment);
        ctx.runtimes = &runtimes;
        for _ in 0..2 {
            Cs2Driver::prepare_install(&ctx, None).await.unwrap();
            assert_eq!(Cs2Driver::binary(&ctx).unwrap(), steam::binary(&server));
            #[cfg(target_os = "linux")]
            if !fail {
                assert_eq!(
                    std::fs::read(steam::binary(&server).with_file_name("steamclient.so")).unwrap(),
                    b"library"
                );
            }
        }
        assert_eq!(
            *runtimes.calls.lock().unwrap(),
            vec![std::fs::canonicalize(&server).unwrap(); 2]
        );
    }
}

#[tokio::test]
async fn failed_update_that_leaves_incomplete_install_errors() {
    let root = tempfile::tempdir().unwrap();
    let (private, server, mut runtimes) = managed_install(root.path(), "previous");
    runtimes.fail = true;
    runtimes.invalidate = true;
    let cfg = json!({});
    let deployment = DeploymentSpec::parse(&json!({}));
    let mut ctx = context(&private, &cfg, &deployment);
    ctx.runtimes = &runtimes;
    let error = Cs2Driver::prepare_install(&ctx, None).await.unwrap_err();
    assert!(error.to_string().contains("update failed"));
    assert!(!steam_app_installed(APP_ID, &server));
    assert_eq!(runtimes.calls.lock().unwrap().len(), 1);
    assert!(!private.join("cs2-install.json").exists());
}

struct LiveExecutor {
    exited: tokio::sync::watch::Sender<bool>,
    fail: bool,
}

#[async_trait]
impl Executor for LiveExecutor {
    fn kind(&self) -> &'static str {
        "test"
    }
    async fn spawn(&self, _: &ProcessSpec) -> executor_api::Result<Box<dyn ProcessHandle>> {
        if self.fail {
            return Err("spawn failed".into());
        }
        Ok(Box::new(Process {
            exited: Some(self.exited.clone()),
            ..Process::default()
        }))
    }
}

#[tokio::test]
async fn live_steamcmd_install_is_locked_until_last_process_exits() {
    let root = tempfile::tempdir().unwrap();
    let (private, server, runtimes) = managed_install(root.path(), "previous");
    let cfg = json!({});
    let deployment = DeploymentSpec::parse(&json!({}));
    let mut ctx = context(&private, &cfg, &deployment);
    ctx.runtimes = &runtimes;
    Cs2Driver::prepare_install(&ctx, None).await.unwrap();
    runtimes.calls.lock().unwrap().clear();
    let a = LiveExecutor {
        exited: tokio::sync::watch::channel(false).0,
        fail: false,
    };
    let b = LiveExecutor {
        exited: tokio::sync::watch::channel(false).0,
        fail: false,
    };
    let first = Cs2Driver.spawn(&ctx, &a).await.unwrap();
    let second = Cs2Driver.spawn(&ctx, &b).await.unwrap();
    let lock = install_lock::open(&server).unwrap();
    assert!(fs2::FileExt::try_lock_exclusive(&lock).is_err());
    Cs2Driver::prepare_install(&ctx, None).await.unwrap();
    assert!(runtimes.calls.lock().unwrap().is_empty());
    a.exited.send_replace(true);
    first.wait().await.unwrap();
    assert!(fs2::FileExt::try_lock_exclusive(&lock).is_err());
    second.kill().await.unwrap();
    second.wait().await.unwrap();
    fs2::FileExt::try_lock_exclusive(&lock).unwrap();
    fs2::FileExt::unlock(&lock).unwrap();
    Cs2Driver::prepare_install(&ctx, None).await.unwrap();
    assert_eq!(runtimes.calls.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn failed_spawn_and_dropped_process_handles_do_not_leave_stale_locks() {
    let root = tempfile::tempdir().unwrap();
    let (private, server, _) = managed_install(root.path(), "previous");
    let cfg = json!({});
    let deployment = DeploymentSpec::parse(&json!({}));
    let ctx = context(&private, &cfg, &deployment);
    let lock = install_lock::open(&server).unwrap();
    fs2::FileExt::lock_shared(&lock).unwrap();
    Cs2Driver::prepare_install(&ctx, None).await.unwrap();
    fs2::FileExt::unlock(&lock).unwrap();
    let mut executor = LiveExecutor {
        exited: tokio::sync::watch::channel(false).0,
        fail: true,
    };
    assert!(Cs2Driver.spawn(&ctx, &executor).await.is_err());
    fs2::FileExt::try_lock_exclusive(&lock).unwrap();
    fs2::FileExt::unlock(&lock).unwrap();
    executor.fail = false;
    let process = Cs2Driver.spawn(&ctx, &executor).await.unwrap();
    drop(process);
    assert!(fs2::FileExt::try_lock_exclusive(&lock).is_err());
    executor.exited.send_replace(true);
    tokio::time::timeout(Duration::from_secs(2), async {
        while fs2::FileExt::try_lock_exclusive(&lock).is_err() {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
}

#[tokio::test]
async fn launch_waits_for_updater_and_rechecks_manifest() {
    let root = tempfile::tempdir().unwrap();
    let (private, server, _) = managed_install(root.path(), "previous");
    let cfg = json!({});
    let deployment = DeploymentSpec::parse(&json!({}));
    let ctx = context(&private, &cfg, &deployment);
    let lock = install_lock::open(&server).unwrap();
    fs2::FileExt::lock_shared(&lock).unwrap();
    Cs2Driver::prepare_install(&ctx, None).await.unwrap();
    fs2::FileExt::unlock(&lock).unwrap();
    fs2::FileExt::lock_exclusive(&lock).unwrap();
    let executor = LiveExecutor {
        exited: tokio::sync::watch::channel(false).0,
        fail: true,
    };
    let (launch, ()) = tokio::join!(Cs2Driver.spawn(&ctx, &executor), async {
        tokio::time::sleep(Duration::from_millis(20)).await;
        std::fs::write(
            server.join("steamapps/appmanifest_730.acf"),
            "\"AppState\" { \"StateFlags\" \"6\" }",
        )
        .unwrap();
        fs2::FileExt::unlock(&lock).unwrap();
    });
    let error = launch.err().unwrap();
    assert!(error.to_string().contains("incomplete before launch"));
    fs2::FileExt::try_lock_exclusive(&lock).unwrap();
}

#[tokio::test]
async fn prepare_leaves_steam_client_updates_to_steam() {
    let root = tempfile::tempdir().unwrap();
    let client = fake_steam_install(root.path());
    let private = root.path().join("deployments/current");
    std::fs::create_dir_all(&private).unwrap();
    let cfg = json!({});
    let deployment = DeploymentSpec::parse(&json!({}));
    let ctx = context(&private, &cfg, &deployment);
    for _ in 0..2 {
        Cs2Driver::prepare_install(&ctx, steam::in_root(root.path()))
            .await
            .unwrap();
        assert_eq!(
            Cs2Driver::binary(&ctx).unwrap(),
            steam::binary(&std::fs::canonicalize(&client).unwrap())
        );
    }
    assert!(!private.join("server").exists());
}

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
    exited: Option<tokio::sync::watch::Sender<bool>>,
    fail_stdin: bool,
    stall: bool,
    interrupts: AtomicUsize,
    writes: AtomicUsize,
    waits: AtomicUsize,
    kills: AtomicUsize,
}
#[async_trait]
impl ProcessHandle for Process {
    fn pid(&self) -> u32 {
        0
    }
    async fn write_stdin(&self, line: &str) -> Result<()> {
        self.writes.fetch_add(1, Ordering::SeqCst);
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
        self.waits.fetch_add(1, Ordering::SeqCst);
        if let Some(exited) = &self.exited {
            let mut rx = exited.subscribe();
            while !*rx.borrow_and_update() {
                rx.changed().await?;
            }
        }
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
        self.kills.fetch_add(1, Ordering::SeqCst);
        if let Some(exited) = &self.exited {
            exited.send_replace(true);
        }
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
            "-nominidumps",
            "-usercon",
            "-ip",
            "127.0.0.1",
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
fn startup_commands_preserve_positive_bot_quotas() {
    let root = tempfile::tempdir().unwrap();
    let deployment = DeploymentSpec::parse(&json!({}));
    for (bots, expected) in [
        (0, "exec varde.cfg; bot_kick; bot_quota 0"),
        (3, "exec varde.cfg; bot_quota 3"),
    ] {
        let cfg = json!({"bots": bots});
        let ctx = context(root.path(), &cfg, &deployment);
        assert_eq!(Cs2Driver::startup_command(&ctx).unwrap(), expected);
    }
}

#[test]
fn dashboard_null_optional_config_uses_defaults() {
    let driver = Cs2Driver::new();
    let cfg = json!({"server_name":"Varde"});
    let dashboard = json!({"server_name":"Varde","password":null,"gslt":null,"game_mode":"competitive","map":"de_dust2","max_players":10,"bots":0});
    driver.validate(&dashboard).unwrap();
    assert_eq!(
        Cs2Driver::cfg(&dashboard).unwrap(),
        Cs2Driver::cfg(&cfg).unwrap()
    );
    assert_eq!(
        Cs2Driver::args(&dashboard, PORT).unwrap(),
        Cs2Driver::args(&cfg, PORT).unwrap()
    );
    let mut nulls = cfg.clone();
    for key in [
        "password",
        "gslt",
        "game_mode",
        "map",
        "max_players",
        "bots",
    ] {
        nulls[key] = Value::Null;
    }
    driver.validate(&nulls).unwrap();
    assert_eq!(
        Cs2Driver::cfg(&nulls).unwrap(),
        Cs2Driver::cfg(&cfg).unwrap()
    );
    assert_eq!(
        Cs2Driver::args(&nulls, PORT).unwrap(),
        Cs2Driver::args(&cfg, PORT).unwrap()
    );
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
        ("server_name", json!(null)),
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
    #[cfg(windows)]
    assert!(!spec.stdin);
    #[cfg(not(windows))]
    assert!(spec.stdin);
    let port_arg = spec.args.iter().position(|arg| arg == "-port").unwrap();
    assert_eq!(spec.args[port_arg + 1], "32123");
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
    #[cfg(windows)]
    {
        let p = Process::default();
        driver.graceful_stop(&ctx, &p).await.unwrap();
        assert_eq!(p.writes.load(Ordering::SeqCst), 0);
        assert_eq!(p.kills.load(Ordering::SeqCst), 1);
        assert_eq!(p.waits.load(Ordering::SeqCst), 1);
        assert_eq!(p.interrupts.load(Ordering::SeqCst), 0);
    }
    #[cfg(not(windows))]
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
async fn selected_install_uses_unique_shared_cfg_and_cleans_up() {
    let root = tempfile::tempdir().unwrap();
    let steam_root = root.path().join("steam");
    let install_dir = fake_steam_install(&steam_root);
    let deployment_dir = root.path().join("deployment");
    std::fs::create_dir_all(&deployment_dir).unwrap();
    let cfg = json!({"server_name":"Varde","password":"secret"});
    let deployment = DeploymentSpec::parse(&json!({}));
    let mut ctx = context(&deployment_dir, &cfg, &deployment);
    ctx.server_id = "server-123";
    let install = steam::in_root(&steam_root).unwrap();
    std::fs::write(
        deployment_dir.join("cs2-install.json"),
        serde_json::to_vec(&install).unwrap(),
    )
    .unwrap();
    let bans = deployment_dir.join("game/csgo/cfg/banned_user.cfg");
    std::fs::create_dir_all(bans.parent().unwrap()).unwrap();
    std::fs::write(&bans, "banid 0 STEAM_0:1:123\n").unwrap();

    let socket = tokio::net::UdpSocket::bind("127.0.0.1:0").await.unwrap();
    let bindings = [PortBinding {
        service_port: PORT,
        local_port: socket.local_addr().unwrap().port().into(),
        protocol: GameProtocol::Udp,
    }];
    ctx.ports = &bindings;

    let driver = Cs2Driver::new();
    driver.configure(&ctx).await.unwrap();
    let shared_cfg = install_dir.join("game/csgo/cfg/varde_server-123.cfg");
    let shared = tokio::fs::read_to_string(&shared_cfg).await.unwrap();
    assert!(shared.contains("hostname \"Varde\"\n"));
    assert!(shared.contains("sv_password \"secret\"\n"));
    assert!(shared.contains("banid 0 STEAM_0:1:123\n"));
    assert!(!shared.contains("exec banned_user.cfg"));
    assert!(!shared.contains("exec banned_ip.cfg"));
    let private_cfg = deployment_dir.join("cs2-runtime/csgo/cfg/varde.cfg");
    assert!(tokio::fs::read_to_string(private_cfg)
        .await
        .unwrap()
        .contains("exec banned_user.cfg\n"));

    let spec = driver.process_spec(&ctx).unwrap();
    let exec = spec.args.iter().position(|arg| arg == "+exec").unwrap();
    assert_eq!(spec.args[exec + 1], "varde_server-123");
    assert!(!spec.args.contains(&OsString::from("varde.cfg")));
    ctx.server_id = "invalid/id";
    assert!(driver.process_spec(&ctx).is_err());
    assert!(driver.configure(&ctx).await.is_err());
    ctx.server_id = "server-123";

    let listener = tokio::net::TcpListener::bind((
        std::net::Ipv4Addr::LOCALHOST,
        bindings[0].local_port as u16,
    ))
    .await
    .unwrap();
    let password = shared
        .lines()
        .find_map(|s| {
            s.strip_prefix("rcon_password \"")
                .and_then(|s| s.strip_suffix('"'))
        })
        .unwrap()
        .to_string();
    let rcon_server = mock_rcon(listener, password, 1);
    let task = tokio::spawn(async move {
        let mut buf = [0; 256];
        let (n, addr) = socket.recv_from(&mut buf).await.unwrap();
        assert_eq!(&buf[..n], INFO_REQUEST);
        socket.send_to(CHALLENGE, addr).await.unwrap();
        let (n, addr) = socket.recv_from(&mut buf).await.unwrap();
        assert_eq!(&buf[..n], [INFO_REQUEST, &CHALLENGE[5..]].concat());
        socket.send_to(&info(), addr).await.unwrap();
    });
    assert_eq!(
        driver.probe(&ctx, &Process::default()).await.unwrap(),
        GameHealth::Healthy
    );
    task.await.unwrap();
    assert_eq!(
        rcon_server.await.unwrap(),
        "exec varde_server-123.cfg; bot_kick; bot_quota 0"
    );
    assert!(!shared_cfg.exists());
    assert!(deployment_dir.join("cs2-configured").is_file());
    driver.process_spec(&ctx).unwrap();
    assert!(shared_cfg.exists());
    assert!(!deployment_dir.join("cs2-configured").exists());

    driver.configure(&ctx).await.unwrap();
    assert!(shared_cfg.exists());
    driver
        .graceful_stop(&ctx, &Process::default())
        .await
        .unwrap();
    assert!(!shared_cfg.exists());
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
