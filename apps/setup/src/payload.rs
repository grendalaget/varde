// The embedded payload: build.rs zips apps/setup/payload/ into OUT_DIR; at
// install time we stream it out to $INSTDIR. The lite build (uninstall.exe)
// is compiled without the `payload` feature and carries an empty zip.
#[cfg(feature = "payload")]
use anyhow::Context;
use anyhow::{bail, Result};
#[cfg(feature = "payload")]
use std::io::Read;
use std::path::Path;

#[cfg(feature = "payload")]
const PAYLOAD_ZIP: &[u8] = include_bytes!(concat!(env!("OUT_DIR"), "/payload.zip"));

#[cfg(feature = "payload")]
pub fn zip_bytes() -> &'static [u8] {
    PAYLOAD_ZIP
}

pub fn extract_to(dir: &Path) -> Result<u64> {
    #[cfg(not(feature = "payload"))]
    {
        let _ = dir;
        bail!("this build carries no payload")
    }
    #[cfg(feature = "payload")]
    {
        std::fs::create_dir_all(dir)?;
        let cur = std::io::Cursor::new(PAYLOAD_ZIP);
        let mut z = zip::ZipArchive::new(cur).context("read embedded payload")?;
        let mut total = 0u64;
        for i in 0..z.len() {
            let mut f = z.by_index(i)?;
            let name = f.name().to_string();
            // flat archive — refuse traversal defensively
            if name.contains("..") || name.contains('/') || name.contains('\\') {
                bail!("unsafe payload entry {name}");
            }
            let dest = dir.join(&name);
            let mut out = std::fs::File::create(&dest)
                .with_context(|| format!("write {}", dest.display()))?;
            let mut buf = Vec::new();
            f.read_to_end(&mut buf)?;
            std::io::Write::write_all(&mut out, &buf)?;
            total += f.size();
        }
        if total == 0 {
            bail!("empty payload");
        }
        Ok(total)
    }
}
