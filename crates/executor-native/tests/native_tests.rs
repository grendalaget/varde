#![cfg(unix)]

use std::ffi::OsString;
use std::path::PathBuf;
use std::time::Duration;

use executor_api::*;
use executor_native::NativeExecutor;

fn spec(program: &str, args: &[&str]) -> ProcessSpec {
    ProcessSpec {
        program: PathBuf::from(program),
        args: args.iter().map(OsString::from).collect(),
        env: vec![],
        cwd: std::env::temp_dir(),
        stdin: true,
    }
}

fn process_alive(pid: u32) -> bool {
    PathBuf::from(format!("/proc/{pid}")).exists()
}

fn children_of(ppid: u32) -> Vec<u32> {
    let mut out = vec![];
    for e in std::fs::read_dir("/proc").unwrap() {
        let e = e.unwrap();
        let Ok(pid) = e.file_name().to_string_lossy().parse::<u32>() else {
            continue;
        };
        if let Ok(stat) = std::fs::read_to_string(format!("/proc/{pid}/stat")) {
            if let Some(rest) = stat.rsplit(')').next() {
                let fields: Vec<&str> = rest.split_whitespace().collect();
                if fields.get(1).and_then(|s| s.parse::<u32>().ok()) == Some(ppid) {
                    out.push(pid);
                }
            }
        }
    }
    out
}

#[tokio::test]
async fn kill_takes_grandchildren() {
    let ex = NativeExecutor;
    // shell forks a long-lived grandchild then waits
    let h = ex
        .spawn(&spec("/bin/sh", &["-c", "sleep 300 & sleep 300"]))
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
    let grandkids = children_of(h.pid());
    assert!(!grandkids.is_empty(), "expected grandchild processes");
    h.kill().await.unwrap();
    h.wait().await.unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
    for g in &grandkids {
        assert!(!process_alive(*g), "grandchild {g} survived group kill");
    }
}

#[tokio::test]
async fn stdin_round_trip() {
    let ex = NativeExecutor;
    let h = ex.spawn(&spec("/bin/cat", &[])).await.unwrap();
    let mut rx = h.output();
    h.write_stdin("hello executor").await.unwrap();
    let line = tokio::time::timeout(Duration::from_secs(3), rx.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(&*line.line, "hello executor");
    h.kill().await.unwrap();
}

#[tokio::test]
async fn output_capture_and_ring() {
    let ex = NativeExecutor;
    let h = ex
        .spawn(&spec(
            "/bin/sh",
            &["-c", "echo one; echo two; echo err >&2"],
        ))
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
    h.wait().await.unwrap();
    let tail = h.output_tail(10);
    let lines: Vec<String> = tail.iter().map(|l| l.line.to_string()).collect();
    assert!(lines.contains(&"one".to_string()));
    assert!(lines.contains(&"two".to_string()));
    assert!(tail
        .iter()
        .any(|l| l.stream == OutputStream::Stderr && &*l.line == "err"));
}

#[tokio::test]
async fn terminate_then_wait() {
    let ex = NativeExecutor;
    let h = ex.spawn(&spec("/bin/sleep", &["300"])).await.unwrap();
    h.terminate().await.unwrap();
    let st = tokio::time::timeout(Duration::from_secs(3), h.wait())
        .await
        .unwrap()
        .unwrap();
    assert!(!st.success(), "SIGTERM should be a signal exit: {st:?}");
}
