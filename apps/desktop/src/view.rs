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
    /// The agent service answered: it can be stopped, not started.
    pub service_running: bool,
    /// "Update to x.y.z" when the agent's checker found a newer release.
    pub update_label: Option<String>,
    /// User pref (agent-side): upload captured crash reports to this
    /// machine's control plane.
    pub crash_reporting: bool,
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
            service_running: false,
            update_label: None,
            crash_reporting: true,
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
    let safe_save = save_line(&s.hosting, now_ms);
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
        service_running: true,
        crash_reporting: s.crash_reporting,
        update_label: s
            .update
            .as_ref()
            .filter(|u| !u.available_version.is_empty())
            .map(|u| format!("Update to {}", u.available_version)),
    }
}

/// "Latest safe save: 3 min ago on nas, gaming-pc (offline)" — the newest
/// committed save and the machines holding it, or None when not hosting.
pub fn save_line(hosting: &[pb::Hosting], now_ms: i64) -> Option<String> {
    if hosting.is_empty() {
        return None;
    }
    let latest = |f: fn(&pb::Hosting) -> i64| hosting.iter().map(f).max().unwrap_or(0);
    let safe = latest(|h| h.latest_safe_save_at_unix_ms);
    let any = latest(|h| h.latest_save_at_unix_ms);
    // machines holding the latest safe save, per the control plane
    let mut holders: Vec<(String, bool)> = hosting
        .iter()
        .flat_map(|h| {
            h.latest_safe_save_on
                .iter()
                .map(|x| (x.node_name.clone(), x.online))
        })
        .collect();
    holders.sort();
    holders.dedup();
    let on = if holders.is_empty() {
        String::new()
    } else {
        let names = holders
            .iter()
            .map(|(n, online)| {
                if *online {
                    n.clone()
                } else {
                    format!("{n} (offline)")
                }
            })
            .collect::<Vec<_>>()
            .join(", ");
        format!(" on {names}")
    };
    Some(if any > safe {
        format!("Latest save: {} (not on enough PCs yet)", ago(now_ms - any))
    } else if safe > 0 {
        format!("Latest safe save: {}{on}", ago(now_ms - safe))
    } else {
        "No safe save yet".to_string()
    })
}

pub(crate) fn hosting_line(h: &[pb::Hosting]) -> Option<String> {
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
    pub link_error_detail: String,
    pub hosting: bool,
    /// The pending code is an administrator's re-link.
    pub relink: bool,
    /// Where the newest save lives, while hosting (same line as the tray).
    pub save_line: Option<String>,
}

impl UiStatus {
    pub fn from_status(s: &pb::Status, now_ms: i64) -> Self {
        let mut u = Self::from(s);
        u.save_line = save_line(&s.hosting, now_ms);
        u
    }
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
            link_error_detail: s.link_error_detail.clone(),
            hosting: !s.hosting.is_empty(),
            relink: link.relink,
            save_line: None,
        }
    }
}

/// Command line: `--link` (installer: open the link window and start linking
/// with the address the service reports), `--relink` (elevated re-link
/// window), `--autostart`, `--service-start` / `--service-stop` (elevated
/// headless service control; start also restarts a running-but-dead one).
/// A `varde://` URL arrives from the protocol handler when the browser
/// returns a federated sign-in token.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Launch {
    pub open_link: bool,
    pub relink: bool,
    pub autostart: bool,
    pub service_start: bool,
    pub service_stop: bool,
    /// varde:// deep link (federated sign-in callback).
    pub deep_link: Option<String>,
}

pub fn parse_launch(args: impl IntoIterator<Item = String>) -> Launch {
    let mut l = Launch::default();
    let mut it = args.into_iter().peekable();
    while let Some(a) = it.next() {
        match a.as_str() {
            "--link" => {
                l.open_link = true;
                // older installers passed the address; the service has it now
                let _ = it.next_if(|n| !n.starts_with("--"));
            }
            "--relink" => l.relink = true,
            "--autostart" => l.autostart = true,
            "--service-start" => l.service_start = true,
            "--service-stop" => l.service_stop = true,
            _ if a.starts_with("varde://") => l.deep_link = Some(a),
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
            latest_save_at_unix_ms: 0,
            latest_safe_save_on: vec![],
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
    fn unsafe_save_until_it_has_enough_copies() {
        let now = 1_000_000;
        let mut s = status(pb::State::Online);
        let mut h = hosting("Mc", "running", 0);
        h.latest_save_at_unix_ms = now - 10_000;
        s.hosting = vec![h.clone()];
        assert_eq!(
            tray_view(Some(&s), now).safe_save.as_deref(),
            Some("Latest save: 10 s ago (not on enough PCs yet)")
        );
        h.latest_safe_save_at_unix_ms = now - 10_000;
        s.hosting = vec![h];
        assert_eq!(
            tray_view(Some(&s), now).safe_save.as_deref(),
            Some("Latest safe save: 10 s ago")
        );
    }

    #[test]
    fn safe_save_lists_holders_with_offline_marked() {
        let now = 1_000_000;
        let mut s = status(pb::State::Online);
        let mut h = hosting("Mc", "running", now - 10_000);
        h.latest_safe_save_on = vec![
            pb::SaveHolder {
                node_name: "nas".into(),
                anchor: true,
                online: true,
            },
            pb::SaveHolder {
                node_name: "gaming-pc".into(),
                anchor: false,
                online: false,
            },
        ];
        s.hosting = vec![h];
        assert_eq!(
            tray_view(Some(&s), now).safe_save.as_deref(),
            Some("Latest safe save: 10 s ago on gaming-pc (offline), nas")
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
        assert!(l.open_link && !l.autostart);
        let l = p(&["--link", "--autostart"]);
        assert!(l.open_link && l.autostart);
        assert!(p(&["--relink"]).relink);
        assert!(p(&["--service-start"]).service_start);
        assert!(p(&["--service-stop"]).service_stop);
        let l = p(&["varde://auth/callback?token=t.jwt"]);
        assert_eq!(
            l.deep_link.as_deref(),
            Some("varde://auth/callback?token=t.jwt")
        );
    }

    #[test]
    fn ui_status_maps_link() {
        let mut s = status(pb::State::Linking);
        s.link = Some(pb::Link {
            user_code: "WXYZ-1234".into(),
            link_url: "https://varde.games/link?code=WXYZ-1234".into(),
            expires_at_unix_ms: 42,
            relink: true,
        });
        let u = UiStatus::from(&s);
        assert_eq!(u.state, "linking");
        assert_eq!(u.user_code, "WXYZ-1234");
        assert_eq!(u.expires_at_unix_ms, 42);
        assert!(u.relink);
    }
}
