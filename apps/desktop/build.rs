fn main() {
    // the tray is Windows-only; elsewhere the binary is a stub
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows") {
        tauri_build::build()
    }
}
