fn main() {
    println!("cargo:rerun-if-changed=windows.rc");
    println!("cargo:rerun-if-changed=../../packaging/windows/varde.ico");
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows") {
        embed_resource::compile_for("windows.rc", ["varde-agent"], embed_resource::NONE)
            .manifest_required()
            .expect("embed Varde agent icon");
    }
}
