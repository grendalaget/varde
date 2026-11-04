use std::io::Read;
use std::path::{Component, Path, PathBuf};

use game_driver_api::{OciSpec, Result};
use hyper::{StatusCode, Uri};
use serde_json::Value;
use sha2::{Digest, Sha256};

use super::{
    copy_limited, http_response, resolve_redirect, spawn_fs, HttpRuntimes, REQUEST_TIMEOUT,
};

const ACCEPT: &str = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json";
const JSON_LIMIT: u64 = 8 * 1024 * 1024;
const DOWNLOAD_LIMIT: u64 = 512 * 1024 * 1024;
const UNPACKED_LIMIT: u64 = 4 * 1024 * 1024 * 1024;
const ENTRY_LIMIT: usize = 200_000;

fn digest_hex(digest: &str) -> Result<&str> {
    let hex = digest
        .strip_prefix("sha256:")
        .ok_or("OCI requires a sha256 digest")?;
    if hex.len() != 64
        || !hex
            .bytes()
            .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
    {
        return Err("invalid OCI sha256 digest".into());
    }
    Ok(hex)
}

fn verify(body: &[u8], digest: &str) -> Result<()> {
    let want = digest_hex(digest)?;
    if hex::encode(Sha256::digest(body)) != want {
        return Err(format!("OCI digest mismatch: wanted {digest}").into());
    }
    Ok(())
}

fn field<'a>(value: &'a Value, name: &str) -> Result<&'a str> {
    value[name]
        .as_str()
        .ok_or_else(|| format!("OCI missing {name}").into())
}

fn component(value: &str) -> bool {
    !value.is_empty()
        && value != "."
        && value != ".."
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_-.".contains(&b))
}

fn prefixes(spec: &OciSpec) -> Result<Vec<PathBuf>> {
    if !component(&spec.os) || !component(&spec.arch) || !spec.repository.split('/').all(component)
    {
        return Err("invalid OCI repository or platform".into());
    }
    let mut paths = Vec::new();
    for path in &spec.paths {
        let relative = path
            .strip_prefix('/')
            .ok_or("OCI prefixes must be absolute image paths")?;
        let relative = clean_path(Path::new(relative))?;
        paths.push(relative);
    }
    paths.sort();
    paths.dedup();
    if paths.is_empty() {
        return Err("OCI requires at least one path prefix".into());
    }
    Ok(paths)
}

pub(super) async fn fetch(provider: &HttpRuntimes, spec: &OciSpec) -> Result<PathBuf> {
    let hex = digest_hex(&spec.digest)?;
    let paths = prefixes(spec)?;
    let registry = Registry::new(&spec.registry, &spec.repository)?;
    let key = hex::encode(Sha256::digest(serde_json::to_vec(&(
        &spec.registry,
        &spec.repository,
        &spec.os,
        &spec.arch,
        &paths,
    ))?));
    let dest = provider.root.join("oci").join(hex).join(key);
    let lock = provider.dest_lock(&dest);
    let _guard = lock.lock().await;
    if HttpRuntimes::done(&dest) {
        return Ok(dest.join("root"));
    }
    let staging = dest.with_extension(format!(
        "staging-{}-{}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)?
            .as_nanos()
    ));
    let dir = staging.clone();
    spawn_fs(move || {
        std::fs::create_dir_all(dir.join("root"))?;
        Ok(())
    })
    .await?;
    let result = tokio::time::timeout(REQUEST_TIMEOUT, fetch_into(registry, spec, &staging, paths))
        .await
        .map_err(|_| "OCI fetch timed out".into())
        .and_then(|r| r);
    if let Err(error) = result {
        let dir = staging.clone();
        let _ = spawn_fs(move || {
            std::fs::remove_dir_all(dir)?;
            Ok(())
        })
        .await;
        return Err(error);
    }
    let dir = dest.clone();
    spawn_fs(move || {
        std::fs::write(staging.join(".complete"), b"ok")?;
        match std::fs::rename(&staging, &dir) {
            Ok(()) => Ok(()),
            Err(_) if HttpRuntimes::done(&dir) => {
                std::fs::remove_dir_all(staging)?;
                Ok(())
            }
            Err(error) => Err(error.into()),
        }
    })
    .await?;
    Ok(dest.join("root"))
}

struct Registry {
    base: String,
    token: Option<String>,
}

impl Registry {
    fn new(host: &str, repository: &str) -> Result<Self> {
        let authority: hyper::http::uri::Authority = host.parse()?;
        if authority.as_str().contains('@') {
            return Err("invalid OCI registry".into());
        }
        let host = match host {
            "docker.io" | "index.docker.io" => "registry-1.docker.io",
            other => other,
        };
        let scheme = if authority
            .host()
            .parse::<std::net::IpAddr>()
            .is_ok_and(|ip| ip.is_loopback())
        {
            "http"
        } else {
            "https"
        };
        Ok(Self {
            base: format!("{scheme}://{host}/v2/{repository}"),
            token: None,
        })
    }

    async fn get(&mut self, suffix: &str, limit: u64) -> Result<Vec<u8>> {
        let url = format!("{}/{}", self.base, suffix);
        for attempt in 0..2 {
            let auth = self.token.as_ref().map(|t| format!("Bearer {t}"));
            let mut headers = vec![("accept", ACCEPT)];
            if let Some(auth) = &auth {
                headers.push(("authorization", auth));
            }
            let (status, response_headers, body) = get_redirects(&url, headers, limit).await?;
            if status.is_success() {
                return Ok(body);
            }
            if status != StatusCode::UNAUTHORIZED || attempt != 0 {
                return Err(format!("OCI registry GET {suffix}: {status}").into());
            }
            let challenge = response_headers
                .get("www-authenticate")
                .ok_or("OCI registry missing bearer challenge")?
                .to_str()?;
            let token_url = token_url(challenge, &url)?;
            let (status, _, body) = get_redirects(&token_url, Vec::new(), JSON_LIMIT).await?;
            if !status.is_success() {
                return Err(format!("OCI token request: {status}").into());
            }
            let body: Value = serde_json::from_slice(&body)?;
            self.token = Some(
                body["token"]
                    .as_str()
                    .or_else(|| body["access_token"].as_str())
                    .filter(|s| !s.is_empty())
                    .ok_or("OCI token response missing token")?
                    .to_string(),
            );
        }
        Err("OCI authentication failed".into())
    }
}

async fn get_redirects(
    url: &str,
    mut headers: Vec<(&str, &str)>,
    limit: u64,
) -> Result<(StatusCode, hyper::HeaderMap, Vec<u8>)> {
    let mut url = url.to_string();
    for _ in 0..=8 {
        let (status, response_headers, body) = http_response(&url, &headers, Some(limit)).await?;
        if !status.is_redirection() {
            return Ok((status, response_headers, body));
        }
        let uri: Uri = url.parse()?;
        let next = resolve_redirect(
            &uri,
            response_headers
                .get("location")
                .ok_or("OCI redirect without location")?
                .to_str()?,
        )?;
        let next_uri: Uri = next.parse()?;
        if next_uri.scheme_str() != Some("https")
            && !(uri.scheme_str() == Some("http")
                && uri.authority() == next_uri.authority()
                && next_uri.scheme_str() == Some("http"))
        {
            return Err("OCI redirect downgraded to http".into());
        }
        if uri.authority() != next_uri.authority() || uri.scheme() != next_uri.scheme() {
            headers.retain(|(name, _)| *name != "authorization");
        }
        url = next;
    }
    Err("too many OCI redirects".into())
}

fn encode(value: &str) -> String {
    value
        .bytes()
        .map(|b| {
            if b.is_ascii_alphanumeric() || b"-._~".contains(&b) {
                (b as char).to_string()
            } else {
                format!("%{b:02X}")
            }
        })
        .collect()
}

fn token_url(challenge: &str, origin: &str) -> Result<String> {
    let (scheme, params) = challenge
        .split_once(' ')
        .ok_or("invalid OCI auth challenge")?;
    if !scheme.eq_ignore_ascii_case("bearer") {
        return Err("OCI requires bearer auth".into());
    }
    let mut values = std::collections::HashMap::new();
    let mut rest = params.trim();
    while !rest.is_empty() {
        let (key, value) = rest.split_once('=').ok_or("invalid OCI bearer parameter")?;
        let value = value
            .trim_start()
            .strip_prefix('"')
            .ok_or("unquoted OCI bearer parameter")?;
        let (value, tail) = value
            .split_once('"')
            .ok_or("unterminated OCI bearer parameter")?;
        values.insert(key.trim().to_ascii_lowercase(), value);
        rest = tail.trim_start();
        if !rest.is_empty() {
            rest = rest
                .strip_prefix(',')
                .ok_or("invalid OCI bearer separator")?
                .trim_start();
        }
    }
    let realm = *values
        .get("realm")
        .ok_or("OCI bearer challenge missing realm")?;
    let uri: Uri = realm.parse()?;
    let origin: Uri = origin.parse()?;
    if uri.authority().is_none()
        || uri.authority().is_some_and(|a| a.as_str().contains('@'))
        || (uri.scheme_str() != Some("https")
            && !(origin.scheme_str() == Some("http")
                && uri.scheme_str() == Some("http")
                && origin.authority() == uri.authority()))
    {
        return Err("unsafe OCI token realm".into());
    }
    let mut url = realm.to_string();
    for key in ["service", "scope"] {
        if let Some(value) = values.get(key) {
            url.push(if url.contains('?') { '&' } else { '?' });
            url.push_str(&format!("{key}={}", encode(value)));
        }
    }
    Ok(url)
}

async fn fetch_into(
    mut registry: Registry,
    spec: &OciSpec,
    staging: &Path,
    paths: Vec<PathBuf>,
) -> Result<()> {
    let body = registry
        .get(&format!("manifests/{}", spec.digest), JSON_LIMIT)
        .await?;
    verify(&body, &spec.digest)?;
    let mut manifest: Value = serde_json::from_slice(&body)?;
    if let Some(entries) = manifest["manifests"].as_array() {
        let descriptor = entries
            .iter()
            .find(|entry| {
                entry["platform"]["os"] == spec.os && entry["platform"]["architecture"] == spec.arch
            })
            .ok_or("OCI platform not found")?;
        let digest = field(descriptor, "digest")?;
        digest_hex(digest)?;
        let body = registry
            .get(&format!("manifests/{digest}"), JSON_LIMIT)
            .await?;
        verify(&body, digest)?;
        manifest = serde_json::from_slice(&body)?;
    }
    let config_digest = field(&manifest["config"], "digest")?;
    digest_hex(config_digest)?;
    let config = registry
        .get(&format!("blobs/{config_digest}"), JSON_LIMIT)
        .await?;
    verify(&config, config_digest)?;
    let config: Value = serde_json::from_slice(&config)?;
    if config["os"] != spec.os || config["architecture"] != spec.arch {
        return Err("OCI config platform mismatch".into());
    }
    let layers = manifest["layers"]
        .as_array()
        .ok_or("OCI manifest missing layers")?;
    let mut compressed = 0u64;
    let mut budget = Budget::default();
    for layer in layers {
        let digest = field(layer, "digest")?;
        digest_hex(digest)?;
        let media_type = field(layer, "mediaType")?.to_string();
        let body = registry
            .get(
                &format!("blobs/{digest}"),
                DOWNLOAD_LIMIT.saturating_sub(compressed),
            )
            .await?;
        compressed = compressed
            .checked_add(body.len() as u64)
            .ok_or("OCI size overflow")?;
        let digest = digest.to_string();
        let root = staging.join("root");
        let paths = paths.clone();
        budget = spawn_fs(move || {
            verify(&body, &digest)?;
            apply_layer(&body, &media_type, &root, &paths, &mut budget)?;
            Ok(budget)
        })
        .await?;
    }
    let root = staging.join("root");
    spawn_fs(move || validate_symlinks(&root)).await?;
    Ok(())
}

fn validate_symlinks(root: &Path) -> Result<()> {
    fn walk(dir: &Path, root: &Path) -> Result<()> {
        for entry in std::fs::read_dir(dir)? {
            let entry = entry?;
            let kind = entry.file_type()?;
            if kind.is_symlink() {
                let target = std::fs::canonicalize(entry.path())?;
                if !target.starts_with(root) {
                    return Err("unsafe OCI symlink escape".into());
                }
            } else if kind.is_dir() {
                walk(&entry.path(), root)?;
            }
        }
        Ok(())
    }
    let root = std::fs::canonicalize(root)?;
    walk(&root, &root)
}

#[derive(Default)]
struct Budget {
    entries: usize,
    unpacked: u64,
}

fn clean_path(path: &Path) -> Result<PathBuf> {
    let mut clean = PathBuf::new();
    for part in path.components() {
        match part {
            Component::Normal(name) => {
                if name.to_string_lossy().contains(['\\', ':']) {
                    return Err("unsafe OCI path".into());
                }
                clean.push(name);
            }
            Component::CurDir => (),
            _ => return Err("unsafe OCI path traversal".into()),
        }
    }
    Ok(clean)
}

fn selected(path: &Path, prefixes: &[PathBuf]) -> bool {
    prefixes.iter().any(|prefix| path.starts_with(prefix))
}

fn relevant(path: &Path, prefixes: &[PathBuf]) -> bool {
    selected(path, prefixes) || prefixes.iter().any(|prefix| prefix.starts_with(path))
}

fn no_symlink_parents(root: &Path, path: &Path) -> Result<()> {
    let mut parent = root.to_path_buf();
    if let Some(parts) = path.parent() {
        for part in parts.components() {
            parent.push(part);
            if std::fs::symlink_metadata(&parent).is_ok_and(|m| m.file_type().is_symlink()) {
                return Err("OCI entry has a symlink ancestor".into());
            }
        }
    }
    Ok(())
}

fn remove(path: &Path) -> Result<()> {
    match std::fs::symlink_metadata(path) {
        Ok(meta) if meta.is_dir() => std::fs::remove_dir_all(path)?,
        Ok(_) => std::fs::remove_file(path)?,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => (),
        Err(error) => return Err(error.into()),
    }
    Ok(())
}

fn reader<'a>(body: &'a [u8], media_type: &str) -> Result<Box<dyn Read + 'a>> {
    match media_type {
        "application/vnd.oci.image.layer.v1.tar+gzip"
        | "application/vnd.docker.image.rootfs.diff.tar.gzip" => {
            Ok(Box::new(flate2::read::GzDecoder::new(body)))
        }
        "application/vnd.oci.image.layer.v1.tar" => Ok(Box::new(body)),
        _ => Err(format!("unsupported OCI layer media type: {media_type}").into()),
    }
}

fn whiteout(path: &Path) -> Option<(PathBuf, bool)> {
    let name = path.file_name()?.to_str()?;
    let parent = path.parent()?;
    if name == ".wh..wh..opq" {
        return Some((parent.to_path_buf(), true));
    }
    name.strip_prefix(".wh.")
        .map(|name| (parent.join(name), false))
}

fn apply_layer(
    body: &[u8],
    media_type: &str,
    root: &Path,
    prefixes: &[PathBuf],
    budget: &mut Budget,
) -> Result<()> {
    // Whiteouts affect lower layers, never files added by this layer, regardless of tar order.
    let mut archive = tar::Archive::new(reader(body, media_type)?.take(UNPACKED_LIMIT + 1));
    let mut directory_modes = Vec::new();
    for entry in archive.entries()? {
        let entry = entry?;
        budget.entries += 1;
        budget.unpacked = budget
            .unpacked
            .checked_add(entry.size())
            .ok_or("OCI size overflow")?;
        if budget.entries > ENTRY_LIMIT || budget.unpacked > UNPACKED_LIMIT {
            return Err("OCI archive exceeds entry or unpacked byte limit".into());
        }
        let path = clean_path(&entry.path()?)?;
        if path
            .file_name()
            .and_then(|n| n.to_str())
            .is_some_and(|n| [".wh.", ".wh..", ".wh..."].contains(&n))
        {
            return Err("invalid OCI whiteout name".into());
        }
        if let Some((target, opaque)) = whiteout(&path) {
            let target = clean_path(&target)?;
            if !relevant(&target, prefixes) {
                continue;
            }
            if !entry.header().entry_type().is_file() || entry.size() != 0 {
                return Err("invalid OCI whiteout".into());
            }
            no_symlink_parents(root, &target)?;
            let output = root.join(&target);
            if opaque {
                if std::fs::symlink_metadata(&output).is_ok_and(|m| m.file_type().is_symlink()) {
                    return Err("OCI opaque directory is a symlink".into());
                }
                if output.is_dir() {
                    for entry in std::fs::read_dir(output)? {
                        remove(&entry?.path())?;
                    }
                }
            } else {
                remove(&output)?;
            }
        }
    }
    let mut archive = tar::Archive::new(reader(body, media_type)?.take(UNPACKED_LIMIT + 1));
    for entry in archive.entries()? {
        let mut entry = entry?;
        let path = clean_path(&entry.path()?)?;
        if whiteout(&path).is_some() || !selected(&path, prefixes) {
            continue;
        }
        no_symlink_parents(root, &path)?;
        let output = root.join(&path);
        let kind = entry.header().entry_type();
        if path.as_os_str().is_empty() && !kind.is_dir() {
            return Err("invalid OCI root entry".into());
        }
        if kind.is_dir() {
            if !output.is_dir() || std::fs::symlink_metadata(&output)?.file_type().is_symlink() {
                remove(&output)?;
                std::fs::create_dir_all(&output)?;
            }
            directory_modes.push((output, entry.header().mode()? & 0o777));
        } else {
            if let Some(parent) = output.parent() {
                std::fs::create_dir_all(parent)?;
            }
            remove(&output)?;
            if kind.is_file() {
                let mut file = std::fs::File::create(&output)?;
                copy_limited(
                    &mut entry,
                    &mut file,
                    UNPACKED_LIMIT,
                    &path.to_string_lossy(),
                )?;
                #[cfg(unix)]
                {
                    use std::os::unix::fs::PermissionsExt;
                    std::fs::set_permissions(
                        &output,
                        std::fs::Permissions::from_mode(entry.header().mode()? & 0o777),
                    )?;
                }
            } else if kind.is_symlink() {
                let target = entry.link_name()?.ok_or("OCI symlink missing target")?;
                safe_link(&path, &target)?;
                #[cfg(unix)]
                std::os::unix::fs::symlink(&target, &output)?;
                #[cfg(windows)]
                std::os::windows::fs::symlink_file(&target, &output)?;
                #[cfg(not(any(unix, windows)))]
                return Err("OCI symlinks unsupported on this platform".into());
            } else {
                return Err("unsupported OCI entry type".into());
            }
        }
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        for (path, mode) in directory_modes.into_iter().rev() {
            let relative = path.strip_prefix(root)?;
            no_symlink_parents(root, relative)?;
            if std::fs::symlink_metadata(&path).is_ok_and(|m| m.is_dir()) {
                std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode))?;
            }
        }
    }
    Ok(())
}

fn safe_link(path: &Path, target: &Path) -> Result<()> {
    let mut depth = path.parent().map_or(0, |p| p.components().count());
    let mut normal_seen = false;
    if target.as_os_str().is_empty() {
        return Err("empty OCI symlink".into());
    }
    for part in target.components() {
        match part {
            Component::Normal(name) if !name.to_string_lossy().contains(['\\', ':']) => {
                normal_seen = true;
                depth += 1;
            }
            Component::CurDir => (),
            Component::ParentDir if depth > 0 && !normal_seen => depth -= 1,
            _ => return Err("unsafe OCI symlink escape".into()),
        }
    }
    Ok(())
}

#[cfg(all(test, target_os = "linux"))]
mod tests {
    use super::*;
    use game_driver_api::RuntimeProvider;
    use std::collections::HashMap;
    use std::io::Write;
    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        Arc,
    };
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    const GZIP: &str = "application/vnd.oci.image.layer.v1.tar+gzip";

    fn digest(bytes: &[u8]) -> String {
        format!("sha256:{}", hex::encode(Sha256::digest(bytes)))
    }

    fn layer(entries: &[(&str, u8, &str)]) -> Vec<u8> {
        let mut tar = tar::Builder::new(Vec::new());
        for (path, kind, contents) in entries {
            let mut header = tar::Header::new_gnu();
            header.set_mode(0o755);
            header.set_entry_type(tar::EntryType::new(*kind));
            header.set_size(if *kind == b'0' {
                contents.len() as u64
            } else {
                0
            });
            if *kind == b'2' {
                header.set_link_name(contents).unwrap();
            }
            // Raw names also let tests construct malicious traversal entries.
            header.as_mut_bytes()[..path.len()].copy_from_slice(path.as_bytes());
            header.set_cksum();
            tar.append(
                &header,
                if *kind == b'0' {
                    contents.as_bytes()
                } else {
                    b""
                },
            )
            .unwrap();
        }
        let mut gz = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::fast());
        gz.write_all(&tar.into_inner().unwrap()).unwrap();
        gz.finish().unwrap()
    }

    struct Fake {
        spec: OciSpec,
        requests: Arc<AtomicUsize>,
        task: tokio::task::JoinHandle<()>,
    }

    impl Drop for Fake {
        fn drop(&mut self) {
            self.task.abort();
        }
    }

    async fn registry(layers: Vec<Vec<u8>>, index: bool, corrupt: Option<&'static str>) -> Fake {
        let mut routes = HashMap::new();
        let config = br#"{"os":"linux","architecture":"amd64"}"#.to_vec();
        let config_digest = digest(&config);
        routes.insert(format!("/v2/test/image/blobs/{config_digest}"), config);
        let mut descriptors = Vec::new();
        for layer in layers {
            let hash = digest(&layer);
            descriptors
                .push(serde_json::json!({"mediaType":GZIP,"digest":hash,"size":layer.len()}));
            routes.insert(
                format!("/v2/test/image/blobs/{hash}"),
                if corrupt == Some("layer") {
                    b"corrupt".to_vec()
                } else {
                    layer
                },
            );
        }
        let manifest = serde_json::to_vec(&serde_json::json!({"schemaVersion":2,
            "config":{"digest":config_digest},"layers":descriptors}))
        .unwrap();
        let manifest_digest = digest(&manifest);
        routes.insert(
            format!("/v2/test/image/manifests/{manifest_digest}"),
            if corrupt == Some("manifest") {
                b"{}".to_vec()
            } else {
                manifest.clone()
            },
        );
        let pin = if index {
            let body = serde_json::to_vec(&serde_json::json!({"schemaVersion":2,"manifests":[
                {"digest":digest(b"wrong-platform"),"platform":{"os":"linux","architecture":"arm64"}},
                {"digest":manifest_digest,"platform":{"os":"linux","architecture":"amd64"}}
            ]})).unwrap();
            let pin = digest(&body);
            routes.insert(
                format!("/v2/test/image/manifests/{pin}"),
                if corrupt == Some("index") {
                    b"{}".to_vec()
                } else {
                    body
                },
            );
            pin
        } else {
            manifest_digest
        };
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let requests = Arc::new(AtomicUsize::new(0));
        let count = requests.clone();
        let task = tokio::spawn(async move {
            while let Ok((mut stream, _)) = listener.accept().await {
                count.fetch_add(1, Ordering::SeqCst);
                let mut request = Vec::new();
                loop {
                    let mut buf = [0u8; 4096];
                    let n = stream.read(&mut buf).await.unwrap();
                    if n == 0 {
                        break;
                    }
                    request.extend_from_slice(&buf[..n]);
                    if request.windows(4).any(|w| w == b"\r\n\r\n") {
                        break;
                    }
                }
                let request = String::from_utf8(request).unwrap();
                let path = request.split_whitespace().nth(1).unwrap();
                let (status, extra, body) = if path.starts_with("/token?") {
                    assert!(path.contains("service=fake"));
                    assert!(path.contains("scope=repository%3Atest%2Fimage%3Apull"));
                    (
                        "200 OK",
                        String::new(),
                        br#"{"access_token":"anonymous-token"}"#.to_vec(),
                    )
                } else if !request
                    .to_ascii_lowercase()
                    .contains("authorization: bearer anonymous-token")
                {
                    ("401 Unauthorized", format!("WWW-Authenticate: Bearer realm=\"http://{addr}/token\",service=\"fake\",scope=\"repository:test/image:pull\"\r\n"), Vec::new())
                } else if corrupt == Some("redirect")
                    && !path.contains('?')
                    && routes.contains_key(path)
                {
                    (
                        "307 Temporary Redirect",
                        format!("Location: {path}?redirected\r\n"),
                        Vec::new(),
                    )
                } else if let Some(body) = routes.get(path.split('?').next().unwrap()) {
                    ("200 OK", String::new(), body.clone())
                } else {
                    ("404 Not Found", String::new(), Vec::new())
                };
                let response = format!(
                    "HTTP/1.1 {status}\r\n{extra}Content-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                stream.write_all(response.as_bytes()).await.unwrap();
                stream.write_all(&body).await.unwrap();
            }
        });
        Fake {
            spec: OciSpec {
                registry: addr.to_string(),
                repository: "test/image".into(),
                digest: pin,
                os: "linux".into(),
                arch: "amd64".into(),
                paths: vec!["/app".into()],
            },
            requests,
            task,
        }
    }

    #[tokio::test]
    async fn bearer_index_layers_whiteouts_and_cache() {
        let fake = registry(
            vec![
                layer(&[
                    ("app/old", b'0', "old"),
                    ("app/opaque/gone", b'0', "gone"),
                    ("app/bin/run", b'0', "old executable"),
                    ("apple/not-selected", b'0', "no"),
                ]),
                layer(&[
                    ("app/opaque/new", b'0', "new"),
                    ("app/opaque/.wh..wh..opq", b'0', ""),
                    ("app/.wh.old", b'0', ""),
                    ("app/bin/run", b'0', "new executable"),
                    ("app/link", b'2', "bin/run"),
                ]),
            ],
            true,
            None,
        )
        .await;
        let temp = tempfile::tempdir().unwrap();
        let provider = HttpRuntimes::new(temp.path().to_path_buf());
        let root = provider.fetch_oci(&fake.spec).await.unwrap();
        assert!(!root.join("app/old").exists());
        assert!(!root.join("app/opaque/gone").exists());
        assert!(!root.join("apple").exists());
        assert!(!root.join("app/.wh.old").exists());
        assert_eq!(std::fs::read(root.join("app/opaque/new")).unwrap(), b"new");
        assert_eq!(
            std::fs::read(root.join("app/link")).unwrap(),
            b"new executable"
        );
        use std::os::unix::fs::PermissionsExt;
        assert_eq!(
            std::fs::metadata(root.join("app/bin/run"))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o755
        );
        let requests = fake.requests.load(Ordering::SeqCst);
        assert_eq!(provider.fetch_oci(&fake.spec).await.unwrap(), root);
        assert_eq!(fake.requests.load(Ordering::SeqCst), requests);
        let mut other = fake.spec.clone();
        other.paths = vec!["/apple".into()];
        let other_root = provider.fetch_oci(&other).await.unwrap();
        assert_ne!(other_root, root);
        assert!(other_root.join("apple/not-selected").is_file());
    }

    #[tokio::test]
    async fn rejects_pin_child_manifest_and_layer_digest_mismatches() {
        for corrupt in ["index", "manifest", "layer"] {
            let fake = registry(
                vec![layer(&[("app/file", b'0', "hi")])],
                true,
                Some(corrupt),
            )
            .await;
            let temp = tempfile::tempdir().unwrap();
            let error = HttpRuntimes::new(temp.path().to_path_buf())
                .fetch_oci(&fake.spec)
                .await
                .unwrap_err();
            assert!(error.to_string().contains("digest mismatch"), "{error}");
            assert!(!temp.path().join("outside").exists());
            assert!(!walk_complete(temp.path()));
        }
    }

    fn walk_complete(path: &Path) -> bool {
        std::fs::read_dir(path).unwrap().any(|e| {
            let e = e.unwrap();
            e.file_name() == ".complete"
                || (e.file_type().unwrap().is_dir() && walk_complete(&e.path()))
        })
    }

    #[tokio::test]
    async fn rejects_traversal_and_symlink_escapes_without_publishing() {
        for entries in [
            vec![("../outside", b'0', "bad")],
            vec![("/app/absolute", b'0', "bad")],
            vec![("app/link", b'2', "/outside")],
            vec![("app/link", b'2', "../../outside")],
            vec![("app/link", b'2', "bin"), ("app/link/file", b'0', "bad")],
            vec![
                ("app/link", b'2', "../"),
                ("app/evil", b'2', "link/../../outside"),
            ],
            vec![("app/.wh...", b'0', "")],
            vec![("app/hardlink", b'1', "")],
        ] {
            let fake = registry(vec![layer(&entries)], false, None).await;
            let temp = tempfile::tempdir().unwrap();
            assert!(
                HttpRuntimes::new(temp.path().join("cache"))
                    .fetch_oci(&fake.spec)
                    .await
                    .is_err(),
                "{entries:?}"
            );
            assert!(!temp.path().join("outside").exists());
            assert!(!walk_complete(temp.path()));
        }
    }

    #[tokio::test]
    async fn empty_image_and_missing_platform() {
        let fake = registry(Vec::new(), true, None).await;
        let temp = tempfile::tempdir().unwrap();
        let provider = HttpRuntimes::new(temp.path().to_path_buf());
        let root = provider.fetch_oci(&fake.spec).await.unwrap();
        assert_eq!(std::fs::read_dir(root).unwrap().count(), 0);
        let mut missing = fake.spec.clone();
        missing.arch = "riscv64".into();
        assert!(provider
            .fetch_oci(&missing)
            .await
            .unwrap_err()
            .to_string()
            .contains("platform not found"));
        let mut no_paths = fake.spec.clone();
        no_paths.paths.clear();
        assert!(provider.fetch_oci(&no_paths).await.is_err());
    }

    #[tokio::test]
    async fn symlink_chains_are_checked_after_all_layers() {
        for links_first in [true, false] {
            let links = layer(&[
                ("app/start", b'2', "bin/alias"),
                ("app/bin/alias", b'2', "tool"),
            ]);
            let files = layer(&[("app/bin/tool", b'0', "executable")]);
            let layers = if links_first {
                vec![links, files]
            } else {
                vec![files, links]
            };
            let fake = registry(layers, false, None).await;
            let temp = tempfile::tempdir().unwrap();
            let provider = HttpRuntimes::new(temp.path().to_path_buf());
            let root = provider.fetch_oci(&fake.spec).await.unwrap();
            assert_eq!(
                std::fs::read(root.join("app/start")).unwrap(),
                b"executable"
            );
        }
        for entries in [
            vec![("app/link", b'2', "missing")],
            vec![("app/a", b'2', "b"), ("app/b", b'2', "a")],
        ] {
            let fake = registry(vec![layer(&entries)], false, None).await;
            let temp = tempfile::tempdir().unwrap();
            assert!(HttpRuntimes::new(temp.path().to_path_buf())
                .fetch_oci(&fake.spec)
                .await
                .is_err());
            assert!(!walk_complete(temp.path()));
        }
    }

    #[test]
    fn final_symlink_validation_rejects_effective_escape() {
        let temp = tempfile::tempdir().unwrap();
        let root = temp.path().join("root");
        std::fs::create_dir(&root).unwrap();
        std::fs::write(temp.path().join("outside"), "outside").unwrap();
        std::os::unix::fs::symlink("../outside", root.join("second")).unwrap();
        std::os::unix::fs::symlink("second", root.join("first")).unwrap();
        assert!(validate_symlinks(&root)
            .unwrap_err()
            .to_string()
            .contains("symlink escape"));
        assert!(safe_link(Path::new("second"), Path::new("../outside")).is_err());
    }

    #[tokio::test]
    async fn redirects_and_config_platform_check() {
        let fake = registry(
            vec![layer(&[("app/file", b'0', "data")])],
            false,
            Some("redirect"),
        )
        .await;
        let temp = tempfile::tempdir().unwrap();
        let provider = HttpRuntimes::new(temp.path().to_path_buf());
        assert!(provider
            .fetch_oci(&fake.spec)
            .await
            .unwrap()
            .join("app/file")
            .is_file());
        let mut wrong = fake.spec.clone();
        wrong.os = "windows".into();
        assert!(provider
            .fetch_oci(&wrong)
            .await
            .unwrap_err()
            .to_string()
            .contains("config platform mismatch"));
    }

    #[test]
    fn whiteout_of_selected_ancestor_and_resource_limits() {
        let temp = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(temp.path().join("app/sub")).unwrap();
        std::fs::write(temp.path().join("app/sub/old"), "old").unwrap();
        apply_layer(
            &layer(&[(".wh.app", b'0', ""), ("app/sub/new", b'0', "new")]),
            GZIP,
            temp.path(),
            &[PathBuf::from("app/sub")],
            &mut Budget::default(),
        )
        .unwrap();
        assert!(!temp.path().join("app/sub/old").exists());
        assert!(temp.path().join("app/sub/new").exists());
        for mut budget in [
            Budget {
                entries: ENTRY_LIMIT,
                unpacked: 0,
            },
            Budget {
                entries: 0,
                unpacked: UNPACKED_LIMIT,
            },
        ] {
            assert!(apply_layer(
                &layer(&[("app/file", b'0', "x")]),
                GZIP,
                temp.path(),
                &[PathBuf::from("app")],
                &mut budget
            )
            .is_err());
        }
        assert!(reader(b"", "application/vnd.oci.image.layer.v1.tar+zstd").is_err());
    }

    #[test]
    fn auth_realm_and_spec_validation() {
        assert_eq!(
            Registry::new("docker.io", "library/node").unwrap().base,
            "https://registry-1.docker.io/v2/library/node"
        );
        assert!(token_url(
            "Basic realm=\"https://example.com\"",
            "https://ghcr.io/v2/x"
        )
        .is_err());
        assert!(token_url(
            "Bearer realm=\"http://example.com\"",
            "https://ghcr.io/v2/x"
        )
        .is_err());
        assert!(token_url("Bearer service=\"test\"", "https://ghcr.io/v2/x").is_err());
        assert!(digest_hex("sha256:../bad").is_err());
        assert!(safe_link(Path::new("app/link"), Path::new("../inside")).is_ok());
    }

    #[tokio::test]
    #[ignore = "manual public registry check; downloads the AT image"]
    async fn pull_auto_tournament() {
        let mut registry = Registry::new("ghcr.io", "auto-tournament/auto-tournament").unwrap();
        let body = registry
            .get("manifests/3.0.0-beta.251", JSON_LIMIT)
            .await
            .unwrap();
        let pin = digest(&body);
        let cache = std::env::var_os("VARDE_OCI_CHECK_CACHE")
            .map(PathBuf::from)
            .expect("set VARDE_OCI_CHECK_CACHE to a persistent cache directory");
        let root = HttpRuntimes::new(cache)
            .fetch_oci(&OciSpec {
                registry: "ghcr.io".into(),
                repository: "auto-tournament/auto-tournament".into(),
                digest: pin.clone(),
                os: "linux".into(),
                arch: "amd64".into(),
                paths: vec!["/app".into()],
            })
            .await
            .unwrap();
        fn count(path: &Path) -> usize {
            std::fs::read_dir(path)
                .unwrap()
                .map(|e| {
                    let e = e.unwrap();
                    if e.file_type().unwrap().is_dir() {
                        count(&e.path())
                    } else {
                        1
                    }
                })
                .sum()
        }
        println!(
            "digest: {pin}\nextracted file count: {}\nroot: {}",
            count(&root),
            root.display()
        );
        let output = std::process::Command::new("ls")
            .arg("-la")
            .arg(root.join("app"))
            .output()
            .unwrap();
        print!("{}", String::from_utf8_lossy(&output.stdout));
        assert!(output.status.success());
    }
}
