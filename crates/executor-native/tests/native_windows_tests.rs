#![cfg(windows)]

use std::ffi::OsString;
use std::time::Duration;

use executor_api::*;
use executor_native::NativeExecutor;

/// `findstr x` blocks reading stdin and installs no ctrl handler, so a
/// delivered ctrl event exits it with STATUS_CONTROL_C_EXIT while a job
/// kill exits with 1.
fn spec() -> ProcessSpec {
    ProcessSpec {
        program: "findstr.exe".into(),
        args: vec![OsString::from("x")],
        env: vec![],
        cwd: std::env::temp_dir(),
        stdin: true,
    }
}

async fn wait_code(p: &dyn ProcessHandle) -> Option<i32> {
    tokio::time::timeout(Duration::from_secs(10), p.wait())
        .await
        .expect("process did not exit within 10s")
        .expect("wait failed")
        .code
}

#[tokio::test]
async fn interrupt_delivers_ctrl_break() {
    let p = NativeExecutor.spawn(&spec()).await.unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
    p.interrupt().await.unwrap();
    assert_eq!(wait_code(p.as_ref()).await, Some(0xC000013Au32 as i32));
}

#[tokio::test]
async fn kill_terminates_job() {
    let p = NativeExecutor.spawn(&spec()).await.unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
    p.kill().await.unwrap();
    assert_eq!(wait_code(p.as_ref()).await, Some(1));
}
