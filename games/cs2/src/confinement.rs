use std::ffi::OsString;
use std::path::{Path, PathBuf};

use game_driver_api::Result;

const LIBRARY: &str = "libcs2_bind.so";

fn locate(executable: &Path, override_path: Option<OsString>) -> Result<PathBuf> {
    let candidates = match override_path {
        Some(path) => vec![PathBuf::from(path)],
        None => {
            let parent = executable
                .parent()
                .ok_or("cs2: agent directory unavailable")?;
            vec![
                parent.join(LIBRARY),
                parent.join("../lib/varde").join(LIBRARY),
            ]
        }
    };
    candidates.into_iter().find(|p| p.is_file()).ok_or_else(|| {
        "cs2: loopback bind shim is missing; install libcs2_bind.so alongside the agent or in /usr/lib/varde (VARDE_CS2_BIND_SHIM overrides the path); VARDE_CS2_ALLOW_EXPOSED_PORT=1 explicitly allows exposed ports".into()
    })
}

pub fn environment(port: u16) -> Result<Vec<(OsString, OsString)>> {
    if std::env::var("VARDE_CS2_ALLOW_EXPOSED_PORT").as_deref() == Ok("1") {
        tracing::warn!("CS2 port confinement explicitly disabled; private ports may be exposed");
        return Ok(Vec::new());
    }
    let path = locate(
        &std::env::current_exe()?,
        std::env::var_os("VARDE_CS2_BIND_SHIM"),
    )?
    .canonicalize()?;
    let name = path.to_str().ok_or("cs2: bind shim path must be UTF-8")?;
    if name.contains(':') || name.chars().any(char::is_whitespace) {
        return Err("cs2: bind shim path cannot contain whitespace or a colon".into());
    }
    validate(&path)?;
    let mut preload = path.into_os_string();
    if let Some(existing) = std::env::var_os("LD_PRELOAD").filter(|v| !v.is_empty()) {
        preload.push(":");
        preload.push(existing);
    }
    Ok(vec![
        ("LD_PRELOAD".into(), preload),
        ("VARDE_CS2_BIND_PORT".into(), port.to_string().into()),
    ])
}

fn validate(path: &Path) -> Result<()> {
    use std::os::unix::ffi::OsStrExt;
    let name = std::ffi::CString::new(path.as_os_str().as_bytes())?;
    unsafe {
        let handle = libc::dlopen(name.as_ptr(), libc::RTLD_NOW | libc::RTLD_LOCAL);
        if handle.is_null() {
            return Err(format!("cs2: cannot load loopback bind shim {}", path.display()).into());
        }
        let symbol = libc::dlsym(handle, c"varde_cs2_bind_version".as_ptr());
        let valid = !symbol.is_null()
            && std::mem::transmute::<*mut libc::c_void, unsafe extern "C" fn() -> u32>(symbol)()
                == 1;
        libc::dlclose(handle);
        if !valid {
            return Err("cs2: incompatible loopback bind shim".into());
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn missing_or_invalid_shims_fail_closed() {
        let temp = tempfile::tempdir().unwrap();
        let executable = temp.path().join("varde-agent");
        assert!(locate(&executable, None).is_err());
        let path = temp.path().join(LIBRARY);
        std::fs::write(&path, b"not a shared library").unwrap();
        assert_eq!(locate(&executable, None).unwrap(), path);
        assert!(validate(&path).is_err());
        assert!(locate(&executable, Some(temp.path().join("missing").into())).is_err());
    }
}
