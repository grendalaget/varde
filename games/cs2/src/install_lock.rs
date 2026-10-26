use std::fs::{File, OpenOptions};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use executor_api::*;

pub fn open(dir: &Path) -> Result<File> {
    Ok(OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(dir.join(".varde-cs2.lock"))?)
}

pub async fn shared(dir: PathBuf) -> Result<File> {
    tokio::task::spawn_blocking(move || {
        let lock = open(&dir)?;
        fs2::FileExt::lock_shared(&lock)?;
        Ok(lock)
    })
    .await?
}

pub async fn spawn(
    executor: &dyn Executor,
    spec: &ProcessSpec,
    lock: Option<File>,
    config: Option<crate::shared_config::SharedConfig>,
) -> Result<Box<dyn ProcessHandle>> {
    let process: Arc<dyn ProcessHandle> = Arc::from(executor.spawn(spec).await?);
    let lease = Arc::new(Mutex::new(lock));
    let watched = process.clone();
    let held = lease.clone();
    let config = Arc::new(Mutex::new(config));
    let cleanup = config.clone();
    tokio::spawn(async move {
        let result = watched.wait().await;
        cleanup.lock().unwrap().take();
        if let Err(error) = result {
            tracing::warn!(%error, "CS2 exit unknown; retaining install lock");
            std::future::pending::<()>().await;
        }
        held.lock().unwrap().take();
    });
    Ok(Box::new(LockedProcess {
        process,
        lease,
        config,
    }))
}

struct LockedProcess {
    process: Arc<dyn ProcessHandle>,
    lease: Arc<Mutex<Option<File>>>,
    config: Arc<Mutex<Option<crate::shared_config::SharedConfig>>>,
}

impl Drop for LockedProcess {
    fn drop(&mut self) {
        self.config.lock().unwrap().take();
    }
}

#[async_trait]
impl ProcessHandle for LockedProcess {
    fn pid(&self) -> u32 {
        self.process.pid()
    }
    async fn write_stdin(&self, line: &str) -> Result<()> {
        self.process.write_stdin(line).await
    }
    fn output(&self) -> tokio::sync::broadcast::Receiver<OutputLine> {
        self.process.output()
    }
    fn output_tail(&self, n: usize) -> Vec<OutputLine> {
        self.process.output_tail(n)
    }
    async fn wait(&self) -> Result<ExitStatus> {
        let status = self.process.wait().await;
        self.config.lock().unwrap().take();
        let status = status?;
        self.lease.lock().unwrap().take();
        Ok(status)
    }
    async fn terminate(&self) -> Result<()> {
        self.process.terminate().await
    }
    async fn interrupt(&self) -> Result<()> {
        self.process.interrupt().await
    }
    async fn kill(&self) -> Result<()> {
        self.process.kill().await
    }
    fn resource_usage(&self) -> Option<ResourceUsage> {
        self.process.resource_usage()
    }
}
