//! Varde tray (Windows + macOS): link this PC and show what Varde is doing.
//! Talks only to the local agent service over agent-ipc; the service owns
//! the identity, config and the device-code flow.
#![cfg_attr(windows, windows_subsystem = "windows")]

#[cfg(any(windows, target_os = "macos"))]
mod app;
#[cfg(any(windows, target_os = "macos"))]
mod autostart;
#[cfg(any(windows, target_os = "macos"))]
mod crash;
#[cfg(windows)]
#[path = "svcctl.rs"]
mod svcctl;
#[cfg(target_os = "macos")]
#[path = "svcctl_macos.rs"]
mod svcctl;
#[cfg(any(windows, target_os = "macos"))]
mod update;
#[cfg_attr(not(any(windows, target_os = "macos")), allow(dead_code))]
mod view;

fn main() {
    #[cfg(any(windows, target_os = "macos"))]
    app::run();
    #[cfg(not(any(windows, target_os = "macos")))]
    {
        eprintln!("varde-tray runs on Windows and macOS only");
        std::process::exit(2);
    }
}
