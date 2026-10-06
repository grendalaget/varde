//! Varde tray (Windows): link this PC and show what Varde is doing. Talks
//! only to the local agent service over agent-ipc; the service owns the
//! identity, config and the device-code flow.
#![cfg_attr(windows, windows_subsystem = "windows")]

#[cfg(windows)]
mod app;
#[cfg(windows)]
mod autostart;
#[cfg(windows)]
mod svcctl;
#[cfg_attr(not(windows), allow(dead_code))]
mod view;

fn main() {
    #[cfg(windows)]
    app::run();
    #[cfg(not(windows))]
    {
        eprintln!("varde-tray runs on Windows only");
        std::process::exit(2);
    }
}
