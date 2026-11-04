use super::*;
use serde_json::json;

fn config() -> serde_json::Value {
    json!({"public_url":"https://at.example.org", "licence":"noncommercial"})
}

#[test]
fn private_ports_are_distinct_and_reserved_until_released() {
    let (api, pg) = reserve_ports(0, 0).unwrap();
    let api_port = api.local_addr().unwrap().port();
    let pg_port = pg.local_addr().unwrap().port();
    assert_ne!(api_port, pg_port);
    assert!(reserve_ports(api_port, 0).is_err());
    assert!(reserve_ports(0, pg_port).is_err());
    drop(api);
    let api = std::net::TcpListener::bind(("127.0.0.1", api_port)).unwrap();
    assert!(reserve_ports(0, pg_port).is_err());
    drop(pg);
    let pg = std::net::TcpListener::bind(("127.0.0.1", pg_port)).unwrap();
    assert!(reserve_ports(
        api.local_addr().unwrap().port(),
        pg.local_addr().unwrap().port()
    )
    .is_err());
}

#[test]
fn validates_explicit_licence_origin_and_token() {
    let driver = AutoTournamentDriver;
    assert!(driver.validate(&config()).is_ok());
    assert!(driver.validate(&json!({})).is_err());
    for (key, value) in [
        ("licence", json!(null)),
        ("licence", json!("")),
        ("licence", json!("other")),
        ("public_url", json!("")),
        ("public_url", json!("ftp://at.example.org")),
        ("public_url", json!("http://at.example.org")),
        ("public_url", json!("http://localhost.example.org")),
        ("public_url", json!("http://192.168.1.2")),
        ("public_url", json!("https://at.example.org/prefix")),
        ("public_url", json!("https://at.example.org/?x=1")),
        ("public_url", json!("https://user@at.example.org")),
        ("api_token", json!("short")),
        ("api_token", json!("abcdefghijklmnop,other")),
        ("steam_web_api_key", json!(123)),
    ] {
        let mut cfg = config();
        cfg[key] = value;
        assert!(driver.validate(&cfg).is_err(), "{key}");
    }
    let mut cfg = config();
    cfg["licence"] = json!("commercial");
    cfg["api_token"] = json!("abcdefghijklmnop");
    assert!(driver.validate(&cfg).is_ok());
    for url in [
        "http://localhost:3069",
        "http://127.0.0.1:3069",
        "http://127.1.2.3",
        "http://[::1]:3069",
    ] {
        cfg["public_url"] = json!(url);
        assert!(driver.validate(&cfg).is_ok(), "{url}");
    }
}

#[test]
fn setup_code_from_boot_log() {
    let driver = AutoTournamentDriver;
    let code = "0123-4567-89AB-CDEF-GHJK-MNPQ-RSTV";
    assert_eq!(driver.join_code_from_log(&format!("[SETUP] No admin account exists yet. Open https://at.example.org/setup and enter this code: {code}\u{1b}[0m")),Some(code.into()));
    for line in [
        "",
        "unrelated boot log",
        "enter this code: 0123-4567",
        "enter this code: O123-4567-89AB-CDEF-GHJK-MNPQ-RSTV",
    ] {
        assert_eq!(driver.join_code_from_log(line), None);
    }
}

#[tokio::test]
async fn restored_dump_is_authoritative_over_a_ready_cluster() {
    let tmp = tempfile::tempdir().unwrap();
    let cluster = tmp.path().join("postgres");
    let dump = tmp.path().join("database.dump");
    assert!(dump_identity(dump.clone()).await.unwrap().is_empty());
    assert!(needs_restore(&cluster, &[]));
    std::fs::create_dir(&cluster).unwrap();
    std::fs::write(cluster.join(".varde-ready"), "ok").unwrap();
    assert!(!needs_restore(&cluster, &[]));
    let partial = tmp.path().join("database.dump.partial");
    std::fs::write(&partial, "identical dump").unwrap();
    let first = publish_dump(tmp.path()).await.unwrap();
    assert_eq!(dump_identity(dump.clone()).await.unwrap(), first);
    assert!(needs_restore(&cluster, &first));
    remember_dump(&cluster, &first).await.unwrap();
    assert!(!needs_restore(&cluster, &first));
    std::fs::write(cluster.join("committed-after-dump"), "newer state").unwrap();
    std::fs::write(cluster.join(".varde-dirty"), "legacy dirty marker").unwrap();
    assert!(!needs_restore(&cluster, &first));
    // A final dump after frontend shutdown has its own identity, even for identical bytes.
    std::fs::write(&partial, "identical dump").unwrap();
    let second = publish_dump(tmp.path()).await.unwrap();
    assert_ne!(first, second);
    assert!(needs_restore(&cluster, &second));
    remember_dump(&cluster, &second).await.unwrap();
    assert!(!needs_restore(&cluster, &second));
    // Even an identical restored dump must roll back any post-snapshot DB writes.
    std::fs::write(tmp.path().join("database.dump.id"), &first).unwrap();
    let restored = dump_identity(dump.clone()).await.unwrap();
    assert!(needs_restore(&cluster, &restored));
    std::fs::write(cluster.join(".varde-dump"), "invalid").unwrap();
    assert!(needs_restore(&cluster, &restored));
    assert!(!needs_restore(&cluster, &[]));
    assert!(dump_identity(cluster.clone()).await.is_err());
}

#[tokio::test]
async fn dump_identity_handles_legacy_missing_and_corrupt_sidecars() {
    let tmp = tempfile::tempdir().unwrap();
    let dump = tmp.path().join("database.dump");
    std::fs::write(&dump, "legacy dump").unwrap();
    assert!(!dump_identity(dump.clone()).await.unwrap().is_empty());
    let id = tmp.path().join("database.dump.id");
    std::fs::write(&id, "invalid").unwrap();
    assert!(dump_identity(dump.clone()).await.is_err());
    std::fs::write(tmp.path().join("database.dump.partial"), "new dump").unwrap();
    publish_dump(tmp.path()).await.unwrap();
    std::fs::write(&dump, "interrupted publication").unwrap();
    assert!(dump_identity(dump).await.is_err());
}

#[tokio::test]
async fn health_requires_the_api_response_and_uses_loopback_host() {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    for (status, body, healthy) in [
        ("200 OK", r#"{"status":"ok"}"#, true),
        ("200 OK", "", false),
        ("200 OK", r#"{"status":"error"}"#, false),
        ("503 Unavailable", r#"{"status":"ok"}"#, false),
    ] {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        let server = tokio::spawn(async move {
            let (mut conn, _) = listener.accept().await.unwrap();
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                request.push(conn.read_u8().await.unwrap());
            }
            assert!(String::from_utf8(request)
                .unwrap()
                .contains(&format!("host: 127.0.0.1:{port}")));
            conn.write_all(
                format!(
                    "HTTP/1.1 {status}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                    body.len()
                )
                .as_bytes(),
            )
            .await
            .unwrap();
        });
        assert_eq!(health(port.into()).await.unwrap_or(false), healthy);
        server.await.unwrap();
    }
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    drop(listener);
    assert!(health(port.into()).await.is_err());
}

#[test]
fn snapshot_patterns_include_deep_data_not_cluster_or_credentials() {
    let patterns = AutoTournamentDriver.persistent_paths(&json!({}));
    for path in [
        "database.dump",
        "database.dump.id",
        "data/varde-api-token",
        "data/modules/cs2/module.json",
        "data/demos/deep/demo.dem",
    ] {
        assert!(patterns.iter().any(|p| p.matches(path, true)), "{path}");
    }
    for path in [
        "postgres/PG_VERSION",
        "postgres/base/1/2",
        "settings.json",
        "postgres-password",
        "database.dump.partial",
        "database.dump.id.partial",
    ] {
        assert!(!patterns.iter().any(|p| p.matches(path, true)), "{path}");
    }
}

#[tokio::test]
async fn configure_persists_secrets_maps_ports_and_constructs_process() {
    let tmp = tempfile::tempdir().unwrap();
    let dir = tmp.path();
    let provider = runtimes::HttpRuntimes::new(dir.join("cache"));
    let deployment = DeploymentSpec::parse(&json!({}));
    let mut cfg = config();
    cfg["steam_web_api_key"] = json!("test-steam-key");
    let ports = [PortBinding {
        service_port: PORT,
        local_port: 23456,
        protocol: GameProtocol::Tcp,
    }];
    let ctx = DriverContext {
        server_id: "test",
        server_dir: dir,
        deployment_dir: dir,
        runtimes: &provider,
        deployment: &deployment,
        config: &cfg,
        ports: &ports,
        memory_mb: 2048,
        addons: &[],
    };
    std::fs::create_dir(dir.join("app")).unwrap();
    std::fs::write(
        dir.join("app/Caddyfile"),
        "{\n\tservers {}\n}\n:3069 {\nreverse_proxy /health localhost:3000\nroot * /app/public\n}",
    )
    .unwrap();
    let install = Installation {
        node: dir.join("node"),
        postgres: dir.join("pg"),
        caddy: dir.join("caddy"),
        app: dir.join("app"),
    };
    std::fs::write(
        dir.join("installation.json"),
        serde_json::to_vec(&install).unwrap(),
    )
    .unwrap();
    let driver = AutoTournamentDriver;
    driver.configure(&ctx).await.unwrap();
    let token = std::fs::read_to_string(dir.join("data/varde-api-token")).unwrap();
    assert!(token.len() >= 16);
    let password = std::fs::read_to_string(dir.join("postgres-password")).unwrap();
    driver.configure(&ctx).await.unwrap();
    assert_eq!(
        token,
        std::fs::read_to_string(dir.join("data/varde-api-token")).unwrap()
    );
    assert_eq!(
        password,
        std::fs::read_to_string(dir.join("postgres-password")).unwrap()
    );
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        assert_eq!(
            std::fs::metadata(dir.join("data/varde-api-token"))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o600
        );
    }
    let caddy = std::fs::read_to_string(dir.join("Caddyfile")).unwrap();
    assert!(caddy.contains("admin off"));
    assert!(caddy.contains("http://:23456 {\n\tbind 127.0.0.1"));
    assert!(caddy.contains(&format!("127.0.0.1:{}", settings(&ctx).unwrap().api_port)));
    let spec = driver.process_spec(&ctx).unwrap();
    assert_eq!(spec.program, install.node);
    let env: std::collections::HashMap<_, _> = spec.env.into_iter().collect();
    assert_eq!(
        env.get(&OsString::from("API_TOKENS")),
        Some(&OsString::from(&token))
    );
    assert_eq!(
        env.get(&OsString::from("STEAM_API_KEY")),
        Some(&OsString::from("test-steam-key"))
    );
    assert_eq!(
        env.get(&OsString::from("DATA_DIR")),
        Some(&dir.join("data").into_os_string())
    );
    assert_eq!(
        env.get(&OsString::from("AT_ACCEPT_LICENSE")),
        Some(&OsString::from("noncommercial"))
    );
    assert_eq!(
        env.get(&OsString::from("FRONTEND_BASE_URL")),
        Some(&OsString::from("https://at.example.org"))
    );
    let mut configured = cfg.clone();
    configured["api_token"] = json!("configured-token-123456");
    let ctx = DriverContext {
        config: &configured,
        ..ctx
    };
    driver.configure(&ctx).await.unwrap();
    assert_eq!(
        std::fs::read_to_string(dir.join("data/varde-api-token")).unwrap(),
        "configured-token-123456"
    );
}

#[cfg(target_os = "linux")]
mod supervision {
    use super::*;

    async fn children() -> Vec<Arc<dyn ProcessHandle>> {
        let mut children = Vec::new();
        for _ in 0..3 {
            let child = executor_native::NativeExecutor
                .spawn(&spec(Path::new("/bin/sleep"), Path::new("/"), &["60"]))
                .await
                .unwrap();
            children.push(Arc::from(child));
        }
        children
    }

    async fn exited(children: &[Arc<dyn ProcessHandle>]) {
        for child in children {
            tokio::time::timeout(Duration::from_secs(5), child.wait())
                .await
                .unwrap()
                .unwrap();
        }
    }

    #[tokio::test]
    async fn clean_stop_pause_resume_and_unknown_command() {
        let children = children().await;
        let stack = process::Stack::new(children.clone());
        assert!(stack.write_stdin("unknown").await.is_err());
        stack.write_stdin("pause").await.unwrap();
        stack.write_stdin("resume").await.unwrap();
        stack.write_stdin("frontend-stop").await.unwrap();
        assert!(stack.write_stdin("pause").await.is_err());
        assert!(
            tokio::time::timeout(Duration::from_millis(30), children[0].wait())
                .await
                .is_err()
        );
        stack.terminate().await.unwrap();
        assert_eq!(stack.wait().await.unwrap().code, Some(0));
        exited(&children).await;
    }

    #[tokio::test]
    async fn child_failure_stops_entire_stack() {
        let children = children().await;
        let stack = process::Stack::new(children.clone());
        children[1].kill().await.unwrap();
        let status = tokio::time::timeout(Duration::from_secs(5), stack.wait())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(status.code, Some(1));
        exited(&children).await;
    }

    #[tokio::test]
    async fn dropping_handle_leaves_no_children() {
        let children = children().await;
        let stack = process::Stack::new(children.clone());
        drop(stack);
        exited(&children).await;
    }
}
