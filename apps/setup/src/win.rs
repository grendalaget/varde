// Windows helpers: registry (both 32- and 64-bit views), services, ACLs,
// shortcuts, WebView2 detection and "launch as the interactive user" — the
// RunAsUser equivalent the NSIS template used nsis_tauri_utils for.
use anyhow::{anyhow, bail, Context, Result};
use std::ffi::OsString;
use std::os::windows::ffi::{OsStrExt, OsStringExt};
use std::path::{Path, PathBuf};
use std::process::Command;
use winreg::enums::*;
use winreg::RegKey;

pub const UNINST_KEY: &str = "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\Varde";
pub const INNO_UNINST_KEY: &str =
    "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\Varde Agent_is1";
pub const COMPONENTS_KEY: &str = "Software\\Varde";
pub const RUN_KEY: &str = "Software\\Microsoft\\Windows\\CurrentVersion\\Run";
pub const MANUPRODUCT_KEY: &str = "Software\\Varde contributors\\Varde";
pub const CLASSES_KEY: &str = "Software\\Classes\\varde";
pub const PRODUCT_NAME: &str = "Varde";
pub const PUBLISHER: &str = "Varde contributors";
pub const HOMEPAGE: &str = "https://varde.games";
pub const MAIN_BINARY: &str = "varde-tray.exe";
const WEBVIEW2_APPGUID: &str = "{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}";
const AUMID: &str = "games.varde.tray";

pub fn w(s: &str) -> Vec<u16> {
    OsString::from(s).encode_wide().chain(Some(0)).collect()
}

fn wp(p: &Path) -> Vec<u16> {
    p.as_os_str().encode_wide().chain(Some(0)).collect()
}

// ---- paths ---------------------------------------------------------------

pub fn default_instdir() -> PathBuf {
    PathBuf::from(std::env::var_os("ProgramFiles").unwrap_or_else(|| "C:\\Program Files".into()))
        .join(PRODUCT_NAME)
}
pub fn program_data() -> PathBuf {
    PathBuf::from(std::env::var_os("ProgramData").unwrap_or_else(|| "C:\\ProgramData".into()))
}
pub fn data_dir() -> PathBuf {
    program_data().join("Varde")
}
pub fn cp_data_dir() -> PathBuf {
    program_data().join("VardeCP")
}
pub fn start_menu_dir() -> PathBuf {
    program_data()
        .join("Microsoft\\Windows\\Start Menu\\Programs")
        .join(PRODUCT_NAME)
}
pub fn start_menu_lnk() -> PathBuf {
    start_menu_dir().join(format!("{PRODUCT_NAME}.lnk"))
}
pub fn public_desktop_lnk() -> PathBuf {
    PathBuf::from(std::env::var_os("PUBLIC").unwrap_or_else(|| "C:\\Users\\Public".into()))
        .join("Desktop")
        .join(format!("{PRODUCT_NAME}.lnk"))
}

/// Where a previous install lives (RestorePreviousInstallLocation parity).
pub fn previous_instdir() -> Option<PathBuf> {
    for view in [KEY_WOW64_64KEY, KEY_WOW64_32KEY] {
        if let Ok(k) = RegKey::predef(HKEY_LOCAL_MACHINE)
            .open_subkey_with_flags(MANUPRODUCT_KEY, KEY_READ | view)
        {
            if let Ok(v) = k.get_value::<String, _>("") {
                if !v.is_empty() {
                    return Some(PathBuf::from(v));
                }
            }
        }
    }
    None
}

pub fn resolve_instdir(cli: Option<PathBuf>) -> PathBuf {
    cli.or_else(previous_instdir)
        .unwrap_or_else(default_instdir)
}

// ---- process plumbing -----------------------------------------------------

/// Spawn a console tool without flashing a window (we're a GUI app), wait,
/// and bail on nonzero exit — a service command that fails must not read as
/// a successful install step.
fn run_quiet(program: &str, args: &[&str]) -> Result<()> {
    use std::os::windows::process::CommandExt;
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    let st = Command::new(program)
        .args(args)
        .creation_flags(CREATE_NO_WINDOW)
        .status()
        .with_context(|| format!("spawn {program}"))?;
    if !st.success() {
        anyhow::bail!("{program} {} exited with {st}", args.join(" "));
    }
    Ok(())
}

pub fn kill_tray() {
    let _ = run_quiet("taskkill.exe", &["/F", "/IM", "varde-tray.exe"]);
}

/// `exe service uninstall` — the binary unregisters the service and waits
/// for Stopped (final save + replication) itself.
pub fn service_uninstall(exe: &Path) -> Result<()> {
    if !exe.exists() {
        return Ok(());
    }
    run_quiet(&exe.to_string_lossy(), &["service", "uninstall"])?;
    // The service record is gone but the service process can outlive the
    // command by a beat — Windows keeps its image locked until it exits.
    wait_unlocked(exe);
    Ok(())
}

/// Poll until a file can be opened for writing, i.e. no running process
/// still has its image locked. Bounded so a stubborn lock can't hang setup.
fn wait_unlocked(path: &Path) {
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(30);
    while path.exists() {
        if std::fs::OpenOptions::new().write(true).open(path).is_ok() {
            return;
        }
        if std::time::Instant::now() >= deadline {
            return;
        }
        std::thread::sleep(std::time::Duration::from_millis(250));
    }
}

pub fn service_install(exe: &Path) -> Result<()> {
    run_quiet(&exe.to_string_lossy(), &["service", "install"])?;
    Ok(())
}

pub fn sc_start(name: &str) -> Result<()> {
    run_quiet("sc.exe", &["start", name])?;
    Ok(())
}

/// icacls: SYSTEM + Administrators full, optionally one extra grant
/// (logs\ gets Users read/execute).
pub fn acl_dir(dir: &Path, extra_grant: Option<&str>) -> Result<()> {
    let d = dir.to_string_lossy().to_string();
    let mut args: Vec<String> = vec![
        d,
        "/inheritance:r".into(),
        "/grant".into(),
        "*S-1-5-18:(OI)(CI)F".into(),
        "*S-1-5-32-544:(OI)(CI)F".into(),
    ];
    if let Some(g) = extra_grant {
        args.push(g.to_string());
    }
    let argrefs: Vec<&str> = args.iter().map(String::as_str).collect();
    run_quiet("icacls.exe", &argrefs)?;
    Ok(())
}

// ---- registry -------------------------------------------------------------

pub fn read_hklm(path: &str, name: &str) -> Option<String> {
    for view in [KEY_WOW64_64KEY, KEY_WOW64_32KEY] {
        if let Ok(k) =
            RegKey::predef(HKEY_LOCAL_MACHINE).open_subkey_with_flags(path, KEY_READ | view)
        {
            if let Ok(v) = k.get_value::<String, _>(name) {
                if !v.is_empty() {
                    return Some(v);
                }
            }
        }
    }
    None
}

pub fn hklm_set(key: &str, name: &str, val: &str) -> Result<()> {
    let (k, _) = RegKey::predef(HKEY_LOCAL_MACHINE)
        .create_subkey(key)
        .with_context(|| format!("create {key}"))?;
    k.set_value(name, &val.to_string())?;
    Ok(())
}

pub fn hklm_set_dword(key: &str, name: &str, val: u32) -> Result<()> {
    let (k, _) = RegKey::predef(HKEY_LOCAL_MACHINE).create_subkey(key)?;
    k.set_value(name, &val)?;
    Ok(())
}

pub fn hklm_del_value(key: &str, name: &str) {
    for view in [KEY_WOW64_64KEY, KEY_WOW64_32KEY] {
        if let Ok(k) =
            RegKey::predef(HKEY_LOCAL_MACHINE).open_subkey_with_flags(key, KEY_WRITE | view)
        {
            let _ = k.delete_value(name);
        }
    }
}

pub fn hklm_del_key(key: &str) {
    for view in [KEY_WOW64_64KEY, KEY_WOW64_32KEY] {
        if let Ok(k) =
            RegKey::predef(HKEY_LOCAL_MACHINE).open_subkey_with_flags(key, KEY_WRITE | view)
        {
            let _ = k.delete_subkey_all("");
        }
    }
    // delete_subkey_all("") isn't reliable across winreg versions — also try
    // deleting the key outright from its parent.
    if let Some((parent, leaf)) = key.rsplit_once('\\') {
        for view in [KEY_WOW64_64KEY, KEY_WOW64_32KEY] {
            if let Ok(k) =
                RegKey::predef(HKEY_LOCAL_MACHINE).open_subkey_with_flags(parent, KEY_WRITE | view)
            {
                let _ = k.delete_subkey_all(leaf);
            }
        }
    }
}

pub fn hkcu_del_value(key: &str, name: &str) {
    if let Ok(k) = RegKey::predef(HKEY_CURRENT_USER).open_subkey_with_flags(key, KEY_WRITE) {
        let _ = k.delete_value(name);
    }
}

pub fn hkcu_del_key(key: &str) {
    if let Some((parent, leaf)) = key.rsplit_once('\\') {
        if let Ok(k) = RegKey::predef(HKEY_CURRENT_USER).open_subkey_with_flags(parent, KEY_WRITE) {
            let _ = k.delete_subkey_all(leaf);
        }
    }
}

/// remembered component selections (present-but-empty = all off).
/// !Components: presence is what matters — present-but-empty means the
/// user unticked everything, which `read_hklm` (empty-string dropping)
/// would conflate with absent. Check the raw value first.
pub fn remembered_components() -> Option<String> {
    for view in [KEY_WOW64_64KEY, KEY_WOW64_32KEY] {
        if let Ok(k) = RegKey::predef(HKEY_LOCAL_MACHINE)
            .open_subkey_with_flags(COMPONENTS_KEY, KEY_READ | view)
        {
            if k.get_raw_value("Components").is_ok() {
                return Some(k.get_value::<String, _>("Components").unwrap_or_default());
            }
        }
    }
    None
}

/// UninstallString of the Inno shim, if that install still exists.
pub fn inno_uninstall_string() -> Option<String> {
    read_hklm(INNO_UNINST_KEY, "UninstallString")
}

/// UninstallString of a *legacy* (NSIS) install at Uninstall\Varde.
/// Ours is told apart by the SetupEngine marker we write under
/// Software\Varde contributors\Varde.
pub fn legacy_nsis_uninstall_string() -> Option<String> {
    let marker = read_hklm(MANUPRODUCT_KEY, "SetupEngine");
    if marker.as_deref() == Some("varde-setup") {
        return None;
    }
    read_hklm(UNINST_KEY, "UninstallString")
}

pub fn existing_version() -> Option<String> {
    read_hklm(UNINST_KEY, "DisplayVersion")
}

pub fn write_arp(instdir: &Path, version: &str, estimated_kb: u32) -> Result<()> {
    let dir = instdir.to_string_lossy().to_string();
    hklm_set(UNINST_KEY, "DisplayName", PRODUCT_NAME)?;
    hklm_set(
        UNINST_KEY,
        "DisplayIcon",
        &format!("\"{dir}\\{MAIN_BINARY}\""),
    )?;
    hklm_set(UNINST_KEY, "DisplayVersion", version)?;
    hklm_set(UNINST_KEY, "Publisher", PUBLISHER)?;
    hklm_set(UNINST_KEY, "InstallLocation", &format!("\"{dir}\""))?;
    hklm_set(
        UNINST_KEY,
        "UninstallString",
        &format!("\"{dir}\\uninstall.exe\""),
    )?;
    hklm_set(
        UNINST_KEY,
        "QuietUninstallString",
        &format!("\"{dir}\\uninstall.exe\" /S"),
    )?;
    hklm_set(UNINST_KEY, "MainBinaryName", MAIN_BINARY)?;
    hklm_set(UNINST_KEY, "URLInfoAbout", HOMEPAGE)?;
    hklm_set(UNINST_KEY, "URLUpdateInfo", HOMEPAGE)?;
    hklm_set(UNINST_KEY, "HelpLink", HOMEPAGE)?;
    hklm_set_dword(UNINST_KEY, "NoModify", 1)?;
    hklm_set_dword(UNINST_KEY, "NoRepair", 1)?;
    hklm_set_dword(UNINST_KEY, "EstimatedSize", estimated_kb)?;
    Ok(())
}

pub fn write_classes_varde(tray: &Path) -> Result<()> {
    hklm_set(CLASSES_KEY, "", "URL:Varde Protocol")?;
    hklm_set(CLASSES_KEY, "URL Protocol", "")?;
    hklm_set(
        &format!("{CLASSES_KEY}\\shell\\open\\command"),
        "",
        &format!("\"{}\" \"%1\"", tray.to_string_lossy()),
    )?;
    Ok(())
}

// ---- WebView2 --------------------------------------------------------------

pub fn webview2_present() -> bool {
    let key = format!("SOFTWARE\\Microsoft\\EdgeUpdate\\Clients\\{WEBVIEW2_APPGUID}");
    // x64 build: check the 32-bit view (WOW6432Node), then HKCU — same order
    // as the NSIS template.
    for view in [KEY_WOW64_32KEY, KEY_WOW64_64KEY] {
        if let Ok(k) =
            RegKey::predef(HKEY_LOCAL_MACHINE).open_subkey_with_flags(&key, KEY_READ | view)
        {
            if k.get_value::<String, _>("pv")
                .map(|v| !v.is_empty())
                .unwrap_or(false)
            {
                return true;
            }
        }
    }
    if let Ok(k) = RegKey::predef(HKEY_CURRENT_USER).open_subkey(&key) {
        if k.get_value::<String, _>("pv")
            .map(|v| !v.is_empty())
            .unwrap_or(false)
        {
            return true;
        }
    }
    false
}

pub fn run_bootstrapper(path: &Path) -> Result<()> {
    run_quiet(&path.to_string_lossy(), &["/silent", "/install"])
}

const WEBVIEW_PROFILE_PREFIX: &str = "varde-setup-webview-";

/// A fresh user data folder for the setup window's own webview.
///
/// Not under %ProgramData%\Varde: that tree is SYSTEM + Administrators only,
/// and when a standard user elevates setup with an admin's credentials,
/// WebView2 runs its browser processes as the signed-in user rather than the
/// elevating admin — they can't open the profile and the window never comes
/// up ("We couldn't create the data directory"). `service install` also
/// resets ACLs under that tree mid-install, and a delete-data uninstall
/// removes it while the webview still runs.
///
/// Instead: a new, uniquely named dir in the Windows Temp dir (users may
/// create entries there but can't list it), created by us so nobody else
/// owns it, then locked to SYSTEM + Administrators plus the user signed in
/// to this session when that's a different account from ours — the identity
/// the browser processes run as. An elevated admin's own profile stays out
/// of reach of its unelevated processes.
pub fn setup_webview_profile() -> Result<PathBuf> {
    let base = windows_dir()?.join("Temp");
    // sweep profiles left by earlier runs whose setup process is gone; a
    // concurrent setup's live profile is left alone
    if let Ok(rd) = std::fs::read_dir(&base) {
        for e in rd.flatten() {
            let name = e.file_name().to_string_lossy().to_string();
            let Some(rest) = name.strip_prefix(WEBVIEW_PROFILE_PREFIX) else {
                continue;
            };
            let owner = rest.split('-').next().and_then(|p| p.parse::<u32>().ok());
            if owner.is_some_and(|pid| pid != std::process::id() && !process_alive(pid)) {
                let _ = std::fs::remove_dir_all(e.path());
            }
        }
    }
    let stamp = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or_default();
    let mut n = 0u32;
    let dir = loop {
        let d = base.join(format!(
            "{WEBVIEW_PROFILE_PREFIX}{}-{stamp}-{n}",
            std::process::id()
        ));
        // create_dir, not create_dir_all: a pre-created (squatted) dir must
        // not be adopted
        match std::fs::create_dir(&d) {
            Ok(()) => break d,
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists && n < 16 => n += 1,
            Err(e) => return Err(e).with_context(|| format!("create {}", d.display())),
        }
    };
    let user = other_session_user_sid()
        .context("look up the signed-in user")?
        .map(|s| format!("*{s}:(OI)(CI)F"));
    acl_dir(&dir, user.as_deref())?;
    Ok(dir)
}

/// %SystemRoot% from the OS rather than the (inheritable) environment.
fn windows_dir() -> Result<PathBuf> {
    use windows::Win32::System::SystemInformation::GetSystemWindowsDirectoryW;
    let mut buf = [0u16; 260];
    // SAFETY: the buffer is writable and its length is passed implicitly
    let n = unsafe { GetSystemWindowsDirectoryW(Some(&mut buf)) } as usize;
    if n == 0 || n >= buf.len() {
        bail!("GetSystemWindowsDirectory failed");
    }
    Ok(PathBuf::from(OsString::from_wide(&buf[..n])))
}

fn process_alive(pid: u32) -> bool {
    use windows::Win32::Foundation::{CloseHandle, STILL_ACTIVE};
    use windows::Win32::System::Threading::{
        GetExitCodeProcess, OpenProcess, PROCESS_QUERY_LIMITED_INFORMATION,
    };
    unsafe {
        // SAFETY: the handle is closed on every path that opened it.
        let Ok(h) = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, pid) else {
            return false;
        };
        let mut code = 0u32;
        let alive = GetExitCodeProcess(h, &mut code).is_err() || code == STILL_ACTIVE.0 as u32;
        let _ = CloseHandle(h);
        alive
    }
}

/// The SID (as a string) of the user signed in to this process's session,
/// if that isn't the user this process runs as — i.e. setup was elevated
/// with someone else's credentials. Session user name via WTS (no TCB
/// privilege needed, unlike WTSQueryUserToken), resolved to a SID.
fn other_session_user_sid() -> Result<Option<String>> {
    use windows::core::{PCWSTR, PWSTR};
    use windows::Win32::Foundation::{CloseHandle, HANDLE};
    use windows::Win32::Security::{EqualSid, LookupAccountNameW, PSID, SID_NAME_USE, TOKEN_QUERY};
    use windows::Win32::System::RemoteDesktop::{
        ProcessIdToSessionId, WTSDomainName, WTSFreeMemory, WTSQuerySessionInformationW,
        WTSUserName, WTS_CURRENT_SERVER_HANDLE, WTS_INFO_CLASS,
    };
    use windows::Win32::System::Threading::{GetCurrentProcess, OpenProcessToken};

    unsafe fn session_string(session: u32, class: WTS_INFO_CLASS) -> Result<String> {
        let mut p = PWSTR::null();
        let mut bytes = 0u32;
        WTSQuerySessionInformationW(
            Some(WTS_CURRENT_SERVER_HANDLE),
            session,
            class,
            &mut p,
            &mut bytes,
        )
        .map_err(|e| anyhow!("WTSQuerySessionInformation: {e}"))?;
        let s = p.to_string();
        WTSFreeMemory(p.0.cast());
        s.map_err(|e| anyhow!("session string: {e}"))
    }

    unsafe {
        // SAFETY: WTS strings are freed once after copying; the token handle
        // is closed once; the SID buffers outlive every PSID pointing into
        // them.
        let mut session = 0u32;
        ProcessIdToSessionId(std::process::id(), &mut session)
            .map_err(|e| anyhow!("ProcessIdToSessionId: {e}"))?;
        let user = session_string(session, WTSUserName)?;
        if user.is_empty() {
            // no one signed in to this session (e.g. a service context)
            return Ok(None);
        }
        let domain = session_string(session, WTSDomainName)?;
        let account = w(&if domain.is_empty() {
            user
        } else {
            format!("{domain}\\{user}")
        });

        let (mut sid_len, mut dom_len) = (0u32, 0u32);
        let mut kind = SID_NAME_USE::default();
        let _ = LookupAccountNameW(
            PCWSTR::null(),
            PCWSTR(account.as_ptr()),
            None,
            &mut sid_len,
            None,
            &mut dom_len,
            &mut kind,
        );
        if sid_len == 0 {
            bail!("LookupAccountName: no size");
        }
        let mut sid = vec![0u64; (sid_len as usize).div_ceil(8)];
        let mut dom = vec![0u16; dom_len.max(1) as usize];
        LookupAccountNameW(
            PCWSTR::null(),
            PCWSTR(account.as_ptr()),
            Some(PSID(sid.as_mut_ptr().cast())),
            &mut sid_len,
            Some(PWSTR(dom.as_mut_ptr())),
            &mut dom_len,
            &mut kind,
        )
        .map_err(|e| anyhow!("LookupAccountName: {e}"))?;
        let session_sid = PSID(sid.as_mut_ptr().cast());

        let mut own = HANDLE::default();
        OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut own)
            .map_err(|e| anyhow!("OpenProcessToken: {e}"))?;
        let own_user = token_user(own);
        let _ = CloseHandle(own);
        let own_user = own_user?;
        if EqualSid(session_sid, sid_of(&own_user)).is_ok() {
            return Ok(None);
        }
        sid_string(session_sid).map(Some)
    }
}

/// TOKEN_USER of a token, as the raw (u64-aligned) buffer it lives in.
///
/// # Safety
/// `tok` must be a valid token handle with TOKEN_QUERY access.
unsafe fn token_user(tok: windows::Win32::Foundation::HANDLE) -> Result<Vec<u64>> {
    use windows::Win32::Security::{GetTokenInformation, TokenUser};
    let mut len = 0u32;
    let _ = GetTokenInformation(tok, TokenUser, None, 0, &mut len);
    if len == 0 {
        bail!("GetTokenInformation(TokenUser): no size");
    }
    let mut buf = vec![0u64; (len as usize).div_ceil(8)];
    GetTokenInformation(tok, TokenUser, Some(buf.as_mut_ptr().cast()), len, &mut len)
        .map_err(|e| anyhow!("GetTokenInformation(TokenUser): {e}"))?;
    Ok(buf)
}

/// # Safety
/// `buf` must hold a TOKEN_USER filled in by `token_user`.
unsafe fn sid_of(buf: &[u64]) -> windows::Win32::Security::PSID {
    (*(buf.as_ptr() as *const windows::Win32::Security::TOKEN_USER))
        .User
        .Sid
}

/// # Safety
/// `sid` must point to a valid SID.
unsafe fn sid_string(sid: windows::Win32::Security::PSID) -> Result<String> {
    use windows::core::PWSTR;
    use windows::Win32::Foundation::{LocalFree, HLOCAL};
    use windows::Win32::Security::Authorization::ConvertSidToStringSidW;
    let mut s = PWSTR::null();
    ConvertSidToStringSidW(sid, &mut s).map_err(|e| anyhow!("ConvertSidToStringSid: {e}"))?;
    let out = s.to_string();
    let _ = LocalFree(Some(HLOCAL(s.0.cast())));
    out.map_err(|e| anyhow!("SID string: {e}"))
}

// ---- shortcuts -------------------------------------------------------------

pub fn create_shortcut(lnk: &Path, target: &Path) -> Result<()> {
    use windows::core::{Interface, GUID, PCWSTR, PWSTR};
    use windows::Win32::Foundation::PROPERTYKEY;
    use windows::Win32::System::Com::StructuredStorage::PROPVARIANT;
    use windows::Win32::System::Com::{
        CoCreateInstance, CoInitializeEx, IPersistFile, CLSCTX_INPROC_SERVER,
        COINIT_APARTMENTTHREADED,
    };
    use windows::Win32::System::Variant::VT_LPWSTR;
    use windows::Win32::UI::Shell::IShellLinkW;
    use windows::Win32::UI::Shell::PropertiesSystem::IPropertyStore;

    const CLSID_SHELL_LINK: GUID = GUID::from_u128(0x00021401_0000_0000_c000_000000000046);
    const PKEY_AUMID: PROPERTYKEY = PROPERTYKEY {
        fmtid: GUID::from_u128(0x9f4c2855_9f79_4b39_a8d0_e1d42de1d5f3),
        pid: 5,
    };

    unsafe {
        // SAFETY: standard COM setup on this thread; interface pointers are
        // used per contract and `wp` produces NUL-terminated UTF-16 strings.
        let _ = CoInitializeEx(None, COINIT_APARTMENTTHREADED);
        let link: IShellLinkW = CoCreateInstance(&CLSID_SHELL_LINK, None, CLSCTX_INPROC_SERVER)
            .context("CoCreateInstance ShellLink")?;
        link.SetPath(PCWSTR(wp(target).as_ptr()))?;
        let icon = target.with_file_name("varde.ico");
        if icon.exists() {
            link.SetIconLocation(PCWSTR(wp(&icon).as_ptr()), 0)?;
        }
        if let Some(dir) = target.parent() {
            link.SetWorkingDirectory(PCWSTR(wp(dir).as_ptr()))?;
        }
        // AUMID so a pinned shortcut groups with the tray.
        if let Ok(ps) = link.cast::<IPropertyStore>() {
            // pwszVal must live in COM task memory: PROPVARIANT's Drop calls
            // PropVariantClear which CoTaskMemFree()s it — a Rust-heap
            // pointer there corrupts the heap.
            let mut pv = PROPVARIANT::default();
            let val = w(AUMID);
            let bytes = val.len() * 2;
            let mem = windows::Win32::System::Com::CoTaskMemAlloc(bytes);
            if mem.is_null() {
                anyhow::bail!("CoTaskMemAlloc failed");
            }
            std::ptr::copy_nonoverlapping(val.as_ptr() as *const u8, mem as *mut u8, bytes);
            let mut inner = windows::Win32::System::Com::StructuredStorage::PROPVARIANT_0_0 {
                vt: VT_LPWSTR,
                ..Default::default()
            };
            inner.Anonymous.pwszVal = PWSTR(mem as *mut u16);
            pv.Anonymous.Anonymous = std::mem::ManuallyDrop::new(inner);
            let _ = ps.SetValue(&PKEY_AUMID, &pv);
            let _ = ps.Commit();
        }
        let pf: IPersistFile = link.cast().context("IPersistFile")?;
        if let Some(d) = lnk.parent() {
            std::fs::create_dir_all(d)?;
        }
        pf.Save(PCWSTR(wp(lnk).as_ptr()), true)?;
    }
    Ok(())
}

// ---- launch as interactive user --------------------------------------------

/// A primary token for the user on the active console session: WTS query
/// first, explorer.exe fallback (what RunAsUser did under the hood).
///
/// # Safety
/// Returns an owned HANDLE — the caller must close it (CloseHandle) once.
unsafe fn console_user_token() -> Result<windows::Win32::Foundation::HANDLE> {
    use windows::Win32::Foundation::{CloseHandle, HANDLE};
    use windows::Win32::Security::{
        DuplicateTokenEx, SecurityImpersonation, TokenPrimary, TOKEN_ALL_ACCESS, TOKEN_DUPLICATE,
        TOKEN_QUERY,
    };
    use windows::Win32::System::Diagnostics::ToolHelp::{
        CreateToolhelp32Snapshot, Process32FirstW, Process32NextW, PROCESSENTRY32W,
        TH32CS_SNAPPROCESS,
    };
    use windows::Win32::System::RemoteDesktop::{
        ProcessIdToSessionId, WTSGetActiveConsoleSessionId, WTSQueryUserToken,
    };
    use windows::Win32::System::Threading::{
        OpenProcess, OpenProcessToken, PROCESS_QUERY_LIMITED_INFORMATION,
    };

    let session = WTSGetActiveConsoleSessionId();
    let mut tok = HANDLE::default();
    if WTSQueryUserToken(session, &mut tok).is_ok() && !tok.is_invalid() {
        return Ok(tok);
    }

    let snap =
        CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0).map_err(|e| anyhow!("snapshot: {e}"))?;
    let mut found = HANDLE::default();
    let mut pe: PROCESSENTRY32W = std::mem::zeroed();
    pe.dwSize = std::mem::size_of::<PROCESSENTRY32W>() as u32;
    if Process32FirstW(snap, &mut pe).is_ok() {
        loop {
            let name = {
                let end = pe
                    .szExeFile
                    .iter()
                    .position(|c| *c == 0)
                    .unwrap_or(pe.szExeFile.len());
                String::from_utf16_lossy(&pe.szExeFile[..end])
            };
            if name.eq_ignore_ascii_case("explorer.exe") {
                let mut psid = 0u32;
                if ProcessIdToSessionId(pe.th32ProcessID, &mut psid).is_ok() && psid == session {
                    if let Ok(p) =
                        OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, pe.th32ProcessID)
                    {
                        let mut t = HANDLE::default();
                        if OpenProcessToken(p, TOKEN_DUPLICATE | TOKEN_QUERY, &mut t).is_ok() {
                            found = t;
                            break;
                        }
                        let _ = CloseHandle(t);
                    }
                }
            }
            if Process32NextW(snap, &mut pe).is_err() {
                break;
            }
        }
    }
    let _ = CloseHandle(snap);
    if found.is_invalid() {
        bail!("could not get a user token for session {session}");
    }
    let mut primary = HANDLE::default();
    DuplicateTokenEx(
        found,
        TOKEN_ALL_ACCESS,
        None,
        SecurityImpersonation,
        TokenPrimary,
        &mut primary,
    )
    .map_err(|e| anyhow!("DuplicateTokenEx: {e}"))?;
    let _ = CloseHandle(found);
    Ok(primary)
}

/// Spawn `exe args` as the interactive console user — the tray at the end of
/// install, or `--autostart` when the updater relaunches it.
pub fn launch_as_user(exe: &Path, args: &str) -> Result<()> {
    use windows::Win32::Foundation::CloseHandle;
    use windows::Win32::System::Threading::{
        CreateProcessAsUserW, CREATE_UNICODE_ENVIRONMENT, PROCESS_INFORMATION,
        STARTF_USESHOWWINDOW, STARTUPINFOW,
    };

    unsafe {
        // SAFETY: all out-params are zeroed POD structs; `w()` produces
        // NUL-terminated UTF-16; PROCESS_INFORMATION handles are closed.
        let tok = console_user_token()?;
        let mut si: STARTUPINFOW = std::mem::zeroed();
        si.cb = std::mem::size_of::<STARTUPINFOW>() as u32;
        si.dwFlags = STARTF_USESHOWWINDOW;
        let mut desktop = w("winsta0\\default");
        si.lpDesktop = windows::core::PWSTR(desktop.as_mut_ptr());
        let mut cmdline = w(&format!("\"{}\" {}", exe.to_string_lossy(), args));
        let dir = exe
            .parent()
            .map(|p| p.to_path_buf())
            .unwrap_or_else(|| PathBuf::from("."));
        let dirw = wp(&dir);
        let mut pi: PROCESS_INFORMATION = std::mem::zeroed();
        let r = CreateProcessAsUserW(
            Some(tok),
            windows::core::PCWSTR(wp(exe).as_ptr()),
            Some(windows::core::PWSTR(cmdline.as_mut_ptr())),
            None,
            None,
            false,
            CREATE_UNICODE_ENVIRONMENT,
            None,
            windows::core::PCWSTR(dirw.as_ptr()),
            &si,
            &mut pi,
        );
        let _ = CloseHandle(tok);
        if let Err(e) = r {
            bail!("CreateProcessAsUser {}: {e}", exe.display());
        }
        let _ = CloseHandle(pi.hProcess);
        let _ = CloseHandle(pi.hThread);
    }
    Ok(())
}

// ---- misc -------------------------------------------------------------------

/// Mark a file for deletion at next boot (MoveFileEx DELAY_UNTIL_REBOOT).
/// For the staged %TEMP% uninstaller copy - a running image can't delete
/// itself, and detached-cmd tricks die with the spawning process under
/// job objects.
pub fn delete_on_reboot(path: &Path) -> Result<()> {
    use windows::core::PCWSTR;
    use windows::Win32::Storage::FileSystem::{MoveFileExW, MOVEFILE_DELAY_UNTIL_REBOOT};
    unsafe {
        // SAFETY: PCWSTR args are NUL-terminated (`wp`); no ownership moves.
        MoveFileExW(
            PCWSTR(wp(path).as_ptr()),
            PCWSTR::null(),
            MOVEFILE_DELAY_UNTIL_REBOOT,
        )
    }
    .context("MoveFileExW")
}

/// Wait (bounded) for a process to exit - the staged uninstaller uses this
/// to know when its instdir parent is fully gone and its image unmapped.
pub fn wait_for_process_exit(pid: u32) {
    use windows::Win32::System::Threading::{
        OpenProcess, WaitForSingleObject, PROCESS_SYNCHRONIZE,
    };
    unsafe {
        // SAFETY: the opened handle is validity-checked and closed before
        // returning.
        if let Ok(h) = OpenProcess(PROCESS_SYNCHRONIZE, false, pid) {
            if !h.is_invalid() {
                let _ = WaitForSingleObject(h, 30_000);
                let _ = windows::Win32::Foundation::CloseHandle(h);
                return;
            }
        }
    }
    // no handle (already dead or inaccessible) - small grace period either way
    std::thread::sleep(std::time::Duration::from_millis(400));
}
pub fn is_admin() -> bool {
    use windows::Win32::Security::{
        GetTokenInformation, TokenElevation, TOKEN_ELEVATION, TOKEN_QUERY,
    };
    use windows::Win32::System::Threading::{GetCurrentProcess, OpenProcessToken};
    unsafe {
        // SAFETY: out-params are valid POD structs; the token handle is
        // closed on every path.
        let mut t = windows::Win32::Foundation::HANDLE::default();
        if OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut t).is_err() {
            return false;
        }
        let mut e: TOKEN_ELEVATION = std::mem::zeroed();
        let mut sz = 0u32;
        let ok = GetTokenInformation(
            t,
            TokenElevation,
            Some(&mut e as *mut _ as *mut _),
            std::mem::size_of::<TOKEN_ELEVATION>() as u32,
            &mut sz,
        );
        let _ = windows::Win32::Foundation::CloseHandle(t);
        ok.is_ok() && e.TokenIsElevated != 0
    }
}

/// `/S` installs have no UI; let output reach the parent console when there
/// is one (CI shells) — mirrors the NSIS AttachConsole trick.
pub fn attach_parent_console() {
    use windows::Win32::System::Console::{AttachConsole, FreeConsole, ATTACH_PARENT_PROCESS};
    unsafe {
        // SAFETY: parameterless console attach/detach calls.
        let _ = FreeConsole();
        let _ = AttachConsole(ATTACH_PARENT_PROCESS);
    }
}
