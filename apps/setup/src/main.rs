// varde-setup — Varde's Windows installer and uninstaller, one binary.
//
// Flag surface (NSIS-compatible, see packaging/windows/README.md):
//   VardeSetup.exe            → interactive install
//   VardeSetup.exe /S         → silent install (the updater runs
//                               `/S /R /ARGS --autostart`)
//   uninstall.exe             → interactive uninstall (from Program Files)
//   uninstall.exe /S          → silent uninstall (always keeps app data)
//   VardeSetup.exe /uninstall → uninstall via the setup exe itself
//
// The uninstaller can't delete itself, so when run from inside the install
// dir it copies itself to %TEMP% and the copy does the work.
//
// The crate is Windows-only: everything lives behind cfg(windows) so the
// workspace still compiles on Linux/macOS CI (tauri would pull glib).
#[cfg(windows)]
mod app;
#[cfg(windows)]
mod args;
#[cfg(windows)]
mod engine;
#[cfg(windows)]
mod payload;
#[cfg(windows)]
mod win;

#[cfg(windows)]
mod imp {
    use crate::args::{Args, Mode};
    use anyhow::{Context, Result};
    use std::io::Write;
    use std::path::{Path, PathBuf};

    fn log_path() -> PathBuf {
        std::env::temp_dir().join(format!("varde-setup-{}.log", std::process::id()))
    }

    macro_rules! log {
        ($($t:tt)*) => {{
            let line = format!($($t)*);
            println!("{line}");
            if let Ok(mut f) = std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(log_path())
            {
                let _ = writeln!(f, "{line}");
            }
        }};
    }

    /// uninstall.exe running from inside $INSTDIR copies itself out first —
    /// Windows won't let the process delete its own image.
    fn maybe_relaunch_from_temp(args: &Args, mode: &Mode, instdir: &Path) -> Result<bool> {
        if *mode != Mode::Uninstall || args.target.is_some() {
            return Ok(false);
        }
        let exe = std::env::current_exe()?;
        let parent = exe
            .parent()
            .map(|p| p.canonicalize().unwrap_or_else(|_| p.to_path_buf()));
        let want = instdir
            .canonicalize()
            .unwrap_or_else(|_| instdir.to_path_buf());
        if parent != Some(want) {
            return Ok(false);
        }
        let tmp = std::env::temp_dir().join(format!("varde-uninstall-{}.exe", std::process::id()));
        // sweep staged copies from earlier runs (running ones just fail delete)
        if let Ok(rd) = std::fs::read_dir(std::env::temp_dir()) {
            for e in rd.filter_map(|x| x.ok()) {
                let name = e.file_name().to_string_lossy().to_string();
                if name.starts_with("varde-uninstall-") && name.ends_with(".exe") && e.path() != tmp
                {
                    let _ = std::fs::remove_file(e.path());
                }
            }
        }
        std::fs::copy(&exe, &tmp).context("stage uninstaller copy")?;
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        std::process::Command::new(&tmp)
            .arg("/uninstall")
            .arg(format!("/target={}", instdir.display()))
            .arg(format!("/waitpid={}", std::process::id()))
            .args(args.silent.then_some("/S"))
            .args(args.update.then_some("/UPDATE"))
            .args(args.no_shortcut.then_some("/NS"))
            .creation_flags(CREATE_NO_WINDOW)
            .spawn()
            .context("spawn staged uninstaller")?;
        // mark the staged copy for deletion at next boot - detached-cmd tricks
        // die with the spawning process under job objects
        let _ = crate::win::delete_on_reboot(&tmp);
        // exit now: while we wait our own image stays mapped and the staged
        // copy can never delete uninstall.exe (the NSIS _?= flow is the same -
        // callers get our exit, not the staged copy's)
        Ok(true)
    }

    fn run_silent_install(args: &Args) -> Result<()> {
        let instdir = crate::win::resolve_instdir(args.dir.clone());
        let (autostart, controlplane) = crate::engine::resolve_components(args.tasks.as_deref());
        let opts = crate::engine::InstallOpts {
            instdir,
            autostart,
            controlplane,
            cp_url: args.cp_url.clone(),
            no_shortcut: args.no_shortcut,
            silent: true,
            relaunch: args.relaunch,
            relaunch_args: args.relaunch_args.clone(),
            launch_tray: false,
        };
        let mut prog = |id: &str| log!("[setup] {id}");
        let mut confirm = |_: &str| false; // silent never asks; Inno default was No
        crate::engine::install(&opts, &mut prog, &mut confirm)
    }

    fn run_silent_uninstall(args: &Args, instdir: &Path) -> Result<()> {
        log!("[uninstall] instdir={}", instdir.display());
        let mut prog = |id: &str| log!("[uninstall] {id}");
        // silent uninstalls keep app data — same rule as the NSIS hooks
        crate::engine::uninstall(instdir, false, args.update, &mut prog)
    }

    pub(crate) fn main() {
        let args = crate::args::parse();
        // infer mode from the binary name so payload\uninstall.exe needs no flag
        let mode = if args.mode == Mode::Uninstall {
            Mode::Uninstall
        } else {
            std::env::current_exe()
                .ok()
                .and_then(|p| p.file_stem().map(|s| s.to_string_lossy().to_lowercase()))
                .map(|s| {
                    if s.contains("uninstall") {
                        Mode::Uninstall
                    } else {
                        Mode::Install
                    }
                })
                .unwrap_or(Mode::Install)
        };

        if !crate::win::is_admin() {
            // requireAdministrator in the manifest normally guarantees this; a
            // dev build without it should fail loudly rather than half-install.
            eprintln!("Varde setup must run as administrator.");
            std::process::exit(5);
        }

        let instdir = args
            .target
            .clone()
            .unwrap_or_else(|| crate::win::resolve_instdir(args.dir.clone()));

        // staged uninstaller: wait for the parent (the instdir uninstall.exe
        // that spawned us) to exit so its image unmaps before we delete it
        if let Some(pid) = args.wait_pid {
            crate::win::wait_for_process_exit(pid);
        }

        // staged-temp-copy relocation for the uninstaller living in $INSTDIR —
        // it spawns its worker (or replaces the process entirely when silent)
        match maybe_relaunch_from_temp(&args, &mode, &instdir) {
            Ok(true) => return,
            Ok(false) => {}
            Err(e) => eprintln!("relaunch: {e:#}"),
        }

        let silent = args.silent || args.passive;
        if silent {
            crate::win::attach_parent_console();
            let r = match mode {
                Mode::Install => run_silent_install(&args),
                Mode::Uninstall => run_silent_uninstall(&args, &instdir),
            };
            match r {
                Ok(()) => {
                    log!("done");
                    std::process::exit(0);
                }
                Err(e) => {
                    log!("error: {e:#}");
                    std::process::exit(1);
                }
            }
        }

        // Interactive UI needs WebView2 — a fresh box may not have it, and
        // the engine's bootstrap step only runs after Install is clicked.
        // Bootstrap first so the window can actually open.
        #[cfg(feature = "payload")]
        if mode == Mode::Install && !crate::win::webview2_present() {
            let tmp = std::env::temp_dir().join(format!(
                "varde-webview2-setup-{}-{}.exe",
                std::process::id(),
                std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .map(|d| d.as_millis())
                    .unwrap_or_default()
            ));
            if let Err(e) = crate::engine::extract_one("MicrosoftEdgeWebview2Setup.exe", &tmp)
                .and_then(|_| crate::win::run_bootstrapper(&tmp))
            {
                eprintln!("WebView2 bootstrap failed: {e:#}");
            }
            let _ = std::fs::remove_file(&tmp);
        }

        crate::app::run(args, instdir, mode);
    }
}

#[cfg(windows)]
fn main() {
    imp::main()
}

#[cfg(not(windows))]
fn main() {
    eprintln!("varde-setup is a Windows-only binary");
    std::process::exit(1);
}
