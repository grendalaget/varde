//! Gated integration test (VARDE_NET_TESTS=1 and VARDE_VALHEIM_TEST=1):
//! installs the dedicated server through steamcmd (~1-2 GB) and starts it
//! until the log reports "Game server connected". VARDE_VALHEIM_ARTIFACTS
//! can retain/reuse the installation and saves for inspection. With
//! VARDE_VALHEIM_CROSSPLAY=1 it waits for and reports a join code.

#![cfg(target_os = "linux")]

use std::path::PathBuf;
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
    let (root, _temporary) = if let Some(path) = std::env::var_os("VARDE_VALHEIM_ARTIFACTS") {
        (PathBuf::from(path), None)
    } else {
        let tmp = tempfile::tempdir().unwrap();
        (tmp.path().to_path_buf(), Some(tmp))
    };
    std::fs::create_dir_all(&root).unwrap();
    let server_dir = root.join("server");
    let dep_dir = root.join("deployment");
    std::fs::create_dir_all(&server_dir).unwrap();
    std::fs::create_dir_all(&dep_dir).unwrap();
    let rt = runtimes::HttpRuntimes::new(root.join("runtimes"));
    let crossplay = std::env::var("VARDE_VALHEIM_CROSSPLAY")
        .map(|value| value == "1")
        .unwrap_or(false);
    let cfg = serde_json::json!({
        "server_name": "Varde Test", "world_name": "Varde Test",
        "password": "hunter22", "modifiers": "hard", "save_interval_s": 60,
        "crossplay": crossplay,
    });
    let d = ValheimDriver::new();
    d.validate(&cfg).unwrap();
    let bindings = vec![];
    let ctx = DriverContext {
        server_id: "valheim-crossplay-test",
        server_dir: &server_dir,
        deployment_dir: &dep_dir,
        runtimes: &rt,
        deployment: &DeploymentSpec::parse(&serde_json::json!({})),
        config: &cfg,
        ports: &bindings,
        memory_mb: 2048,
    };
    d.prepare(&ctx).await.expect("steamcmd install");
    let binary = dep_dir.join("server/valheim_server.x86_64");
    let ldd = tokio::process::Command::new("ldd")
        .arg(&binary)
        .env("LD_LIBRARY_PATH", dep_dir.join("server/linux64"))
        .output()
        .await
        .expect("ldd is required for the real install check");
    let ldd_text = format!(
        "{}\n{}",
        String::from_utf8_lossy(&ldd.stdout),
        String::from_utf8_lossy(&ldd.stderr)
    );
    println!("ldd valheim_server.x86_64:\n{ldd_text}");
    for library in ["libatomic.so", "libpulse.so"] {
        println!(
            "ldd direct dependency {library}: {}",
            ldd_text.contains(library)
        );
    }
    assert!(ldd.status.success(), "ldd failed: {ldd_text}");

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
    if !listening {
        for line in proc.output_tail(1000).iter().filter(|line| {
            let line = line.line.to_lowercase();
            line.contains("preset")
                || line.contains("world save")
                || line.contains("world saved")
                || line.contains("backup")
                || line.contains("error")
                || line.contains("failed")
        }) {
            println!("VALHEIM LOG [{}] {}", line.at_unix_ms, line.line);
        }
    }
    assert!(listening, "server never reported listening");
    let runtime_maps =
        std::fs::read_to_string(format!("/proc/{}/maps", proc.pid())).unwrap_or_default();
    for library in ["libatomic.so.1", "libpulse.so.0"] {
        println!(
            "Valheim runtime maps contain {library}: {}",
            runtime_maps.contains(library)
        );
    }
    let connected_ms = proc
        .output_tail(200)
        .iter()
        .rev()
        .find(|line| ValheimDriver::is_listening(&line.line))
        .map(|line| line.at_unix_ms)
        .unwrap_or_else(now_ms);
    println!("server reported Game server connected at {connected_ms}");
    if crossplay {
        if let Some(line) = proc
            .output_tail(200)
            .iter()
            .find(|line| line.at_unix_ms == connected_ms)
        {
            println!("VALHEIM LOG [{}] {}", line.at_unix_ms, line.line);
        }
        let join_deadline = Instant::now() + Duration::from_secs(180);
        let mut join_code = None;
        while Instant::now() < join_deadline && join_code.is_none() {
            for line in proc.output_tail(1000).iter() {
                if line.at_unix_ms < connected_ms {
                    continue;
                }
                if let Some(code) = d.join_code_from_log(&line.line) {
                    if code.len() == 6 && code.bytes().all(|byte| byte.is_ascii_digit()) {
                        join_code = Some((code, line.at_unix_ms, line.line.to_string()));
                        break;
                    }
                }
            }
            if join_code.is_none() {
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
        }
        let Some((code, at, line)) = join_code else {
            let _ = d.graceful_stop(&ctx, &*proc).await;
            let _ = tokio::time::timeout(Duration::from_secs(120), proc.wait()).await;
            panic!("no six-digit crossplay join code within 180 seconds");
        };
        println!("CROSSPLAY JOIN CODE LOG [{at}] {line}");
        println!("crossplay join code: {code}");
        d.graceful_stop(&ctx, &*proc).await.unwrap();
        let exit = tokio::time::timeout(Duration::from_secs(120), proc.wait())
            .await
            .expect("Valheim did not exit after SIGINT")
            .expect("waiting for Valheim exit failed");
        println!("SIGINT exit status: {exit:?}");
        assert_eq!(exit.code, Some(0), "Valheim did not exit with status 0");
        assert_eq!(exit.signal, None, "Valheim was signaled instead of exiting");
        return;
    }
    let save_deadline = Instant::now() + Duration::from_secs(120);
    let mut first_save = None;
    while Instant::now() < save_deadline {
        if let Some(line) = proc.output_tail(1000).iter().find(|line| {
            line.at_unix_ms >= connected_ms && ValheimDriver::is_world_saved(&line.line)
        }) {
            first_save = Some(line.at_unix_ms);
            break;
        }
        tokio::time::sleep(Duration::from_secs(1)).await;
    }
    if first_save.is_none() {
        for line in proc.output_tail(1000).iter().filter(|line| {
            let line = line.line.to_lowercase();
            line.contains("preset")
                || line.contains("world save")
                || line.contains("world saved")
                || line.contains("backup")
                || line.contains("error")
                || line.contains("failed")
        }) {
            println!("VALHEIM LOG [{}] {}", line.at_unix_ms, line.line);
        }
    }
    assert!(
        first_save.is_some(),
        "no completed world-save line within 120 seconds"
    );
    println!(
        "first completed World save after {:.3}s from Game server connected",
        (first_save.unwrap() - connected_ms) as f64 / 1000.0
    );

    let log_lines = proc.output_tail(1000);
    for line in log_lines.iter().filter(|line| {
        let line = line.line.to_lowercase();
        line.contains("game server connected")
            || line.contains("preset")
            || line.contains("world save")
            || line.contains("world saved")
            || line.contains("backup")
            || line.contains("shutdown")
            || line.contains("stopping")
    }) {
        println!("VALHEIM LOG [{}] {}", line.at_unix_ms, line.line);
    }
    if !log_lines
        .iter()
        .any(|line| line.line.to_lowercase().contains("preset"))
    {
        println!("no preset-specific acceptance line; process started with -preset Hard");
    }

    let stop_started_ms = now_ms();
    let stop_started = Instant::now();
    d.graceful_stop(&ctx, &*proc).await.unwrap();
    let exit = tokio::time::timeout(Duration::from_secs(120), proc.wait())
        .await
        .expect("Valheim did not exit after SIGINT")
        .expect("waiting for Valheim exit failed");
    let stop_elapsed = stop_started.elapsed();
    println!(
        "SIGINT-to-exit: {:.3}s; status: {exit:?}",
        stop_elapsed.as_secs_f64()
    );
    tokio::time::sleep(Duration::from_millis(250)).await;

    let shutdown_lines = proc.output_tail(1000);
    let shutdown_save = shutdown_lines.iter().any(|line| {
        line.at_unix_ms >= stop_started_ms && ValheimDriver::is_world_saved(&line.line)
    });
    for line in shutdown_lines.iter().filter(|line| {
        line.at_unix_ms >= stop_started_ms
            && (line.line.contains("World save")
                || line.line.contains("World saved")
                || line.line.to_lowercase().contains("shutdown")
                || line.line.to_lowercase().contains("stopping"))
    }) {
        println!("SHUTDOWN LOG [{}] {}", line.at_unix_ms, line.line);
    }
    assert!(
        shutdown_save,
        "no completed world save appeared during SIGINT shutdown"
    );

    let worlds_local = server_dir.join("saves/worlds_local");
    let world_dir = worlds_local.join("Varde Test");
    for path in [
        server_dir.join("saves"),
        worlds_local.clone(),
        world_dir.clone(),
    ] {
        let listing = tokio::process::Command::new("ls")
            .arg("-la")
            .arg(&path)
            .output()
            .await
            .unwrap();
        println!(
            "ls -la {} (status {}):\n{}{}",
            path.display(),
            listing.status,
            String::from_utf8_lossy(&listing.stdout),
            String::from_utf8_lossy(&listing.stderr)
        );
    }

    let world_files: Vec<_> = std::fs::read_dir(&world_dir)
        .unwrap()
        .filter_map(|entry| entry.ok())
        .map(|entry| entry.file_name().to_string_lossy().into_owned())
        .collect();
    for extension in [".db2", ".fwl2", ".chunks", ".ok", ".chunk"] {
        assert!(
            world_files.iter().any(|name| name.ends_with(extension)),
            "missing {extension} save file under {}: {world_files:?}",
            world_dir.display()
        );
    }
    let patterns = d.persistent_paths(&cfg);
    for name in world_files
        .iter()
        .filter(|name| !name.contains("_backup") && !name.ends_with(".old"))
    {
        let path = format!("saves/worlds_local/Varde Test/{name}");
        assert!(
            patterns.iter().any(|pattern| pattern.matches(&path, true)),
            "persistent_paths does not include {path}"
        );
    }
    let backups = find_backup_files(&worlds_local);
    assert!(backups.is_empty(), "Valheim wrote backups: {backups:?}");
}

fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64
}

fn find_backup_files(path: &std::path::Path) -> Vec<String> {
    let Ok(entries) = std::fs::read_dir(path) else {
        return Vec::new();
    };
    entries
        .filter_map(|entry| entry.ok())
        .flat_map(|entry| {
            let path = entry.path();
            if path.is_dir() {
                find_backup_files(&path)
            } else if path
                .file_name()
                .is_some_and(|name| name.to_string_lossy().contains("_backup"))
            {
                vec![path.display().to_string()]
            } else {
                Vec::new()
            }
        })
        .collect()
}
