//! Best-effort capabilities for the heartbeat (no heavy deps).

use cp_api::Capabilities;

pub fn capabilities(
    data_dir: &std::path::Path,
    drivers: &[&'static str],
    started_at_ms: i64,
    draining: bool,
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
        runtimes: Some(
            std::iter::once("native".to_string())
                .chain(runtimes::provisionable().into_iter().map(String::from))
                .collect(),
        ),
        cached_deployments: Some(vec![]),
        drivers: Some(drivers.iter().map(|d| d.to_string()).collect()),
        uptime_s: Some(uptime()),
        agent_started_at: Some(started_at_ms),
        draining: Some(draining),
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
#[cfg(windows)]
fn mem_status() -> Option<windows_sys::Win32::System::SystemInformation::MEMORYSTATUSEX> {
    use windows_sys::Win32::System::SystemInformation::{GlobalMemoryStatusEx, MEMORYSTATUSEX};
    unsafe {
        let mut st: MEMORYSTATUSEX = std::mem::zeroed();
        st.dwLength = std::mem::size_of::<MEMORYSTATUSEX>() as u32;
        (GlobalMemoryStatusEx(&mut st) != 0).then_some(st)
    }
}
#[cfg(windows)]
fn mem_kb() -> Option<u64> {
    mem_status().map(|s| s.ullTotalPhys / 1024)
}
#[cfg(windows)]
fn mem_avail_kb() -> Option<u64> {
    mem_status().map(|s| s.ullAvailPhys / 1024)
}
#[cfg(not(any(unix, windows)))]
fn mem_kb() -> Option<u64> {
    None
}
#[cfg(not(any(unix, windows)))]
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
#[cfg(windows)]
fn disk_free(dir: &std::path::Path) -> Option<u64> {
    use std::os::windows::ffi::OsStrExt;
    use windows_sys::Win32::Storage::FileSystem::GetDiskFreeSpaceExW;
    let wide: Vec<u16> = dir.as_os_str().encode_wide().chain(Some(0)).collect();
    let mut avail = 0u64;
    let ok = unsafe {
        GetDiskFreeSpaceExW(
            wide.as_ptr(),
            &mut avail,
            std::ptr::null_mut(),
            std::ptr::null_mut(),
        )
    };
    (ok != 0).then_some(avail)
}
#[cfg(not(any(unix, windows)))]
fn disk_free(_dir: &std::path::Path) -> Option<u64> {
    None
}

#[cfg(windows)]
fn uptime() -> i64 {
    (unsafe { windows_sys::Win32::System::SystemInformation::GetTickCount64() } / 1000) as i64
}

#[cfg(not(windows))]
fn uptime() -> i64 {
    std::fs::read_to_string("/proc/uptime")
        .ok()
        .and_then(|s| s.split_whitespace().next()?.parse::<f64>().ok())
        .map(|f| f as i64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn capabilities_reflects_draining() {
        let dir = std::env::temp_dir();
        let live = capabilities(&dir, &[], 0, false);
        let draining = capabilities(&dir, &[], 0, true);
        assert_eq!(live.draining, Some(false));
        assert_eq!(draining.draining, Some(true));
    }

    #[cfg(any(target_os = "linux", windows))]
    #[test]
    fn capabilities_report_memory_disk_and_uptime() {
        let c = capabilities(&std::env::temp_dir(), &[], 0, false);
        assert!(c.memory_total_mb.unwrap() > 0);
        assert!(c.memory_available_mb.unwrap() > 0);
        assert!(c.disk_free_bytes.unwrap() > 0);
        assert!(c.uptime_s.unwrap() > 0);
    }
}
