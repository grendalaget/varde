use std::fs::{self, OpenOptions};
use std::io;
use std::os::windows::fs::{MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};

use executor_api::LocalPathError;

use crate::{path_error, shared_config};

const FILE_ATTRIBUTE_REPARSE_POINT: u32 = 0x400;

fn checked_cfg_dir(install: &Path, create_cfg: bool) -> io::Result<PathBuf> {
    let mut path = install.to_path_buf();
    for component in ["game", "csgo", "cfg"] {
        path.push(component);
        let metadata = match fs::symlink_metadata(&path) {
            Err(e) if create_cfg && component == "cfg" && e.kind() == io::ErrorKind::NotFound => {
                match fs::create_dir(&path) {
                    Ok(()) => (),
                    Err(e) if e.kind() == io::ErrorKind::AlreadyExists => (),
                    Err(e) => return Err(path_error("create CS2 config directory", &path, e)),
                }
                fs::symlink_metadata(&path)
            }
            result => result,
        }
        .map_err(|e| path_error("inspect CS2 config directory", &path, e))?;
        if !metadata.is_dir() || metadata.file_attributes() & FILE_ATTRIBUTE_REPARSE_POINT != 0 {
            return Err(io::Error::other(LocalPathError::new(
                "inspect CS2 config directory",
                &path,
                "not a regular directory",
            )));
        }
    }
    Ok(path)
}

fn grant_modify(icacls: &Path, path: &Path, ace: &str) -> io::Result<()> {
    use std::os::windows::process::CommandExt;

    let output = std::process::Command::new(icacls)
        .arg(path)
        .args(["/grant", ace, "/L"])
        .creation_flags(0x08000000)
        .output()
        .map_err(|e| path_error("spawn ACL repair tool", icacls, e))?;
    if !output.status.success() {
        return Err(io::Error::other(LocalPathError::new(
            "grant SYSTEM modify rights on",
            path,
            format!(
                "{}: {}",
                output.status,
                String::from_utf8_lossy(&output.stderr).trim()
            ),
        )));
    }
    Ok(())
}

pub(crate) fn repair(install: &Path) -> io::Result<()> {
    let cfg = checked_cfg_dir(install, false)?;
    repair_cfg(&cfg, None)
}

fn repair_cfg(cfg: &Path, current: Option<&Path>) -> io::Result<()> {
    let system = std::env::var_os("SystemRoot")
        .ok_or_else(|| io::Error::other("Windows system directory unavailable"))?;
    let icacls = PathBuf::from(system).join("System32/icacls.exe");
    // Files Varde creates here must inherit the grant, or SYSTEM cannot
    // delete or reopen them on a later start.
    grant_modify(&icacls, cfg, "*S-1-5-18:(OI)M")?;
    let grant = |path: &Path| grant_modify(&icacls, path, "*S-1-5-18:M");
    if let Some(config) = current {
        shared_config::repair_managed_file(config, false, grant)?;
        shared_config::repair_managed_file(&shared_config::lock_path(config)?, true, grant)
    } else {
        shared_config::repair_managed_files(cfg, grant)
    }
}

fn probe(cfg: &Path, config: &Path) -> io::Result<()> {
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
    fs::remove_file(&path)
        .map_err(|e| path_error("remove CS2 config permission probe", &path, e))?;
    const GENERIC_READ: u32 = 0x8000_0000;
    const DELETE: u32 = 0x0001_0000;
    probe_existing(
        OpenOptions::new().access_mode(GENERIC_READ | DELETE),
        config,
        "open existing CS2 startup config for reading and deletion",
    )?;
    probe_existing(
        OpenOptions::new().read(true).write(true),
        &shared_config::lock_path(config)?,
        "open existing CS2 startup config lock",
    )
}

fn probe_existing(options: &OpenOptions, path: &Path, operation: &str) -> io::Result<()> {
    match options.open(path) {
        Ok(_) => Ok(()),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(path_error(operation, path, e)),
    }
}

pub(crate) fn ensure_writable(install: &Path, name: &str) -> io::Result<()> {
    let cfg = checked_cfg_dir(install, true)
        .map_err(|e| permission_error(&install.join("game/csgo/cfg"), &e, &e))?;
    let config = cfg.join(format!("{name}.cfg"));
    match probe(&cfg, &config) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == io::ErrorKind::PermissionDenied => repair_cfg(&cfg, Some(&config))
            .and_then(|()| probe(&cfg, &config))
            .map_err(|repair| permission_error(&cfg, &e, format_args!("{e}; {repair}"))),
        Err(e) => Err(e),
    }
}

fn permission_error(cfg: &Path, error: &io::Error, detail: impl std::fmt::Display) -> io::Error {
    let path = error
        .get_ref()
        .and_then(|error| error.downcast_ref::<LocalPathError>())
        .map_or(cfg, LocalPathError::path);
    io::Error::new(
        error.kind(),
        LocalPathError::new(
            "could not access CS2 config path",
            path,
            format_args!("{detail}; reinstall or update Varde to repair CS2 access"),
        ),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn preflight_does_not_leave_a_file_in_the_steam_config_directory() {
        let root = tempfile::tempdir().unwrap();
        let cfg = root.path().join("game/csgo/cfg");
        fs::create_dir_all(&cfg).unwrap();
        ensure_writable(root.path(), "varde_current").unwrap();
        assert_eq!(fs::read_dir(&cfg).unwrap().count(), 0);
    }

    #[test]
    fn preflight_recreates_a_missing_config_directory() {
        let root = tempfile::tempdir().unwrap();
        let csgo = root.path().join("game/csgo");
        fs::create_dir_all(&csgo).unwrap();
        ensure_writable(root.path(), "varde_current").unwrap();
        assert!(csgo.join("cfg").is_dir());
    }

    #[test]
    fn probe_checks_only_the_current_servers_lock_without_rewriting_it() {
        let cfg = tempfile::tempdir().unwrap();
        let current = cfg.path().join("varde_current.cfg");
        let lock = shared_config::lock_path(&current).unwrap();
        fs::write(&lock, "").unwrap();
        let writable = fs::metadata(&lock).unwrap().permissions();
        let mut readonly = writable.clone();
        readonly.set_readonly(true);
        fs::set_permissions(&lock, readonly).unwrap();

        let unrelated = probe(cfg.path(), &cfg.path().join("varde_other.cfg"));
        let denied = probe(cfg.path(), &current);
        fs::set_permissions(&lock, writable).unwrap();
        unrelated.unwrap();
        let error = denied.unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::PermissionDenied);
        assert!(error.to_string().contains(&*lock.to_string_lossy()));
        probe(cfg.path(), &current).unwrap();
        assert_eq!(fs::read(&lock).unwrap(), b"");
        assert_eq!(fs::read_dir(cfg.path()).unwrap().count(), 1);
    }

    #[test]
    fn probe_requires_access_to_the_current_servers_existing_config() {
        let cfg = tempfile::tempdir().unwrap();
        let current = cfg.path().join("varde_current.cfg");
        let file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .share_mode(0)
            .open(&current)
            .unwrap();
        probe(cfg.path(), &cfg.path().join("varde_other.cfg")).unwrap();
        let error = probe(cfg.path(), &current).unwrap_err().to_string();
        assert!(error.contains(&*current.to_string_lossy()));
        drop(file);
        probe(cfg.path(), &current).unwrap();
    }
}
