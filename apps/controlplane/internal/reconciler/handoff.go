package reconciler

import (
	"context"
	"encoding/json"

	"github.com/grendalaget/varde/go/ids"

	"github.com/grendalaget/varde/apps/controlplane/internal/catalog"
	"github.com/grendalaget/varde/apps/controlplane/internal/scheduler"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

// SupportsHandoff reports whether the game's driver can hand connected
// clients off to a replacement server (Minecraft `/transfer`, Java 1.20.5+).
// Such servers take the warm, start-first move; others keep the serial move.
func SupportsHandoff(gameID string) bool {
	return gameID == "minecraft"
}

// StartWarmMigration opens a warm move of srv's serving execution to
// targetNodeID: the source keeps serving under freeze while it takes a live
// 'migration' snapshot, the target pre-stages from it, then routes flip and
// the source hands clients off.
func (r *Reconciler) StartWarmMigration(ctx context.Context, srv *store.Server, active *store.Execution, targetNodeID string) (*store.Migration, error) {
	s := r.Store
	now := s.NowMs()
	m := &store.Migration{
		ID: ids.Must("mig_"), ServerID: srv.ID, FromExecutionID: active.ID,
		ToNodeID: targetNodeID, State: "stopping", Mode: "warm", CreatedAt: now,
	}
	if err := s.CreateMigration(ctx, m); err != nil {
		return nil, err
	}
	if err := s.CreateSnapshotRequest(ctx, &store.SnapshotRequest{
		ID: ids.Must("snr_"), ServerID: srv.ID, Reason: "migration", CreatedAt: now,
	}); err != nil {
		return nil, err
	}
	_ = s.SetObservedState(ctx, s.DB, srv.ID, "migrating", now)
	return m, nil
}

// shutdownHandoffs warm-migrates servers off nodes that are shutting down
// cleanly, so players are transferred instead of disconnected. Nodes report
// draining in their heartbeat and keep their servers running while the agent
// waits for the move; if no target can take the server the agent falls back
// to its graceful stop and normal recovery re-places it.
func (r *Reconciler) shutdownHandoffs(ctx context.Context, servers []store.Server) error {
	s := r.Store
	now := s.NowMs()
	open, err := s.OpenMigrations(ctx)
	if err != nil {
		return err
	}
	moving := map[string]bool{}
	for i := range open {
		moving[open[i].ServerID] = true
	}
	draining := map[string]bool{}
	for i := range servers {
		srv := &servers[i]
		if srv.DesiredState != "running" || moving[srv.ID] || !SupportsHandoff(srv.GameID) {
			continue
		}
		active, err := s.ActiveExecution(ctx, srv.ID)
		if err != nil || active.Action != "run" || active.State != "running" || active.LeaseExpiresAt <= now {
			continue
		}
		d, seen := draining[active.NodeID]
		if !seen {
			d = r.nodeDraining(ctx, active.NodeID)
			draining[active.NodeID] = d
		}
		if !d {
			continue
		}
		target, err := r.handoffTarget(ctx, srv, active.NodeID)
		if err != nil {
			r.Log.Info("shutdown handoff: no target; host will stop the server",
				"server", srv.ID, "node", active.NodeID, "error", err)
			continue
		}
		m, err := r.StartWarmMigration(ctx, srv, active, target)
		if err != nil {
			r.Log.Error("shutdown handoff", "server", srv.ID, "error", err)
			continue
		}
		_ = s.EmitEvent(ctx, s.DB, srv.GroupID, &srv.ID, &active.NodeID, "server.handoff_requested",
			map[string]any{"migration": m.ID, "to": target, "reason": "node_shutdown"})
	}
	return nil
}

// nodeDraining reports whether an online node advertises a clean shutdown.
func (r *Reconciler) nodeDraining(ctx context.Context, nodeID string) bool {
	n, err := r.Store.GetNode(ctx, nodeID)
	if err != nil || r.Liveness(n) != "online" {
		return false
	}
	st, err := r.Store.GetNodeStatus(ctx, nodeID)
	if err != nil {
		return false
	}
	var caps struct {
		Draining bool `json:"draining"`
	}
	_ = json.Unmarshal([]byte(st.StatusJSON), &caps)
	return caps.Draining
}

// handoffTarget is the node a shutdown handoff moves srv to: its hot
// standby when still eligible, else the scheduler's pick off the source.
func (r *Reconciler) handoffTarget(ctx context.Context, srv *store.Server, sourceNodeID string) (string, error) {
	return r.pickStandby(ctx, srv, sourceNodeID)
}

// placeOff places srv as recovery would, never on exclude; only, when set,
// restricts the candidates to that node (an eligibility check).
func (r *Reconciler) placeOff(ctx context.Context, srv *store.Server, exclude, only string) (string, error) {
	game := catalog.Get(srv.GameID)
	if game == nil {
		return "", &scheduler.Error{Reasons: []string{"unknown game " + srv.GameID}}
	}
	nodes, err := r.Store.ListNodes(ctx, srv.GroupID)
	if err != nil {
		return "", err
	}
	// size and holders of the save a recovery would restore, so the
	// scheduler checks the target's disk and prefers nodes that hold it
	restore, sources, err := r.chooseRestore(ctx, srv, true, nil)
	if err != nil {
		restore, sources = nil, nil
	}
	var snap scheduler.SnapView
	if restore != nil {
		snap.RestoreBytes = restore.StoredBytes
	}
	var views []scheduler.NodeView
	for i := range nodes {
		if nodes[i].ID == exclude || (only != "" && nodes[i].ID != only) {
			continue
		}
		views = append(views, r.placementView(ctx, srv, &nodes[i], restore, sources))
	}
	preferred := ""
	if srv.PreferredNodeID != nil {
		preferred = *srv.PreferredNodeID
	}
	dec, err := scheduler.Place(
		scheduler.ServerView{ID: srv.ID, GameID: srv.GameID, PreferredNodeID: preferred},
		scheduler.GameView{ID: game.ID, MinMemoryMB: game.MinMemoryMB, MinDiskMB: game.MinDiskMB, SteamApp: game.SteamApp, OS: game.OS, Arch: game.Arch, Runtimes: game.Runtimes},
		views,
		snap,
	)
	if err != nil {
		return "", err
	}
	return dec.NodeID, nil
}

// warmSourceSuperseded reports whether a warm migration's source is leaving
// on its own before the flip: it began a graceful stop (its host gave up on
// the handoff — the final save and replication hold follow, so it reports
// 'stopping' long before it ends), or it ended after saving past the snapshot
// the target stages from. Finishing the move would roll players back to the
// older migration snapshot; aborting lets recovery restart from the newer
// save once the source ends.
func (r *Reconciler) warmSourceSuperseded(ctx context.Context, m *store.Migration) bool {
	if m.Mode != "warm" || m.FinalSnapshotID == nil {
		return false
	}
	src, err := r.Store.GetExecution(ctx, m.FromExecutionID)
	if err != nil {
		return false
	}
	if src.EndedAt == nil {
		return src.State == "stopping"
	}
	snaps, err := r.Store.SnapshotsForServer(ctx, m.ServerID)
	if err != nil {
		return false
	}
	var staged *store.Snapshot
	var newest int64
	for i := range snaps {
		sn := &snaps[i]
		if sn.ID == *m.FinalSnapshotID {
			staged = sn
		}
		if sn.ExecutionID == m.FromExecutionID && sn.State != "invalid" && sn.CreatedAt > newest {
			newest = sn.CreatedAt
		}
	}
	return staged != nil && newest > staged.CreatedAt
}
