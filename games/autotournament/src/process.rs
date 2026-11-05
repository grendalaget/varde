use std::collections::VecDeque;
use std::sync::{
    atomic::{AtomicBool, Ordering},
    Arc, Mutex,
};
use std::time::Duration;

use async_trait::async_trait;
use executor_api::*;
use tokio::sync::{broadcast, watch};

pub(super) struct Stack {
    inner: Arc<Inner>,
    #[cfg(windows)]
    tasks: Vec<tokio::task::JoinHandle<()>>,
}
struct Inner {
    children: Vec<Arc<dyn ProcessHandle>>,
    exited: Vec<AtomicBool>,
    stopping: AtomicBool,
    frontend_stopped: AtomicBool,
    shutdown: tokio::sync::Mutex<()>,
    output: broadcast::Sender<OutputLine>,
    ring: Mutex<VecDeque<OutputLine>>,
    exit: watch::Sender<Option<ExitStatus>>,
    #[cfg(windows)]
    suspended: Mutex<[Option<super::windows::SuspendedTree>; 2]>,
}
impl Inner {
    fn emit(&self, line: OutputLine) {
        let mut ring = self.ring.lock().unwrap();
        if ring.len() == OUTPUT_RING_LINES {
            ring.pop_front();
        }
        ring.push_back(line.clone());
        let _ = self.output.send(line);
    }
    async fn stop_one(&self, index: usize, hard: bool) -> Result<()> {
        #[cfg(windows)]
        if index > 0 {
            if let Some(tree) = self.suspended.lock().unwrap()[index - 1].take() {
                let _ = tree.resume();
            }
        }
        if !self.exited[index].load(Ordering::SeqCst) {
            let child = &self.children[index];
            #[cfg(unix)]
            unsafe {
                libc::kill(-(child.pid() as i32), libc::SIGCONT);
            }
            if hard {
                child.kill().await?;
            } else {
                child.terminate().await?;
            }
            match tokio::time::timeout(Duration::from_secs(15), child.wait()).await {
                Ok(result) => {
                    result?;
                }
                Err(_) => {
                    child.kill().await?;
                    child.wait().await?;
                }
            }
            self.exited[index].store(true, Ordering::SeqCst);
        }
        Ok(())
    }
    async fn stop_all(&self, clean: bool) -> Result<()> {
        let _guard = self.shutdown.lock().await;
        if self.exit.borrow().is_some() {
            return Ok(());
        }
        self.stopping.store(true, Ordering::SeqCst);
        let mut error = None;
        for index in [2, 1, 0] {
            if let Err(e) = self.stop_one(index, !clean).await {
                error = Some(e);
            }
        }
        self.exit.send_replace(Some(ExitStatus {
            code: Some(if clean && error.is_none() { 0 } else { 1 }),
            signal: None,
        }));
        match error {
            Some(e) => Err(e),
            None => Ok(()),
        }
    }
}
impl Stack {
    pub(super) fn new(children: Vec<Arc<dyn ProcessHandle>>) -> Self {
        let (output, _) = broadcast::channel(512);
        let (exit, _) = watch::channel(None);
        let inner = Arc::new(Inner {
            exited: children.iter().map(|_| AtomicBool::new(false)).collect(),
            children,
            stopping: AtomicBool::new(false),
            frontend_stopped: AtomicBool::new(false),
            shutdown: tokio::sync::Mutex::new(()),
            output,
            ring: Mutex::new(VecDeque::new()),
            exit,
            #[cfg(windows)]
            suspended: Mutex::new([None, None]),
        });
        #[cfg(windows)]
        let mut tasks = Vec::new();
        for index in 0..inner.children.len() {
            let child = inner.children[index].clone();
            let mut output = child.output();
            for line in child.output_tail(OUTPUT_RING_LINES) {
                inner.emit(line);
            }
            let state = inner.clone();
            let task = tokio::spawn(async move {
                let mut exit = state.exit.subscribe();
                loop {
                    if exit.borrow().is_some() {
                        break;
                    }
                    let line = tokio::select! {
                        line = output.recv() => line,
                        _ = exit.changed() => break,
                    };
                    match line {
                        Ok(line) => state.emit(line),
                        Err(broadcast::error::RecvError::Lagged(_)) => continue,
                        Err(_) => break,
                    }
                }
            });
            #[cfg(windows)]
            tasks.push(task);
            #[cfg(not(windows))]
            drop(task);
            let state = inner.clone();
            let task = tokio::spawn(async move {
                let _ = child.wait().await;
                state.exited[index].store(true, Ordering::SeqCst);
                if !state.stopping.load(Ordering::SeqCst)
                    && (index == 0 || !state.frontend_stopped.load(Ordering::SeqCst))
                {
                    let _ = state.stop_all(false).await;
                }
            });
            #[cfg(windows)]
            tasks.push(task);
            #[cfg(not(windows))]
            drop(task);
        }
        Self {
            inner,
            #[cfg(windows)]
            tasks,
        }
    }
}
impl Drop for Stack {
    fn drop(&mut self) {
        #[cfg(windows)]
        for task in &self.tasks {
            task.abort();
        }
        #[cfg(unix)]
        for (index, child) in self.inner.children.iter().enumerate() {
            if !self.inner.exited[index].load(Ordering::SeqCst) {
                unsafe {
                    libc::kill(-(child.pid() as i32), libc::SIGKILL);
                }
            }
        }
    }
}
#[async_trait]
impl ProcessHandle for Stack {
    fn pid(&self) -> u32 {
        self.inner.children[1].pid()
    }
    async fn write_stdin(&self, line: &str) -> Result<()> {
        match line {
            "pause" | "resume" => {
                #[cfg(unix)]
                {
                    for index in [1, 2] {
                        if self.inner.exited[index].load(Ordering::SeqCst) {
                            return Err("autotournament frontend exited".into());
                        }
                        let signal = if line == "pause" {
                            libc::SIGSTOP
                        } else {
                            libc::SIGCONT
                        };
                        if unsafe { libc::kill(-(self.inner.children[index].pid() as i32), signal) }
                            != 0
                        {
                            return Err(std::io::Error::last_os_error().into());
                        }
                    }
                    Ok(())
                }
                #[cfg(windows)]
                {
                    let mut suspended = self.inner.suspended.lock().unwrap();
                    if line == "pause" {
                        let mut trees = [None, None];
                        for index in [1, 2] {
                            if self.inner.exited[index].load(Ordering::SeqCst) {
                                return Err("autotournament frontend exited".into());
                            }
                            if suspended[index - 1].is_none() {
                                trees[index - 1] = Some(super::windows::SuspendedTree::suspend(
                                    self.inner.children[index].pid(),
                                )?);
                            }
                        }
                        for (target, tree) in suspended.iter_mut().zip(trees) {
                            if tree.is_some() {
                                *target = tree;
                            }
                        }
                    } else {
                        let mut error = None;
                        for tree in suspended.iter_mut().filter_map(Option::take) {
                            if let Err(e) = tree.resume() {
                                error = Some(e);
                            }
                        }
                        if let Some(error) = error {
                            return Err(error);
                        }
                    }
                    Ok(())
                }
                #[cfg(not(any(unix, windows)))]
                {
                    Err("autotournament snapshot pause requires Linux or Windows".into())
                }
            }
            "frontend-stop" => {
                self.inner.frontend_stopped.store(true, Ordering::SeqCst);
                self.inner.stop_one(2, false).await?;
                self.inner.stop_one(1, false).await
            }
            _ => Err("autotournament: unsupported stack command".into()),
        }
    }
    fn output(&self) -> broadcast::Receiver<OutputLine> {
        self.inner.output.subscribe()
    }
    fn output_tail(&self, n: usize) -> Vec<OutputLine> {
        self.inner
            .ring
            .lock()
            .unwrap()
            .iter()
            .rev()
            .take(n)
            .rev()
            .cloned()
            .collect()
    }
    async fn wait(&self) -> Result<ExitStatus> {
        let mut exit = self.inner.exit.subscribe();
        loop {
            if let Some(status) = exit.borrow().clone() {
                return Ok(status);
            }
            exit.changed().await?;
        }
    }
    async fn terminate(&self) -> Result<()> {
        self.inner.stop_all(true).await
    }
    async fn interrupt(&self) -> Result<()> {
        self.terminate().await
    }
    async fn kill(&self) -> Result<()> {
        self.inner.stop_all(false).await
    }
    fn resource_usage(&self) -> Option<ResourceUsage> {
        let mut usage = ResourceUsage::default();
        for child in &self.inner.children {
            if let Some(value) = child.resource_usage() {
                usage.rss_bytes += value.rss_bytes;
                usage.cpu_percent += value.cpu_percent;
            }
        }
        Some(usage)
    }
}
