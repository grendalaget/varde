//! Content-addressed snapshot storage per docs/architecture/storage.md.
//!
//! Synchronous API — call it via `tokio::task::spawn_blocking` from async
//! code. No networking lives here; the agent's replication module moves
//! chunks over the mesh.

mod manifest;
mod paths;
mod pattern;

pub use manifest::{
    Manifest, ManifestEntry, SnapshotId, SnapshotInfo, SnapshotMeta, CHUNK_ID_LEN, FORMAT_V1,
};
pub use paths::{validate_rel_path, PathError};
pub use pattern::PathPattern;

use std::collections::{HashMap, HashSet};
use std::fs;
use std::io::Read;
use std::path::{Path, PathBuf};

pub const CHUNK_MIN: usize = 256 * 1024;
pub const CHUNK_AVG: usize = 1024 * 1024;
pub const CHUNK_MAX: usize = 4 * 1024 * 1024;
pub const ZSTD_LEVEL: i32 = 3;
/// Unreferenced chunks younger than this are kept (races in-flight transfers).
pub const GC_GRACE_MS: u64 = 60 * 60 * 1000;

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    #[error("json: {0}")]
    Json(#[from] serde_json::Error),
    #[error("invalid path {0:?}: {1}")]
    Path(String, PathError),
    #[error("bad manifest: {0}")]
    Manifest(String),
    #[error("chunk {0} corrupt (hash mismatch)")]
    ChunkCorrupt(String),
    #[error("chunk {0} missing")]
    ChunkMissing(String),
    #[error("snapshot {0} not found")]
    SnapshotNotFound(String),
    #[error("snapshot id mismatch: digest says {0}")]
    IdMismatch(String),
}

pub type Result<T> = std::result::Result<T, Error>;

pub type ChunkId = String; // lowercase hex BLAKE3 of plaintext

pub struct Store {
    root: PathBuf,
}

#[derive(Debug, Default)]
pub struct VerifyReport {
    pub total_chunks: usize,
    pub ok_chunks: usize,
    pub missing: Vec<ChunkId>,
    pub corrupt: Vec<ChunkId>,
}
impl VerifyReport {
    pub fn ok(&self) -> bool {
        self.missing.is_empty() && self.corrupt.is_empty()
    }
}

#[derive(Debug, Default)]
pub struct GcReport {
    pub removed_chunks: usize,
    pub freed_bytes: u64,
    pub kept_young: usize,
    pub kept_referenced: usize,
}

impl Store {
    pub fn open(root: impl AsRef<Path>) -> Result<Store> {
        let root = root.as_ref().to_path_buf();
        for d in ["chunks/tmp", "chunks/quarantine", "snapshots"] {
            fs::create_dir_all(root.join(d))?;
        }
        Ok(Store { root })
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    // ---- snapshot ----

    /// Snapshot every included file under `base` into a canonical manifest
    /// plus content-addressed chunks. Synchronous.
    pub fn snapshot(
        &self,
        base: &Path,
        include: &[PathPattern],
        meta: SnapshotMeta,
    ) -> Result<SnapshotInfo> {
        let mut entries: Vec<ManifestEntry> = Vec::new();
        let mut dirs: HashMap<String, u32> = HashMap::new();
        let mut size_bytes = 0u64;
        let mut stored_bytes = 0u64;
        let mut chunk_count = 0usize;
        let mut new_chunks = 0usize;

        for f in walk(base) {
            let rel = f.rel.clone();
            match f.kind {
                WalkKind::Symlink | WalkKind::Special => {
                    tracing_warn_skip(&rel);
                    continue;
                }
                WalkKind::Dir => {
                    if include.iter().any(|p| p.matches_dir(&rel)) {
                        dirs.insert(rel, f.mode);
                    }
                    continue;
                }
                WalkKind::File => {
                    if !include.iter().any(|p| p.matches(&rel, true)) {
                        continue;
                    }
                }
            }
            validate_rel_path(&rel).map_err(|e| Error::Path(rel.clone(), e))?;
            // record ancestor dirs so restore recreates the tree
            record_ancestors(&rel, base, &mut dirs)?;

            let data = fs::read(&f.abs)?;
            size_bytes += data.len() as u64;
            let whole = blake3::hash(&data).to_hex().to_string();
            let mut chunk_ids = Vec::new();
            for (off, len) in chunk_ranges(&data) {
                let bytes = &data[off..off + len];
                let id = blake3::hash(bytes).to_hex().to_string();
                if !self.has_chunk(&id) {
                    let z = zstd::bulk::compress(bytes, ZSTD_LEVEL)?;
                    self.write_chunk_atomic(&id, &z)?;
                    stored_bytes += z.len() as u64;
                    new_chunks += 1;
                }
                chunk_ids.push(id);
                chunk_count += 1;
            }
            entries.push(ManifestEntry {
                path: rel,
                typ: "file".into(),
                size: Some(data.len() as u64),
                mode: Some(f.mode),
                mtime_unix_ms: Some(f.mtime_ms),
                blake3: Some(whole),
                chunks: Some(chunk_ids),
            });
        }
        for (d, mode) in dirs {
            entries.push(ManifestEntry {
                path: d,
                typ: "dir".into(),
                size: None,
                mode: Some(mode),
                mtime_unix_ms: None,
                blake3: None,
                chunks: None,
            });
        }
        entries.sort_by(|a, b| a.path.as_bytes().cmp(b.path.as_bytes()));

        let m = Manifest {
            format: FORMAT_V1,
            server_id: meta.server_id,
            execution_id: meta.execution_id,
            epoch: meta.epoch,
            node_id: meta.node_id,
            parent: meta.parent,
            deployment_id: meta.deployment_id,
            created_at_unix_ms: meta.created_at_unix_ms,
            reason: meta.reason,
            files: entries,
        };
        let file_count = m.files.iter().filter(|e| e.typ == "file").count();
        let bytes = serde_json::to_vec(&m)?;
        let id = self.put_manifest_bytes(&bytes)?;
        Ok(SnapshotInfo {
            id,
            digest: blake3::hash(&bytes).to_hex().to_string(),
            size_bytes,
            stored_bytes,
            file_count,
            chunk_count,
            new_chunks,
        })
    }

    /// Restore through a staging dir + rename. Included paths missing from the
    /// manifest are removed; paths outside `include` are left alone.
    pub fn restore(
        &self,
        snapshot_id: &SnapshotId,
        dest: &Path,
        include: &[PathPattern],
    ) -> Result<()> {
        let m = self.manifest(snapshot_id)?;
        for e in &m.files {
            validate_rel_path(&e.path).map_err(|e2| Error::Path(e.path.clone(), e2))?;
        }
        let parent = dest.parent().unwrap_or_else(|| Path::new("."));
        let staging = parent.join(format!(
            ".{}.staging-{}",
            dest.file_name().unwrap_or_default().to_string_lossy(),
            std::process::id()
        ));
        if staging.exists() {
            fs::remove_dir_all(&staging)?;
        }
        fs::create_dir_all(&staging)?;
        let res = self.populate(&m, &staging);
        if let Err(e) = res {
            let _ = fs::remove_dir_all(&staging);
            return Err(e);
        }
        // move staged entries into dest (per-path rename); non-included paths
        // in dest are left alone
        fs::create_dir_all(dest)?;
        for e in &m.files {
            let src = staging.join(&e.path);
            let dst = dest.join(&e.path);
            if e.typ == "dir" {
                fs::create_dir_all(&dst)?;
                apply_mode(&dst, e.mode);
                continue;
            }
            if let Some(p) = dst.parent() {
                fs::create_dir_all(p)?;
            }
            // an included dir may currently occupy this path
            if dst.is_dir() {
                fs::remove_dir_all(&dst)?;
            }
            fs::rename(&src, &dst)?;
            apply_mode(&dst, e.mode);
        }
        let _ = fs::remove_dir_all(&staging);
        // remove included paths that are not in the manifest
        let want: HashSet<&str> = m.files.iter().map(|e| e.path.as_str()).collect();
        for f in walk(dest) {
            if want.contains(f.rel.as_str()) {
                continue;
            }
            // only remove paths at/under an included root, and only if some
            // pattern selects them or their subtree
            if include
                .iter()
                .any(|p| p.matches(&f.rel, f.kind == WalkKind::File))
                || include.iter().any(|p| p.matches_dir(&f.rel))
            {
                let _ = if f.kind == WalkKind::Dir {
                    fs::remove_dir_all(&f.abs)
                } else {
                    fs::remove_file(&f.abs)
                };
            }
        }
        Ok(())
    }

    fn populate(&self, m: &Manifest, staging: &Path) -> Result<()> {
        for e in &m.files {
            let rel = &e.path;
            validate_rel_path(rel).map_err(|e2| Error::Path(rel.clone(), e2))?;
            let abs = staging.join(rel);
            if e.typ == "dir" {
                fs::create_dir_all(&abs)?;
                apply_mode(&abs, e.mode);
                continue;
            }
            if let Some(p) = abs.parent() {
                fs::create_dir_all(p)?;
            }
            let tmp = abs.with_extension("part");
            let mut out = fs::File::create(&tmp)?;
            use std::io::Write;
            for cid in e.chunks.clone().unwrap_or_default() {
                let data = self.read_chunk_plaintext(&cid)?;
                out.write_all(&data)?;
            }
            drop(out);
            fs::rename(&tmp, &abs)?;
            apply_mode(&abs, e.mode);
        }
        Ok(())
    }

    // ---- manifests ----

    pub fn manifest(&self, id: &SnapshotId) -> Result<Manifest> {
        let bytes = self.manifest_bytes(id)?;
        let m: Manifest = serde_json::from_slice(&bytes)?;
        if m.format != FORMAT_V1 {
            return Err(Error::Manifest(format!("format {}", m.format)));
        }
        Ok(m)
    }

    pub fn manifest_bytes(&self, id: &SnapshotId) -> Result<Vec<u8>> {
        let p = self.root.join("snapshots").join(format!("{}.json", id.0));
        let bytes =
            fs::read(&p).map_err(|e| not_found(e, || Error::SnapshotNotFound(id.0.clone())))?;
        if Manifest::digest_id(&bytes) != *id {
            // file content does not match its name — quarantine it
            let q = p.with_extension("quarantine");
            let _ = fs::rename(&p, &q);
            return Err(Error::IdMismatch(id.0.clone()));
        }
        Ok(bytes)
    }

    /// Store manifest bytes: verifies the digest ⇒ snapshot id, parses and
    /// validates every path. Returns the derived id.
    pub fn put_manifest_bytes(&self, bytes: &[u8]) -> Result<SnapshotId> {
        let id = Manifest::digest_id(bytes);
        let m: Manifest = serde_json::from_slice::<Manifest>(bytes)?;
        if m.format != FORMAT_V1 {
            return Err(Error::Manifest(format!("format {}", m.format)));
        }
        for e in &m.files {
            validate_rel_path(&e.path).map_err(|x| Error::Path(e.path.clone(), x))?;
        }
        let p = self.root.join("snapshots").join(format!("{}.json", id.0));
        write_atomic(&p, bytes)?;
        Ok(id)
    }

    pub fn list_snapshots(&self) -> Result<Vec<SnapshotId>> {
        let mut out = Vec::new();
        let dir = self.root.join("snapshots");
        if !dir.exists() {
            return Ok(out);
        }
        for e in fs::read_dir(&dir)? {
            let e = e?;
            let name = e.file_name().to_string_lossy().to_string();
            if let Some(id) = name.strip_suffix(".json") {
                out.push(SnapshotId(id.to_string()));
            }
        }
        out.sort();
        Ok(out)
    }

    // ---- chunks ----

    pub fn missing_chunks(&self, m: &Manifest) -> Result<Vec<ChunkId>> {
        let mut out = Vec::new();
        for e in &m.files {
            for c in e.chunks.iter().flatten() {
                if !self.has_chunk(c) {
                    out.push(c.clone());
                }
            }
        }
        Ok(out)
    }

    pub fn has_chunk(&self, id: &ChunkId) -> bool {
        self.chunk_path(id).exists()
    }

    fn chunk_path(&self, id: &str) -> PathBuf {
        self.root
            .join("chunks")
            .join(&id[..2.min(id.len())])
            .join(id)
    }

    /// Compressed bytes for transfer — verifies plaintext hash on every read;
    /// a mismatch moves the file to quarantine and errors (I6).
    pub fn read_chunk_compressed(&self, id: &ChunkId) -> Result<Vec<u8>> {
        let p = self.chunk_path(id);
        let z = fs::read(&p).map_err(|e| not_found(e, || Error::ChunkMissing(id.clone())))?;
        match verify_compressed(id, &z) {
            Ok(()) => Ok(z),
            Err(e) => {
                let q = self.root.join("chunks").join("quarantine").join(id);
                let _ = fs::rename(&p, &q);
                Err(e)
            }
        }
    }

    /// Store a received chunk: decompress + verify hash first (I6).
    pub fn put_chunk_compressed(&self, id: &ChunkId, z: &[u8]) -> Result<()> {
        verify_compressed(id, z)?;
        self.write_chunk_atomic(id, z)
    }

    fn read_chunk_plaintext(&self, id: &ChunkId) -> Result<Vec<u8>> {
        let z = self.read_chunk_compressed(id)?;
        Ok(zstd::bulk::decompress(&z, CHUNK_MAX)?)
    }

    fn write_chunk_atomic(&self, id: &ChunkId, z: &[u8]) -> Result<()> {
        let p = self.chunk_path(id);
        if p.exists() {
            return Ok(());
        }
        if let Some(d) = p.parent() {
            fs::create_dir_all(d)?;
        }
        write_atomic(&p, z)
    }

    // ---- verify / delete / gc ----

    pub fn verify(&self, id: &SnapshotId) -> Result<VerifyReport> {
        let m = self.manifest(id)?;
        let mut r = VerifyReport::default();
        let mut seen = HashSet::new();
        for e in &m.files {
            for c in e.chunks.iter().flatten() {
                if !seen.insert(c.clone()) {
                    continue;
                }
                r.total_chunks += 1;
                match self.read_chunk_compressed(c) {
                    Ok(_) => r.ok_chunks += 1,
                    Err(Error::ChunkMissing(_)) => r.missing.push(c.clone()),
                    Err(_) => r.corrupt.push(c.clone()),
                }
            }
        }
        Ok(r)
    }

    pub fn delete_snapshot(&self, id: &SnapshotId) -> Result<()> {
        let p = self.root.join("snapshots").join(format!("{}.json", id.0));
        match fs::remove_file(&p) {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(e.into()),
        }
    }

    /// Sweep chunks no manifest references; chunks younger than 1 h are kept.
    pub fn gc(&self) -> Result<GcReport> {
        let mut referenced: HashSet<ChunkId> = HashSet::new();
        for id in self.list_snapshots()? {
            if let Ok(m) = self.manifest(&id) {
                for e in &m.files {
                    for c in e.chunks.iter().flatten() {
                        referenced.insert(c.clone());
                    }
                }
            }
        }
        let mut r = GcReport::default();
        let now = std::time::SystemTime::now();
        let chunks_dir = self.root.join("chunks");
        for sub in fs::read_dir(&chunks_dir)? {
            let sub = sub?;
            let name = sub.file_name().to_string_lossy().to_string();
            if name == "tmp" || name == "quarantine" || !sub.file_type()?.is_dir() {
                continue;
            }
            for e in fs::read_dir(sub.path())? {
                let e = e?;
                let id = e.file_name().to_string_lossy().to_string();
                if referenced.contains(&id) {
                    r.kept_referenced += 1;
                    continue;
                }
                let young = e
                    .metadata()
                    .and_then(|md| md.modified())
                    .ok()
                    .and_then(|t| now.duration_since(t).ok())
                    .map(|d| d.as_millis() < GC_GRACE_MS as u128)
                    .unwrap_or(true);
                if young {
                    r.kept_young += 1;
                    continue;
                }
                r.freed_bytes += e.metadata().map(|m| m.len()).unwrap_or(0);
                let _ = fs::remove_file(e.path());
                r.removed_chunks += 1;
            }
        }
        Ok(r)
    }
}

// ---- helpers ----

fn verify_compressed(id: &ChunkId, z: &[u8]) -> Result<()> {
    let plain = zstd::bulk::decompress(z, CHUNK_MAX)
        .map_err(|e| Error::Manifest(format!("decompress {id}: {e}")))?;
    if blake3::hash(&plain).to_hex().to_string() != *id {
        return Err(Error::ChunkCorrupt(id.clone()));
    }
    Ok(())
}

/// Byte ranges of one chunking pass over `data` (FastCDC 2020); files smaller
/// than the minimum are a single chunk.
fn chunk_ranges(data: &[u8]) -> Vec<(usize, usize)> {
    if data.len() <= CHUNK_MIN {
        return vec![(0, data.len())];
    }
    fastcdc::v2020::FastCDC::new(data, CHUNK_MIN as u32, CHUNK_AVG as u32, CHUNK_MAX as u32)
        .map(|c| (c.offset, c.length))
        .collect()
}

fn write_atomic(path: &Path, bytes: &[u8]) -> Result<()> {
    let tmp_dir = if path.starts_with("") {
        path.parent().unwrap_or(Path::new(".")).to_path_buf()
    } else {
        PathBuf::from(".")
    };
    fs::create_dir_all(&tmp_dir)?;
    let tmp = tmp_dir.join(format!(
        ".{}.tmp-{}",
        path.file_name().unwrap_or_default().to_string_lossy(),
        std::process::id()
    ));
    fs::write(&tmp, bytes)?;
    fs::rename(&tmp, path)?;
    Ok(())
}

fn not_found(e: std::io::Error, f: impl FnOnce() -> Error) -> Error {
    if e.kind() == std::io::ErrorKind::NotFound {
        f()
    } else {
        Error::Io(e)
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum WalkKind {
    File,
    Dir,
    Symlink,
    Special,
}

struct WalkEntry {
    abs: PathBuf,
    rel: String,
    kind: WalkKind,
    mode: u32,
    mtime_ms: i64,
}

/// Recursive pre-order walk; yields dirs before their children.
fn walk(base: &Path) -> Vec<WalkEntry> {
    let mut out = Vec::new();
    let mut stack = vec![base.to_path_buf()];
    while let Some(dir) = stack.pop() {
        let mut children: Vec<PathBuf> = match fs::read_dir(&dir) {
            Ok(it) => it.flatten().map(|e| e.path()).collect(),
            Err(_) => continue,
        };
        children.sort();
        for abs in children {
            let rel = abs
                .strip_prefix(base)
                .unwrap_or(&abs)
                .to_string_lossy()
                .replace('\\', "/");
            let md = match fs::symlink_metadata(&abs) {
                Ok(m) => m,
                Err(_) => continue,
            };
            let kind = if md.file_type().is_symlink() {
                WalkKind::Symlink
            } else if md.is_dir() {
                WalkKind::Dir
            } else if md.is_file() {
                WalkKind::File
            } else {
                WalkKind::Special
            };
            let mode = unix_mode(&md);
            let mtime_ms = md
                .modified()
                .ok()
                .and_then(|t| t.duration_since(std::time::UNIX_EPOCH).ok())
                .map(|d| d.as_millis() as i64)
                .unwrap_or(0);
            out.push(WalkEntry {
                abs: abs.clone(),
                rel,
                kind,
                mode,
                mtime_ms,
            });
            if kind == WalkKind::Dir {
                stack.push(abs);
            }
        }
    }
    out
}

fn record_ancestors(rel: &str, base: &Path, dirs: &mut HashMap<String, u32>) -> Result<()> {
    let parts: Vec<&str> = rel.split('/').collect();
    for i in 1..parts.len() {
        let d = parts[..i].join("/");
        if dirs.contains_key(&d) {
            continue;
        }
        let md = fs::metadata(base.join(&d))?;
        dirs.insert(d, unix_mode(&md));
    }
    Ok(())
}

#[cfg(unix)]
fn unix_mode(md: &fs::Metadata) -> u32 {
    use std::os::unix::fs::MetadataExt;
    md.mode() & 0o7777
}
#[cfg(not(unix))]
fn unix_mode(_md: &fs::Metadata) -> u32 {
    0o644
}

#[cfg(unix)]
fn apply_mode(path: &Path, mode: Option<u32>) {
    if let Some(m) = mode {
        use std::os::unix::fs::PermissionsExt;
        let _ = fs::set_permissions(path, fs::Permissions::from_mode(m));
    }
}
#[cfg(not(unix))]
fn apply_mode(_path: &Path, _mode: Option<u32>) {}

fn tracing_warn_skip(rel: &str) {
    eprintln!("snapshot-store: skipping special file {rel}");
}

// Read a whole small file helper used by the HTTP chunk server.
pub fn read_file(p: &Path) -> Result<Vec<u8>> {
    Ok(fs::read(p)?)
}

/// Concatenate plaintext of `chunks` (for tests/tools).
pub fn cat_chunks(store: &Store, ids: &[ChunkId]) -> Result<Vec<u8>> {
    let mut out = Vec::new();
    for c in ids {
        let z = store.read_chunk_compressed(c)?;
        let mut d = zstd::bulk::decompress(&z, CHUNK_MAX)?;
        out.append(&mut d);
    }
    Ok(out)
}

/// Read all bytes of an open file into memory (helper for callers).
pub fn slurp(mut r: impl Read) -> Result<Vec<u8>> {
    let mut v = Vec::new();
    r.read_to_end(&mut v)?;
    Ok(v)
}
