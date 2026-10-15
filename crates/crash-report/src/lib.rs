//! Local crash capture for Varde processes.
//!
//! Reports live as files under a crash dir (`<data>/crash-reports` for the
//! service, `%LOCALAPPDATA%\Varde\crash-reports` for the desktop app):
//! a `.json` describing the crash plus an optional sibling `.dmp` minidump.
//! Rust panics come from [`install_panic_hook`]; hard crashes (access
//! violations) arrive as bare `.dmp` files written by Windows Error
//! Reporting (see [`wer`]); a supervised child that dies abnormally gets a
//! `process` report with its stderr tail via [`record_process_exit`].
//!
//! `collect` lists reports not yet uploaded, `mark_sent` files them under
//! `sent/` once the control plane acknowledges them, and `gc` bounds the
//! directories. Capture is always on; whether reports are *uploaded* to the
//! machine's control plane is the `crash_reporting` pref — for a self-hosted
//! group reports never leave its own infrastructure.

use std::io;
use std::path::{Path, PathBuf};

/// Largest minidump forwarded over IPC or HTTP; bigger files stay local for
/// manual support (flagged `dump_omitted` in the report metadata).
pub const MAX_DUMP_BYTES: u64 = 8 * 1024 * 1024;

/// One captured crash. The `.json` file carries exactly this.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct Report {
    /// panic | dump | process
    pub kind: String,
    /// Image that crashed ("varde-agent", "varde-tray", "varde-mesh").
    pub exe: String,
    pub version: String,
    pub os: String,
    pub arch: String,
    pub pid: u32,
    /// One-line what happened: panic message, "minidump written by Windows
    /// Error Reporting", "process exited".
    pub message: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub location: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub backtrace: String,
    /// Extra context: exit code, stderr tail, why a dump is missing.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub detail: String,
    pub created_at_unix_ms: i64,
    /// Basename of the sibling minidump, when one exists.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub dump_name: String,
}

impl Report {
    pub fn new(kind: &str, exe: &str, version: &str) -> Report {
        Report {
            kind: kind.into(),
            exe: exe.into(),
            version: version.into(),
            os: std::env::consts::OS.into(),
            arch: std::env::consts::ARCH.into(),
            pid: std::process::id(),
            message: String::new(),
            location: String::new(),
            backtrace: String::new(),
            detail: String::new(),
            created_at_unix_ms: now_ms(),
            dump_name: String::new(),
        }
    }
}

/// A report on disk plus the files carrying it.
pub struct Pending {
    pub report: Report,
    /// Filename stem (`<ts>_<pid>_<exe>_<kind>`); also the report's id.
    pub stem: String,
    /// None for a bare WER dump (the report was synthesized at collect).
    pub json_path: Option<PathBuf>,
    pub dump_path: Option<PathBuf>,
}

fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// `<ts>_<pid>_<exe>_<kind>` — unique per crash, safe on every filesystem.
fn stem(exe: &str, kind: &str) -> String {
    let exe = exe.trim_end_matches(".exe");
    format!("{}_{}_{}_{}", now_ms(), std::process::id(), exe, kind)
}

/// Writes `report` as `<dir>/<stem>.json`. Returns the file's stem.
pub fn write_report(dir: &Path, report: &Report) -> io::Result<String> {
    std::fs::create_dir_all(dir)?;
    let base = stem(&report.exe, &report.kind);
    // two same-kind crashes inside one millisecond share a stem — suffix
    // rather than overwrite the earlier report's evidence
    for i in 0..100 {
        let stem = if i == 0 {
            base.clone()
        } else {
            format!("{base}-{i}")
        };
        let path = dir.join(format!("{stem}.json"));
        if !path.exists() {
            std::fs::write(&path, serde_json::to_vec_pretty(report)?)?;
            return Ok(stem);
        }
    }
    Err(io::Error::new(
        io::ErrorKind::AlreadyExists,
        "crash dir saturated with same-stem reports",
    ))
}

/// Writes a report plus its minidump under the given stem.
pub fn write_report_with_dump(
    dir: &Path,
    report: &mut Report,
    dump: &[u8],
    dump_name: &str,
) -> io::Result<String> {
    let stem = write_report(dir, report)?;
    let dmp = dir.join(format!("{stem}.dmp"));
    std::fs::write(&dmp, dump)?;
    report.dump_name = if dump_name.is_empty() {
        format!("{stem}.dmp")
    } else {
        dump_name.into()
    };
    // re-write so dump_name is recorded
    std::fs::write(
        dir.join(format!("{stem}.json")),
        serde_json::to_vec_pretty(report)?,
    )?;
    Ok(stem)
}

/// Installs a panic hook that records the panic to `dir`, then chains to the
/// previous hook (the default prints to stderr / the service log).
/// `exe`/`version` identify the *calling* binary, not this crate.
pub fn install_panic_hook(dir: PathBuf, exe: &'static str, version: &'static str) {
    let prev = std::panic::take_hook();
    std::panic::set_hook(Box::new(move |info| {
        let mut r = Report::new("panic", exe, version);
        r.message = info
            .payload()
            .downcast_ref::<&str>()
            .map(|s| (*s).to_string())
            .or_else(|| info.payload().downcast_ref::<String>().cloned())
            .unwrap_or_else(|| "panic (no message)".into());
        if let Some(l) = info.location() {
            r.location = format!("{}:{}:{}", l.file(), l.line(), l.column());
        }
        r.backtrace = std::backtrace::Backtrace::force_capture().to_string();
        let _ = write_report(&dir, &r);
        prev(info);
    }));
}

/// Records a supervised child that exited abnormally, with the last lines of
/// its stderr. Call only for unexpected exits — a clean stop isn't a crash.
pub fn record_process_exit(
    dir: &Path,
    exe: &str,
    version: &str,
    status: &str,
    stderr_tail: &[String],
) -> io::Result<String> {
    let mut r = Report::new("process", exe, version);
    r.message = format!("{exe} exited: {status}");
    r.detail = stderr_tail.join("\n");
    write_report(dir, &r)
}

/// Cracks a WER-style dump name (`<image>.<pid>.dmp`) into (exe, pid).
/// Falls back to the whole stem and pid 0.
fn crack_dump_name(stem: &str) -> (String, u32) {
    if let Some((img, pid)) = stem.rsplit_once('.') {
        if let Ok(p) = pid.parse::<u32>() {
            return (img.to_string(), p);
        }
    }
    (stem.to_string(), 0)
}

fn read_report(path: &Path) -> Option<Report> {
    serde_json::from_slice(&std::fs::read(path).ok()?).ok()
}

/// Lists reports in `dir` not yet sent (i.e. not under `sent/`). Bare `.dmp`
/// files (WER output) become `dump` reports; `.json` files parse as Reports
/// and pick up a same-stem sibling `.dmp` when present.
pub fn collect(dir: &Path) -> io::Result<Vec<Pending>> {
    let mut out = Vec::new();
    let rd = match std::fs::read_dir(dir) {
        Ok(rd) => rd,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(out),
        Err(e) => return Err(e),
    };
    let mut json_stems = std::collections::BTreeSet::new();
    let mut dumps = Vec::new();
    for e in rd.flatten() {
        let p = e.path();
        if !p.is_file() {
            continue;
        }
        match p.extension().and_then(|x| x.to_str()) {
            Some("json") => {
                if let Some(stem) = p.file_stem().and_then(|s| s.to_str()) {
                    if let Some(report) = read_report(&p) {
                        json_stems.insert(stem.to_string());
                        let dmp = dir.join(format!("{stem}.dmp"));
                        out.push(Pending {
                            report,
                            stem: stem.to_string(),
                            json_path: Some(p),
                            dump_path: dmp.is_file().then_some(dmp),
                        });
                    }
                    // an unparseable .json does not claim its stem: a
                    // sibling .dmp is still collected as a bare dump below
                }
            }
            Some("dmp") => dumps.push(p),
            _ => {}
        }
    }
    for dmp in dumps {
        let stem = dmp.file_stem().and_then(|s| s.to_str()).unwrap_or_default();
        if json_stems.contains(stem) {
            continue; // attached to its report already
        }
        let (exe, pid) = crack_dump_name(stem);
        let mut r = Report::new("dump", exe.trim_end_matches(".exe"), "");
        r.pid = pid;
        r.message = "process crashed; minidump written by Windows Error Reporting".into();
        r.dump_name = dmp
            .file_name()
            .and_then(|s| s.to_str())
            .unwrap_or_default()
            .into();
        if let Ok(md) = dmp.metadata() {
            if let Ok(t) = md.modified() {
                r.created_at_unix_ms = t
                    .duration_since(std::time::UNIX_EPOCH)
                    .map(|d| d.as_millis() as i64)
                    .unwrap_or(0);
            }
        }
        out.push(Pending {
            report: r,
            stem: stem.to_string(),
            json_path: None,
            dump_path: Some(dmp),
        });
    }
    out.sort_by_key(|p| p.report.created_at_unix_ms);
    Ok(out)
}

/// Moves a report's files into `<dir>/sent/` after the control plane took it.
pub fn mark_sent(dir: &Path, pending: &Pending) -> io::Result<()> {
    let sent = dir.join("sent");
    std::fs::create_dir_all(&sent)?;
    for p in [&pending.json_path, &pending.dump_path]
        .into_iter()
        .flatten()
    {
        if let Some(name) = p.file_name() {
            let _ = std::fs::rename(p, sent.join(name));
        }
    }
    Ok(())
}

/// Bounds the crash dir: `sent/` keeps the newest `keep` files; pending
/// reports older than 30 days (never deliverable — format moved on) and
/// pending dumps over [`MAX_DUMP_BYTES`] * 4 are dropped.
pub fn gc(dir: &Path, keep: usize) -> io::Result<()> {
    let sent = dir.join("sent");
    if let Ok(rd) = std::fs::read_dir(&sent) {
        let mut files: Vec<PathBuf> = rd
            .flatten()
            .map(|e| e.path())
            .filter(|p| p.is_file())
            .collect();
        files.sort_by_key(|p| p.metadata().and_then(|m| m.modified()).ok());
        let drop = files.len().saturating_sub(keep);
        for p in files.into_iter().take(drop) {
            let _ = std::fs::remove_file(p);
        }
    }
    const MAX_PENDING_AGE: std::time::Duration = std::time::Duration::from_secs(30 * 86_400);
    if let Ok(rd) = std::fs::read_dir(dir) {
        for e in rd.flatten() {
            let p = e.path();
            if !p.is_file() {
                continue;
            }
            let too_old = p
                .metadata()
                .and_then(|m| m.modified())
                .ok()
                .and_then(|t| t.elapsed().ok())
                .is_some_and(|age| age > MAX_PENDING_AGE);
            let too_big = p.extension().and_then(|x| x.to_str()) == Some("dmp")
                && p.metadata().map(|m| m.len()).unwrap_or(0) > MAX_DUMP_BYTES * 4;
            if too_old || too_big {
                let _ = std::fs::remove_file(&p);
            }
        }
    }
    Ok(())
}

// ---------- prefs ----------

/// `prefs.json` in the data dir: per-machine knobs that must work before a
/// link exists (config.toml is only written by linking).
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct Prefs {
    /// Upload captured crash reports to this machine's control plane.
    /// Capture to disk happens either way.
    #[serde(default = "default_true")]
    pub crash_reporting: bool,
}

fn default_true() -> bool {
    true
}

impl Default for Prefs {
    fn default() -> Self {
        Prefs {
            crash_reporting: true,
        }
    }
}

pub fn load_prefs(data_dir: &Path) -> Prefs {
    match std::fs::read(data_dir.join("prefs.json")) {
        Ok(b) => serde_json::from_slice(&b).unwrap_or(
            // an unreadable opt-out must not silently become an opt-in:
            // fail closed until the file is rewritten
            Prefs {
                crash_reporting: false,
            },
        ),
        Err(_) => Prefs::default(),
    }
}

pub fn save_prefs(data_dir: &Path, prefs: &Prefs) -> io::Result<()> {
    std::fs::create_dir_all(data_dir)?;
    let p = data_dir.join("prefs.json");
    let tmp = data_dir.join("prefs.json.tmp");
    std::fs::write(&tmp, serde_json::to_vec_pretty(prefs)?)?;
    std::fs::rename(&tmp, &p)?;
    Ok(())
}

// ---------- Windows Error Reporting ----------

/// Registers minidump-on-crash for the Varde executables in
/// HKLM\...\Windows Error Reporting\LocalDumps — the OS writes a `.dmp` into
/// our crash dir even when the process can't (access violation, stack
/// overflow). Needs Administrators rights; call best-effort.
#[cfg(windows)]
pub mod wer {
    use std::io;
    use std::path::Path;

    const BASE: &str = r"SOFTWARE\Microsoft\Windows\Windows Error Reporting\LocalDumps";

    /// `service_dir` receives dumps of the Session-0 processes
    /// (varde-agent, varde-mesh). The tray dumps into the crashing user's
    /// LOCALAPPDATA — the path is stored as REG_EXPAND_SZ and expands in the
    /// faulting process's own context.
    pub fn register(service_dir: &Path) -> io::Result<()> {
        for exe in ["varde-agent.exe", "varde-mesh.exe"] {
            set(exe, &service_dir.to_string_lossy(), false)?;
        }
        set(
            "varde-tray.exe",
            r"%LOCALAPPDATA%\Varde\crash-reports",
            true,
        )
    }

    fn set(exe: &str, folder: &str, expand: bool) -> io::Result<()> {
        use winreg::enums::{RegType, HKEY_LOCAL_MACHINE};
        use winreg::RegKey;
        let (key, _) =
            RegKey::predef(HKEY_LOCAL_MACHINE).create_subkey(format!(r"{BASE}\{exe}"))?;
        let wide: Vec<u8> = folder
            .encode_utf16()
            .flat_map(|c| c.to_le_bytes())
            .collect();
        let vtype = if expand {
            RegType::REG_EXPAND_SZ
        } else {
            RegType::REG_SZ
        };
        key.set_raw_value("DumpFolder", &winreg::RegValue { vtype, bytes: wide })?;
        key.set_value("DumpCount", &10u32)?;
        key.set_value("DumpType", &1u32)?; // 1 = mini dump
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn report(kind: &str) -> Report {
        let mut r = Report::new(kind, "varde-agent", "1.2.3");
        r.message = format!("test {kind}");
        r
    }

    #[test]
    fn collect_finds_json_and_sibling_dump() {
        let dir = tempfile::tempdir().unwrap();
        let mut r = report("panic");
        let stem = write_report_with_dump(dir.path(), &mut r, b"DMPBYTES", "x.dmp").unwrap();
        let all = collect(dir.path()).unwrap();
        assert_eq!(all.len(), 1);
        assert_eq!(all[0].stem, stem);
        assert_eq!(all[0].report.kind, "panic");
        assert_eq!(
            all[0].dump_path.as_deref(),
            Some(dir.path().join(format!("{stem}.dmp")).as_path())
        );
        assert_eq!(all[0].report.dump_name, "x.dmp");
    }

    #[test]
    fn collect_synthesizes_bare_wer_dump() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("varde-tray.exe.4321.dmp"), b"DMP").unwrap();
        let all = collect(dir.path()).unwrap();
        assert_eq!(all.len(), 1);
        assert_eq!(all[0].report.kind, "dump");
        assert_eq!(all[0].report.exe, "varde-tray");
        assert_eq!(all[0].report.pid, 4321);
        assert!(all[0].json_path.is_none());
    }

    #[test]
    fn mark_sent_moves_files() {
        let dir = tempfile::tempdir().unwrap();
        let mut r = report("panic");
        let stem = write_report_with_dump(dir.path(), &mut r, b"D", "x.dmp").unwrap();
        let all = collect(dir.path()).unwrap();
        mark_sent(dir.path(), &all[0]).unwrap();
        assert!(collect(dir.path()).unwrap().is_empty());
        assert!(dir
            .path()
            .join("sent")
            .join(format!("{stem}.json"))
            .is_file());
        assert!(dir
            .path()
            .join("sent")
            .join(format!("{stem}.dmp"))
            .is_file());
    }

    #[test]
    fn gc_prunes_sent_beyond_keep() {
        let dir = tempfile::tempdir().unwrap();
        let sent = dir.path().join("sent");
        std::fs::create_dir_all(&sent).unwrap();
        for i in 0..5 {
            let p = sent.join(format!("{i}.json"));
            std::fs::write(&p, b"{}").unwrap();
            // strictly increasing mtimes
            let t = filetime::FileTime::from_unix_time(1_700_000_000 + i, 0);
            filetime::set_file_mtime(&p, t).unwrap();
        }
        gc(dir.path(), 2).unwrap();
        let left: Vec<_> = std::fs::read_dir(&sent).unwrap().flatten().collect();
        assert_eq!(left.len(), 2);
    }

    #[test]
    fn prefs_roundtrip() {
        let dir = tempfile::tempdir().unwrap();
        assert!(load_prefs(dir.path()).crash_reporting);
        save_prefs(
            dir.path(),
            &Prefs {
                crash_reporting: false,
            },
        )
        .unwrap();
        assert!(!load_prefs(dir.path()).crash_reporting);
    }
}
