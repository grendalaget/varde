//! Path validation for manifest entries: relative `/`-separated paths only —
//! no `..`, absolute paths, drive letters, NUL, Windows-reserved names, or
//! trailing dot/space. Checked on snapshot create *and* restore.

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum PathError {
    #[error("empty path")]
    Empty,
    #[error("absolute path")]
    Absolute,
    #[error("drive letter")]
    DriveLetter,
    #[error("parent traversal")]
    ParentTraversal,
    #[error("NUL byte")]
    Nul,
    #[error("empty segment")]
    EmptySegment,
    #[error("trailing dot or space in segment")]
    TrailingDotOrSpace,
    #[error("windows-reserved name")]
    ReservedName,
    #[error("non-UTF8")]
    NonUtf8,
}

const RESERVED: &[&str] = &[
    "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8",
    "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
];

pub fn validate_rel_path(p: &str) -> Result<(), PathError> {
    if p.is_empty() {
        return Err(PathError::Empty);
    }
    if p.contains('\0') {
        return Err(PathError::Nul);
    }
    if p.starts_with('/') || p.starts_with('\\') || p.starts_with("//") {
        return Err(PathError::Absolute);
    }
    let bytes = p.as_bytes();
    if bytes.len() >= 2 && bytes[1] == b':' && bytes[0].is_ascii_alphabetic() {
        return Err(PathError::DriveLetter);
    }
    for seg in p.split('/') {
        if seg.is_empty() {
            return Err(PathError::EmptySegment);
        }
        if seg == ".." {
            return Err(PathError::ParentTraversal);
        }
        if seg.ends_with('.') || seg.ends_with(' ') {
            return Err(PathError::TrailingDotOrSpace);
        }
        // reserved name check applies to the base name (before first '.')
        let base = seg.split('.').next().unwrap_or(seg);
        if RESERVED.iter().any(|r| base.eq_ignore_ascii_case(r)) {
            return Err(PathError::ReservedName);
        }
    }
    Ok(())
}
