//! Manifest format v1: canonical JSON — UTF-8, fixed struct field order,
//! `files` sorted by `path` bytewise, no insignificant whitespace.

use serde::{Deserialize, Serialize};

pub const FORMAT_V1: u32 = 1;
pub const CHUNK_ID_LEN: usize = 64;

/// `snap_` + lowercase base32 (no padding) of the first 16 bytes of the
/// manifest BLAKE3 digest — the id can never change under the same bytes (I5).
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct SnapshotId(pub String);

impl std::fmt::Display for SnapshotId {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl SnapshotId {
    pub fn parse(s: &str) -> Option<SnapshotId> {
        if s.starts_with("snap_") && s.len() > 5 {
            Some(SnapshotId(s.to_string()))
        } else {
            None
        }
    }
}

#[derive(Debug, Clone)]
pub struct SnapshotMeta {
    pub server_id: String,
    pub execution_id: String,
    pub epoch: i64,
    pub node_id: String,
    pub parent: Option<String>,
    pub deployment_id: String,
    pub created_at_unix_ms: i64,
    pub reason: String,
}

#[derive(Debug, Clone)]
pub struct SnapshotInfo {
    pub id: SnapshotId,
    pub digest: String,
    pub size_bytes: u64,
    pub stored_bytes: u64,
    pub file_count: usize,
    pub chunk_count: usize,
    /// chunks written for the first time (dedup visibility)
    pub new_chunks: usize,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Manifest {
    pub format: u32,
    pub server_id: String,
    pub execution_id: String,
    pub epoch: i64,
    pub node_id: String,
    pub parent: Option<String>,
    pub deployment_id: String,
    pub created_at_unix_ms: i64,
    pub reason: String,
    /// entries sorted by `path` bytewise
    pub files: Vec<ManifestEntry>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ManifestEntry {
    pub path: String,
    #[serde(rename = "type")]
    pub typ: String, // "file" | "dir"
    #[serde(skip_serializing_if = "Option::is_none")]
    pub size: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mode: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mtime_unix_ms: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub blake3: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub chunks: Option<Vec<String>>,
}

impl Manifest {
    /// Canonical bytes (serde preserves struct field order; no whitespace).
    pub fn canonical_bytes(&self) -> serde_json::Result<Vec<u8>> {
        serde_json::to_vec(self)
    }

    pub fn digest_id(bytes: &[u8]) -> SnapshotId {
        let d = blake3::hash(bytes);
        SnapshotId(format!("snap_{}", base32_lower(&d.as_bytes()[..16])))
    }

    pub fn digest(bytes: &[u8]) -> String {
        blake3::hash(bytes).to_hex().to_string()
    }
}

/// Lowercase RFC 4648 base32 without padding.
pub fn base32_lower(data: &[u8]) -> String {
    const SYMS: &[u8; 32] = b"abcdefghijklmnopqrstuvwxyz234567";
    let mut out = String::with_capacity(data.len() * 8 / 5 + 1);
    let mut acc: u32 = 0;
    let mut nbits: u32 = 0;
    for &b in data {
        acc = (acc << 8) | b as u32;
        nbits += 8;
        while nbits >= 5 {
            out.push(SYMS[((acc >> (nbits - 5)) & 0x1f) as usize] as char);
            nbits -= 5;
        }
    }
    if nbits > 0 {
        out.push(SYMS[((acc << (5 - nbits)) & 0x1f) as usize] as char);
    }
    out
}
