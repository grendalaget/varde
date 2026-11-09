//! Manual native lifecycle harness: snapshot / stop on stdin. Never prints API secrets.
use game_autotournament::{AutoTournamentDriver, PORT};
use game_driver_api::*;
use std::path::PathBuf;
use std::time::{Duration, Instant};
use tokio::io::{AsyncBufReadExt, BufReader};

fn lifecycle_line(line: &str) -> bool {
    [
        "Received SIGTERM",
        "shutting down",
        "shutdown complete",
        "shutdown finished",
        "database system",
        "recovery",
        "redo starts",
        "redo done",
        "checkpoint",
    ]
    .iter()
    .any(|message| line.contains(message))
}

#[tokio::main]
async fn main() -> Result<()> {
    let root = PathBuf::from(std::env::var("VARDE_AT_CHECK_DIR")?);
    let cache = PathBuf::from(std::env::var("VARDE_AT_CACHE")?);
    let snapshot_hold = std::env::var("VARDE_AT_SNAPSHOT_HOLD_MS")
        .ok()
        .map(|value| value.parse::<u64>())
        .transpose()?
        .unwrap_or(0);
    let provider = runtimes::HttpRuntimes::new(cache);
    let server = root.join("server");
    let store = snapshot_store::Store::open(root.join("snapshots"))?;
    if std::env::var_os("VARDE_AT_RESTORE").is_some() {
        let source = std::env::var_os("VARDE_AT_RESTORE_FROM")
            .map(PathBuf::from)
            .unwrap_or_else(|| root.clone());
        let id = std::fs::read_to_string(source.join("snapshot-id"))?;
        let id = snapshot_store::SnapshotId::parse(&id).ok_or("invalid snapshot id")?;
        snapshot_store::Store::open(source.join("snapshots"))?.restore(
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
    let mut output = process.output();
    for line in process.output_tail(1000) {
        if lifecycle_line(&line.line) {
            println!("lifecycle: {}", line.line);
        }
    }
    let (log_stop, mut stopped) = tokio::sync::oneshot::channel::<()>();
    let logs = tokio::spawn(async move {
        loop {
            let line = tokio::select! {
                line = output.recv() => line,
                _ = &mut stopped => break,
            };
            match line {
                Ok(line) if lifecycle_line(&line.line) => println!("lifecycle: {}", line.line),
                Err(tokio::sync::broadcast::error::RecvError::Closed) => break,
                _ => {}
            }
        }
        while let Ok(line) = output.try_recv() {
            if lifecycle_line(&line.line) {
                println!("lifecycle: {}", line.line);
            }
        }
    });
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
            if lifecycle_line(&line.line) {
                eprintln!("lifecycle: {}", line.line);
            }
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
                    println!("snapshot: frontend paused; holding {snapshot_hold} ms");
                    tokio::time::sleep(Duration::from_millis(snapshot_hold)).await;
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
                println!("snapshot: frontend resumed");
                let snapshot = snapshot?;
                std::fs::write(root.join("snapshot-id"), snapshot.id.to_string())?;
                let manifest = store.manifest(&snapshot.id)?;
                assert!(manifest.files.iter().all(|f| f.path == "database.dump"
                    || f.path == "database.dump.id"
                    || f.path == "database.manifest.json"
                    || f.path == "database-dumps"
                    || f.path.starts_with("database-dumps/")
                    || f.path == "data"
                    || f.path.starts_with("data/")));
                println!(
                    "snapshot: {}; {} files; raw cluster excluded",
                    snapshot.id, snapshot.file_count
                );
                println!(
                    "snapshot barrier: {:?}; database manifest bytes: {}",
                    barrier?,
                    std::fs::metadata(server.join("database.manifest.json"))?.len()
                );
            }
            "stop" => break,
            _ => println!("commands: snapshot, stop"),
        }
    }
    let stopping = Instant::now();
    println!("stop: graceful shutdown requested");
    driver.graceful_stop(&ctx, &*process).await?;
    println!("stack exit: {:?}", process.wait().await?);
    println!("stop: elapsed {} ms", stopping.elapsed().as_millis());
    let _ = log_stop.send(());
    logs.await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::lifecycle_line;

    #[test]
    fn only_lifecycle_messages_are_forwarded() {
        assert!(lifecycle_line("Received SIGTERM"));
        assert!(lifecycle_line("database system was interrupted"));
        assert!(!lifecycle_line(""));
        assert!(!lifecycle_line("[SETUP] enter this code: REDACTED"));
        assert!(!lifecycle_line("API_TOKENS=REDACTED"));
    }
}
