fn main() {
    // the tray is Windows-only; elsewhere the binary is a stub
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows") {
        // generate_context! embeds frontendDist (ui/dist); like the control
        // plane's webui-dist, fall back to a placeholder when the UI bundle
        // hasn't been built (plain `cargo build` without node)
        let dist = std::path::Path::new("ui/dist");
        if !dist.join("index.html").exists() {
            std::fs::create_dir_all(dist).expect("create ui/dist");
            std::fs::copy("placeholder.html", dist.join("index.html"))
                .expect("copy ui placeholder");
        }
        tauri_build::build()
    }
}
