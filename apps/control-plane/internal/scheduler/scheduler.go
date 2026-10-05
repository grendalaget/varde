// Package scheduler is a pure, deterministic placement function. Same input
// always produces the same output; ties break on node_id. Reasons are stored
// in placement_json and shown to users as "Why".
package scheduler

import (
	"fmt"
	"sort"
)

// ServerView is what Place needs to know about the server.
type ServerView struct {
	ID              string
	GameID          string
	PreferredNodeID string
}

// GameView describes the catalog entry for the server's game.
type GameView struct {
	ID          string
	MinMemoryMB int64
	OS          []string
	Arch        []string
	Runtimes    []string
}

// NodeView is what Place needs to know about a candidate node.
type NodeView struct {
	ID                string
	Online            bool
	Draining          bool // agent is draining executions on shutdown
	AdminState        string
	HostingEnabled    bool
	Anchor            bool
	Priority          int64 // 0–100
	OS                string
	Arch              string
	Drivers           []string
	Runtimes          []string
	CachedDeployments []string
	MemoryAvailableMB int64
	MaxMemoryMB       *int64
	DiskFreeBytes     int64
	DiskTotalBytes    int64 // 0 = unknown (skip the <10% penalty)
	OnBattery         bool
	UserActive        bool
	UptimeS           int64
	DirectPeerPaths   int // peers with a direct path
	OnlinePeers       int // total online peers in group
	HasRestoreLocal   bool
	NeededDeployment  string // deployment_id that must be runnable
}

// SnapView supplies restore/deploy sizes for disk checks.
type SnapView struct {
	RestoreBytes   int64
	DeploymentSize int64
}

// Decision is the placement result.
type Decision struct {
	NodeID  string
	Score   float64
	Reasons []string
}

// Error is why placement failed; Reasons are user-visible.
type Error struct {
	Reasons []string
}

func (e *Error) Error() string { return fmt.Sprintf("unschedulable: %v", e.Reasons) }

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Place picks the node for a server. recovery=false means normal start.
// Deterministic: candidates sorted by node_id, equal scores keep node_id
// order, so ties always resolve to the lexicographically smaller node_id.
func Place(srv ServerView, game GameView, nodes []NodeView, snap SnapView) (*Decision, error) {
	sorted := append([]NodeView(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	var reasons []string
	var best *Decision

	for _, n := range sorted {
		if !n.Online {
			reasons = append(reasons, n.ID+": offline")
			continue
		}
		if n.Draining {
			reasons = append(reasons, n.ID+": shutting down")
			continue
		}
		if n.AdminState != "active" {
			reasons = append(reasons, n.ID+": admin_state="+n.AdminState)
			continue
		}
		if !n.HostingEnabled {
			reasons = append(reasons, n.ID+": hosting disabled")
			continue
		}
		if !contains(n.Drivers, game.ID) {
			reasons = append(reasons, n.ID+": missing driver "+game.ID)
			continue
		}
		if len(game.OS) > 0 && !contains(game.OS, n.OS) {
			reasons = append(reasons, n.ID+": unsupported os "+n.OS)
			continue
		}
		if len(game.Arch) > 0 && !contains(game.Arch, n.Arch) {
			reasons = append(reasons, n.ID+": unsupported arch "+n.Arch)
			continue
		}
		if n.MemoryAvailableMB < game.MinMemoryMB {
			reasons = append(reasons, n.ID+": insufficient memory")
			continue
		}
		if n.MaxMemoryMB != nil && game.MinMemoryMB > *n.MaxMemoryMB {
			reasons = append(reasons, n.ID+": over max_memory_mb")
			continue
		}
		need := snap.RestoreBytes*2 + snap.DeploymentSize
		if need > 0 && n.DiskFreeBytes < need {
			reasons = append(reasons, n.ID+": insufficient disk")
			continue
		}
		// Runtime: any game runtime must be present.
		if len(game.Runtimes) > 0 {
			ok := false
			for _, rt := range game.Runtimes {
				if contains(n.Runtimes, rt) {
					ok = true
					break
				}
			}
			if !ok {
				reasons = append(reasons, n.ID+": missing runtime")
				continue
			}
		}

		var score float64
		var rsn []string
		add := func(w float64, why string) { score += w; rsn = append(rsn, why) }

		if n.HasRestoreLocal {
			add(100, "restore snapshot already local")
		}
		if n.NeededDeployment != "" && contains(n.CachedDeployments, n.NeededDeployment) {
			add(30, "deployment cached")
		}
		if n.Anchor {
			add(80, "anchor (always-on)")
		}
		if n.ID == srv.PreferredNodeID {
			add(60, "preferred node")
		}
		if n.Priority > 0 {
			add(float64(n.Priority)*0.5, fmt.Sprintf("priority %d", n.Priority))
		}
		if n.UptimeS >= 24*3600 {
			add(20, "uptime ≥ 24h")
		}
		if n.OnlinePeers > 0 && n.DirectPeerPaths*2 >= n.OnlinePeers {
			add(20, "direct paths to most peers")
		}
		if n.UserActive {
			add(-40, "user active (gaming)")
		}
		if n.DiskTotalBytes > 0 && n.DiskFreeBytes*10 < n.DiskTotalBytes {
			add(-50, "disk free < 10%")
		}
		if n.OnBattery {
			add(-500, "on battery")
		}

		if best == nil || score > best.Score {
			best = &Decision{NodeID: n.ID, Score: score, Reasons: rsn}
		}
	}

	if best == nil {
		if len(reasons) == 0 {
			reasons = []string{"no nodes in group"}
		}
		return nil, &Error{Reasons: reasons}
	}
	return best, nil
}

// --- Replication target selection (deterministic) ---
//
// All online anchors first (up to replicationFactor-1), then other online
// nodes by score: already has parent snapshot +50 · hosting enabled +20 ·
// free disk headroom · node_id tie-break. The producing node is excluded.

type ReplCandidate struct {
	NodeID         string
	Online         bool
	Anchor         bool
	HostingEnabled bool
	HasParent      bool
	DiskFreeBytes  int64
}

// SelectReplicationTargets returns up to replicationFactor-1 target node ids
// (the producer's local copy counts as one), anchors first.
func SelectReplicationTargets(producerID string, replicationFactor int, candidates []ReplCandidate) []string {
	sorted := append([]ReplCandidate(nil), candidates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].NodeID < sorted[j].NodeID })

	remaining := replicationFactor - 1
	var out []string

	// pass 1: online anchors
	for _, n := range sorted {
		if remaining <= 0 {
			break
		}
		if n.NodeID == producerID || !n.Online || !n.Anchor {
			continue
		}
		out = append(out, n.NodeID)
		remaining--
	}

	// pass 2: others by score
	type scored struct {
		id    string
		score int64
	}
	var rest []scored
	for _, n := range sorted {
		if n.NodeID == producerID || !n.Online || n.Anchor {
			continue
		}
		var sc int64
		if n.HasParent {
			sc += 50
		}
		if n.HostingEnabled {
			sc += 20
		}
		sc += n.DiskFreeBytes >> 30 // free GiB headroom, bounded vs. the fixed weights
		rest = append(rest, scored{n.NodeID, sc})
	}
	// deterministic: score desc, node_id asc
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].score != rest[j].score {
			return rest[i].score > rest[j].score
		}
		return rest[i].id < rest[j].id
	})
	for _, r := range rest {
		if remaining <= 0 {
			break
		}
		out = append(out, r.id)
		remaining--
	}
	return out
}
