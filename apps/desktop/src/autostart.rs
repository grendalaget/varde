//! "Start at login". The installer may add a machine-wide Run entry (all
//! users); each user can opt out without admin rights, or opt in with a
//! per-user Run entry when there's no machine-wide one. On macOS the same
//! model maps to a per-user LaunchAgent plus an opt-out marker — the first
//! app launch installs it, matching the Windows installer's default.

use std::io;

#[cfg(windows)]
mod imp {
    use std::io;
    use winreg::enums::{HKEY_CURRENT_USER, HKEY_LOCAL_MACHINE};
    use winreg::RegKey;

    const RUN: &str = r"Software\Microsoft\Windows\CurrentVersion\Run";
    const VALUE: &str = "Varde";
    const PREFS: &str = r"Software\Varde\Tray";
    const PREF: &str = "StartAtLogin";

    fn has_run(hive: isize) -> bool {
        RegKey::predef(hive as _)
            .open_subkey(RUN)
            .and_then(|k| k.get_value::<String, _>(VALUE))
            .is_ok()
    }

    pub fn opted_out() -> bool {
        RegKey::predef(HKEY_CURRENT_USER)
            .open_subkey(PREFS)
            .and_then(|k| k.get_value::<u32, _>(PREF))
            .is_ok_and(|v| v == 0)
    }

    pub fn enabled() -> bool {
        (has_run(HKEY_LOCAL_MACHINE as isize) && !opted_out())
            || has_run(HKEY_CURRENT_USER as isize)
    }

    pub fn set(on: bool) -> io::Result<()> {
        let hkcu = RegKey::predef(HKEY_CURRENT_USER);
        let (prefs, _) = hkcu.create_subkey(PREFS)?;
        prefs.set_value(PREF, &u32::from(on))?;
        let (run, _) = hkcu.create_subkey(RUN)?;
        if on && !has_run(HKEY_LOCAL_MACHINE as isize) {
            let exe = std::env::current_exe()?;
            run.set_value(VALUE, &format!("\"{}\" --autostart", exe.display()))?;
        } else {
            match run.delete_value(VALUE) {
                Err(e) if e.kind() != io::ErrorKind::NotFound => return Err(e),
                _ => {}
            }
        }
        Ok(())
    }
}

#[cfg(target_os = "macos")]
mod imp {
    use std::io;
    use std::path::PathBuf;

    const LABEL: &str = "games.varde.tray";

    fn plist_path() -> io::Result<PathBuf> {
        let home = std::env::var_os("HOME")
            .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "HOME is not set"))?;
        Ok(PathBuf::from(home)
            .join("Library/LaunchAgents")
            .join(format!("{LABEL}.plist")))
    }

    /// The opt-out marker: deleting the LaunchAgent alone can't distinguish
    /// "never installed" from "turned off", so the choice persists here.
    fn marker() -> PathBuf {
        agent_ipc::default_data_dir().join("tray.no-autostart")
    }

    pub fn opted_out() -> bool {
        marker().exists()
    }

    pub fn enabled() -> bool {
        plist_path().map(|p| p.exists()).unwrap_or(false) && !opted_out()
    }

    fn launchctl(args: &[&str]) -> io::Result<()> {
        let st = std::process::Command::new("launchctl")
            .args(args)
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()?;
        if st.success() {
            return Ok(());
        }
        Err(io::Error::other(format!(
            "launchctl {} -> {st}",
            args.join(" ")
        )))
    }

    fn gui_domain() -> String {
        // SAFETY: getuid has no failure modes.
        format!("gui/{}", unsafe { libc::getuid() })
    }

    fn install() -> io::Result<()> {
        let path = plist_path()?;
        if let Some(dir) = path.parent() {
            std::fs::create_dir_all(dir)?;
        }
        let exe = std::env::current_exe()?;
        let esc = |s: &str| {
            s.replace('&', "&amp;")
                .replace('<', "&lt;")
                .replace('>', "&gt;")
        };
        let plist = format!(
            concat!(
                "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n",
                "<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n",
                "<plist version=\"1.0\">\n<dict>\n",
                "\t<key>Label</key><string>{}</string>\n",
                "\t<key>ProgramArguments</key>\n\t<array>\n",
                "\t\t<string>{}</string>\n\t\t<string>--autostart</string>\n\t</array>\n",
                "\t<key>RunAtLoad</key><true/>\n",
                "</dict>\n</plist>\n"
            ),
            LABEL,
            esc(&exe.display().to_string()),
        );
        std::fs::write(&path, plist)?;
        // launchd may have an unloaded record from a stale plist; a failed
        // bootstrap just means it's already registered — the file is truth.
        let _ = launchctl(&[
            "bootstrap",
            &gui_domain(),
            path.to_str()
                .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "bad plist path"))?,
        ]);
        Ok(())
    }

    fn uninstall() -> io::Result<()> {
        let path = plist_path()?;
        let _ = launchctl(&["bootout", &format!("{}/{LABEL}", gui_domain())]);
        match std::fs::remove_file(&path) {
            Err(e) if e.kind() != io::ErrorKind::NotFound => Err(e),
            _ => Ok(()),
        }
    }

    pub fn set(on: bool) -> io::Result<()> {
        if on {
            let _ = std::fs::remove_file(marker());
            install()
        } else {
            if let Some(dir) = marker().parent() {
                let _ = std::fs::create_dir_all(dir);
            }
            let _ = std::fs::write(marker(), b"");
            uninstall()
        }
    }

    /// First-launch install: the drag-install has no installer step, so the
    /// app itself lays down the login LaunchAgent unless the user opted out.
    pub fn ensure_installed() -> io::Result<()> {
        if opted_out() || plist_path()?.exists() {
            return Ok(());
        }
        install()
    }
}

pub fn opted_out() -> bool {
    imp::opted_out()
}

pub fn enabled() -> bool {
    imp::enabled()
}

pub fn set(on: bool) -> io::Result<()> {
    imp::set(on)
}

/// First-run setup on a drag-installed .app: install the login integration
/// unless the user opted out (Windows gets this from the installer instead).
#[cfg(target_os = "macos")]
pub fn ensure_installed() -> io::Result<()> {
    imp::ensure_installed()
}
