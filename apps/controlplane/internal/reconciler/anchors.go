package reconciler

import (
	"context"

	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

// seedAnchors gives every online anchor the server's newest committed save.
func (r *Reconciler) seedAnchors(ctx context.Context, servers []store.Server) error {
	nodesByGroup := make(map[string][]store.Node)
	replicasBySnapshot := make(map[string]map[string]struct{})

	for i := range servers {
		srv := &servers[i]
		snapshots, err := r.Store.SnapshotsForServer(ctx, srv.ID)
		if err != nil {
			return err
		}
		var newest *store.Snapshot
		for j := len(snapshots) - 1; j >= 0; j-- {
			if snapshots[j].State == "committed" {
				newest = &snapshots[j]
				break
			}
		}
		if newest == nil {
			continue
		}

		nodes, ok := nodesByGroup[srv.GroupID]
		if !ok {
			nodes, err = r.Store.ListNodes(ctx, srv.GroupID)
			if err != nil {
				return err
			}
			nodesByGroup[srv.GroupID] = nodes
		}
		var anchors []store.Node
		for i := range nodes {
			n := &nodes[i]
			if n.Anchor == 1 && n.ID != newest.NodeID && r.Liveness(n) == "online" {
				anchors = append(anchors, *n)
			}
		}
		if len(anchors) == 0 {
			continue
		}

		replicas, ok := replicasBySnapshot[newest.ID]
		if !ok {
			rows, err := r.Store.ReplicasForSnapshot(ctx, newest.ID)
			if err != nil {
				return err
			}
			replicas = make(map[string]struct{}, len(rows))
			for _, row := range rows {
				replicas[row.NodeID] = struct{}{}
			}
			replicasBySnapshot[newest.ID] = replicas
		}
		for i := range anchors {
			n := &anchors[i]
			if _, exists := replicas[n.ID]; exists {
				continue
			}
			if err := r.Store.UpsertReplica(ctx, r.Store.DB, &store.Replica{
				SnapshotID: newest.ID,
				NodeID:     n.ID,
				State:      "assigned",
				AssignedAt: r.Store.NowMs(),
			}); err != nil {
				return err
			}
			replicas[n.ID] = struct{}{}
		}
	}
	return nil
}
