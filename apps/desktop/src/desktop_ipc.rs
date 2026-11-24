use serde::{Deserialize, Serialize};

use crate::view::Facet;

#[derive(Serialize, Clone)]
#[cfg_attr(test, derive(ts_rs::TS))]
pub(crate) struct FaceInfo {
    pub(crate) facet: Facet,
    pub(crate) cp_url: String,
    pub(crate) group_id: String,
    pub(crate) node_id: String,
    pub(crate) can_join_cs2: bool,
}

#[derive(Deserialize)]
#[cfg_attr(test, derive(ts_rs::TS))]
pub(crate) struct JoinCs2ServerRequest {
    pub(crate) address: String,
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{fs, path::Path};
    use ts_rs::{Config, TS};

    #[test]
    fn desktop_ipc_bindings() {
        let config = Config::default();
        let path = Path::new(env!("CARGO_MANIFEST_DIR")).join("ui/src/generated/desktop-ipc.ts");
        let mut generated =
            String::from("// Generated from the Rust desktop IPC types by desktop_ipc_bindings.\n");
        for declaration in [
            Facet::decl(&config),
            FaceInfo::decl(&config),
            JoinCs2ServerRequest::decl(&config),
        ] {
            generated.push_str(&format!("\n// prettier-ignore\nexport {declaration}\n"));
        }
        if std::env::var_os("VARDE_UPDATE_DESKTOP_IPC").as_deref() == Some("1".as_ref()) {
            fs::create_dir_all(path.parent().unwrap()).expect("create bindings directory");
            fs::write(path, generated).expect("write desktop IPC bindings");
        } else {
            let committed = fs::read_to_string(path).unwrap_or_default();
            assert_eq!(
                committed.replace("\r\n", "\n"),
                generated,
                "Regenerate with VARDE_UPDATE_DESKTOP_IPC=1 cargo test -p varde-tray desktop_ipc_bindings"
            );
        }
    }
}
