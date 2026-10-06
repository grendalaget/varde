//! What the tray and the link window show for an agent status. Pure, so it is
//! tested on every platform.

use agent_ipc::pb;
use serde::Serialize;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Icon {
    /// Linked and fine, not hosting: no stone lit.
    Idle,
    /// This PC hosts a server: the host stone lit Glød (the only orange state).
    Hosting,
    /// Not linked / offline / agent not running: yellow badge.
    Warn,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TrayView {
    pub icon: Icon,
    pub tooltip: String,
    pub headline: String,
    pub hosting: Option<String>,
    pub safe_save: Option<String>,
    pub link_label: &'static str,
    pub link_enabled: bool,
    /// Re-link (needs an administrator) rather than a first link.
    pub relink: bool,
    pub dashboard_url: Option<String>,
}

pub fn tray_view(status: Option<&pb::Status>, now_ms: i64) -> TrayView {
    let Some(s) = status else {
        return TrayView {
            icon: Icon::Warn,
            tooltip: "Varde: service isn't running".into(),
            headline: "Varde: service isn't running".into(),
            hosting: None,
            safe_save: None,
            link_label: "Link this PC…",
            link_enabled: false,
            relink: false,
            dashboard_url: None,
        };
    };
    let state = s.state();
    let hosting_now = !s.hosting.is_empty();
    let group = if s.group_name.is_empty() {
        String::new()
    } else {
        format!(" · {}", s.group_name)
    };
    let headline = match state {
        pb::State::NotLinked | pb::State::Unspecified => "Varde: Not linked".to_string(),
        pb::State::Linking => "Varde: Linking…".to_string(),
        pb::State::Connecting => format!("Varde: Connecting…{group}"),
        pb::State::Online => format!("Varde: Online{group}"),
        pb::State::Offline => "Varde: Can't reach the control plane".to_string(),
        pb::State::ShuttingDown => "Varde: Shutting down".to_string(),
    };
    let icon = match state {
        pb::State::Online if hosting_now => Icon::Hosting,
        pb::State::Online => Icon::Idle,
        // still hosting while the control plane is briefly unreachable
        pb::State::Offline | pb::State::Connecting if hosting_now => Icon::Hosting,
        _ => Icon::Warn,
    };
    let hosting = hosting_line(&s.hosting);
    let safe_save = hosting_now.then(|| {
        match s
            .hosting
            .iter()
            .map(|h| h.latest_safe_save_at_unix_ms)
            .max()
        {
            Some(t) if t > 0 => format!("Latest safe save: {}", ago(now_ms - t)),
            _ => "No safe save yet".to_string(),
        }
    });
    let linked = matches!(
        state,
        pb::State::Connecting | pb::State::Online | pb::State::Offline
    );
    let (link_label, link_enabled) = match state {
        pb::State::Linking => ("Show link code…", true),
        _ if linked && hosting_now => ("Re-link (stop hosting first)", false),
        _ if linked => ("Re-link this PC… (administrator)", true),
        pb::State::ShuttingDown => ("Link this PC…", false),
        _ => ("Link this PC…", true),
    };
    let tooltip = match &hosting {
        Some(h) => format!("{headline}\n{h}"),
        None => headline.clone(),
    };
    TrayView {
        icon,
        tooltip,
        headline,
        hosting,
        safe_save,
        link_label,
        link_enabled,
        relink: linked,
        dashboard_url: (!s.control_plane_url.is_empty()).then(|| s.control_plane_url.clone()),
    }
}

fn hosting_line(h: &[pb::Hosting]) -> Option<String> {
    match h {
        [] => None,
        [one] => {
            let verb = match one.phase.as_str() {
                "running" => "Hosting",
                "stopping" => "Stopping",
                _ => "Starting",
            };
            let name = if one.server_name.is_empty() {
                "a server"
            } else {
                &one.server_name
            };
            Some(format!("{verb} {name} on this PC"))
        }
        many => Some(format!("Hosting {} servers on this PC", many.len())),
    }
}

/// "18 s ago", "4 min ago", "2 h ago", "3 d ago".
pub fn ago(ms: i64) -> String {
    let s = (ms.max(0) / 1000) as u64;
    match s {
        0..=59 => format!("{s} s ago"),
        60..=3599 => format!("{} min ago", s / 60),
        3600..=86_399 => format!("{} h ago", s / 3600),
        _ => format!("{} d ago", s / 86_400),
    }
}

/// Status as the link window sees it.
#[derive(Debug, Clone, Default, Serialize, PartialEq)]
pub struct UiStatus {
    pub state: &'static str,
    pub control_plane_url: String,
    pub group_name: String,
    pub node_name: String,
    pub user_code: String,
    pub link_url: String,
    pub expires_at_unix_ms: i64,
    pub link_error: String,
    pub hosting: bool,
}

impl From<&pb::Status> for UiStatus {
    fn from(s: &pb::Status) -> Self {
        let link = s.link.clone().unwrap_or_default();
        UiStatus {
            state: match s.state() {
                pb::State::Unspecified | pb::State::NotLinked => "not_linked",
                pb::State::Linking => "linking",
                pb::State::Connecting => "connecting",
                pb::State::Online => "online",
                pb::State::Offline => "offline",
                pb::State::ShuttingDown => "shutting_down",
            },
            control_plane_url: s.control_plane_url.clone(),
            group_name: s.group_name.clone(),
            node_name: s.node_name.clone(),
            user_code: link.user_code,
            link_url: link.link_url,
            expires_at_unix_ms: link.expires_at_unix_ms,
            link_error: s.link_error.clone(),
            hosting: !s.hosting.is_empty(),
        }
    }
}

/// Command line: `--link [URL]` (installer: open the link window, optionally
/// starting with URL), `--relink` (elevated re-link window), `--autostart`.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Launch {
    pub open_link: bool,
    pub url: Option<String>,
    pub relink: bool,
    pub autostart: bool,
}

pub fn parse_launch(args: impl IntoIterator<Item = String>) -> Launch {
    let mut l = Launch::default();
    let mut it = args.into_iter().peekable();
    while let Some(a) = it.next() {
        match a.as_str() {
            "--link" => {
                l.open_link = true;
                if let Some(u) = it.next_if(|n| !n.starts_with("--")) {
                    l.url = Some(u);
                }
            }
            "--relink" => l.relink = true,
            "--autostart" => l.autostart = true,
            _ => {}
        }
    }
    l
}

#[cfg(test)]
mod tests {
    use super::*;

    fn status(state: pb::State) -> pb::Status {
        pb::Status {
            state: state as i32,
            control_plane_url: "https://varde.games".into(),
            group_name: "Grendalaget".into(),
            ..Default::default()
        }
    }

    fn hosting(name: &str, phase: &str, save: i64) -> pb::Hosting {
        pb::Hosting {
            server_id: format!("srv_{name}"),
            server_name: name.into(),
            phase: phase.into(),
            latest_safe_save_at_unix_ms: save,
        }
    }

    #[test]
    fn orange_only_while_hosting() {
        let now = 1_000_000;
        let online = status(pb::State::Online);
        let v = tray_view(Some(&online), now);
        assert_eq!(v.icon, Icon::Idle);
        assert_eq!(v.headline, "Varde: Online · Grendalaget");
        assert_eq!(v.hosting, None);
        assert_eq!(v.safe_save, None);
        assert!(v.relink && v.link_enabled);

        let mut h = online.clone();
        h.hosting = vec![hosting("Valheim", "running", now - 18_000)];
        let v = tray_view(Some(&h), now);
        assert_eq!(v.icon, Icon::Hosting);
        assert_eq!(v.hosting.as_deref(), Some("Hosting Valheim on this PC"));
        assert_eq!(v.safe_save.as_deref(), Some("Latest safe save: 18 s ago"));
        assert!(!v.link_enabled, "no re-link while hosting");

        for st in [
            pb::State::NotLinked,
            pb::State::Linking,
            pb::State::ShuttingDown,
        ] {
            assert_eq!(tray_view(Some(&status(st)), now).icon, Icon::Warn);
        }
        assert_eq!(
            tray_view(Some(&status(pb::State::Offline)), now).icon,
            Icon::Warn
        );
        assert_eq!(tray_view(None, now).icon, Icon::Warn);
    }

    #[test]
    fn not_linked_offers_link() {
        let v = tray_view(Some(&status(pb::State::NotLinked)), 0);
        assert_eq!(v.headline, "Varde: Not linked");
        assert_eq!(v.link_label, "Link this PC…");
        assert!(v.link_enabled && !v.relink);
        let v = tray_view(None, 0);
        assert!(!v.link_enabled);
        assert_eq!(v.dashboard_url, None);
    }

    #[test]
    fn hosting_lines() {
        assert_eq!(
            hosting_line(&[hosting("Mc", "starting", 0)]).as_deref(),
            Some("Starting Mc on this PC")
        );
        assert_eq!(
            hosting_line(&[hosting("a", "running", 0), hosting("b", "running", 0)]).as_deref(),
            Some("Hosting 2 servers on this PC")
        );
        let mut s = status(pb::State::Online);
        s.hosting = vec![hosting("Mc", "running", 0)];
        assert_eq!(
            tray_view(Some(&s), 5).safe_save.as_deref(),
            Some("No safe save yet")
        );
    }

    #[test]
    fn ago_units() {
        assert_eq!(ago(-5), "0 s ago");
        assert_eq!(ago(59_999), "59 s ago");
        assert_eq!(ago(240_000), "4 min ago");
        assert_eq!(ago(7_200_000), "2 h ago");
        assert_eq!(ago(3 * 86_400_000), "3 d ago");
    }

    #[test]
    fn launch_args() {
        let p = |a: &[&str]| parse_launch(a.iter().map(|s| s.to_string()));
        assert_eq!(p(&[]), Launch::default());
        let l = p(&["--link", "https://cp.example"]);
        assert!(l.open_link && l.url.as_deref() == Some("https://cp.example"));
        let l = p(&["--link", "--autostart"]);
        assert!(l.open_link && l.url.is_none() && l.autostart);
        assert!(p(&["--relink"]).relink);
    }

    #[test]
    fn ui_status_maps_link() {
        let mut s = status(pb::State::Linking);
        s.link = Some(pb::Link {
            user_code: "WXYZ-1234".into(),
            link_url: "https://varde.games/link?code=WXYZ-1234".into(),
            expires_at_unix_ms: 42,
        });
        let u = UiStatus::from(&s);
        assert_eq!(u.state, "linking");
        assert_eq!(u.user_code, "WXYZ-1234");
        assert_eq!(u.expires_at_unix_ms, 42);
    }
}
