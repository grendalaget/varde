//! VARDE_NET_TESTS=1 VARDE_CS2_TEST=1 enables the real Linux server test.
//! VARDE_CS2_INSTALL and VARDE_CS2_STEAMCMD reuse existing installations.
#![cfg(target_os = "linux")]

use async_trait::async_trait;
use executor_api::Executor;
use game_cs2::{Cs2Driver, PORT};
use game_driver_api::*;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

struct ExistingInstall {
    install: PathBuf,
    steamcmd: PathBuf,
}
#[async_trait]
impl RuntimeProvider for ExistingInstall {
    fn runtime_path(&self, _: &str, _: &str) -> Option<PathBuf> {
        None
    }
    async fn steam_app_install(&self, app: u32, dest: &Path) -> Result<()> {
        assert_eq!(app, 730);
        assert_eq!(
            std::fs::canonicalize(dest)?,
            std::fs::canonicalize(&self.install)?
        );
        Ok(())
    }
    async fn steamcmd_path(&self) -> Result<PathBuf> {
        Ok(self.steamcmd.clone())
    }
}

#[tokio::test]
async fn real_cs2_start_a2s_and_quit() {
    if std::env::var("VARDE_NET_TESTS").as_deref() != Ok("1")
        || std::env::var("VARDE_CS2_TEST").as_deref() != Ok("1")
    {
        eprintln!("skipping; set VARDE_NET_TESTS=1 VARDE_CS2_TEST=1");
        return;
    }
    let tmp = tempfile::tempdir().unwrap();
    let root = tmp.path();
    let deployment_dir = root.join("deployment");
    std::fs::create_dir_all(&deployment_dir).unwrap();
    let provider: Box<dyn RuntimeProvider> =
        if let Some(install) = std::env::var_os("VARDE_CS2_INSTALL") {
            let install = std::fs::canonicalize(install).unwrap();
            std::os::unix::fs::symlink(&install, deployment_dir.join("server")).unwrap();
            Box::new(ExistingInstall {
                install,
                steamcmd: PathBuf::from(
                    std::env::var_os("VARDE_CS2_STEAMCMD")
                        .expect("set VARDE_CS2_STEAMCMD to steamcmd.sh"),
                ),
            })
        } else {
            Box::new(runtimes::HttpRuntimes::new(root.join("runtimes")))
        };
    let cfg = serde_json::json!({"server_name":"Varde CS2 Test","map":"de_dust2","max_players":10,"bots":0});
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
        server_id: "cs2-net-test",
        server_dir: root,
        deployment_dir: &deployment_dir,
        runtimes: &*provider,
        deployment: &deployment,
        config: &cfg,
        ports: &bindings,
        memory_mb: 4096,
    };
    let driver = Cs2Driver::new();
    driver.validate(&cfg).unwrap();
    driver.prepare(&ctx).await.unwrap();
    driver.configure(&ctx).await.unwrap();
    let mut spec = driver.process_spec(&ctx).unwrap();
    // A clean HOME proves that the copied local steamclient.so is sufficient.
    let home = root.join("empty-home");
    std::fs::create_dir_all(&home).unwrap();
    spec.env.push(("HOME".into(), home.into_os_string()));
    let start = Instant::now();
    let process = executor_native::NativeExecutor.spawn(&spec).await.unwrap();
    let ready = tokio::time::timeout(Duration::from_secs(90), async {
        loop {
            if driver.probe(&ctx, &*process).await.unwrap() == GameHealth::Healthy {
                break;
            }
            tokio::time::sleep(Duration::from_millis(250)).await;
        }
    })
    .await
    .is_ok();
    println!(
        "CS2 A2S ready={ready} elapsed={:.3}s port={port}",
        start.elapsed().as_secs_f64()
    );
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
        {
            println!("CS2 LOG {}", line.line);
        }
    }
    stop.unwrap();
    let status = status.expect("server did not stop").unwrap();
    println!(
        "CS2 quit elapsed={:.3}s exit={status:?}",
        stopped.elapsed().as_secs_f64()
    );
    assert!(status.success());
    assert!(
        ready,
        "server never answered with the configured map and nonzero capacity"
    );
}
