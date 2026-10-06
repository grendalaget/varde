//! Windows probe: does `interrupt()` stop a real Valheim dedicated server
//! gracefully (shutdown save written, exit code 0)?
//!
//! console: `valheim_stop <valheim_dir> <out_dir> <label>`
//! service: `sc create VardeStopProbe binPath= "<exe> service <valheim_dir> <out_dir> <label>"`
//!          then `sc start VardeStopProbe` (runs once as LocalSystem, no console)
//!
//! Writes `<out_dir>/<label>.log` (game output + a final RESULT line).

#[cfg(windows)]
fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.first().map(String::as_str) == Some("service") {
        service::run(args[1..].to_vec());
    } else {
        probe(&args);
    }
}

#[cfg(not(windows))]
fn main() {
    eprintln!("valheim_stop is a Windows-only probe");
}

#[cfg(windows)]
fn probe(args: &[String]) {
    let [dir, out, label] = args else {
        panic!("usage: valheim_stop <valheim_dir> <out_dir> <label>");
    };
    tokio::runtime::Runtime::new().unwrap().block_on(run_probe(
        dir.into(),
        out.into(),
        label.clone(),
    ));
}

#[cfg(windows)]
async fn run_probe(dir: std::path::PathBuf, out: std::path::PathBuf, label: String) {
    use executor_api::{Executor, ProcessSpec};
    use std::ffi::OsString;
    use std::io::Write;
    use std::time::{Duration, Instant};

    std::fs::create_dir_all(&out).unwrap();
    let mut log = std::fs::File::create(out.join(format!("{label}.log"))).unwrap();
    let has_console = !unsafe { windows_sys::Win32::System::Console::GetConsoleWindow() }.is_null();
    writeln!(log, "label={label} agent_has_console={has_console}").unwrap();

    let savedir = out.join(format!("saves-{label}"));
    let args = [
        "-nographics",
        "-batchmode",
        "-name",
        "test",
        "-port",
        "2456",
        "-world",
        "VardeTest",
        "-password",
        "secret123",
        "-public",
        "0",
        "-saveinterval",
        "60",
        "-backups",
        "0",
        "-savedir",
    ];
    let mut args: Vec<OsString> = args.iter().map(OsString::from).collect();
    args.push(savedir.into_os_string());
    let spec = ProcessSpec {
        program: dir.join("valheim_server.exe"),
        args,
        env: vec![
            ("SteamAppId".into(), "892970".into()),
            ("SteamGameId".into(), "892970".into()),
        ],
        cwd: dir,
        stdin: false,
    };
    let proc = executor_native::NativeExecutor.spawn(&spec).await.unwrap();
    let mut rx = proc.output();
    let lines = std::sync::Arc::new(std::sync::Mutex::new(Vec::<(Instant, String)>::new()));
    let (ready_tx, ready_rx) = tokio::sync::oneshot::channel::<()>();
    let collector = {
        let lines = lines.clone();
        let mut log = log.try_clone().unwrap();
        tokio::spawn(async move {
            let mut ready_tx = Some(ready_tx);
            while let Ok(l) = rx.recv().await {
                let _ = writeln!(log, "| {}", l.line);
                if l.line.contains("Game server connected") {
                    if let Some(tx) = ready_tx.take() {
                        let _ = tx.send(());
                    }
                }
                lines
                    .lock()
                    .unwrap()
                    .push((Instant::now(), l.line.to_string()));
            }
        })
    };
    if tokio::time::timeout(Duration::from_secs(300), ready_rx)
        .await
        .is_err()
    {
        writeln!(log, "RESULT timeout waiting for Game server connected").unwrap();
        let _ = proc.kill().await;
        return;
    }
    tokio::time::sleep(Duration::from_secs(5)).await;

    let t0 = Instant::now();
    let interrupt = proc.interrupt().await;
    // same 30 s budget as graceful_stop_and_snapshot in apps/agent/src/exec.rs
    let status = tokio::time::timeout(Duration::from_secs(30), proc.wait()).await;
    let secs = t0.elapsed().as_secs_f64();
    let _ = proc.kill().await;
    tokio::time::sleep(Duration::from_millis(500)).await;
    collector.abort();

    let after: Vec<String> = lines
        .lock()
        .unwrap()
        .iter()
        .filter(|(t, _)| *t >= t0)
        .map(|(_, l)| l.clone())
        .collect();
    let save_done = after.iter().any(|l| l.contains("World save (5/5) done"));
    let shutdown = after.iter().any(|l| l.contains("Net scene destroyed"));
    let exit = match &status {
        Ok(Ok(st)) => format!("{:?}", st.code),
        Ok(Err(e)) => format!("wait error: {e}"),
        Err(_) => "no exit within 30s".into(),
    };
    writeln!(
        log,
        "RESULT agent_has_console={has_console} interrupt={interrupt:?} exit_code={exit} \
         seconds={secs:.2} save_done={save_done} shutdown_sequence={shutdown}"
    )
    .unwrap();
    for l in after.iter().filter(|l| l.contains("World save")) {
        writeln!(log, "SAVE {l}").unwrap();
    }
}

#[cfg(windows)]
mod service {
    use std::ffi::OsString;
    use std::time::Duration;
    use windows_service::service::*;
    use windows_service::service_control_handler::{self, ServiceControlHandlerResult};

    const NAME: &str = "VardeStopProbe";
    static mut ARGS: Vec<String> = Vec::new();

    windows_service::define_windows_service!(ffi_main, service_main);

    pub fn run(args: Vec<String>) {
        unsafe { ARGS = args };
        windows_service::service_dispatcher::start(NAME, ffi_main).unwrap();
    }

    fn status(state: ServiceState) -> ServiceStatus {
        ServiceStatus {
            service_type: ServiceType::OWN_PROCESS,
            current_state: state,
            controls_accepted: ServiceControlAccept::empty(),
            exit_code: ServiceExitCode::Win32(0),
            checkpoint: 0,
            wait_hint: Duration::ZERO,
            process_id: None,
        }
    }

    fn service_main(_: Vec<OsString>) {
        let h = service_control_handler::register(NAME, |_| ServiceControlHandlerResult::NoError)
            .unwrap();
        h.set_service_status(status(ServiceState::Running)).unwrap();
        #[allow(static_mut_refs)]
        super::probe(unsafe { &ARGS });
        h.set_service_status(status(ServiceState::Stopped)).unwrap();
    }
}
