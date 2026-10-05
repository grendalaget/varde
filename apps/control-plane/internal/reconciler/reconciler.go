// Package reconciler is the single writer of lease/execution state
// transitions (besides agent reports). One goroutine ticks and is also woken
// by API mutations and heartbeats; all time comes from an injectable clock.
package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/grendalaget/varde/go/ids"

	"github.com/grendalaget/varde/apps/control-plane/internal/catalog"
	"github.com/grendalaget/varde/apps/control-plane/internal/scheduler"
	"github.com/grendalaget/varde/apps/control-plane/internal/store"
)

// Timings (all configurable on the control plane).
type Timings struct {
	HeartbeatIntervalMs int64
	LeaseTTLMs          int64
	StartGraceMs        int64 // extra lease while preparing/restoring
	SuspectAfterMs      int64
	OfflineAfterMs      int64
	UnschedulableRetry  int64
	TickInterval        time.Duration
}

func DefaultTimings() Timings {
	return Timings{
		HeartbeatIntervalMs: 5000,
		LeaseTTLMs:          20000,
		StartGraceMs:        600000,
		SuspectAfterMs:      15000,
		OfflineAfterMs:      30000,
		UnschedulableRetry:  15000,
		TickInterval:        time.Second,
	}
}

// Reconciler owns desired→observed convergence.
type Reconciler struct {
	Store   *store.Store
	Timings Timings
	Log     *slog.Logger

	wake chan struct{}

	lvMu sync.Mutex
	lv   map[string]string // last emitted liveness state per node
}

func New(s *store.Store, t Timings, log *slog.Logger) *Reconciler {
	return &Reconciler{Store: s, Timings: t, Log: log, wake: make(chan struct{}, 1), lv: map[string]string{}}
}

// Wake asks for an immediate pass (API mutation, heartbeat).
func (r *Reconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run loops until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	t := r.Timings.TickInterval
	if t <= 0 {
		t = time.Second
	}
	ticker := time.NewTicker(t)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.wake:
		}
		if err := r.Pass(ctx); err != nil {
			r.Log.Error("reconcile pass failed", "error", err)
		}
	}
}

// Pass processes nodes (liveness) then servers (table from control-plane.md).
func (r *Reconciler) Pass(ctx context.Context) error {
	if err := r.nodeLiveness(ctx); err != nil {
		return err
	}
	servers, err := r.Store.AllServers(ctx)
	if err != nil {
		return err
	}
	for _, srv := range servers {
		if err := r.reconcileServer(ctx, &srv); err != nil {
			r.Log.Error("reconcile server", "server", srv.ID, "error", err)
		}
	}
	return r.migrations(ctx)
}

// ---- node liveness ----

func (r *Reconciler) nodeLiveness(ctx context.Context) error {
	groups, err := r.Store.AllGroups(ctx)
	if err != nil {
		return err
	}
	for _, g := range groups {
		nodes, err := r.Store.ListNodes(ctx, g.ID)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			state := r.Liveness(&n)
			last, seeded := r.lastLiveness(n.ID)
			r.setLiveness(n.ID, state)
			if !seeded {
				// first pass after startup: seed from last_seen_at so a
				// restart doesn't re-emit node.online for every node
				continue
			}
			if last == state {
				continue
			}
			if state != "online" {
				_ = r.Store.EmitEvent(ctx, r.Store.DB, g.ID, nil, strptr(n.ID), "node."+state, map[string]any{"name": n.Name})
			} else {
				_ = r.Store.EmitEvent(ctx, r.Store.DB, g.ID, nil, strptr(n.ID), "node.online", map[string]any{"name": n.Name})
			}
		}
	}
	return nil
}

// Liveness computes online|suspect|offline from last_seen_at.
func (r *Reconciler) Liveness(n *store.Node) string {
	if n.LastSeenAt == nil {
		return "offline"
	}
	delta := r.Store.NowMs() - *n.LastSeenAt
	switch {
	case delta <= r.Timings.SuspectAfterMs:
		return "online"
	case delta <= r.Timings.OfflineAfterMs:
		return "suspect"
	default:
		return "offline"
	}
}

func (r *Reconciler) lastLiveness(nodeID string) (string, bool) {
	r.lvMu.Lock()
	defer r.lvMu.Unlock()
	st, ok := r.lv[nodeID]
	return st, ok
}

func (r *Reconciler) setLiveness(nodeID, state string) {
	r.lvMu.Lock()
	r.lv[nodeID] = state
	r.lvMu.Unlock()
}

// ---- per-server reconcile ----

func (r *Reconciler) reconcileServer(ctx context.Context, srv *store.Server) error {
	s := r.Store
	now := s.NowMs()
	active, err := s.ActiveExecution(ctx, srv.ID)
	if err != nil && err != store.ErrNotFound {
		return err
	}

	// 1. Active exec whose lease expired → end 'lost'.
	if active != nil && active.LeaseExpiresAt <= now {
		if err := s.Tx(ctx, func(tx *sqlx.Tx) error {
			if err := s.EndExecution(ctx, tx, active.ID, "lost", now); err != nil {
				return err
			}
			if srv.DesiredState == "running" {
				if err := s.SetObservedState(ctx, tx, srv.ID, "recovering", now); err != nil {
					return err
				}
			}
			return s.EmitEvent(ctx, tx, srv.GroupID, &srv.ID, &active.NodeID, "lease.expired",
				map[string]any{"execution_id": active.ID, "epoch": active.Epoch})
		}); err != nil {
			return err
		}
		active = nil
	}

	// 2. desired running, no active exec → place + create. Suppressed while a
	// migration is open for this server: the migration state machine owns
	// re-activation on the target node.
	if srv.DesiredState == "running" && active == nil {
		open, err := s.OpenMigrations(ctx)
		if err != nil {
			return err
		}
		for i := range open {
			if open[i].ServerID == srv.ID {
				return nil
			}
		}
		// Crash-loop guard: an execution that ended 'failed' does not
		// reactivate on its own — only an explicit start clears it.
		// 'lost' (lease expiry above) is the only automatic recovery.
		execs, err := s.ExecutionsForServer(ctx, srv.ID)
		if err != nil {
			return err
		}
		if len(execs) > 0 && execs[0].EndReason != nil && *execs[0].EndReason == "failed" {
			if srv.ObservedState != "failed" {
				// server.failed was already emitted when the agent reported
				// the failure — just converge observed state (once).
				if err := s.SetObservedState(ctx, s.DB, srv.ID, "failed", now); err != nil {
					return err
				}
			}
			return nil
		}
		return r.activate(ctx, srv, active, true)
	}

	// 3. desired stopped, active run → mark stop (unless a migration manages it).
	if srv.DesiredState == "stopped" && active != nil && active.Action == "run" {
		if err := s.Tx(ctx, func(tx *sqlx.Tx) error {
			active.Action = "stop"
			reason := "user"
			active.StopReason = &reason
			if err := s.UpdateExecutionState(ctx, tx, active); err != nil {
				return err
			}
			if err := s.SetObservedState(ctx, tx, srv.ID, "stopping", now); err != nil {
				return err
			}
			return s.EmitEvent(ctx, tx, srv.GroupID, &srv.ID, &active.NodeID, "server.stop_requested",
				map[string]any{"execution_id": active.ID})
		}); err != nil {
			return err
		}
	}

	// 4. observed state updates from agent reports.
	if active != nil {
		want := observedFor(srv, active)
		if want != srv.ObservedState {
			if err := s.SetObservedState(ctx, s.DB, srv.ID, want, now); err != nil {
				return err
			}
		}
	} else if srv.DesiredState == "stopped" && srv.ObservedState != "stopped" {
		if err := s.SetObservedState(ctx, s.DB, srv.ID, "stopped", now); err != nil {
			return err
		}
	}
	return nil
}

func observedFor(srv *store.Server, e *store.Execution) string {
	switch {
	case e.Action == "stop":
		return "stopping"
	case e.State == "running":
		return "running"
	case srv.ObservedState == "recovering":
		return "recovering"
	default:
		return "starting"
	}
}

// activationError carries a 409-worthy reason.
type activationError struct {
	Code    string
	Message string
	Details map[string]any
}

func (e *activationError) Error() string { return e.Message }

// ActivationError unwraps activation failures for the API layer.
func ActivationError(err error) *activationError {
	var ae *activationError
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}

// activate creates a new execution for srv. recovery=true means the previous
// execution was lost (lease expiry) — use committed snapshots only.
func (r *Reconciler) activate(ctx context.Context, srv *store.Server, prev *store.Execution, recovery bool) error {
	return r.activateWith(ctx, srv, recovery, "", nil)
}

// ActivateNormal is used by the start API: fresh start rules, not recovery.
// allowOlderSnapshot relaxes the "newest save must be online" rule.
func (r *Reconciler) ActivateNormal(ctx context.Context, srv *store.Server, preferredNodeID string, allowOlder bool) error {
	return r.activateWith(ctx, srv, false, preferredNodeID, &allowOlder)
}

func (r *Reconciler) activateWith(ctx context.Context, srv *store.Server, recovery bool, preferredOverride string, allowOlder *bool) error {
	s := r.Store
	now := s.NowMs()

	// --- restore selection ---
	restore, sources, err := r.chooseRestore(ctx, srv, recovery, allowOlder)
	if err != nil {
		return err
	}

	// --- candidate nodes ---
	nodes, err := s.ListNodes(ctx, srv.GroupID)
	if err != nil {
		return err
	}
	game := catalog.Get(srv.GameID)
	if game == nil {
		return fmt.Errorf("unknown game %q", srv.GameID)
	}
	var views []scheduler.NodeView
	for i := range nodes {
		n := &nodes[i]
		views = append(views, r.nodeView(ctx, n, restore, sources))
	}
	preferred := preferredOverride
	if preferred == "" && srv.PreferredNodeID != nil {
		preferred = *srv.PreferredNodeID
	}
	var snapSize, depSize int64
	if restore != nil {
		snapSize = restore.StoredBytes
	}
	dec, err := scheduler.Place(
		scheduler.ServerView{ID: srv.ID, GameID: srv.GameID, PreferredNodeID: preferred},
		scheduler.GameView{ID: game.ID, MinMemoryMB: game.MinMemoryMB, OS: game.OS, Arch: game.Arch, Runtimes: game.Runtimes},
		views,
		scheduler.SnapView{RestoreBytes: snapSize, DeploymentSize: depSize},
	)
	if err != nil {
		var serr *scheduler.Error
		if errors.As(err, &serr) {
			// emit server.unschedulable (rate-limited by caller cadence)
			_ = s.EmitEvent(ctx, s.DB, srv.GroupID, &srv.ID, nil, "server.unschedulable",
				map[string]any{"reasons": serr.Reasons})
			if srv.DesiredState == "running" {
				msg := fmt.Sprintf("unschedulable: %v", serr.Reasons)
				_ = s.SetObservedState(ctx, s.DB, srv.ID, "failed", now)
				_ = msg
			}
			return &activationError{Code: "unschedulable", Message: err.Error(), Details: map[string]any{"reasons": serr.Reasons}}
		}
		return err
	}

	// --- lease + execution in one transaction ---
	exec := &store.Execution{
		ID:             ids.Must(ids.Execution),
		ServerID:       srv.ID,
		NodeID:         dec.NodeID,
		Epoch:          srv.Epoch + 1,
		State:          "preparing",
		Action:         "run",
		LeaseExpiresAt: now + r.Timings.LeaseTTLMs + r.Timings.StartGraceMs,
		PlacementJSON:  mustJSON(map[string]any{"score": dec.Score, "reasons": dec.Reasons}),
		CreatedAt:      now,
	}
	if restore != nil {
		exec.RestoreSnapshotID = &restore.ID
	}
	if err := s.BumpEpochAndCreateExecution(ctx, exec); err != nil {
		return err
	}
	newObserved := "starting"
	if recovery {
		newObserved = "recovering"
	}
	return s.Tx(ctx, func(tx *sqlx.Tx) error {
		if err := s.SetObservedState(ctx, tx, srv.ID, newObserved, now); err != nil {
			return err
		}
		if err := s.EmitEvent(ctx, tx, srv.GroupID, &srv.ID, &exec.NodeID, "server.assigned",
			map[string]any{"execution_id": exec.ID, "epoch": exec.Epoch, "score": dec.Score, "reasons": dec.Reasons}); err != nil {
			return err
		}
		return s.EmitEvent(ctx, tx, srv.GroupID, &srv.ID, &exec.NodeID, "lease.issued",
			map[string]any{"execution_id": exec.ID, "epoch": exec.Epoch, "expires_at": exec.LeaseExpiresAt})
	})
}

// chooseRestore implements the restore rules:
//   - recovery: newest committed snapshot with a ready replica on an online node
//   - normal:   newest non-invalid snapshot; if none of its replicas are on
//     online nodes → 409 latest_save_unavailable, unless allowOlder.
func (r *Reconciler) chooseRestore(ctx context.Context, srv *store.Server, recovery bool, allowOlder *bool) (*store.Snapshot, []string, error) {
	s := r.Store
	snaps, err := s.SnapshotsForServer(ctx, srv.ID)
	if err != nil {
		return nil, nil, err
	}
	// newest first
	for i, j := 0, len(snaps)-1; i < j; i, j = i+1, j-1 {
		snaps[i], snaps[j] = snaps[j], snaps[i]
	}
	online, err := r.onlineNodeIDs(ctx, srv.GroupID)
	if err != nil {
		return nil, nil, err
	}

	eligible := func(snap *store.Snapshot) bool {
		if recovery && snap.State != "committed" {
			return false
		}
		if snap.State == "invalid" {
			return false
		}
		return true
	}

	var firstBlocker *store.Snapshot
	var firstBlockerOnline []string
	for i := range snaps {
		snap := &snaps[i]
		if !eligible(snap) {
			continue
		}
		reps, err := s.ReplicasForSnapshot(ctx, snap.ID)
		if err != nil {
			return nil, nil, err
		}
		var ready []string
		for _, rep := range reps {
			if rep.State == "ready" && online[rep.NodeID] {
				ready = append(ready, rep.NodeID)
			}
		}
		if len(ready) > 0 {
			return snap, ready, nil
		}
		if firstBlocker == nil {
			firstBlocker = snap
			for _, rep := range reps {
				if rep.State == "ready" {
					firstBlockerOnline = append(firstBlockerOnline, rep.NodeID)
				}
			}
		}
	}

	if !recovery && firstBlocker != nil && (allowOlder == nil || !*allowOlder) {
		return nil, nil, &activationError{
			Code:    "latest_save_unavailable",
			Message: "newest save is only on offline machines",
			Details: map[string]any{
				"snapshot_id": firstBlocker.ID,
				"created_at":  firstBlocker.CreatedAt,
				"nodes":       firstBlockerOnline,
			},
		}
	}
	return nil, nil, nil // no committed save yet: start fresh
}

func (r *Reconciler) onlineNodeIDs(ctx context.Context, groupID string) (map[string]bool, error) {
	nodes, err := r.Store.ListNodes(ctx, groupID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i := range nodes {
		if r.Liveness(&nodes[i]) == "online" {
			out[nodes[i].ID] = true
		}
	}
	return out, nil
}

func (r *Reconciler) nodeView(ctx context.Context, n *store.Node, restore *store.Snapshot, sources []string) scheduler.NodeView {
	v := scheduler.NodeView{
		ID:             n.ID,
		Online:         r.Liveness(n) == "online",
		AdminState:     n.AdminState,
		HostingEnabled: n.HostingEnabled == 1,
		Anchor:         n.Anchor == 1,
		Priority:       n.Priority,
		OS:             n.OS,
		Arch:           n.Arch,
		MaxMemoryMB:    n.MaxMemoryMB,
	}
	if st, err := r.Store.GetNodeStatus(ctx, n.ID); err == nil {
		var caps struct {
			Drivers           []string `json:"drivers"`
			Runtimes          []string `json:"runtimes"`
			CachedDeployments []string `json:"cached_deployments"`
			MemoryAvailableMB int64    `json:"memory_available_mb"`
			DiskFreeBytes     int64    `json:"disk_free_bytes"`
			OnBattery         bool     `json:"on_battery"`
			UserActive        bool     `json:"user_active"`
			UptimeS           int64    `json:"uptime_s"`
			DiskTotalBytes    int64    `json:"disk_total_bytes"`
		}
		_ = json.Unmarshal([]byte(st.StatusJSON), &caps)
		v.Drivers = caps.Drivers
		v.Runtimes = caps.Runtimes
		v.CachedDeployments = caps.CachedDeployments
		v.MemoryAvailableMB = caps.MemoryAvailableMB
		v.DiskFreeBytes = caps.DiskFreeBytes
		v.DiskTotalBytes = caps.DiskTotalBytes
		v.OnBattery = caps.OnBattery
		v.UserActive = caps.UserActive
		v.UptimeS = caps.UptimeS
		var mesh struct {
			Peers []struct {
				NodeID string `json:"node_id"`
				Path   string `json:"path"`
			} `json:"peers"`
		}
		_ = json.Unmarshal([]byte(st.MeshJSON), &mesh)
		for _, p := range mesh.Peers {
			if p.Path == "direct" {
				v.DirectPeerPaths++
			}
		}
	}
	if restore != nil {
		for _, src := range sources {
			if src == n.ID {
				v.HasRestoreLocal = true
			}
		}
	}
	return v
}

// ---- migrations ----
//
// move → old exec action=stop reason=migration; when old exec ended and the
// final/migration snapshot is ready on the target → new exec on target.

func (r *Reconciler) migrations(ctx context.Context) error {
	s := r.Store
	now := s.NowMs()
	ms, err := s.OpenMigrations(ctx)
	if err != nil {
		return err
	}
	for i := range ms {
		m := &ms[i]
		switch m.State {
		case "stopping":
			exec, err := s.GetExecution(ctx, m.FromExecutionID)
			if err != nil {
				continue
			}
			if exec.EndedAt != nil {
				m.State = "await_replication"
			} else {
				continue
			}
			// find the final/migration snapshot produced by that exec
			snaps, err := s.SnapshotsForServer(ctx, m.ServerID)
			if err != nil {
				continue
			}
			var finalSnap *store.Snapshot
			for i := range snaps {
				if snaps[i].ExecutionID == m.FromExecutionID &&
					(snaps[i].Reason == "final" || snaps[i].Reason == "migration") {
					if finalSnap == nil || snaps[i].CreatedAt > finalSnap.CreatedAt {
						finalSnap = &snaps[i]
					}
				}
			}
			if finalSnap == nil {
				m.State = "failed"
				m.FinishedAt = &now
				_ = s.UpdateMigration(ctx, m)
				continue
			}
			m.FinalSnapshotID = &finalSnap.ID
			// ensure a replica is assigned on the target node
			if err := s.UpsertReplica(ctx, s.DB, &store.Replica{
				SnapshotID: finalSnap.ID, NodeID: m.ToNodeID, State: "assigned", AssignedAt: now,
			}); err != nil {
				return err
			}
			_ = s.UpdateMigration(ctx, m)
			_ = s.EmitEvent(ctx, s.DB, snapGroupID(ctx, s, finalSnap), &m.ServerID, &m.ToNodeID,
				"migration.started", map[string]any{"snapshot_id": finalSnap.ID})

		case "await_replication":
			if m.FinalSnapshotID == nil {
				continue
			}
			reps, err := s.ReplicasForSnapshot(ctx, *m.FinalSnapshotID)
			if err != nil {
				continue
			}
			ready := false
			for _, rep := range reps {
				if rep.NodeID == m.ToNodeID && rep.State == "ready" {
					ready = true
				}
			}
			if !ready {
				continue
			}
			srv, err := s.GetServer(ctx, m.ServerID)
			if err != nil {
				continue
			}
			m.State = "activating"
			_ = s.UpdateMigration(ctx, m)
			if err := r.activateMigration(ctx, srv, m); err != nil {
				m.State = "failed"
				m.FinishedAt = &now
				_ = s.UpdateMigration(ctx, m)
				continue
			}
			m.State = "done"
			m.FinishedAt = &now
			_ = s.UpdateMigration(ctx, m)
			_ = s.EmitEvent(ctx, s.DB, srv.GroupID, &srv.ID, &m.ToNodeID,
				"migration.completed", map[string]any{"snapshot_id": *m.FinalSnapshotID})
		}
	}
	return nil
}

// activateMigration creates the new execution on the target restoring the
// migration snapshot (bypasses scheduler; target was user-chosen).
func (r *Reconciler) activateMigration(ctx context.Context, srv *store.Server, m *store.Migration) error {
	s := r.Store
	now := s.NowMs()
	exec := &store.Execution{
		ID:                ids.Must(ids.Execution),
		ServerID:          srv.ID,
		NodeID:            m.ToNodeID,
		Epoch:             srv.Epoch + 1,
		State:             "preparing",
		Action:            "run",
		LeaseExpiresAt:    now + r.Timings.LeaseTTLMs + r.Timings.StartGraceMs,
		RestoreSnapshotID: m.FinalSnapshotID,
		PlacementJSON:     mustJSON(map[string]any{"reasons": []string{"migration to " + m.ToNodeID}}),
		CreatedAt:         now,
	}
	if err := s.BumpEpochAndCreateExecution(ctx, exec); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx *sqlx.Tx) error {
		if err := s.SetObservedState(ctx, tx, srv.ID, "starting", now); err != nil {
			return err
		}
		return s.EmitEvent(ctx, tx, srv.GroupID, &srv.ID, &m.ToNodeID, "server.assigned",
			map[string]any{"execution_id": exec.ID, "epoch": exec.Epoch, "migration": m.ID})
	})
}

func snapGroupID(ctx context.Context, s *store.Store, snap *store.Snapshot) string {
	return snap.GroupID
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func strptr(s string) *string { return &s }
