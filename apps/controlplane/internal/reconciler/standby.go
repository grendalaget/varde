package reconciler

import (
	"context"

	"github.com/grendalaget/varde/apps/controlplane/internal/scheduler"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

// standbys keeps one hot standby per running server: the node recovery
// would pick if the host vanished. Agents pre-install its deployment and
// every snapshot replicates there, so a failover or shutdown handoff skips
// the runtime download and restores from local chunks. The choice is sticky
// while the standby stays eligible, so replication and installs don't
// chase small score changes. Servers mid-migration keep theirs unchanged.
func (r *Reconciler) standbys(ctx context.Context, servers []store.Server) error {
	s := r.Store
	open, err := s.OpenMigrations(ctx)
	if err != nil {
		return err
	}
	moving := map[string]bool{}
	for i := range open {
		moving[open[i].ServerID] = true
	}
	for i := range servers {
		srv := &servers[i]
		if moving[srv.ID] {
			continue
		}
		want := ""
		if srv.DesiredState == "running" {
			if active, err := s.ActiveExecution(ctx, srv.ID); err == nil && active.State == "running" {
				want, _ = r.pickStandby(ctx, srv, active.NodeID)
			}
		}
		cur := ""
		if srv.StandbyNodeID != nil {
			cur = *srv.StandbyNodeID
		}
		if want == cur {
			continue
		}
		var next *string
		if want != "" {
			next = &want
		}
		if err := s.SetServerStandby(ctx, srv.ID, next); err != nil {
			return err
		}
		srv.StandbyNodeID = next
		r.Log.Info("hot standby", "server", srv.ID, "from", cur, "to", want)
		if want != "" {
			if err := r.seedStandby(ctx, srv, want); err != nil {
				return err
			}
		}
	}
	return nil
}

// seedStandby gives a newly chosen standby the save recovery would restore:
// snapshots replicate to the standby when they are created, so one taken
// before it became standby would otherwise never reach it.
func (r *Reconciler) seedStandby(ctx context.Context, srv *store.Server, nodeID string) error {
	restore, _, err := r.chooseRestore(ctx, srv, true, nil)
	if err != nil || restore == nil {
		return nil
	}
	reps, err := r.Store.ReplicasForSnapshot(ctx, restore.ID)
	if err != nil {
		return err
	}
	for _, rep := range reps {
		if rep.NodeID == nodeID {
			return nil
		}
	}
	return r.Store.UpsertReplica(ctx, r.Store.DB, &store.Replica{
		SnapshotID: restore.ID, NodeID: nodeID, State: "assigned", AssignedAt: r.Store.NowMs(),
	})
}

// pickStandby returns srv's current standby if it is still eligible to take
// over from host, else the scheduler's pick among the other nodes.
func (r *Reconciler) pickStandby(ctx context.Context, srv *store.Server, host string) (string, error) {
	if srv.StandbyNodeID != nil && *srv.StandbyNodeID != host {
		if id, err := r.placeOff(ctx, srv, host, *srv.StandbyNodeID); err == nil {
			return id, nil
		}
	}
	return r.placeOff(ctx, srv, host, "")
}

// placementView is nodeView plus the server's deployment, so nodes that
// already installed it (e.g. its standby) get the scheduler's cached bonus.
func (r *Reconciler) placementView(ctx context.Context, srv *store.Server, n *store.Node, restore *store.Snapshot, sources []string) scheduler.NodeView {
	v := r.nodeView(ctx, n, restore, sources)
	if srv.DeploymentID != nil {
		v.NeededDeployment = *srv.DeploymentID
	}
	return v
}
