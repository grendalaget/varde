// Command-line surface, kept compatible with the NSIS template and the
// Inno shim before it (the tray updater drives `/S /R /ARGS --autostart`,
// winget-style tooling drives `/S`, and upgrades chain the old
// uninstallers). Parsing is done by hand off the raw command line — NSIS
// GetOptions semantics — so /CPURL= survives the '/' inside a URL.
use std::path::PathBuf;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Mode {
    Install,
    Uninstall,
}

#[derive(Debug, Clone, Default)]
pub struct Args {
    pub mode: Mode,
    /// /S — fully silent, no window.
    pub silent: bool,
    /// /P — passive: NSIS showed a progress-only UI. We run the same silent
    /// engine; documented divergence in packaging/windows/README.md.
    pub passive: bool,
    /// /NS — skip shortcut creation.
    pub no_shortcut: bool,
    /// /UPDATE — chain-uninstall: keep shortcuts/data/reg (the old
    /// uninstaller received this from its parent installer).
    pub update: bool,
    /// /R — silent installs relaunch the tray afterwards (updater uses it).
    pub relaunch: bool,
    /// /ARGS <rest> — arguments for the relaunched tray; runs to EOL.
    pub relaunch_args: String,
    /// /TASKS="autostart controlplane" — the whole set when present.
    pub tasks: Option<String>,
    /// /CPURL=<host|url> — see parse_cp_url.
    pub cp_url: Option<String>,
    /// /D=<dir> — install dir override (NSIS convention, unquoted, last arg).
    pub dir: Option<PathBuf>,
    /// /target=<dir> — internal: uninstall runner's install dir.
    pub target: Option<PathBuf>,
    /// /waitpid=<n> - internal: staged uninstaller waits for this pid.
    pub wait_pid: Option<u32>,
}

impl std::default::Default for Mode {
    fn default() -> Self {
        Mode::Install
    }
}

fn raw_cmdline() -> String {
    // GetCommandLineW keeps quoting intact, which /CPURL= parsing needs.
    let cmd = unsafe {
        let p = windows::Win32::System::Environment::GetCommandLineW();
        if p.is_null() {
            return String::new();
        }
        p.to_string().unwrap_or_default()
    };
    cmd
}

/// Whole raw command line minus argv[0], like NSIS's $CMDLINE.
fn tail() -> String {
    let cmd = raw_cmdline();
    let b = cmd.as_bytes();
    let mut i = 0;
    // skip argv[0]: either "quoted path" or unquoted token
    while i < b.len() && b[i].is_ascii_whitespace() {
        i += 1;
    }
    if i < b.len() && b[i] == b'"' {
        i += 1;
        while i < b.len() && b[i] != b'"' {
            i += 1;
        }
        if i < b.len() {
            i += 1; // closing quote
        }
    } else {
        while i < b.len() && !b[i].is_ascii_whitespace() {
            i += 1;
        }
    }
    cmd[i..].to_string()
}

fn has_flag(t: &str, flag: &str) -> bool {
    let tl = t.to_lowercase();
    let fl = flag.to_lowercase();
    let mut start = 0;
    while let Some(off) = tl[start..].find(&fl) {
        let i = start + off;
        let before_ok = i == 0 || tl.as_bytes()[i - 1].is_ascii_whitespace();
        let after = &tl[i + fl.len()..];
        let after_ok = after.is_empty()
            || after
                .chars()
                .next()
                .map(|c| c.is_whitespace())
                .unwrap_or(true);
        if before_ok && after_ok {
            return true;
        }
        start = i + fl.len();
    }
    false
}

/// `/CPURL=` — manual parse, NSIS VardeParseCpUrlFrom semantics: the value
/// runs to EOL or first whitespace (a quoted value honors its quotes);
/// trailing '/' trimmed; the key match is case-insensitive.
pub fn parse_cp_url(t: &str) -> Option<String> {
    let low = t.to_lowercase();
    let idx = low.find("/cpurl=")?;
    let mut v = &t[idx + 7..];
    let out: &str;
    if v.starts_with('"') {
        v = &v[1..];
        out = match v.find('"') {
            Some(end) => &v[..end],
            None => v,
        };
    } else {
        let end = v.find(|c: char| c.is_whitespace()).unwrap_or(v.len());
        out = &v[..end];
    }
    let mut s = out.to_string();
    while s.ends_with('/') {
        s.pop();
    }
    if s.is_empty() {
        None
    } else {
        Some(s)
    }
}

/// `/TASKS=` — Inno semantics: when present it is the whole set. Quoted
/// honors quotes, else runs to whitespace (same shape as /CPURL=).
fn parse_value(t: &str, key: &str) -> Option<String> {
    let low = t.to_lowercase();
    let idx = low.find(&key.to_lowercase())?;
    // Two quoted shapes must both work:
    //   /target="C:\Program Files\Varde"   - NSIS-style (quote after '=')
    //   "/target=C:\Program Files\Varde"  - argv whole-token quoting,
    //     what std::process::Command emits for args containing spaces
    if idx > 0 && t.as_bytes()[idx - 1] == b'"' {
        let rest = &t[idx + key.len()..];
        let end = rest.find('"').unwrap_or(rest.len());
        return Some(rest[..end].to_string());
    }
    let mut v = &t[idx + key.len()..];
    let out: String;
    if v.starts_with('"') {
        v = &v[1..];
        out = match v.find('"') {
            Some(end) => v[..end].to_string(),
            None => v.to_string(),
        };
    } else {
        let end = v.find(|c: char| c.is_whitespace()).unwrap_or(v.len());
        out = v[..end].to_string();
    }
    Some(out)
}

/// `/ARGS <rest>` — everything after the flag, used verbatim as the
/// relaunched binary's argument string (update.rs: /S /R /ARGS --autostart).
fn parse_rest(t: &str, flag: &str) -> Option<String> {
    let low = t.to_lowercase();
    let fl = flag.to_lowercase();
    let idx = low.find(&fl)?;
    // require a whitespace boundary so "/ARGSX" can't match
    let end = idx + fl.len();
    if !low[end..]
        .chars()
        .next()
        .map(|c| c.is_whitespace())
        .unwrap_or(true)
    {
        return None;
    }
    Some(t[end..].trim().to_string())
}

pub fn parse() -> Args {
    let t = tail();
    let mut a = Args {
        silent: has_flag(&t, "/S") || has_flag(&t, "/VERYSILENT"),
        passive: has_flag(&t, "/P"),
        no_shortcut: has_flag(&t, "/NS"),
        update: has_flag(&t, "/UPDATE"),
        relaunch: has_flag(&t, "/R"),
        relaunch_args: parse_rest(&t, "/ARGS").unwrap_or_default(),
        tasks: parse_value(&t, "/TASKS="),
        cp_url: parse_cp_url(&t),
        dir: parse_value(&t, "/D=").map(PathBuf::from),
        target: parse_value(&t, "/target=").map(PathBuf::from),
        wait_pid: parse_value(&t, "/waitpid=").and_then(|s| s.parse().ok()),
        ..Default::default()
    };
    if has_flag(&t, "--uninstall") || has_flag(&t, "/uninstall") {
        a.mode = Mode::Uninstall;
    }
    a
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cpurl_variants() {
        assert_eq!(
            parse_cp_url("/CPURL=cp.example.com"),
            Some("cp.example.com".into())
        );
        assert_eq!(
            parse_cp_url("/S /cpurl=https://cp.example.com/ /TASKS=x"),
            Some("https://cp.example.com".into())
        );
        assert_eq!(
            parse_cp_url(r#"/CPURL="https://a.b c""#),
            Some("https://a.b c".into())
        );
        assert_eq!(parse_cp_url("/CPURL="), None);
        assert_eq!(parse_cp_url(""), None);
    }
}
