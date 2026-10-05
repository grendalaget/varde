//! Canonical agent request signing (control-plane.md §Node authentication),
//! identical to go/identity: ed25519 over
//! `"p2pgames-agent-v1\n" + METHOD + "\n" + PATH + "\n" + TIMESTAMP + "\n" + hex(sha256(body))`.

use base64::Engine;
use ed25519_dalek::{Signer, SigningKey};
use sha2::{Digest, Sha256};

pub const SIG_VERSION: &str = "p2pgames-agent-v1";
/// Header names (also in go/identity).
pub const HDR_NODE: &str = "X-P2PG-Node";
pub const HDR_TIMESTAMP: &str = "X-P2PG-Timestamp";
pub const HDR_SIGNATURE: &str = "X-P2PG-Signature";

pub fn signing_string(method: &str, path: &str, timestamp_unix_ms: i64, body: &[u8]) -> String {
    let digest = Sha256::digest(body);
    format!(
        "{SIG_VERSION}\n{method}\n{path}\n{timestamp_unix_ms}\n{}",
        hex::encode(digest)
    )
}

pub fn sign(
    key: &SigningKey,
    method: &str,
    path: &str,
    timestamp_unix_ms: i64,
    body: &[u8],
) -> String {
    let sig = key.sign(signing_string(method, path, timestamp_unix_ms, body).as_bytes());
    base64::engine::general_purpose::STANDARD.encode(sig.to_bytes())
}

pub fn public_key_b64(key: &SigningKey) -> String {
    base64::engine::general_purpose::STANDARD.encode(key.verifying_key().to_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;
    use ed25519_dalek::{Signature, VerifyingKey};

    /// Cross-language vector committed at
    /// docs/architecture/testdata/agent-signature.json — the same file the Go
    /// side tests against.
    #[test]
    fn go_vector() {
        #[derive(serde::Deserialize)]
        struct V {
            private_key_pkcs8_pem_b64_seed_hex: String,
            public_key_base64: String,
            request: Req,
            expected_signature_base64: String,
        }
        #[derive(serde::Deserialize)]
        struct Req {
            method: String,
            path: String,
            timestamp_unix_ms: i64,
            body: String,
        }
        let v: V = serde_json::from_str(include_str!(
            "../../../docs/architecture/testdata/agent-signature.json"
        ))
        .unwrap();
        let seed: [u8; 32] = hex::decode(&v.private_key_pkcs8_pem_b64_seed_hex)
            .unwrap()
            .try_into()
            .unwrap();
        let key = SigningKey::from_bytes(&seed);
        assert_eq!(public_key_b64(&key), v.public_key_base64);
        let sig = sign(
            &key,
            &v.request.method,
            &v.request.path,
            v.request.timestamp_unix_ms,
            v.request.body.as_bytes(),
        );
        assert_eq!(sig, v.expected_signature_base64);
        // self-verify
        let vk = VerifyingKey::from_bytes(&key.verifying_key().to_bytes()).unwrap();
        let raw: [u8; 64] = base64::engine::general_purpose::STANDARD
            .decode(&sig)
            .unwrap()
            .try_into()
            .unwrap();
        assert!(vk
            .verify_strict(
                signing_string(
                    &v.request.method,
                    &v.request.path,
                    v.request.timestamp_unix_ms,
                    v.request.body.as_bytes()
                )
                .as_bytes(),
                &Signature::from_bytes(&raw)
            )
            .is_ok());
    }
}
