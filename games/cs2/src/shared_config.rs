use std::fs::{File, OpenOptions};
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};

use crate::path_error;

const MARKER: &[u8] = b"// varde-managed:";

pub struct SharedConfig {
    path: PathBuf,
    _lock: Option<File>,
}

pub(crate) fn lock_path(path: &Path) -> io::Result<PathBuf> {
    let name = path
        .file_name()
        .ok_or_else(|| io::Error::other("CS2 config has no filename"))?;
    let mut lock_name = std::ffi::OsString::from(".");
    lock_name.push(name);
    lock_name.push(".lock");
    Ok(path.with_file_name(lock_name))
}

fn lock_file(path: &Path) -> io::Result<(PathBuf, File)> {
    let path = lock_path(path)?;
    #[cfg(windows)]
    clear_readonly(&path, |_, metadata| Ok(metadata.len() == 0))?;
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(&path)
        .map_err(|e| path_error("open CS2 startup config lock", &path, e))?;
    Ok((path, file))
}

fn generated_name(name: &str) -> bool {
    name.strip_prefix("varde_")
        .and_then(|id| id.strip_suffix(".cfg"))
        .is_some_and(|id| {
            !id.is_empty()
                && id
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'-'))
        })
}

#[cfg(windows)]
fn generated_lock_name(name: &str) -> bool {
    name.strip_prefix('.')
        .and_then(|n| n.strip_suffix(".lock"))
        .is_some_and(generated_name)
}

fn managed(path: &Path) -> io::Result<bool> {
    let mut header = [0; MARKER.len() + 32 + 1];
    let result = File::open(path).and_then(|mut file| file.read_exact(&mut header));
    match result {
        Ok(()) => Ok(header.starts_with(MARKER)
            && header[MARKER.len()..header.len() - 1]
                .iter()
                .all(u8::is_ascii_hexdigit)
            && header.last() == Some(&b'\n')),
        Err(error)
            if matches!(
                error.kind(),
                io::ErrorKind::NotFound | io::ErrorKind::UnexpectedEof
            ) =>
        {
            Ok(false)
        }
        Err(error) => Err(path_error(
            "read CS2 startup config ownership marker",
            path,
            error,
        )),
    }
}

impl SharedConfig {
    pub fn remove_on_drop(path: PathBuf) -> Self {
        Self { path, _lock: None }
    }

    pub fn write(path: PathBuf, contents: &str) -> io::Result<Self> {
        let (lock_path, lock) = lock_file(&path)?;
        fs2::FileExt::try_lock_exclusive(&lock)
            .map_err(|e| path_error("lock CS2 startup config", &lock_path, e))?;
        let mut options = OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut file = options
            .open(&path)
            .map_err(|e| path_error("create CS2 startup config", &path, e))?;
        let guard = Self {
            path,
            _lock: Some(lock),
        };
        file.write_all(MARKER)
            .and_then(|()| writeln!(file, "{:032x}", rand::random::<u128>()))
            .and_then(|()| file.write_all(contents.as_bytes()))
            .map_err(|e| path_error("write CS2 startup config", &guard.path, e))?;
        Ok(guard)
    }

    pub fn sweep(dir: &Path, required_config: Option<&str>) -> io::Result<()> {
        let entries = match std::fs::read_dir(dir) {
            Ok(entries) => entries,
            Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(()),
            Err(error) => return Err(path_error("list CS2 startup configs", dir, error)),
        };
        for entry in entries {
            let entry = entry.map_err(|e| path_error("read CS2 config directory entry", dir, e))?;
            let path = entry.path();
            let name = entry.file_name();
            let Some(name) = name.to_str().filter(|name| generated_name(name)) else {
                continue;
            };
            let result = (|| {
                if !entry
                    .file_type()
                    .map_err(|e| path_error("inspect CS2 config directory entry", &path, e))?
                    .is_file()
                {
                    return Ok(());
                }
                let (lock_path, lock) = lock_file(&path)?;
                match fs2::FileExt::try_lock_exclusive(&lock) {
                    Ok(()) if managed(&path)? => remove(&path),
                    Ok(()) => Ok(()),
                    Err(error)
                        if error.raw_os_error() == fs2::lock_contended_error().raw_os_error() =>
                    {
                        Ok(())
                    }
                    Err(error) => Err(path_error(
                        "lock stale CS2 startup config",
                        &lock_path,
                        error,
                    )),
                }
            })();
            match result {
                Err(error)
                    if required_config
                        .is_some_and(|current| !name.eq_ignore_ascii_case(current)) =>
                {
                    tracing::warn!(%error, path = %path.display(), "skipping another server's CS2 startup config");
                }
                result => result?,
            }
        }
        Ok(())
    }
}

pub(crate) fn remove(path: &Path) -> io::Result<()> {
    #[cfg(windows)]
    clear_readonly(path, |path, _| managed(path))?;
    match std::fs::remove_file(path) {
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        result => result.map_err(|e| path_error("remove CS2 startup config", path, e)),
    }
}

#[cfg(windows)]
#[allow(clippy::permissions_set_readonly_false)]
fn clear_readonly(
    path: &Path,
    owned: impl FnOnce(&Path, &std::fs::Metadata) -> io::Result<bool>,
) -> io::Result<()> {
    use std::os::windows::fs::MetadataExt;

    let metadata = match std::fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(path_error("inspect CS2 startup file attributes", path, e)),
    };
    const FILE_ATTRIBUTE_REPARSE_POINT: u32 = 0x400;
    let mut permissions = metadata.permissions();
    if metadata.is_file()
        && metadata.file_attributes() & FILE_ATTRIBUTE_REPARSE_POINT == 0
        && permissions.readonly()
        && owned(path, &metadata)?
    {
        permissions.set_readonly(false);
        std::fs::set_permissions(path, permissions)
            .map_err(|e| path_error("clear read-only CS2 startup file attribute", path, e))?;
    }
    Ok(())
}

#[cfg(windows)]
pub(crate) fn repair_managed_file(
    path: &Path,
    lock: bool,
    grant: impl FnOnce(&Path) -> io::Result<()>,
) -> io::Result<()> {
    use std::os::windows::fs::MetadataExt;

    const FILE_ATTRIBUTE_REPARSE_POINT: u32 = 0x400;
    let metadata = match std::fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(path_error("inspect CS2 startup file", path, e)),
    };
    let owned = |path: &Path, metadata: &std::fs::Metadata| {
        if lock {
            Ok(metadata.len() == 0)
        } else {
            managed(path)
        }
    };
    if metadata.is_file()
        && metadata.file_attributes() & FILE_ATTRIBUTE_REPARSE_POINT == 0
        && owned(path, &metadata)?
    {
        grant(path)?;
        clear_readonly(path, owned)?;
    }
    Ok(())
}

#[cfg(windows)]
pub(crate) fn repair_managed_files(
    dir: &Path,
    mut grant: impl FnMut(&Path) -> io::Result<()>,
) -> io::Result<()> {
    let entries =
        std::fs::read_dir(dir).map_err(|e| path_error("list CS2 startup configs", dir, e))?;
    for entry in entries {
        let entry = entry.map_err(|e| path_error("read CS2 config directory entry", dir, e))?;
        let name = entry.file_name();
        let Some(name) = name.to_str() else { continue };
        let lock = generated_lock_name(name);
        if !lock && !generated_name(name) {
            continue;
        }
        let path = entry.path();
        if let Err(error) = repair_managed_file(&path, lock, &mut grant) {
            tracing::warn!(%error, path = %path.display(), "could not repair CS2 startup file");
        }
    }
    Ok(())
}

impl Drop for SharedConfig {
    fn drop(&mut self) {
        if let Err(error) = remove(&self.path) {
            tracing::warn!(%error, path = %self.path.display(), "CS2 startup config cleanup failed");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[cfg(windows)]
    #[test]
    fn repair_and_sweep_remove_readonly_only_from_varde_files() {
        fn set_readonly(path: &Path, readonly: bool) {
            let mut perms = std::fs::metadata(path).unwrap().permissions();
            perms.set_readonly(readonly);
            std::fs::set_permissions(path, perms).unwrap();
        }

        let root = tempfile::tempdir().unwrap();
        let cfg = root.path().join("varde_stale.cfg");
        let lock = lock_path(&cfg).unwrap();
        let user_cfg = root.path().join("varde_user.cfg");
        let orphan = root.path().join(".varde_orphan.cfg.lock");
        std::fs::write(&cfg, format!("// varde-managed:{:032x}\n", 1)).unwrap();
        std::fs::write(&lock, "").unwrap();
        std::fs::write(&user_cfg, "user configuration").unwrap();
        std::fs::write(&orphan, "").unwrap();
        for file in [&cfg, &lock, &user_cfg] {
            set_readonly(file, true);
        }

        let mut granted = Vec::new();
        repair_managed_files(root.path(), |path| {
            if path == orphan {
                return Err(io::ErrorKind::PermissionDenied.into());
            }
            granted.push(path.to_path_buf());
            Ok(())
        })
        .unwrap();
        assert_eq!(granted.len(), 2);
        assert!(granted.contains(&cfg));
        assert!(granted.contains(&lock));
        assert!(!std::fs::metadata(&cfg).unwrap().permissions().readonly());
        assert!(!std::fs::metadata(&lock).unwrap().permissions().readonly());
        assert!(std::fs::metadata(&user_cfg)
            .unwrap()
            .permissions()
            .readonly());

        set_readonly(&cfg, true);
        set_readonly(&lock, true);
        SharedConfig::sweep(root.path(), None).unwrap();
        assert!(!cfg.exists());
        assert!(std::fs::metadata(&user_cfg)
            .unwrap()
            .permissions()
            .readonly());
        set_readonly(&user_cfg, false);
    }

    #[cfg(windows)]
    #[test]
    fn sweep_skips_an_unusable_lock_only_for_another_server() {
        let root = tempfile::tempdir().unwrap();
        let cfg = root.path().join("varde_blocked.cfg");
        let lock = lock_path(&cfg).unwrap();
        std::fs::write(&cfg, format!("// varde-managed:{:032x}\n", 1)).unwrap();
        std::fs::create_dir(&lock).unwrap();
        SharedConfig::sweep(root.path(), Some("varde_current.cfg")).unwrap();
        assert!(cfg.exists());
        let error = SharedConfig::sweep(root.path(), Some("varde_blocked.cfg")).unwrap_err();
        assert!(error.to_string().contains(&*lock.to_string_lossy()));
    }

    #[test]
    fn sweep_error_names_the_config_directory() {
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("not-a-directory");
        std::fs::write(&path, "").unwrap();
        let error = SharedConfig::sweep(&path, None).unwrap_err().to_string();
        assert!(error.contains("list CS2 startup configs"));
        assert!(error.contains(&*path.to_string_lossy()));
    }

    #[test]
    fn sweep_only_removes_exact_generated_names_and_preserves_live_configs() {
        let dir = tempfile::tempdir().unwrap();
        SharedConfig::sweep(&dir.path().join("missing"), None).unwrap();
        let live = SharedConfig::write(dir.path().join("varde_live.cfg"), "secret").unwrap();
        for name in [
            "varde_old.cfg",
            "varde_A-1_2.cfg",
            "varde_.cfg",
            "varde.cfg",
            "varde_old.cfg.bak",
            "varde_odd.name.cfg",
            "varde_é.cfg",
            "server.cfg",
            "varde_user.cfg",
        ] {
            std::fs::write(dir.path().join(name), "config").unwrap();
        }
        for name in ["varde_old.cfg", "varde_A-1_2.cfg"] {
            std::fs::write(
                dir.path().join(name),
                format!("// varde-managed:{:032x}\nsecret", 1),
            )
            .unwrap();
        }
        std::fs::write(dir.path().join("varde_empty.cfg"), "").unwrap();
        std::fs::write(
            dir.path().join("varde_partial.cfg"),
            "// varde-managed:abc\nsecret",
        )
        .unwrap();
        std::fs::write(
            dir.path().join("varde_bad_marker.cfg"),
            "// varde-managed:zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz\nsecret",
        )
        .unwrap();
        std::fs::create_dir(dir.path().join("varde_directory.cfg")).unwrap();
        SharedConfig::sweep(dir.path(), None).unwrap();
        for name in ["varde_old.cfg", "varde_A-1_2.cfg"] {
            assert!(!dir.path().join(name).exists());
        }
        for name in [
            "varde_live.cfg",
            "varde_.cfg",
            "varde.cfg",
            "varde_old.cfg.bak",
            "varde_odd.name.cfg",
            "varde_é.cfg",
            "server.cfg",
            "varde_directory.cfg",
            "varde_user.cfg",
            "varde_empty.cfg",
            "varde_partial.cfg",
            "varde_bad_marker.cfg",
        ] {
            assert!(dir.path().join(name).exists());
        }
        drop(live);
        assert!(!dir.path().join("varde_live.cfg").exists());
    }

    #[test]
    fn guard_cleans_on_drop_and_does_not_overwrite_existing_files() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("varde_server.cfg");
        std::fs::write(&path, "existing").unwrap();
        assert!(SharedConfig::write(path.clone(), "new").is_err());
        assert_eq!(std::fs::read_to_string(&path).unwrap(), "existing");
        SharedConfig::sweep(dir.path(), None).unwrap();
        assert_eq!(std::fs::read_to_string(&path).unwrap(), "existing");
        std::fs::remove_file(&path).unwrap();
        let guard = SharedConfig::write(path.clone(), "new").unwrap();
        assert!(managed(&path).unwrap());
        assert!(SharedConfig::write(path.clone(), "other launch").is_err());
        std::fs::remove_file(&path).unwrap();
        drop(guard);
        assert!(!path.exists());
    }
}
