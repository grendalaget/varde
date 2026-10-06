fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Vendored protoc so builds work without a system protoc install.
    std::env::set_var("PROTOC", protoc_bin_vendored::protoc_bin_path()?);
    tonic_build::configure()
        .build_client(true)
        .build_server(true)
        .compile_protos(&["../../proto/agent/v1/local.proto"], &["../../proto"])?;
    Ok(())
}
