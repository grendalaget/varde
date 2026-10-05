//! Gated integration test (VARDE_NET_TESTS=1 and VARDE_VALHEIM_TEST=1):
//! installs the dedicated server through steamcmd (~1-2 GB) and starts it
//! until the log reports "Game server connected".

#![cfg(unix)]

use std::sync::Arc;
use std::time::{Duration, Instant};

use executor_api::{Executor, ProcessHandle};
use game_driver_api::*;
use game_valheim::ValheimDriver;

#[tokio::test]
async fn real_valheim_install_and_start() {
    if std::env::var("VARDE_NET_TESTS").is_err() || std::env::var("VARDE_VALHEIM_TEST").is_err() {
        eprintln!("skipping; set VARDE_NET_TESTS=1 VARDE_VALHEIM_TEST=1");
        return;
    }
    let tmp = tempfile::tempdir().unwrap();
    let server_dir = tmp.path().join("server");
    let dep_dir = tmp.path().join("deployment");
    std::fs::create_dir_all(&server_dir).unwrap();
    std::fs::create_dir_all(&dep_dir).unwrap();
    let rt = runtimes::HttpRuntimes::new(tmp.path().join("runtimes"));
    let cfg = serde_json::json!({
        "server_name": "Varde Test", "world_name": "vardetest",
        "password": "hunter22",
    });
    let d = ValheimDriver::new();
    d.validate(&cfg).unwrap();
    let ctx = DriverContext {
        server_dir: &server_dir,
        deployment_dir: &dep_dir,
        runtimes: &rt,
        deployment: &DeploymentSpec::parse(&serde_json::json!({})),
        config: &cfg,
        memory_mb: 2048,
    };
    d.prepare(&ctx).await.expect("steamcmd install");
    let ex = executor_native::NativeExecutor;
    let spec = d.process_spec(&ctx).unwrap();
    let proc: Arc<dyn ProcessHandle> = Arc::from(ex.spawn(&spec).await.unwrap());
    let deadline = Instant::now() + Duration::from_secs(180);
    let mut listening = false;
    while Instant::now() < deadline {
        if let Ok(GameHealth::Healthy) = d.probe(&ctx, &*proc).await {
            listening = true;
            break;
        }
        tokio::time::sleep(Duration::from_secs(2)).await;
    }
    let _ = proc.interrupt().await;
    let _ = tokio::time::timeout(Duration::from_secs(30), proc.wait()).await;
    assert!(listening, "server never reported listening");
}
