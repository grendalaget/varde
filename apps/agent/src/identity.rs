//! Node identity: an ed25519 key stored as PKCS#8 PEM at
//! `<data>/identity/node.key` (mode 0600). fingerprint = "node_" + first 16 hex
//! chars of SHA-256(public key), matching go/identity.

use std::fs;
use std::path::Path;

use anyhow::{Context, Result};
use ed25519_dalek::pkcs8::spki::der::pem::LineEnding;
use ed25519_dalek::pkcs8::{DecodePrivateKey, EncodePrivateKey};
use ed25519_dalek::{SigningKey, VerifyingKey};

/// Loads the node key from `path`, generating a fresh one on first start.
/// Returns (fingerprint, created).
pub fn load_or_create(path: &Path) -> Result<(String, bool)> {
    if path.exists() {
        let pem =
            fs::read_to_string(path).with_context(|| format!("read key {}", path.display()))?;
        let key = SigningKey::from_pkcs8_pem(&pem)
            .with_context(|| format!("parse key {}", path.display()))?;
        return Ok((fingerprint(&key.verifying_key()), false));
    }

    let mut rng = rand::rngs::OsRng;
    let key = SigningKey::generate(&mut rng);
    let pem = key
        .to_pkcs8_pem(LineEnding::LF)
        .context("encode PKCS#8 PEM")?;

    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).with_context(|| format!("create dir {}", parent.display()))?;
    }
    fs::write(path, pem.as_bytes()).with_context(|| format!("write key {}", path.display()))?;
    restrict_permissions(path)?;
    Ok((fingerprint(&key.verifying_key()), true))
}

/// Loads the node key (must exist — run `enroll` first) and returns it.
pub fn load(path: &Path) -> Result<SigningKey> {
    let pem = fs::read_to_string(path).with_context(|| format!("read key {}", path.display()))?;
    SigningKey::from_pkcs8_pem(&pem).with_context(|| format!("parse key {}", path.display()))
}

/// base64(raw 32-byte public key) — the form the control plane stores.
pub fn public_key_b64(key: &SigningKey) -> String {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.encode(key.verifying_key().as_bytes())
}

/// fingerprint = "node_" + first 16 hex chars of SHA-256(raw 32-byte public
/// key), matching go/identity.Fingerprint. Pre-enrollment log label only —
/// after enrollment the agent uses the control-plane-assigned node_id.
fn fingerprint(vk: &VerifyingKey) -> String {
    use sha2::{Digest, Sha256};
    let sum = Sha256::digest(vk.as_bytes());
    format!("node_{}", &hex::encode(sum)[..16])
}

#[cfg(unix)]
fn restrict_permissions(path: &Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o600))
        .with_context(|| format!("chmod {}", path.display()))
}

#[cfg(windows)]
fn restrict_permissions(_path: &Path) -> Result<()> {
    // ACLs are inherited; the data dir sits under %ProgramData% which is
    // already admin-only. Fine-grained ACLing lands with the Windows service.
    Ok(())
}
