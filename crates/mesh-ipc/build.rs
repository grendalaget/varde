fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Vendored protoc so builds work without a system protoc install.
    std::env::set_var("PROTOC", protoc_bin_vendored::protoc_bin_path()?);
    tonic_build::configure()
        .build_client(true)
        .build_server(false)
        .compile_protos(
            &[
                "../../proto/mesh/v1/ipc.proto",
                "../../proto/mesh/v1/frames.proto",
                "../../proto/mesh/v1/relay.proto",
            ],
            &["../../proto"],
        )?;
    Ok(())
}
