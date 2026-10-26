use std::time::{Duration, Instant};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Status {
    StartPending,
    Running,
    StopPending { checkpoint: u32 },
    Stopped { failed: bool },
}

#[derive(PartialEq, Eq)]
enum Phase {
    Starting,
    Running,
    Draining,
    Stopped,
}

pub struct StatusReporter<F> {
    report: F,
    phase: Phase,
    checkpoint: u32,
    last_pending: Option<Instant>,
}

impl<F, E> StatusReporter<F>
where
    F: FnMut(Status) -> Result<(), E>,
{
    pub fn new(report: F) -> Self {
        Self {
            report,
            phase: Phase::Starting,
            checkpoint: 0,
            last_pending: None,
        }
    }

    pub fn start(&mut self) -> Result<(), E> {
        if self.phase == Phase::Starting {
            (self.report)(Status::StartPending)?;
            (self.report)(Status::Running)?;
            self.phase = Phase::Running;
        }
        Ok(())
    }

    pub fn stop(&mut self, now: Instant) -> Result<(), E> {
        if self.phase != Phase::Stopped {
            self.phase = Phase::Draining;
            if self.checkpoint == 0 {
                self.pending(now)?;
            }
        }
        Ok(())
    }

    pub fn tick(&mut self, now: Instant) -> Result<(), E> {
        if self.phase == Phase::Draining
            && self
                .last_pending
                .is_none_or(|last| now.saturating_duration_since(last) >= Duration::from_secs(3))
        {
            self.pending(now)?;
        }
        Ok(())
    }

    fn pending(&mut self, now: Instant) -> Result<(), E> {
        let checkpoint = self.checkpoint.saturating_add(1);
        (self.report)(Status::StopPending { checkpoint })?;
        self.checkpoint = checkpoint;
        self.last_pending = Some(now);
        Ok(())
    }

    pub fn finish(&mut self, failed: bool) -> Result<(), E> {
        if self.phase != Phase::Stopped {
            (self.report)(Status::Stopped { failed })?;
            self.phase = Phase::Stopped;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};

    #[test]
    fn stop_is_immediate_and_checkpoints_only_advance_when_due() {
        let mut reports = vec![];
        let mut reporter = StatusReporter::new(|status| {
            reports.push(status);
            Ok::<_, ()>(())
        });
        let now = Instant::now();
        reporter.start().unwrap();
        reporter.tick(now).unwrap();
        reporter.stop(now).unwrap();
        reporter.stop(now).unwrap();
        reporter.tick(now).unwrap();
        reporter.tick(now + Duration::from_millis(2999)).unwrap();
        reporter.tick(now + Duration::from_secs(3)).unwrap();
        reporter.tick(now + Duration::from_secs(6)).unwrap();
        reporter.finish(false).unwrap();
        reporter.start().unwrap();
        reporter.stop(now).unwrap();
        reporter.tick(now + Duration::from_secs(9)).unwrap();
        reporter.finish(true).unwrap();
        assert_eq!(
            reports,
            [
                Status::StartPending,
                Status::Running,
                Status::StopPending { checkpoint: 1 },
                Status::StopPending { checkpoint: 2 },
                Status::StopPending { checkpoint: 3 },
                Status::Stopped { failed: false },
            ]
        );
    }

    #[test]
    fn early_stop_never_reports_running_and_failures_stop_with_error() {
        for failed in [false, true] {
            let mut reports = vec![];
            let mut reporter = StatusReporter::new(|status| {
                reports.push(status);
                Ok::<_, ()>(())
            });
            reporter.stop(Instant::now()).unwrap();
            reporter.start().unwrap();
            reporter.finish(failed).unwrap();
            assert_eq!(
                reports,
                [
                    Status::StopPending { checkpoint: 1 },
                    Status::Stopped { failed }
                ]
            );
        }
    }

    #[test]
    fn failed_pending_report_is_retried_without_skipping_checkpoint() {
        let mut reports = vec![];
        let mut failed = false;
        let mut reporter = StatusReporter::new(|status| {
            reports.push(status);
            if !failed && matches!(status, Status::StopPending { .. }) {
                failed = true;
                return Err(());
            }
            Ok(())
        });
        let now = Instant::now();
        reporter.start().unwrap();
        assert!(reporter.stop(now).is_err());
        reporter.tick(now).unwrap();
        reporter.tick(now + Duration::from_secs(3)).unwrap();
        reporter.finish(true).unwrap();
        assert_eq!(
            reports,
            [
                Status::StartPending,
                Status::Running,
                Status::StopPending { checkpoint: 1 },
                Status::StopPending { checkpoint: 1 },
                Status::StopPending { checkpoint: 2 },
                Status::Stopped { failed: true },
            ]
        );
    }

    #[test]
    fn concurrent_controls_and_reporter_never_regress_stopped() {
        let reports = Arc::new(Mutex::new(vec![]));
        let sink = reports.clone();
        let reporter = Arc::new(Mutex::new(StatusReporter::new(move |status| {
            sink.lock().unwrap().push(status);
            Ok::<_, ()>(())
        })));
        reporter.lock().unwrap().start().unwrap();
        let now = Instant::now();
        reporter.lock().unwrap().stop(now).unwrap();
        std::thread::scope(|scope| {
            for _ in 0..4 {
                let reporter = reporter.clone();
                scope.spawn(move || {
                    for tick in 0..100 {
                        let mut reporter = reporter.lock().unwrap();
                        reporter.stop(now).unwrap();
                        reporter.tick(now + Duration::from_secs(tick * 3)).unwrap();
                    }
                });
            }
            reporter.lock().unwrap().finish(false).unwrap();
        });
        let reports = reports.lock().unwrap();
        assert_eq!(reports.last(), Some(&Status::Stopped { failed: false }));
        assert_eq!(
            reports
                .iter()
                .filter(|status| matches!(status, Status::Stopped { .. }))
                .count(),
            1
        );
        let checkpoints: Vec<_> = reports
            .iter()
            .filter_map(|status| match status {
                Status::StopPending { checkpoint } => Some(*checkpoint),
                _ => None,
            })
            .collect();
        assert_eq!(
            checkpoints,
            (1..=checkpoints.len() as u32).collect::<Vec<_>>()
        );
    }
}
