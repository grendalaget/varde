//! Best-effort capabilities for the heartbeat (no heavy deps).

use cp_api::Capabilities;

pub fn capabilities(
    data_dir: &std::path::Path,
    drivers: &[&'static str],
    started_at_ms: i64,
) -> Capabilities {
    Capabilities {
        os: Some(std::env::consts::OS.to_string()),
        arch: Some(std::env::consts::ARCH.to_string()),
        cpu_cores: Some(
            std::thread::available_parallelism()
                .map(|n| n.get() as i64)
                .unwrap_or(1),
        ),
        memory_total_mb: mem_kb().map(|k| (k / 1024) as i64),
        memory_available_mb: mem_avail_kb().map(|k| (k / 1024) as i64),
        disk_free_bytes: disk_free(data_dir).map(|b| b as i64),
        on_battery: Some(false),
        user_active: Some(false),
        runtimes: Some(vec!["native".into()]),
        cached_deployments: Some(vec![]),
        drivers: Some(drivers.iter().map(|d| d.to_string()).collect()),
        uptime_s: Some(uptime()),
        agent_started_at: Some(started_at_ms),
    }
}

#[cfg(unix)]
fn meminfo(key: &str) -> Option<u64> {
    let s = std::fs::read_to_string("/proc/meminfo").ok()?;
    s.lines()
        .find(|l| l.starts_with(key))
        .and_then(|l| l.split_whitespace().nth(1)?.parse::<u64>().ok())
}

#[cfg(unix)]
fn mem_kb() -> Option<u64> {
    meminfo("MemTotal:")
}
#[cfg(unix)]
fn mem_avail_kb() -> Option<u64> {
    meminfo("MemAvailable:")
}
#[cfg(not(unix))]
fn mem_kb() -> Option<u64> {
    None
}
#[cfg(not(unix))]
fn mem_avail_kb() -> Option<u64> {
    None
}

#[cfg(unix)]
fn disk_free(dir: &std::path::Path) -> Option<u64> {
    let path = std::ffi::CString::new(dir.to_string_lossy().as_bytes()).ok()?;
    unsafe {
        let mut st: libc::statvfs = std::mem::zeroed();
        if libc::statvfs(path.as_ptr(), &mut st) != 0 {
            return None;
        }
        Some(st.f_bavail as u64 * st.f_frsize as u64)
    }
}
#[cfg(not(unix))]
fn disk_free(_dir: &std::path::Path) -> Option<u64> {
    None
}

fn uptime() -> i64 {
    std::fs::read_to_string("/proc/uptime")
        .ok()
        .and_then(|s| s.split_whitespace().next()?.parse::<f64>().ok())
        .map(|f| f as i64)
        .unwrap_or(0)
}
