//! Include patterns as produced by game drivers (`persistent_paths()`):
//! `/`-separated relative paths with `*` / `?` glob support. A trailing `/`
//! makes it a directory pattern matching the dir and everything under it
//! (e.g. `world*/`, `saves/worlds_local/`, `server.properties`, `saves/*.txt`).
//! A leading `!` makes it an exclude pattern: files it matches are left out
//! even when another pattern selects them (e.g. `!world*/session.lock`).

#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord)]
pub struct PathPattern(pub String);

impl PathPattern {
    pub fn new(s: impl Into<String>) -> PathPattern {
        PathPattern(s.into())
    }

    pub fn is_dir_pattern(&self) -> bool {
        self.0.ends_with('/')
    }

    pub fn is_exclude(&self) -> bool {
        self.0.starts_with('!')
    }

    /// Is `rel` left out by this exclude pattern? Always false for includes.
    pub fn excludes(&self, rel: &str) -> bool {
        match self.0.strip_prefix('!') {
            Some(p) => PathPattern::new(p).matches(rel, true),
            None => false,
        }
    }

    /// Does this pattern select `rel`? `is_file` distinguishes files from
    /// directories for exact dir-pattern matches. Exclude patterns select nothing.
    pub fn matches(&self, rel: &str, is_file: bool) -> bool {
        if self.is_exclude() {
            return false;
        }
        if self.is_dir_pattern() {
            let dir = self.0.trim_end_matches('/');
            return path_under_pattern(rel, dir);
        }
        let _ = is_file;
        glob_match(&self.0, rel)
    }

    /// Matches the directory itself (for manifest dir entries / removals).
    pub fn matches_dir(&self, rel: &str) -> bool {
        if self.is_exclude() {
            return false;
        }
        if self.is_dir_pattern() {
            let dir = self.0.trim_end_matches('/');
            return path_under_pattern(rel, dir);
        }
        glob_match(&self.0, rel)
    }
}

/// `rel` is inside a directory that matches the (possibly globbed) dir
/// pattern `dir_pat`, or equals a dir matching it.
fn path_under_pattern(rel: &str, dir_pat: &str) -> bool {
    let pat_segs: Vec<&str> = dir_pat.split('/').collect();
    let rel_segs: Vec<&str> = rel.split('/').collect();
    if rel_segs.len() < pat_segs.len() {
        return false;
    }
    pat_segs
        .iter()
        .zip(rel_segs.iter())
        .all(|(p, s)| glob_match(p, s))
}

/// Glob match over the whole string: `*` = any run (incl. `/`? no — `*` does
/// not cross `/`), `?` = one char.
fn glob_match(pat: &str, s: &str) -> bool {
    glob_segs(
        &pat.split('/').collect::<Vec<_>>(),
        &s.split('/').collect::<Vec<_>>(),
    )
}

fn glob_segs(pat: &[&str], s: &[&str]) -> bool {
    if pat.is_empty() {
        return s.is_empty();
    }
    if s.is_empty() {
        return false;
    }
    if glob_seg(pat[0], s[0]) {
        return glob_segs(&pat[1..], &s[1..]);
    }
    false
}

/// Match one path segment: `*` and `?` wildcards (not crossing `/` since we
/// already split on it).
fn glob_seg(pat: &str, s: &str) -> bool {
    let p: Vec<char> = pat.chars().collect();
    let t: Vec<char> = s.chars().collect();
    // classic two-pointer glob with backtracking
    let (mut i, mut j) = (0usize, 0usize);
    let (mut star, mut mark) = (usize::MAX, 0usize);
    while j < t.len() {
        if i < p.len() && (p[i] == '?' || p[i] == t[j]) {
            i += 1;
            j += 1;
        } else if i < p.len() && p[i] == '*' {
            star = i;
            mark = j;
            i += 1;
        } else if star != usize::MAX {
            i = star + 1;
            mark += 1;
            j = mark;
        } else {
            return false;
        }
    }
    while i < p.len() && p[i] == '*' {
        i += 1;
    }
    i == p.len()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn patterns() {
        let p = PathPattern::new("world*/");
        assert!(p.matches("world/level.dat", true));
        assert!(p.matches("world_nether/x", true));
        assert!(!p.matches("other/x", true));
        assert!(p.matches_dir("world"));
        let f = PathPattern::new("server.properties");
        assert!(f.matches("server.properties", true));
        assert!(!f.matches("sub/server.properties", true));
        let g = PathPattern::new("saves/*.txt");
        assert!(g.matches("saves/a.txt", true));
        assert!(!g.matches("saves/deep/a.txt", true));
    }

    #[test]
    fn exclude_patterns() {
        let x = PathPattern::new("!world*/session.lock");
        assert!(x.is_exclude());
        assert!(x.excludes("world/session.lock"));
        assert!(x.excludes("world_nether/session.lock"));
        assert!(!x.excludes("world/level.dat"));
        assert!(!x.matches("world/session.lock", true));
        assert!(!x.matches_dir("world"));
        assert!(!PathPattern::new("world*/").excludes("world/session.lock"));
    }
}
