use std::fs::{File, OpenOptions};
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};

const MARKER: &[u8] = b"// varde-managed:";

pub struct SharedConfig {
    path: PathBuf,
    _lock: Option<File>,
}

fn lock_file(path: &Path) -> io::Result<File> {
    let name = path
        .file_name()
        .ok_or_else(|| io::Error::other("CS2 config has no filename"))?;
    let mut lock_name = std::ffi::OsString::from(".");
    lock_name.push(name);
    lock_name.push(".lock");
    OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(path.with_file_name(lock_name))
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
        Err(error) => Err(error),
    }
}

impl SharedConfig {
    pub fn remove_on_drop(path: PathBuf) -> Self {
        Self { path, _lock: None }
    }

    pub fn write(path: PathBuf, contents: &str) -> io::Result<Self> {
        let lock = lock_file(&path)?;
        fs2::FileExt::try_lock_exclusive(&lock)?;
        let mut options = OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut file = options.open(&path)?;
        let guard = Self {
            path,
            _lock: Some(lock),
        };
        file.write_all(MARKER)?;
        writeln!(file, "{:032x}", rand::random::<u128>())?;
        file.write_all(contents.as_bytes())?;
        Ok(guard)
    }

    pub fn sweep(dir: &Path) -> io::Result<()> {
        let entries = match std::fs::read_dir(dir) {
            Ok(entries) => entries,
            Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(()),
            Err(error) => return Err(error),
        };
        for entry in entries {
            let entry = entry?;
            if !entry.file_type()?.is_file()
                || !entry.file_name().to_str().is_some_and(generated_name)
            {
                continue;
            }
            let path = entry.path();
            let lock = lock_file(&path)?;
            match fs2::FileExt::try_lock_exclusive(&lock) {
                Ok(()) => {
                    if managed(&path)? {
                        remove(&path)?;
                    }
                }
                Err(error)
                    if error.raw_os_error() == fs2::lock_contended_error().raw_os_error() => {}
                Err(error) => return Err(error),
            }
        }
        Ok(())
    }
}

fn remove(path: &Path) -> io::Result<()> {
    match std::fs::remove_file(path) {
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        result => result,
    }
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

    #[test]
    fn sweep_only_removes_exact_generated_names_and_preserves_live_configs() {
        let dir = tempfile::tempdir().unwrap();
        SharedConfig::sweep(&dir.path().join("missing")).unwrap();
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
        SharedConfig::sweep(dir.path()).unwrap();
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
        SharedConfig::sweep(dir.path()).unwrap();
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
