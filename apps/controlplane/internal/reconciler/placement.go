package reconciler

import (
	"context"
	"fmt"

	"github.com/grendalaget/varde/apps/controlplane/internal/catalog"
	"github.com/grendalaget/varde/apps/controlplane/internal/scheduler"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func (r *Reconciler) placementGameView(ctx context.Context, srv *store.Server) (scheduler.GameView, error) {
	game := catalog.Get(srv.GameID)
	if game == nil {
		return scheduler.GameView{}, fmt.Errorf("unknown game %q", srv.GameID)
	}
	addons, err := r.Store.ListServerAddons(ctx, srv.ID)
	if err != nil {
		return scheduler.GameView{}, fmt.Errorf("list addons for server %s: %w", srv.ID, err)
	}
	targets := make([]string, 0, len(addons))
	for _, addon := range addons {
		targets = append(targets, addon.Target)
	}
	return scheduler.GameView{
		ExcludeAnchors: game.ExcludeAnchors,
		ID:             game.ID, MinMemoryMB: game.MinMemoryMB, MinDiskMB: game.MinDiskMB,
		SteamApp: game.SteamApp, OS: catalog.PlacementOS(game, targets),
		Arch: game.Arch, Runtimes: game.Runtimes,
	}, nil
}

func (r *Reconciler) ValidateMoveTarget(ctx context.Context, srv *store.Server, nodeID string) error {
	game, err := r.placementGameView(ctx, srv)
	if err != nil {
		return err
	}
	node, err := r.Store.GetNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if node.GroupID != srv.GroupID {
		return fmt.Errorf("target node not in group")
	}
	if reason := scheduler.ExclusionReason(game, scheduler.NodeView{Anchor: node.Anchor == 1}); reason != "" {
		return &activationError{Code: "group_pc_required", Message: "This server needs a compatible PC in your group", Details: map[string]any{"reason": reason}}
	}
	return nil
}
