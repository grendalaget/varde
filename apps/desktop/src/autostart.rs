//! "Start at login". The installer may add a machine-wide Run entry (all
//! users); each user can opt out without admin rights, or opt in with a
//! per-user Run entry when there's no machine-wide one.

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
    (has_run(HKEY_LOCAL_MACHINE as isize) && !opted_out()) || has_run(HKEY_CURRENT_USER as isize)
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
