//! Manual native lifecycle harness: snapshot / stop on stdin. Never prints API secrets.
use game_autotournament::{AutoTournamentDriver, PORT};
use game_driver_api::*;
use std::path::PathBuf;
use std::time::Duration;
use tokio::io::{AsyncBufReadExt, BufReader};

#[tokio::main]
async fn main() -> Result<()> {
    let root = PathBuf::from(std::env::var("VARDE_AT_CHECK_DIR")?);
    let cache = PathBuf::from(std::env::var("VARDE_AT_CACHE")?);
    let provider = runtimes::HttpRuntimes::new(cache);
    let server = root.join("server");
    let store = snapshot_store::Store::open(root.join("snapshots"))?;
    if std::env::var_os("VARDE_AT_RESTORE").is_some() {
        let id = std::fs::read_to_string(root.join("snapshot-id"))?;
        let id = snapshot_store::SnapshotId::parse(&id).ok_or("invalid snapshot id")?;
        store.restore(
            &id,
            &server,
            &AutoTournamentDriver.persistent_paths(&serde_json::json!({})),
        )?;
        println!("restored snapshot: {id}");
    }
    let deployment_dir = root.join("deployment");
    let deployment = DeploymentSpec::parse(&serde_json::json!({}));
    let config = serde_json::json!({"public_url":format!("http://127.0.0.1:{PORT}"),"licence":"noncommercial"});
    let ctx = DriverContext {
        server_id: "native-check",
        server_dir: &server,
        deployment_dir: &deployment_dir,
        runtimes: &provider,
        deployment: &deployment,
        config: &config,
        ports: &[],
        memory_mb: 2048,
        addons: &[],
    };
    let driver = AutoTournamentDriver;
    driver.prepare(&ctx).await?;
    driver.configure(&ctx).await?;
    let process = driver.spawn(&ctx, &executor_native::NativeExecutor).await?;
    let mut ready = false;
    for _ in 0..240 {
        if driver.probe(&ctx, &*process).await? == GameHealth::Healthy {
            ready = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }
    if !ready {
        for line in process.output_tail(100) {
            eprintln!("{}", line.line);
        }
        process.kill().await?;
        return Err("AT readiness timeout".into());
    }
    for _ in 0..20 {
        let code = process
            .output_tail(1000)
            .iter()
            .find_map(|l| driver.join_code_from_log(&l.line));
        if let Some(code) = code {
            println!("setup code parsed: {} groups", code.split('-').count());
            break;
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
    println!("READY http://127.0.0.1:{PORT}");
    let mut lines = BufReader::new(tokio::io::stdin()).lines();
    while let Some(line) = lines.next_line().await? {
        match line.as_str() {
            "snapshot" => {
                let barrier = driver.prepare_snapshot(&ctx, &*process).await;
                let snapshot = if barrier.is_ok() {
                    store.snapshot(
                        &server,
                        &driver.persistent_paths(&config),
                        snapshot_store::SnapshotMeta {
                            server_id: "native-check".into(),
                            execution_id: "check".into(),
                            epoch: 1,
                            node_id: "check".into(),
                            parent: None,
                            deployment_id: "check".into(),
                            created_at_unix_ms: 0,
                            reason: "manual".into(),
                        },
                    )
                } else {
                    driver.resume_after_snapshot(&ctx, &*process).await?;
                    return Err(barrier.unwrap_err());
                };
                driver.resume_after_snapshot(&ctx, &*process).await?;
                let snapshot = snapshot?;
                std::fs::write(root.join("snapshot-id"), snapshot.id.to_string())?;
                let manifest = store.manifest(&snapshot.id)?;
                assert!(manifest.files.iter().all(|f| f.path == "database.dump"
                    || f.path == "data"
                    || f.path.starts_with("data/")));
                println!(
                    "snapshot: {}; {} files; raw cluster excluded",
                    snapshot.id, snapshot.file_count
                );
                println!(
                    "snapshot barrier: {:?}; database.dump bytes: {}",
                    barrier?,
                    std::fs::metadata(server.join("database.dump"))?.len()
                );
            }
            "stop" => break,
            _ => println!("commands: snapshot, stop"),
        }
    }
    driver.graceful_stop(&ctx, &*process).await?;
    println!("stack exit: {:?}", process.wait().await?);
    Ok(())
}
