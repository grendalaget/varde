//! Best-effort, read-only Steam library discovery. No SteamCMD calls.
use std::path::{Path, PathBuf};

use crate::{vdf, APP_ID};

#[derive(Clone, Debug, serde::Serialize, serde::Deserialize)]
pub struct Install {
    pub dir: PathBuf,
    pub steamclient: Option<PathBuf>,
}

pub fn binary(dir: &Path) -> PathBuf {
    dir.join(if cfg!(windows) {
        "game/bin/win64/cs2.exe"
    } else {
        "game/bin/linuxsteamrt64/cs2"
    })
}

fn install(dir: PathBuf, root: &Path) -> Option<Install> {
    let dir = std::fs::canonicalize(dir).ok()?;
    if !binary(&dir).is_file() {
        return None;
    }
    #[cfg(not(windows))]
    let mut libraries = vec![
        dir.join("game/bin/linuxsteamrt64/steamclient.so"),
        root.join("linux64/steamclient.so"),
    ];
    #[cfg(windows)]
    let libraries = vec![
        root.join("steamclient64.dll"),
        dir.join("game/bin/win64/steamclient64.dll"),
    ];
    #[cfg(not(windows))]
    for home in homes() {
        libraries.push(home.join(".steam/sdk64/steamclient.so"));
    }
    Some(Install {
        dir,
        steamclient: libraries.into_iter().find(|p| p.is_file()),
    })
}

pub fn in_root(root: &Path) -> Option<Install> {
    let mut libraries = vec![root.to_path_buf()];
    if let Ok(contents) = std::fs::read_to_string(root.join("steamapps/libraryfolders.vdf")) {
        if let Ok(vdf) = vdf::parse(&contents) {
            if let Some(entries) = vdf.get("libraryfolders").and_then(vdf::Value::entries) {
                for (key, value) in entries {
                    if key.parse::<u32>().is_ok() {
                        if let Some(path) = value
                            .get("path")
                            .and_then(vdf::Value::text)
                            .or_else(|| value.text())
                        {
                            let path = PathBuf::from(path);
                            if path.is_absolute() {
                                libraries.push(path);
                            }
                        }
                    }
                }
            }
        }
    }
    for library in libraries {
        let Some(manifest) =
            std::fs::read_to_string(library.join(format!("steamapps/appmanifest_{APP_ID}.acf")))
                .ok()
                .and_then(|s| vdf::parse(&s).ok())
        else {
            continue;
        };
        let Some(app) = manifest.get("AppState") else {
            continue;
        };
        if app.get("StateFlags").and_then(vdf::Value::text) != Some("4") {
            continue;
        }
        let Some(name) = app.get("installdir").and_then(vdf::Value::text) else {
            continue;
        };
        // The manifest supplies a directory name, not an arbitrary path.
        if name.is_empty() || name == "." || name == ".." || name.contains(['/', '\\']) {
            continue;
        }
        if let Some(found) = install(library.join("steamapps/common").join(name), root) {
            return Some(found);
        }
    }
    None
}

#[cfg(not(windows))]
fn homes() -> Vec<PathBuf> {
    let homes: Vec<_> = std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" })
        .map(PathBuf::from)
        .into_iter()
        .collect();
    #[cfg(target_os = "linux")]
    let mut homes = homes;
    #[cfg(target_os = "linux")]
    if let (Ok(sessions), Ok(passwd)) = (
        std::fs::read_dir("/run/systemd/sessions"),
        std::fs::read_to_string("/etc/passwd"),
    ) {
        for session in sessions.flatten() {
            let Ok(session) = std::fs::read_to_string(session.path()) else {
                continue;
            };
            if !session
                .lines()
                .any(|l| l == "ACTIVE=1" || l == "STATE=active")
            {
                continue;
            }
            let Some(uid) = session.lines().find_map(|l| l.strip_prefix("UID=")) else {
                continue;
            };
            for user in passwd.lines() {
                let fields: Vec<_> = user.split(':').collect();
                if fields.len() >= 7 && fields[2] == uid {
                    homes.push(PathBuf::from(fields[5]));
                }
            }
        }
    }
    homes
}

fn roots() -> Vec<PathBuf> {
    let mut roots: Vec<_> = std::env::var_os("VARDE_STEAM_DIR")
        .map(PathBuf::from)
        .into_iter()
        .collect();
    #[cfg(target_os = "linux")]
    for home in homes() {
        roots.extend([
            home.join(".local/share/Steam"),
            home.join(".steam/steam"),
            home.join(".var/app/com.valvesoftware.Steam/.local/share/Steam"),
        ]);
    }
    #[cfg(windows)]
    {
        use winreg::{
            enums::{
                HKEY_CURRENT_USER, HKEY_LOCAL_MACHINE, KEY_READ, KEY_WOW64_32KEY, KEY_WOW64_64KEY,
            },
            RegKey,
        };
        for hive in [HKEY_CURRENT_USER, HKEY_LOCAL_MACHINE] {
            for view in [KEY_WOW64_32KEY, KEY_WOW64_64KEY] {
                if let Ok(key) = RegKey::predef(hive)
                    .open_subkey_with_flags("Software\\Valve\\Steam", KEY_READ | view)
                {
                    for name in ["InstallPath", "SteamPath"] {
                        if let Ok(path) = key.get_value::<String, _>(name) {
                            roots.push(PathBuf::from(path));
                        }
                    }
                }
            }
        }
        roots.push(PathBuf::from(r"C:\Program Files (x86)\Steam"));
    }
    roots
}

pub fn detect(deployments: Option<&Path>) -> Option<Install> {
    if !cfg!(any(target_os = "linux", windows)) {
        return None;
    }
    detect_client().or_else(|| in_deployments(deployments?))
}

pub fn detect_client() -> Option<Install> {
    if !cfg!(any(target_os = "linux", windows)) {
        return None;
    }
    // Unit tests must not see the host's Steam install or overrides.
    if !cfg!(test) {
        if let Some(dir) = std::env::var_os("VARDE_CS2_DIR") {
            if let Some(found) = install(PathBuf::from(dir), Path::new("")) {
                return Some(found);
            }
        }
        for root in roots() {
            if let Some(found) = in_root(&root) {
                return Some(found);
            }
        }
    }
    None
}

pub fn in_deployments(deployments: &Path) -> Option<Install> {
    let mut dirs: Vec<_> = std::fs::read_dir(deployments)
        .ok()?
        .flatten()
        .map(|e| e.path().join("server"))
        .collect();
    dirs.sort();
    for dir in dirs {
        if game_driver_api::steam_app_installed(APP_ID, &dir) {
            if let Some(found) = install(dir, Path::new("")) {
                return Some(found);
            }
        }
    }
    None
}
