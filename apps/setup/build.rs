use std::io::Write;

// The installer needs elevation: it registers services, writes HKLM and
// Program Files. Tauri's default manifest is requireAdministrator-free, so we
// pass a full replacement with requestedExecutionLevel bumped.
const MANIFEST: &str = r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity version="1.0.0.0" name="games.varde.setup" processorArchitecture="*" type="win32"/>
  <dependency>
    <dependentAssembly>
      <assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0" processorArchitecture="*" publicKeyToken="6595b64144ccf1df" language="*"/>
    </dependentAssembly>
  </dependency>
  <asmv3:application xmlns:asmv3="urn:schemas-microsoft-com:asm.v3">
    <asmv3:windowsSettings>
      <dpiAwareness xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">PerMonitorV2</dpiAwareness>
      <dpiAware xmlns="http://schemas.microsoft.com/SMI/2005/WindowsSettings">true</dpiAware>
    </asmv3:windowsSettings>
  </asmv3:application>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
      <supportedOS Id="{1f676c76-80e1-4239-95bb-83d0f6d0da78}"/>
      <supportedOS Id="{4a2f28e3-53b9-4441-ba9c-d69d4a4a6e38}"/>
      <supportedOS Id="{35138b9a-5d96-4fbd-8e2d-a2440225f93a}"/>
      <supportedOS Id="{e2011457-1546-43c5-a5fe-008deee3d3f0}"/>
    </application>
  </compatibility>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="requireAdministrator" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
</assembly>
"#;

fn main() {
    // Pack whatever payload/ currently holds into a zip the binary embeds.
    // For the release build the staging script has dropped the real binaries
    // here; for the lite uninstaller build the dir is empty and the zip is
    // empty too — payload.rs treats an empty archive as "no payload".
    let out = std::path::PathBuf::from(std::env::var("OUT_DIR").unwrap());
    let zip_path = out.join("payload.zip");
    let file = std::fs::File::create(&zip_path).expect("create payload.zip");
    let mut zip = zip::ZipWriter::new(file);
    let opts = zip::write::SimpleFileOptions::default()
        .compression_method(zip::CompressionMethod::Deflated)
        .compression_level(Some(9));
    let payload_dir = std::path::Path::new("payload");
    if payload_dir.is_dir() {
        let mut names: Vec<_> = std::fs::read_dir(payload_dir)
            .expect("read payload dir")
            .filter_map(|e| e.ok())
            .map(|e| e.file_name())
            .filter(|n| !n.to_string_lossy().starts_with('.'))
            .collect();
        names.sort();
        for name in names {
            let path = payload_dir.join(&name);
            if !path.is_file() {
                continue;
            }
            zip.start_file(name.to_string_lossy().into_owned(), opts)
                .expect("zip start_file");
            zip.write_all(&std::fs::read(&path).expect("read payload file"))
                .expect("zip write");
        }
    }
    zip.finish().expect("finish payload.zip");
    println!("cargo:rerun-if-changed=payload");

    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows") {
        // generate_context! embeds frontendDist (ui/dist); fall back to the
        // placeholder like apps/desktop so plain `cargo build` works.
        let dist = std::path::Path::new("ui/dist");
        if !dist.join("index.html").exists() {
            std::fs::create_dir_all(dist).expect("create ui/dist");
            std::fs::copy("placeholder.html", dist.join("index.html"))
                .expect("copy ui placeholder");
        }
        // The installer runs elevated from wherever it was downloaded or
        // staged (Downloads, %LOCALAPPDATA%\Varde\update), folders the user
        // can write. Resolve its DLL imports from System32 only, so a DLL
        // planted next to the exe can't ride along on the UAC prompt.
        if std::env::var("CARGO_CFG_TARGET_ENV").as_deref() == Ok("msvc") {
            println!("cargo:rustc-link-arg-bins=/DEPENDENTLOADFLAG:0x800");
        }
        let attrs = tauri_build::Attributes::new()
            .windows_attributes(tauri_build::WindowsAttributes::new().app_manifest(MANIFEST));
        tauri_build::try_build(attrs).expect("tauri_build");
    }
}
