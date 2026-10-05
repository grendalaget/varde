//! Gated integration test (VARDE_NET_TESTS=1): download the real Temurin
//! JRE and Mojang server jar through the runtime cache, boot the server
//! through the driver API, run the barrier, stop, then restart from a
//! fresh server dir against the same deployments/runtimes cache.

#![cfg(unix)]

use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, Instant};

use executor_api::{Executor, ProcessHandle};
use game_driver_api::*;
use game_minecraft::MinecraftDriver;

fn ctx_parts(root: &PathBuf) -> (PathBuf, PathBuf, runtimes::HttpRuntimes) {
    (
        root.join("server"),
        root.join("deployment"),
        runtimes::HttpRuntimes::new(root.join("runtimes")),
    )
}

#[tokio::test]
async fn real_minecraft_end_to_end() {
    if std::env::var("VARDE_NET_TESTS").is_err() {
        eprintln!("skipping; set VARDE_NET_TESTS=1");
        return;
    }
    let tmp = tempfile::tempdir().unwrap();
    let root = std::env::var("VARDE_MC_TEST_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|_| tmp.path().to_path_buf());
    let (server_dir, dep_dir, rt) = ctx_parts(&root);
    std::fs::create_dir_all(&server_dir).unwrap();
    std::fs::create_dir_all(&dep_dir).unwrap();
    let cfg = serde_json::json!({"eula_accepted": true, "online_mode": false});
    let d = MinecraftDriver::new();
    d.validate(&cfg).unwrap();
    let ctx = DriverContext {
        server_dir: &server_dir,
        deployment_dir: &dep_dir,
        runtimes: &rt,
        deployment: &DeploymentSpec::parse(&serde_json::json!({})),
        config: &cfg,
        memory_mb: 1024,
    };
    d.prepare(&ctx).await.expect("download jre+jar");
    d.configure(&ctx).await.unwrap();
    assert!(server_dir.join("eula.txt").exists());
    assert!(server_dir.join("server.properties").exists());

    let ex = executor_native::NativeExecutor;
    let spec = d.process_spec(&ctx).unwrap();
    let proc: Arc<dyn ProcessHandle> = Arc::from(ex.spawn(&spec).await.unwrap());

    let deadline = Instant::now() + Duration::from_secs(240);
    let mut healthy = false;
    while Instant::now() < deadline {
        if let Ok(GameHealth::Healthy) = d.probe(&ctx, &*proc).await {
            healthy = true;
            break;
        }
        tokio::time::sleep(Duration::from_secs(2)).await;
    }
    if !healthy {
        for l in proc.output_tail(40) {
            eprintln!("GAME {:?}", l.line);
        }
    }
    assert!(healthy, "server never became healthy");
    match d.prepare_snapshot(&ctx, &*proc).await.unwrap() {
        SnapshotBarrier::Live => {}
        other => panic!("expected Live barrier, got {other:?}"),
    }
    d.resume_after_snapshot(&ctx, &*proc).await.unwrap();
    d.graceful_stop(&ctx, &*proc).await.unwrap();
    let _ = tokio::time::timeout(Duration::from_secs(60), proc.wait()).await;
}
