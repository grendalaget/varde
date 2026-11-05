use std::collections::VecDeque;
use std::os::windows::io::{AsRawHandle, FromRawHandle, OwnedHandle};
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use executor_api::*;
use tokio::sync::broadcast;
use windows_sys::Win32::Foundation::{
    ERROR_INVALID_PARAMETER, ERROR_NO_MORE_FILES, HANDLE, INVALID_HANDLE_VALUE, WAIT_OBJECT_0,
};
use windows_sys::Win32::System::Diagnostics::ToolHelp::{
    CreateToolhelp32Snapshot, Process32FirstW, Process32NextW, PROCESSENTRY32W, TH32CS_SNAPPROCESS,
};
use windows_sys::Win32::System::Threading::{
    GetExitCodeProcess, OpenProcess, WaitForSingleObject, INFINITE,
    PROCESS_QUERY_LIMITED_INFORMATION, PROCESS_SUSPEND_RESUME, PROCESS_SYNCHRONIZE,
};

#[link(name = "ntdll")]
extern "system" {
    fn NtSuspendProcess(process: HANDLE) -> i32;
    fn NtResumeProcess(process: HANDLE) -> i32;
}

fn open_process(pid: u32, access: u32) -> std::io::Result<OwnedHandle> {
    let handle = unsafe { OpenProcess(access, 0, pid) };
    if handle.is_null() {
        return Err(std::io::Error::last_os_error());
    }
    Ok(unsafe { OwnedHandle::from_raw_handle(handle) })
}

fn children(pid: u32) -> Result<Vec<u32>> {
    let snapshot = unsafe { CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0) };
    if snapshot == INVALID_HANDLE_VALUE {
        return Err(std::io::Error::last_os_error().into());
    }
    let snapshot = unsafe { OwnedHandle::from_raw_handle(snapshot) };
    let mut entry = PROCESSENTRY32W {
        dwSize: std::mem::size_of::<PROCESSENTRY32W>() as u32,
        ..unsafe { std::mem::zeroed() }
    };
    let mut found = unsafe { Process32FirstW(snapshot.as_raw_handle(), &mut entry) };
    let mut children = Vec::new();
    while found != 0 {
        if entry.th32ParentProcessID == pid {
            children.push(entry.th32ProcessID);
        }
        found = unsafe { Process32NextW(snapshot.as_raw_handle(), &mut entry) };
    }
    let error = std::io::Error::last_os_error();
    if error.raw_os_error() != Some(ERROR_NO_MORE_FILES as i32) {
        return Err(error.into());
    }
    Ok(children)
}

pub(super) struct SuspendedTree(Vec<OwnedHandle>);

impl SuspendedTree {
    pub(super) fn suspend(pid: u32) -> Result<Self> {
        let mut tree = Self(Vec::new());
        let mut pending = VecDeque::from([pid]);
        while let Some(next) = pending.pop_front() {
            let handle = match open_process(next, PROCESS_SUSPEND_RESUME) {
                Ok(handle) => handle,
                Err(e)
                    if next != pid && e.raw_os_error() == Some(ERROR_INVALID_PARAMETER as i32) =>
                {
                    continue;
                }
                Err(e) => return Err(e.into()),
            };
            let status = unsafe { NtSuspendProcess(handle.as_raw_handle()) };
            if status < 0 {
                return Err(
                    format!("autotournament: NtSuspendProcess({next}): {status:#x}").into(),
                );
            }
            tree.0.push(handle);
            pending.extend(children(next)?);
        }
        Ok(tree)
    }

    pub(super) fn resume(mut self) -> Result<()> {
        let mut error = None;
        while let Some(handle) = self.0.pop() {
            let status = unsafe { NtResumeProcess(handle.as_raw_handle()) };
            if status < 0 {
                error = Some(format!("autotournament: NtResumeProcess: {status:#x}"));
            }
        }
        match error {
            Some(e) => Err(e.into()),
            None => Ok(()),
        }
    }
}

impl Drop for SuspendedTree {
    fn drop(&mut self) {
        for handle in self.0.iter().rev() {
            unsafe { NtResumeProcess(handle.as_raw_handle()) };
        }
    }
}

fn pg_ctl(postgres: &Path, cwd: &Path, cluster: &Path, port: Option<u16>) -> ProcessSpec {
    let mut spec = super::spec(&super::pg_tool(postgres, "pg_ctl"), cwd, &[]);
    spec.args = match port {
        Some(port) => vec![
            "start".into(),
            "-w".into(),
            "-t".into(),
            "120".into(),
            "-D".into(),
            cluster.as_os_str().into(),
            "-o".into(),
            format!("-h 127.0.0.1 -p {port}").into(),
        ],
        None => vec![
            "stop".into(),
            "-D".into(),
            cluster.as_os_str().into(),
            "-m".into(),
            "fast".into(),
            "-w".into(),
            "-t".into(),
            "60".into(),
        ],
    };
    spec
}

pub(super) struct Postgres {
    launcher: Box<dyn ProcessHandle>,
    postmaster: Arc<OwnedHandle>,
    pid: u32,
    stop: ProcessSpec,
}

impl Postgres {
    pub(super) async fn start(
        executor: &dyn Executor,
        postgres: &Path,
        cwd: &Path,
        cluster: &Path,
        port: u16,
    ) -> Result<Box<dyn ProcessHandle>> {
        let launcher = executor
            .spawn(&pg_ctl(postgres, cwd, cluster, Some(port)))
            .await?;
        let result = async {
            let status = tokio::time::timeout(Duration::from_secs(130), launcher.wait()).await??;
            if !status.success() {
                return Err(format!("pg_ctl start exited: {status:?}").into());
            }
            let pid = tokio::fs::read_to_string(cluster.join("postmaster.pid"))
                .await?
                .lines()
                .next()
                .ok_or("autotournament: empty postmaster.pid")?
                .parse::<u32>()?;
            let handle =
                open_process(pid, PROCESS_SYNCHRONIZE | PROCESS_QUERY_LIMITED_INFORMATION)?;
            Ok::<_, DynError>((pid, handle))
        }
        .await;
        match result {
            Ok((pid, handle)) => Ok(Box::new(Self {
                launcher,
                postmaster: Arc::new(handle),
                pid,
                stop: pg_ctl(postgres, cwd, cluster, None),
            })),
            Err(error) => {
                let _ = launcher.kill().await;
                let output = launcher
                    .output_tail(OUTPUT_RING_LINES)
                    .iter()
                    .map(|line| line.line.as_ref())
                    .collect::<Vec<_>>()
                    .join("\n");
                Err(format!("autotournament: {error}\n{output}").into())
            }
        }
    }
}

#[async_trait]
impl ProcessHandle for Postgres {
    fn pid(&self) -> u32 {
        self.pid
    }

    async fn write_stdin(&self, _: &str) -> Result<()> {
        Err("autotournament: PostgreSQL has no stdin commands".into())
    }

    fn output(&self) -> broadcast::Receiver<OutputLine> {
        self.launcher.output()
    }

    fn output_tail(&self, n: usize) -> Vec<OutputLine> {
        self.launcher.output_tail(n)
    }

    async fn wait(&self) -> Result<ExitStatus> {
        let handle = self.postmaster.clone();
        tokio::task::spawn_blocking(move || {
            if unsafe { WaitForSingleObject(handle.as_raw_handle(), INFINITE) } != WAIT_OBJECT_0 {
                return Err(std::io::Error::last_os_error().into());
            }
            let mut code = 0;
            if unsafe { GetExitCodeProcess(handle.as_raw_handle(), &mut code) } == 0 {
                return Err(std::io::Error::last_os_error().into());
            }
            Ok(ExitStatus {
                code: Some(code as i32),
                signal: None,
            })
        })
        .await?
    }

    async fn terminate(&self) -> Result<()> {
        let mut cmd = tokio::process::Command::new(&self.stop.program);
        cmd.args(&self.stop.args).current_dir(&self.stop.cwd);
        super::command(cmd).await?;
        self.wait().await?;
        Ok(())
    }

    async fn interrupt(&self) -> Result<()> {
        self.terminate().await
    }

    async fn kill(&self) -> Result<()> {
        self.launcher.kill().await
    }

    fn resource_usage(&self) -> Option<ResourceUsage> {
        self.launcher.resource_usage()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pg_ctl_arguments_preserve_spaced_paths() {
        let postgres = Path::new(r"C:\cache dir\postgres");
        let cwd = Path::new(r"C:\server dir");
        let cluster = cwd.join("postgres");
        let start = pg_ctl(postgres, cwd, &cluster, Some(54321));
        assert_eq!(start.program, postgres.join("bin/pg_ctl.exe"));
        assert_eq!(start.cwd, cwd);
        assert_eq!(
            start.args,
            [
                "start",
                "-w",
                "-t",
                "120",
                "-D",
                r"C:\server dir\postgres",
                "-o",
                "-h 127.0.0.1 -p 54321",
            ]
            .map(std::ffi::OsString::from)
        );
        assert!(start.env.is_empty());
        let stop = pg_ctl(postgres, cwd, &cluster, None);
        assert_eq!(
            stop.args,
            [
                "stop",
                "-D",
                r"C:\server dir\postgres",
                "-m",
                "fast",
                "-w",
                "-t",
                "60",
            ]
            .map(std::ffi::OsString::from)
        );
    }

    #[test]
    fn suspend_missing_process_fails() {
        assert!(SuspendedTree::suspend(u32::MAX).is_err());
    }

    #[test]
    fn caddy_root_uses_quoted_forward_slashes() {
        assert_eq!(
            super::super::caddy_public_root(Path::new(r"C:\cache dir\app")),
            "\"C:/cache dir/app/public\""
        );
    }

    async fn progressing(path: &Path, previous: u64) {
        tokio::time::timeout(Duration::from_secs(15), async {
            loop {
                if std::fs::metadata(path).is_ok_and(|m| m.len() > previous) {
                    return;
                }
                tokio::time::sleep(Duration::from_millis(50)).await;
            }
        })
        .await
        .unwrap();
    }

    #[tokio::test]
    async fn suspends_and_resumes_descendants() {
        let tmp = tempfile::Builder::new()
            .prefix("at tree ")
            .tempdir()
            .unwrap();
        let parent_log = tmp.path().join("parent.log");
        let child_log = tmp.path().join("child.log");
        let child_script = tmp.path().join("child.ps1");
        let parent_script = tmp.path().join("parent.ps1");
        std::fs::write(&child_script,
            "while ($true) { [IO.File]::AppendAllText((Join-Path $PSScriptRoot 'child.log'), 'x'); Start-Sleep -Milliseconds 40 }").unwrap();
        std::fs::write(&parent_script,
            "Start-Process -FilePath powershell.exe -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', ('\"' + (Join-Path $PSScriptRoot 'child.ps1') + '\"')) -NoNewWindow\nwhile ($true) { [IO.File]::AppendAllText((Join-Path $PSScriptRoot 'parent.log'), 'x'); Start-Sleep -Milliseconds 40 }").unwrap();
        let mut spec = super::super::spec(
            Path::new("powershell.exe"),
            tmp.path(),
            &["-NoProfile", "-ExecutionPolicy", "Bypass", "-File"],
        );
        spec.args.push(parent_script.into_os_string());
        let process = executor_native::NativeExecutor.spawn(&spec).await.unwrap();
        progressing(&parent_log, 0).await;
        progressing(&child_log, 0).await;
        let tree = SuspendedTree::suspend(process.pid()).unwrap();
        let parent_size = std::fs::metadata(&parent_log).unwrap().len();
        let child_size = std::fs::metadata(&child_log).unwrap().len();
        tokio::time::sleep(Duration::from_millis(300)).await;
        assert_eq!(std::fs::metadata(&parent_log).unwrap().len(), parent_size);
        assert_eq!(std::fs::metadata(&child_log).unwrap().len(), child_size);
        tree.resume().unwrap();
        progressing(&parent_log, parent_size).await;
        progressing(&child_log, child_size).await;
        process.kill().await.unwrap();
        process.wait().await.unwrap();
    }

    async fn stack_children() -> Vec<Arc<dyn ProcessHandle>> {
        let mut children = Vec::new();
        for _ in 0..3 {
            children.push(Arc::from(
                executor_native::NativeExecutor
                    .spawn(&super::super::spec(
                        Path::new("cmd.exe"),
                        Path::new(r"C:\"),
                        &["/c", "ping -t 127.0.0.1 >nul"],
                    ))
                    .await
                    .unwrap(),
            ));
        }
        children
    }

    #[tokio::test]
    async fn stack_kill_resumes_paused_frontends() {
        let stack = super::super::process::Stack::new(stack_children().await);
        assert!(stack.write_stdin("unknown").await.is_err());
        stack.write_stdin("pause").await.unwrap();
        stack.write_stdin("pause").await.unwrap();
        stack.write_stdin("resume").await.unwrap();
        stack.write_stdin("resume").await.unwrap();
        stack.write_stdin("pause").await.unwrap();
        stack.kill().await.unwrap();
        assert_eq!(stack.wait().await.unwrap().code, Some(1));
        assert!(stack.write_stdin("pause").await.is_err());
    }

    #[tokio::test]
    async fn stack_drop_releases_jobs() {
        let children = stack_children().await;
        let handles = children
            .iter()
            .map(|child| open_process(child.pid(), PROCESS_SYNCHRONIZE).unwrap())
            .collect::<Vec<_>>();
        let stack = super::super::process::Stack::new(children);
        stack.write_stdin("pause").await.unwrap();
        drop(stack);
        tokio::task::spawn_blocking(move || {
            for handle in handles {
                assert_eq!(
                    unsafe { WaitForSingleObject(handle.as_raw_handle(), 5000) },
                    WAIT_OBJECT_0
                );
            }
        })
        .await
        .unwrap();
    }
}
