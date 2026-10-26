//! Rejoin prompt: when the host of a server this PC was playing on goes
//! away, follow the failover (host lost → moving → ready) so the tray can
//! tell the player to wait, then to rejoin at the same address.
//!
//! Planned warm moves are skipped: Minecraft `/transfer`s players itself.

use std::collections::{HashMap, HashSet};

use agent_ipc::pb;

/// A connection that ended this long ago still counts as "was playing".
const PLAYED_WITHIN_MS: i64 = 3 * 60_000;
/// No host at all (lease expired, not yet re-placed) for this long: the
/// server isn't coming back on its own.
const NO_HOST_GRACE_MS: i64 = 60_000;
/// Host lost / moving for longer than this: give up the prompt.
const STUCK_MS: i64 = 15 * 60_000;
/// Ready prompt stays this long unless the game reconnects.
const READY_SHOWN_MS: i64 = 10 * 60_000;

/// One directive route, joined with what the mesh sees locally.
#[derive(Debug, Clone, Default)]
pub struct Route {
    pub server_id: String,
    pub server_name: String,
    pub address: String,
    pub host: String,
    pub host_name: String,
    pub host_running: bool,
    /// Warm-migration source still draining (players being transferred).
    pub drain_host: String,
    /// Local clock; 0 = unknown.
    pub restore_save_at_ms: i64,
    /// Local game sessions through this route, by the host each was opened to.
    pub sessions: HashMap<String, u32>,
}

#[derive(Debug, Default, Clone)]
pub struct Tracker {
    servers: HashMap<String, Track>,
}

#[derive(Debug, Default, Clone)]
struct Track {
    played_host: String,
    played_host_name: String,
    /// Last time a session to played_host was seen; None = never/forgotten.
    played_at: Option<i64>,
    incident: Option<Incident>,
}

#[derive(Debug, Clone)]
struct Incident {
    from: String,
    from_name: String,
    phase: Phase,
    since: i64,
    started: i64,
    host_name: String,
    save_at: i64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Phase {
    HostLost,
    Moving,
    Ready,
}

impl Phase {
    fn as_str(self) -> &'static str {
        match self {
            Phase::HostLost => "host_lost",
            Phase::Moving => "moving",
            Phase::Ready => "ready",
        }
    }
}

impl Tracker {
    /// Advance every server with the latest directives; returns the prompts
    /// to show. `reachable`: peers the mesh currently has a path to.
    pub fn update(
        &mut self,
        now: i64,
        self_node: &str,
        routes: &[Route],
        reachable: &HashSet<String>,
    ) -> Vec<pb::Rejoin> {
        self.servers
            .retain(|id, _| routes.iter().any(|r| &r.server_id == id));
        let mut out = vec![];
        for r in routes {
            let t = self.servers.entry(r.server_id.clone()).or_default();
            t.step(now, self_node, r, reachable);
            if let Some(i) = &t.incident {
                out.push(pb::Rejoin {
                    server_id: r.server_id.clone(),
                    server_name: r.server_name.clone(),
                    phase: i.phase.as_str().into(),
                    host_name: i.host_name.clone(),
                    previous_host_name: i.from_name.clone(),
                    address: r.address.clone(),
                    save_at_unix_ms: i.save_at,
                    since_unix_ms: i.since,
                });
            }
        }
        out.sort_by(|a, b| a.server_name.cmp(&b.server_name));
        out
    }
}

impl Track {
    fn step(&mut self, now: i64, self_node: &str, r: &Route, reachable: &HashSet<String>) {
        let lost = |h: &str| !h.is_empty() && h != self_node && !reachable.contains(h);
        let on = |h: &str| r.sessions.get(h).copied().unwrap_or(0);
        // Running and reachable from here: the player can actually rejoin.
        let ready = r.host_running && !lost(&r.host);

        // Planned warm move off the host we played on: the game is
        // transferred, there's nothing to rejoin.
        if !r.drain_host.is_empty() && r.drain_host == self.played_host {
            self.incident = None;
            self.played_host = r.host.clone();
            self.played_host_name = r.host_name.clone();
            return;
        }

        if let Some(mut i) = self.incident.take() {
            let keep = if r.host.is_empty() {
                now - i.since < NO_HOST_GRACE_MS
            } else if r.host == i.from {
                // Same host again: recovered in place (or still lost).
                if lost(&r.host) {
                    now - i.started < STUCK_MS
                } else {
                    false
                }
            } else {
                let phase = if ready { Phase::Ready } else { Phase::Moving };
                if phase != i.phase {
                    i.phase = phase;
                    i.since = now;
                }
                i.host_name = r.host_name.clone();
                if r.restore_save_at_ms > 0 {
                    i.save_at = r.restore_save_at_ms;
                }
                match phase {
                    // Rejoined: a session to the new host.
                    Phase::Ready => on(&r.host) == 0 && now - i.since < READY_SHOWN_MS,
                    _ => now - i.started < STUCK_MS,
                }
            };
            if keep {
                self.incident = Some(i);
            } else {
                self.played_host = r.host.clone();
                self.played_host_name = r.host_name.clone();
                self.played_at = (on(&r.host) > 0).then_some(now);
            }
            return;
        }

        let recent = self
            .played_at
            .is_some_and(|at| now - at <= PLAYED_WITHIN_MS);
        if r.host.is_empty() {
            // No host: the one we played on died before a replacement was
            // placed, or the server was stopped on purpose (its host still
            // answers) — no prompt for that.
            if recent && lost(&self.played_host) {
                self.incident = Some(Incident {
                    from: self.played_host.clone(),
                    from_name: self.played_host_name.clone(),
                    phase: Phase::HostLost,
                    since: now,
                    started: now,
                    host_name: String::new(),
                    save_at: 0,
                });
            }
            self.played_at = None;
            return;
        }
        if on(&self.played_host) > 0 || on(&r.host) > 0 {
            if self.played_host.is_empty() || on(&r.host) > 0 {
                self.played_host = r.host.clone();
                self.played_host_name = r.host_name.clone();
            }
            self.played_at = Some(now);
        }
        if self.played_at.is_none_or(|at| now - at > PLAYED_WITHIN_MS) {
            return;
        }
        let phase = if r.host != self.played_host {
            if ready {
                Phase::Ready
            } else {
                Phase::Moving
            }
        } else if lost(&r.host) {
            Phase::HostLost
        } else {
            return;
        };
        self.incident = Some(Incident {
            from: self.played_host.clone(),
            from_name: self.played_host_name.clone(),
            phase,
            since: now,
            started: now,
            host_name: if phase == Phase::HostLost {
                String::new()
            } else {
                r.host_name.clone()
            },
            save_at: r.restore_save_at_ms,
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn route(host: &str, running: bool) -> Route {
        Route {
            server_id: "s1".into(),
            server_name: "Survival".into(),
            address: "127.77.12.4:25565".into(),
            host: host.into(),
            host_name: format!("{host}-PC"),
            host_running: running,
            ..Default::default()
        }
    }

    fn playing(mut r: Route, host: &str) -> Route {
        r.sessions.insert(host.into(), 1);
        r
    }

    fn peers(ids: &[&str]) -> HashSet<String> {
        ids.iter().map(|s| s.to_string()).collect()
    }

    fn phases(v: &[pb::Rejoin]) -> Vec<&str> {
        v.iter().map(|r| r.phase.as_str()).collect()
    }

    #[test]
    fn crash_goes_lost_moving_ready_then_clears_on_rejoin() {
        let mut t = Tracker::default();
        let all = peers(&["ola", "kari"]);
        let kari_only = peers(&["kari"]);
        assert!(t
            .update(0, "me", &[playing(route("ola", true), "ola")], &all)
            .is_empty());
        // host dies; the stale connection is still open
        let v = t.update(
            1_000,
            "me",
            &[playing(route("ola", true), "ola")],
            &kari_only,
        );
        assert_eq!(phases(&v), ["host_lost"]);
        assert_eq!(v[0].previous_host_name, "ola-PC");
        let mut moving = route("kari", false);
        moving.restore_save_at_ms = 900;
        let v = t.update(9_000, "me", &[moving], &kari_only);
        assert_eq!(phases(&v), ["moving"]);
        assert_eq!(v[0].host_name, "kari-PC");
        assert_eq!(v[0].save_at_unix_ms, 900);
        let v = t.update(15_000, "me", &[route("kari", true)], &kari_only);
        assert_eq!(phases(&v), ["ready"]);
        assert_eq!(v[0].address, "127.77.12.4:25565");
        assert_eq!(v[0].since_unix_ms, 15_000);
        // a stale session to the dead host isn't a rejoin
        let v = t.update(
            20_000,
            "me",
            &[playing(route("kari", true), "ola")],
            &kari_only,
        );
        assert_eq!(phases(&v), ["ready"]);
        let v = t.update(
            60_000,
            "me",
            &[playing(route("kari", true), "kari")],
            &kari_only,
        );
        assert!(v.is_empty());
        // and it doesn't re-trigger afterwards
        assert!(t
            .update(61_000, "me", &[route("kari", true)], &kari_only)
            .is_empty());
    }

    #[test]
    fn nobody_playing_sees_nothing() {
        let mut t = Tracker::default();
        assert!(t
            .update(0, "me", &[route("ola", true)], &peers(&["ola"]))
            .is_empty());
        assert!(t
            .update(1_000, "me", &[route("ola", true)], &peers(&[]))
            .is_empty());
        assert!(t
            .update(9_000, "me", &[route("kari", true)], &peers(&["kari"]))
            .is_empty());
    }

    #[test]
    fn played_recently_still_counts_but_not_long_ago() {
        let mut t = Tracker::default();
        t.update(
            0,
            "me",
            &[playing(route("ola", true), "ola")],
            &peers(&["ola"]),
        );
        let v = t.update(60_000, "me", &[route("kari", true)], &peers(&["kari"]));
        assert_eq!(phases(&v), ["ready"]);

        let mut t = Tracker::default();
        t.update(
            0,
            "me",
            &[playing(route("ola", true), "ola")],
            &peers(&["ola"]),
        );
        let v = t.update(
            PLAYED_WITHIN_MS + 1,
            "me",
            &[route("kari", true)],
            &peers(&[]),
        );
        assert!(v.is_empty());
    }

    #[test]
    fn warm_move_is_skipped() {
        let mut t = Tracker::default();
        let all = peers(&["ola", "kari"]);
        t.update(0, "me", &[playing(route("ola", true), "ola")], &all);
        let mut flip = playing(route("kari", true), "ola");
        flip.drain_host = "ola".into();
        assert!(t.update(1_000, "me", &[flip], &all).is_empty());
        assert!(t
            .update(30_000, "me", &[route("kari", true)], &peers(&["kari"]))
            .is_empty());
    }

    #[test]
    fn host_blip_recovers_in_place() {
        let mut t = Tracker::default();
        t.update(
            0,
            "me",
            &[playing(route("ola", true), "ola")],
            &peers(&["ola"]),
        );
        let v = t.update(1_000, "me", &[route("ola", true)], &peers(&[]));
        assert_eq!(phases(&v), ["host_lost"]);
        assert!(t
            .update(3_000, "me", &[route("ola", true)], &peers(&["ola"]))
            .is_empty());
    }

    #[test]
    fn ready_prompt_times_out_and_stopped_server_clears() {
        let mut t = Tracker::default();
        t.update(
            0,
            "me",
            &[playing(route("ola", true), "ola")],
            &peers(&["ola"]),
        );
        assert_eq!(
            phases(&t.update(1_000, "me", &[route("kari", true)], &peers(&["kari"]))),
            ["ready"]
        );
        assert!(t
            .update(
                1_000 + READY_SHOWN_MS,
                "me",
                &[route("kari", true)],
                &peers(&["kari"])
            )
            .is_empty());

        let mut t = Tracker::default();
        t.update(
            0,
            "me",
            &[playing(route("ola", true), "ola")],
            &peers(&["ola"]),
        );
        assert_eq!(
            phases(&t.update(1_000, "me", &[route("ola", true)], &peers(&[]))),
            ["host_lost"]
        );
        assert_eq!(
            phases(&t.update(2_000, "me", &[route("", false)], &peers(&[]))),
            ["host_lost"]
        );
        assert!(t
            .update(
                2_000 + NO_HOST_GRACE_MS,
                "me",
                &[route("", false)],
                &peers(&[])
            )
            .is_empty());
    }

    #[test]
    fn host_dies_before_a_replacement_is_placed() {
        let mut t = Tracker::default();
        t.update(
            0,
            "me",
            &[playing(route("ola", true), "ola")],
            &peers(&["ola"]),
        );
        // lease expired, route left hostless; ola is gone
        let v = t.update(1_000, "me", &[route("", false)], &peers(&["kari"]));
        assert_eq!(phases(&v), ["host_lost"]);
        let v = t.update(9_000, "me", &[route("kari", true)], &peers(&["kari"]));
        assert_eq!(phases(&v), ["ready"]);

        // stopped on purpose: the host still answers, no prompt
        let mut t = Tracker::default();
        t.update(
            0,
            "me",
            &[playing(route("ola", true), "ola")],
            &peers(&["ola"]),
        );
        assert!(t
            .update(1_000, "me", &[route("", false)], &peers(&["ola"]))
            .is_empty());
        assert!(t
            .update(
                9_000,
                "me",
                &[route("kari", true)],
                &peers(&["ola", "kari"])
            )
            .is_empty());
    }

    #[test]
    fn not_ready_until_the_new_host_is_reachable() {
        let mut t = Tracker::default();
        t.update(
            0,
            "me",
            &[playing(route("ola", true), "ola")],
            &peers(&["ola"]),
        );
        let v = t.update(9_000, "me", &[route("kari", true)], &peers(&[]));
        assert_eq!(phases(&v), ["moving"]);
        let v = t.update(10_000, "me", &[route("kari", true)], &peers(&["kari"]));
        assert_eq!(phases(&v), ["ready"]);
    }
}
