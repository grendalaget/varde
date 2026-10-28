// Install/uninstall orchestration — a faithful port of
// apps/desktop/installer-hooks/hooks.nsh (which itself carried the Inno
// shim's behavior). Ordering matters; keep it in sync with
// packaging/windows/README.md.
use anyhow::{bail, Context, Result};
use std::path::{Path, PathBuf};

use crate::payload;
use crate::win;

pub struct InstallOpts {
    pub instdir: PathBuf,
    pub autostart: bool,
    pub controlplane: bool,
    pub cp_url: Option<String>,
    pub no_shortcut: bool,
    pub silent: bool,
    pub relaunch: bool,
    pub relaunch_args: String,
}

/// Step ids the UI renders; keep this list stable — progress is index/total.
pub const INSTALL_STEPS: &[&str] = &[
    "webview2",
    "migrate",
    "drain",
    "files",
    "datadirs",
    "serverurl",
    "registry",
    "services",
    "shortcuts",
    "launch",
];

/// Files the installer owns inside $INSTDIR — uninstall removes only
/// these, never a whole dir that may hold foreign files.
const KNOWN_INSTDIR_FILES: &[&str] = &[
    "varde-agent.exe",
    "varde-mesh.exe",
    "varde-tray.exe",
    "varde-control-plane.exe",
    "uninstall.exe",
    "varde.ico",
    "LICENSE",
    "MicrosoftEdgeWebview2Setup.exe",
];

pub type Progress<'a> = dyn FnMut(&str) + 'a;
/// Return true to keep a plain-http server.url (the NSIS Yes/No prompt;
/// silent installs never ask and treat it as No).
pub type Confirm<'a> = dyn FnMut(&str) -> bool + 'a;

fn step(prog: &mut Progress, id: &str) {
    prog(id);
}

/// Resolve the effective component set: /TASKS wins outright (Inno
/// semantics — the flag IS the whole set), else remembered selections,
/// else defaults (autostart on, control plane off).
pub fn resolve_components(tasks: Option<&str>) -> (bool, bool) {
    if let Some(t) = tasks {
        return (t.contains("autostart"), t.contains("controlplane"));
    }
    if let Some(saved) = win::remembered_components() {
        return (saved.contains("autostart"), saved.contains("controlplane"));
    }
    (true, false)
}

/// http:// + non-local host → ask before keeping (interactive) or skip
/// (silent). Localhost-boundary-checked like VardeCheckPlainHttp.
fn is_remote_plain_http(url: &str) -> bool {
    let u = url.to_lowercase();
    if !u.starts_with("http://") {
        return false;
    }
    if u == "http://localhost" || u == "http://[::1]" {
        return false;
    }
    if u.starts_with("http://localhost:")
        || u.starts_with("http://localhost/")
        || u.starts_with("http://127.")
        || u.starts_with("http://[::1]:")
        || u.starts_with("http://[::1]/")
    {
        return false;
    }
    true
}

fn write_server_url(o: &InstallOpts, confirm: &mut Confirm) -> Result<()> {
    // already linked? never overwrite the machine's own server choice
    if win::data_dir().join("config.toml").exists() {
        return Ok(());
    }
    let mut url = o.cp_url.clone().unwrap_or_default();
    if url.is_empty() && o.controlplane {
        url = "http://localhost:8080".to_string();
    }
    if url.is_empty() {
        return Ok(());
    }
    if is_remote_plain_http(&url) && !confirm(&url) {
        return Ok(());
    }
    std::fs::write(win::data_dir().join("server.url"), format!("{url}\r\n"))?;
    Ok(())
}

/// Split an ARP UninstallString into (exe, args) without a shell — the
/// registry value is opaque input, so never route it through cmd /C.
fn split_uninstall_string(s: &str) -> (PathBuf, Vec<String>) {
    let s = s.trim();
    if let Some(rest) = s.strip_prefix('"') {
        if let Some(end) = rest.find('"') {
            return (
                PathBuf::from(&rest[..end]),
                rest[end + 1..]
                    .split_whitespace()
                    .map(String::from)
                    .collect(),
            );
        }
    }
    let mut it = s.split_whitespace();
    (
        PathBuf::from(it.next().unwrap_or("")),
        it.map(String::from).collect(),
    )
}

fn run_uninstall_string(uninstall: &str, extra: &str) -> Result<()> {
    let (exe, mut args) = split_uninstall_string(uninstall);
    args.extend(extra.split_whitespace().map(String::from));
    use std::os::windows::process::CommandExt;
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    let st = std::process::Command::new(&exe)
        .args(&args)
        .creation_flags(CREATE_NO_WINDOW)
        .status()
        .context("run legacy uninstaller")?;
    if !st.success() {
        bail!("legacy uninstaller exited with {st}");
    }
    Ok(())
}

/// Run the old NSIS uninstaller silently and wait for it to actually finish.
///
/// The NSIS stub copies itself to %TEMP% and the copy does the real work
/// (service drain, file and key removal) — the process we spawn can exit
/// early, and its exit code doesn't always reflect the copy's outcome. So
/// we wait on the observable end state instead: the legacy ARP key gone or
/// the uninstaller binary deleted from the install dir.
fn run_nsis_uninstall(uninstall: &str) -> Result<()> {
    let exe = uninstall.trim().trim_matches('"');
    let exe_path = Path::new(exe).to_path_buf();
    use std::os::windows::process::CommandExt;
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    let mut child = std::process::Command::new(&exe_path)
        .arg("/S")
        .creation_flags(CREATE_NO_WINDOW)
        .spawn()
        .context("spawn NSIS uninstaller")?;
    let _ = child.wait();
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(120);
    loop {
        if win::read_hklm(win::UNINST_KEY, "UninstallString").is_none() || !exe_path.exists() {
            break;
        }
        if std::time::Instant::now() >= deadline {
            bail!("NSIS uninstaller did not finish within 120s");
        }
        std::thread::sleep(std::time::Duration::from_millis(500));
    }
    // The detached copy deletes its own stub early — the dir going empty is
    // the true end of its work; wait for that (or the dir gone) plus a beat
    // before we start writing the new files on top of it.
    if let Some(dir) = exe_path.parent() {
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(60);
        loop {
            let done = match std::fs::read_dir(dir) {
                Err(_) => true,            // dir gone
                Ok(rd) => rd.count() == 0, // or emptied
            };
            if done {
                break;
            }
            if std::time::Instant::now() >= deadline {
                break; // leftovers get drain-and-swapped by our own uninstall path anyway
            }
            std::thread::sleep(std::time::Duration::from_millis(500));
        }
    }
    std::thread::sleep(std::time::Duration::from_millis(1500));
    Ok(())
}

pub fn install(o: &InstallOpts, prog: &mut Progress, confirm: &mut Confirm) -> Result<()> {
    // WebView2 first — the section order of the NSIS template (WebView2
    // before the Varde section that drains services).
    step(prog, "webview2");
    if !win::webview2_present() {
        // unpredictable temp name: a shared %TEMP% path with a fixed name is
        // replaceable between extract and launch
        let tmp = std::env::temp_dir().join(format!(
            "varde-webview2-setup-{}-{}.exe",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_millis())
                .unwrap_or_default()
        ));
        extract_one("MicrosoftEdgeWebview2Setup.exe", &tmp)?;
        let r = win::run_bootstrapper(&tmp).context("WebView2 bootstrapper failed");
        let _ = std::fs::remove_file(&tmp);
        r?;
    }

    // Inno → bespoke migration: the Inno install isn't visible to our own
    // upgrade detection; uninstall it silently — it drains services itself
    // and keeps %ProgramData%\Varde.
    step(prog, "migrate");
    if let Some(u) = win::inno_uninstall_string() {
        run_uninstall_string(&u, "/VERYSILENT /SUPPRESSMSGBOXES /NORESTART")
            .context("Inno uninstall")?;
    }
    // NSIS → bespoke migration: run the old uninstaller silently; it drains
    // services, removes files/keys/shortcuts and keeps %ProgramData%\Varde
    // (the data prompt only exists interactively). Remembered components
    // were already resolved before this point — its key wipe is safe.
    if let Some(u) = win::legacy_nsis_uninstall_string() {
        run_nsis_uninstall(&u).context("NSIS uninstall")?;
    }

    // drain-and-swap: tray → agent → CP, then replace files
    step(prog, "drain");
    win::kill_tray();
    let agent = o.instdir.join("varde-agent.exe");
    let cp = o.instdir.join("varde-control-plane.exe");
    win::service_uninstall(&agent).context("stop VardeAgent")?;
    win::service_uninstall(&cp).context("stop VardeControlPlane")?;

    step(prog, "files");
    let bytes = payload::extract_to(&o.instdir).context("extract payload")?;

    step(prog, "datadirs");
    for (d, extra) in [
        (win::data_dir(), None),
        (win::data_dir().join("identity"), None),
        (
            win::data_dir().join("logs"),
            Some("*S-1-5-32-545:(OI)(CI)RX"),
        ),
    ] {
        std::fs::create_dir_all(&d)?;
        win::acl_dir(&d, extra)?;
    }

    step(prog, "serverurl");
    write_server_url(o, confirm)?;

    step(prog, "registry");
    win::write_classes_varde(&o.instdir.join(win::MAIN_BINARY))?;
    if o.autostart {
        win::hklm_set(
            win::RUN_KEY,
            "Varde",
            &format!(
                "\"{}\" --autostart",
                o.instdir.join(win::MAIN_BINARY).display()
            ),
        )?;
    } else {
        // upgrade with it unticked: drop the earlier value
        win::hklm_del_value(win::RUN_KEY, "Varde");
    }

    // control plane component
    if o.controlplane {
        std::fs::create_dir_all(win::cp_data_dir())?;
        win::acl_dir(&win::cp_data_dir(), None)?;
        win::service_install(&cp).context("VardeControlPlane install")?;
        let _ = win::sc_start("VardeControlPlane");
    } else {
        // unticked on upgrade: service already removed above; drop binary
        let _ = std::fs::remove_file(&cp);
        // drop the bundled-CP default server.url, but only when it is
        // exactly the value the component wrote (any other loopback is a
        // deliberate choice we must not touch); explicit /CPURL always wins
        if o.cp_url.is_none() {
            let f = win::data_dir().join("server.url");
            if let Ok(s) = std::fs::read_to_string(&f) {
                let t = s.trim().to_lowercase();
                if t == "http://localhost:8080" || t == "http://localhost:8080/" {
                    let _ = std::fs::remove_file(&f);
                }
            }
        }
    }

    step(prog, "services");
    win::service_install(&agent).context("VardeAgent install")?;
    let _ = win::sc_start("VardeAgent");

    // product key + ARP
    win::hklm_set(win::MANUPRODUCT_KEY, "", &o.instdir.to_string_lossy())?;
    win::hklm_set(win::MANUPRODUCT_KEY, "SetupEngine", "varde-setup")?;
    win::write_arp(&o.instdir, env!("CARGO_PKG_VERSION"), (bytes / 1024) as u32)?;

    step(prog, "shortcuts");
    if !o.no_shortcut {
        let _ = win::create_shortcut(&win::start_menu_lnk(), &o.instdir.join(win::MAIN_BINARY));
        if o.silent {
            // NSIS created a desktop shortcut for silent/passive installs
            // because no finish page offers one.
            let _ = win::create_shortcut(
                &win::public_desktop_lnk(),
                &o.instdir.join(win::MAIN_BINARY),
            );
        }
    }

    // remember selections for the next install (present-but-empty = all off)
    let mut sel = String::new();
    if o.autostart {
        sel.push_str("autostart");
    }
    if o.controlplane {
        if !sel.is_empty() {
            sel.push(' ');
        }
        sel.push_str("controlplane");
    }
    win::hklm_set(win::COMPONENTS_KEY, "Components", &sel)?;

    step(prog, "launch");
    if o.silent && o.relaunch {
        // /R + /ARGS — the updater's post-install relaunch of the tray
        let _ = win::launch_as_user(&o.instdir.join(win::MAIN_BINARY), &o.relaunch_args);
    }
    Ok(())
}

/// Interactive finish (the "Open Varde" tick): start the tray as the
/// signed-in user, straight into linking when this PC isn't linked yet.
pub fn launch_tray(instdir: &Path) -> Result<()> {
    let args = if win::data_dir().join("config.toml").exists() {
        ""
    } else {
        "--link"
    };
    win::launch_as_user(&instdir.join(win::MAIN_BINARY), args)
}

pub(crate) fn extract_one(name: &str, dest: &Path) -> Result<()> {
    #[cfg(feature = "payload")]
    use std::io::Read;
    #[cfg(not(feature = "payload"))]
    {
        let _ = (name, dest);
        bail!("no payload in this build")
    }
    #[cfg(feature = "payload")]
    {
        let cur = std::io::Cursor::new(crate::payload::zip_bytes());
        let mut z = zip::ZipArchive::new(cur)?;
        let mut f = z.by_name(name)?;
        let mut buf = Vec::new();
        f.read_to_end(&mut buf)?;
        std::fs::write(dest, &buf)?;
        Ok(())
    }
}

pub const UNINSTALL_STEPS: &[&str] = &["drain", "registry", "files", "data"];

/// Full uninstall. `update` = /UPDATE parity (keep shortcuts/data/registry
/// so a chained reinstall leaves no trace behind); silent never deletes data.
pub fn uninstall(
    instdir: &Path,
    delete_data: bool,
    update: bool,
    prog: &mut Progress,
) -> Result<()> {
    step(prog, "drain");
    win::kill_tray();
    win::service_uninstall(&instdir.join("varde-control-plane.exe"))?;
    win::service_uninstall(&instdir.join("varde-agent.exe"))?;

    step(prog, "registry");
    win::hklm_del_value(win::RUN_KEY, "Varde");
    win::hklm_del_key(win::CLASSES_KEY);
    win::hklm_del_key(win::COMPONENTS_KEY);

    step(prog, "files");
    if instdir.exists() {
        // Only Varde-owned files: a custom /D= dir may hold unrelated
        // content that must survive uninstall. Known payload names plus
        // logs and the staged uninstaller pattern.
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(30);
        loop {
            let mut left: Vec<String> = Vec::new();
            for name in KNOWN_INSTDIR_FILES {
                let p = instdir.join(name);
                if p.is_file() && std::fs::remove_file(&p).is_err() {
                    left.push(name.to_string());
                }
            }
            // anything we still own but couldn't delete? retry bounded
            if left.is_empty() {
                break;
            }
            if std::time::Instant::now() >= deadline {
                bail!(
                    "remove {}: access denied (left: {})",
                    instdir.display(),
                    left.join(",")
                );
            }
            std::thread::sleep(std::time::Duration::from_millis(400));
        }
        // remove the dir only when Varde files were the last thing in it —
        // foreign files keep their home; read_dir failing means it's gone
        if let Ok(rd) = std::fs::read_dir(instdir) {
            if rd.count() == 0 {
                let _ = std::fs::remove_dir(instdir);
            }
        }
    }
    if !update {
        let _ = std::fs::remove_file(win::start_menu_lnk());
        let _ = std::fs::remove_dir(win::start_menu_dir());
        let _ = std::fs::remove_file(win::public_desktop_lnk());
        win::hklm_del_key(win::UNINST_KEY);
        win::hkcu_del_value(win::RUN_KEY, "Varde");
    }

    step(prog, "data");
    if delete_data && !update {
        let _ = std::fs::remove_dir_all(win::data_dir());
        let _ = std::fs::remove_dir_all(win::cp_data_dir());
        win::hklm_del_key(win::MANUPRODUCT_KEY);
        win::hkcu_del_key(win::MANUPRODUCT_KEY);
    }
    Ok(())
}
