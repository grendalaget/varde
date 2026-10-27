package reconciler

import (
	"context"
	"time"
)

const (
	eventRetention = 30 * 24 * time.Hour
	// The grace lets device-link pollers and invite links still see "expired" briefly.
	expiredGrace      = 24 * time.Hour
	retentionInterval = time.Hour
)

// RunRetention prunes old events and expired sessions once at start and then
// hourly, on the leader only (like Run). Blocks until ctx is cancelled.
func (r *Reconciler) RunRetention(ctx context.Context) {
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	for {
		if r.Leader == nil || r.Leader.IsLeader() {
			r.PruneEvents(ctx)
			r.PruneSessions(ctx)
			r.PruneExpired(ctx)
			// Fallback: deleted servers are purged where their last save
			// copy goes (the delete ack, the server or machine delete);
			// this sweeps up any a failed purge left behind.
			if err := r.Store.PurgeDeletedServers(ctx); err != nil {
				r.Log.Error("purge deleted servers", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// PruneSessions deletes expired sessions.
func (r *Reconciler) PruneSessions(ctx context.Context) {
	n, err := r.Store.PruneSessions(ctx, r.Store.NowMs())
	if err != nil {
		r.Log.Error("prune sessions", "error", err)
		return
	}
	if n > 0 {
		r.Log.Info("pruned sessions", "deleted", n)
	}
}

// PruneExpired removes invites, enrollment tokens, and device links past the grace period.
func (r *Reconciler) PruneExpired(ctx context.Context) {
	cutoff := r.Store.NowMs() - expiredGrace.Milliseconds()
	n, err := r.Store.PruneExpired(ctx, cutoff)
	if err != nil {
		r.Log.Error("prune expired records", "error", err)
		return
	}
	if n > 0 {
		r.Log.Info("pruned expired records", "deleted", n)
	}
}

// PruneEvents deletes events past eventRetention.
func (r *Reconciler) PruneEvents(ctx context.Context) {
	cutoff := r.Store.NowMs() - eventRetention.Milliseconds()
	n, err := r.Store.PruneEvents(ctx, cutoff)
	if err != nil {
		r.Log.Error("prune events", "error", err)
		return
	}
	if n > 0 {
		r.Log.Info("pruned events", "deleted", n)
	}
}
