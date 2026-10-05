use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};

use proptest::prelude::*;
use snapshot_store::*;

fn meta() -> SnapshotMeta {
    SnapshotMeta {
        server_id: "srv_p".into(),
        execution_id: "exec_p".into(),
        epoch: 3,
        node_id: "node_p".into(),
        parent: None,
        deployment_id: "dep_p".into(),
        created_at_unix_ms: 1_790_000_000_000,
        reason: "scheduled".into(),
    }
}

fn name() -> impl Strategy<Value = String> {
    "[a-z][a-z0-9_]{0,7}".prop_map(|s| s)
}

fn tree() -> impl Strategy<Value = BTreeMap<String, Vec<u8>>> {
    prop::collection::btree_map(
        prop::collection::vec(name(), 1..=3).prop_map(|segs| segs.join("/")),
        prop::collection::vec(any::<u8>(), 0..4096),
        1..12,
    )
    .prop_map(|m| {
        // a path can't be both a file and a directory: drop any key that has
        // another key as a proper path prefix
        let keys: Vec<String> = m.keys().cloned().collect();
        m.into_iter()
            .filter(|(k, _)| {
                !keys
                    .iter()
                    .any(|o| *o != *k && k.starts_with(&format!("{o}/")))
            })
            .collect()
    })
}

fn materialize(root: &Path, files: &BTreeMap<String, Vec<u8>>) {
    for (rel, data) in files {
        let p = root.join(rel);
        fs::create_dir_all(p.parent().unwrap()).unwrap();
        fs::write(p, data).unwrap();
    }
}

fn read_all(root: &Path) -> BTreeMap<String, Vec<u8>> {
    let mut out = BTreeMap::new();
    let mut stack = vec![root.to_path_buf()];
    while let Some(d) = stack.pop() {
        for e in fs::read_dir(d).unwrap() {
            let p: PathBuf = e.unwrap().path();
            if p.is_dir() {
                stack.push(p);
            } else {
                out.insert(
                    p.strip_prefix(root)
                        .unwrap()
                        .to_string_lossy()
                        .replace('\\', "/"),
                    fs::read(&p).unwrap(),
                );
            }
        }
    }
    out
}

proptest! {
    #[test]
    fn random_tree_roundtrip(files in tree()) {
        let tmp = tempfile::tempdir().unwrap();
        let store = Store::open(tmp.path().join("store")).unwrap();
        let base = tmp.path().join("srv");
        materialize(&base, &files);
        // include everything under the tree: top-level dir patterns
        let inc = files.keys().map(|k| {
            PathPattern::new(format!("{}/", k.split('/').next().unwrap()))
        }).collect::<std::collections::BTreeSet<_>>().into_iter().collect::<Vec<_>>();
        let info = store.snapshot(&base, &inc, meta()).unwrap();
        prop_assert!(store.verify(&info.id).unwrap().ok());
        let dest = tmp.path().join("out");
        store.restore(&info.id, &dest, &inc).unwrap();
        prop_assert_eq!(read_all(&dest), files);
    }
}
