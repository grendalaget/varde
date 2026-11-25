use std::fs::{self, OpenOptions};
use std::io;
use std::os::windows::fs::MetadataExt;
use std::path::{Path, PathBuf};

use crate::{path_error, shared_config};

const FILE_ATTRIBUTE_REPARSE_POINT: u32 = 0x400;

fn checked_cfg_dir(install: &Path) -> io::Result<PathBuf> {
    let mut path = install.to_path_buf();
    for component in ["game", "csgo", "cfg"] {
        path.push(component);
        let metadata = fs::symlink_metadata(&path)
            .map_err(|e| path_error("inspect CS2 config directory", &path, e))?;
        if !metadata.is_dir() || metadata.file_attributes() & FILE_ATTRIBUTE_REPARSE_POINT != 0 {
            return Err(io::Error::other(format!(
                "CS2 config directory is not a regular directory: {}",
                path.display()
            )));
        }
    }
    Ok(path)
}

pub(crate) fn repair(install: &Path) -> io::Result<()> {
    use std::os::windows::process::CommandExt;

    let cfg = checked_cfg_dir(install)?;
    let system = std::env::var_os("SystemRoot")
        .ok_or_else(|| io::Error::other("Windows system directory unavailable"))?;
    let icacls = PathBuf::from(system).join("System32/icacls.exe");
    let output = std::process::Command::new(&icacls)
        .arg(&cfg)
        .args(["/grant", "*S-1-5-18:M", "/L"])
        .creation_flags(0x08000000)
        .output()
        .map_err(|e| path_error("spawn ACL repair tool", &icacls, e))?;
    if !output.status.success() {
        return Err(io::Error::other(format!(
            "grant SYSTEM modify rights on {}: {}: {}",
            cfg.display(),
            output.status,
            String::from_utf8_lossy(&output.stderr).trim()
        )));
    }
    shared_config::clear_managed_readonly(&cfg)
}

fn probe(cfg: &Path) -> io::Result<()> {
    let path = cfg.join(format!(
        ".varde-permission-{:032x}.tmp",
        rand::random::<u128>()
    ));
    let file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&path)
        .map_err(|e| path_error("create CS2 config permission probe", &path, e))?;
    drop(file);
    fs::remove_file(&path).map_err(|e| path_error("remove CS2 config permission probe", &path, e))
}

pub(crate) fn ensure_writable(install: &Path) -> io::Result<()> {
    let cfg = checked_cfg_dir(install).map_err(|e| {
        io::Error::other(format!(
            "{e}; reinstall or update Varde to repair CS2 access"
        ))
    })?;
    match probe(&cfg) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == io::ErrorKind::PermissionDenied => repair(install)
            .and_then(|()| probe(&cfg))
            .map_err(|repair| {
                io::Error::other(format!(
                    "CS2 config directory {} is not writable; reinstall or update Varde to repair CS2 access ({e}; {repair})",
                    cfg.display()
                ))
            }),
        Err(e) => Err(e),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn preflight_does_not_leave_a_file_in_the_steam_config_directory() {
        let root = tempfile::tempdir().unwrap();
        let cfg = root.path().join("game/csgo/cfg");
        fs::create_dir_all(&cfg).unwrap();
        ensure_writable(root.path()).unwrap();
        assert_eq!(fs::read_dir(&cfg).unwrap().count(), 0);
    }
}
