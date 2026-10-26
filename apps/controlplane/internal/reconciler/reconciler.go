// Package reconciler is the single writer of lease/execution state
// transitions (besides agent reports). One goroutine ticks and is also woken
// by API mutations and heartbeats; all time comes from an injectable clock.
package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/grendalaget/varde/go/ids"

	"github.com/grendalaget/varde/apps/controlplane/internal/catalog"
	"github.com/grendalaget/varde/apps/controlplane/internal/scheduler"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

// Timings (all configurable on the control plane).
type Timings struct {
	HeartbeatIntervalMs      int64
	LeaseTTLMs               int64
	ShortHeartbeatIntervalMs int64
	ShortLeaseTTLMs          int64
	// FastLeaseTTLMs is the lease for agents advertising fast_lease, which
	// renew every LeaseRenewIntervalMs; 0 disables fast leases.
	FastLeaseTTLMs       int64
	LeaseRenewIntervalMs int64
	StartGraceMs         int64 // extra lease while preparing/restoring
	SuspectAfterMs       int64
	OfflineAfterMs       int64
	UnschedulableRetry   int64
	TickInterval         time.Duration
}

type NodeTimings struct {
	HeartbeatIntervalMs int64
	LeaseTTLMs          int64
	SuspectAfterMs      int64
	OfflineAfterMs      int64
}

func (t Timings) ForProfile(profile string) NodeTimings {
	if profile == "short" {
		return NodeTimings{
			HeartbeatIntervalMs: t.ShortHeartbeatIntervalMs,
			LeaseTTLMs:          t.ShortLeaseTTLMs,
			SuspectAfterMs:      3 * t.ShortHeartbeatIntervalMs,
			OfflineAfterMs:      6 * t.ShortHeartbeatIntervalMs,
		}
	}
	return NodeTimings{
		HeartbeatIntervalMs: t.HeartbeatIntervalMs,
		LeaseTTLMs:          t.LeaseTTLMs,
		SuspectAfterMs:      t.SuspectAfterMs,
		OfflineAfterMs:      t.OfflineAfterMs,
	}
}

func DefaultTimings() Timings {
	return Timings{
		HeartbeatIntervalMs:      5000,
		LeaseTTLMs:               20000,
		ShortHeartbeatIntervalMs: 1000,
		ShortLeaseTTLMs:          5000,
		StartGraceMs:             30000,
		SuspectAfterMs:           15000,
		OfflineAfterMs:           30000,
		UnschedulableRetry:       15000,
		TickInterval:             time.Second,
		FastLeaseTTLMs:           8000,
		LeaseRenewIntervalMs:     1000,
	}
}

// Reconciler owns desired→observed convergence.
type Reconciler struct {
	Store   *store.Store
	Timings Timings
	Log     *slog.Logger

	// Leader, when set, gates the periodic Run loop: non-leader instances skip
	// their passes. User-triggered calls (Wake, ActivateNormal) are unaffected —
	// they must work on any replica. Nil means single-instance (always runs).
	Leader interface{ IsLeader() bool }

	wake chan struct{}

	changedMu sync.Mutex
	changed   map[string]chan struct{}

	fingerprintMu           sync.Mutex
	fingerprints            map[string]string
	fingerprintsInitialized bool

	lvMu sync.Mutex
	lv   map[string]string // last emitted liveness state per node
}

func New(s *store.Store, t Timings, log *slog.Logger) *Reconciler {
	return &Reconciler{
		Store: s, Timings: t, Log: log, wake: make(chan struct{}, 1),
		fingerprints: make(map[string]string), lv: map[string]string{},
	}
}

// Wake asks for an immediate pass (API mutation, heartbeat).
func (r *Reconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// WakeGroup notifies waiters for groupID and asks for an immediate pass.
func (r *Reconciler) WakeGroup(groupID string) {
	r.NotifyGroup(groupID)
	r.Wake()
}

// Changed returns the current change generation channel for groupID.
func (r *Reconciler) Changed(groupID string) <-chan struct{} {
	r.changedMu.Lock()
	defer r.changedMu.Unlock()
	if r.changed == nil {
		r.changed = make(map[string]chan struct{})
	}
	if r.changed[groupID] == nil {
		r.changed[groupID] = make(chan struct{})
	}
	return r.changed[groupID]
}

// NotifyGroup closes the active generation for groupID, if there is one.
func (r *Reconciler) NotifyGroup(groupID string) {
	r.changedMu.Lock()
	defer r.changedMu.Unlock()
	if ch := r.changed[groupID]; ch != nil {
		close(ch)
		delete(r.changed, groupID)
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
		if r.Leader != nil && !r.Leader.IsLeader() {
			continue
		}
		if err := r.Pass(ctx); err != nil {
			r.Log.Error("reconcile pass failed", "error", err)
		}
	}
}

// Pass processes nodes (liveness) then servers (table from control-plane.md).
func (r *Reconciler) Pass(ctx context.Context) (passErr error) {
	defer func() {
		if err := r.notifyChangedGroups(ctx); err != nil {
			r.Log.Error("compute directive fingerprints", "error", err)
			if passErr == nil {
				passErr = err
			}
		}
	}()
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
	if err := r.standbys(ctx, servers); err != nil {
		return err
	}
	if err := r.shutdownHandoffs(ctx, servers); err != nil {
		return err
	}
	return r.migrations(ctx)
}

type directiveGroupFingerprint struct {
	Executions []store.DirectiveExecutionFingerprint `json:"executions"`
	Migrations []store.DirectiveMigrationFingerprint `json:"migrations"`
	Nodes      []directiveNodeFingerprint            `json:"nodes"`
	Replicas   []store.DirectiveReplicaFingerprint   `json:"replicas"`
	Events     int64                                 `json:"max_event_id"`
	Servers    []store.DirectiveServerFingerprint    `json:"servers"`
}

type directiveNodeFingerprint struct {
	ID             string `json:"id"`
	LeaseProfile   string `json:"lease_profile"`
	Liveness       string `json:"liveness"`
	AdminState     string `json:"admin_state"`
	HostingEnabled int64  `json:"hosting_enabled"`
	Anchor         int64  `json:"anchor"`
}

func (r *Reconciler) notifyChangedGroups(ctx context.Context) error {
	data, err := r.Store.DirectiveFingerprintData(ctx)
	if err != nil {
		return err
	}

	groups := make(map[string]*directiveGroupFingerprint, len(data.Groups))
	for _, groupID := range data.Groups {
		groups[groupID] = &directiveGroupFingerprint{
			Executions: make([]store.DirectiveExecutionFingerprint, 0),
			Migrations: make([]store.DirectiveMigrationFingerprint, 0),
			Nodes:      make([]directiveNodeFingerprint, 0),
			Replicas:   make([]store.DirectiveReplicaFingerprint, 0),
			Servers:    make([]store.DirectiveServerFingerprint, 0),
		}
	}
	groupFor := func(groupID string) *directiveGroupFingerprint {
		group := groups[groupID]
		if group == nil {
			group = &directiveGroupFingerprint{
				Executions: make([]store.DirectiveExecutionFingerprint, 0),
				Migrations: make([]store.DirectiveMigrationFingerprint, 0),
				Nodes:      make([]directiveNodeFingerprint, 0),
				Replicas:   make([]store.DirectiveReplicaFingerprint, 0),
				Servers:    make([]store.DirectiveServerFingerprint, 0),
			}
			groups[groupID] = group
		}
		return group
	}
	for _, execution := range data.Executions {
		group := groupFor(execution.GroupID)
		group.Executions = append(group.Executions, execution)
	}
	for _, migration := range data.Migrations {
		group := groupFor(migration.GroupID)
		group.Migrations = append(group.Migrations, migration)
	}
	for _, node := range data.Nodes {
		group := groupFor(node.GroupID)
		group.Nodes = append(group.Nodes, directiveNodeFingerprint{
			ID:             node.ID,
			LeaseProfile:   node.LeaseProfile,
			Liveness:       r.Liveness(&store.Node{LastSeenAt: node.LastSeenAt, LeaseProfile: node.LeaseProfile}),
			AdminState:     node.AdminState,
			HostingEnabled: node.HostingEnabled,
			Anchor:         node.Anchor,
		})
	}
	for _, replica := range data.Replicas {
		group := groupFor(replica.GroupID)
		group.Replicas = append(group.Replicas, replica)
	}
	for _, event := range data.Events {
		groupFor(event.GroupID).Events = event.MaxID
	}
	for _, server := range data.Servers {
		group := groupFor(server.GroupID)
		group.Servers = append(group.Servers, server)
	}

	current := make(map[string]string, len(groups))
	for groupID, group := range groups {
		encoded, err := json.Marshal(group)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(encoded)
		current[groupID] = hex.EncodeToString(sum[:])
	}

	r.fingerprintMu.Lock()
	if !r.fingerprintsInitialized {
		r.fingerprints = current
		r.fingerprintsInitialized = true
		r.fingerprintMu.Unlock()
		return nil
	}
	changed := make([]string, 0)
	for groupID, fingerprint := range current {
		if previous, ok := r.fingerprints[groupID]; !ok || previous != fingerprint {
			changed = append(changed, groupID)
		}
	}
	for groupID := range r.fingerprints {
		if _, ok := current[groupID]; !ok {
			changed = append(changed, groupID)
		}
	}
	r.fingerprints = current
	r.fingerprintMu.Unlock()

	for _, groupID := range changed {
		r.NotifyGroup(groupID)
	}
	return nil
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
			_ = r.Store.EmitEvent(ctx, r.Store.DB, g.ID, nil, strptr(n.ID), "node."+state, map[string]any{"name": n.Name})
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
	timings := r.Timings.ForProfile(n.LeaseProfile)
	switch {
	case delta <= timings.SuspectAfterMs:
		return "online"
	case delta <= timings.OfflineAfterMs:
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
	if err != nil && !errors.Is(err, store.ErrNotFound) {
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
		restoreFailures := 0
		if len(execs) > 0 && execs[0].EndReason != nil && *execs[0].EndReason == "restore_failed" {
			for _, ex := range execs {
				if ex.EndReason == nil || *ex.EndReason != "restore_failed" {
					break
				}
				restoreFailures++
			}
			if restoreFailures < 3 {
				lastFailure := execs[0].EndedAt
				if lastFailure != nil && now >= *lastFailure+10_000 {
					return r.activate(ctx, srv, true)
				}
				if srv.ObservedState != "recovering" {
					if err := s.SetObservedState(ctx, s.DB, srv.ID, "recovering", now); err != nil {
						return err
					}
				}
				return nil
			}
		}
		if len(execs) > 0 && execs[0].EndReason != nil &&
			(*execs[0].EndReason == "failed" || *execs[0].EndReason == "restore_failed") {
			if srv.ObservedState != "failed" {
				// The failure event was already emitted when the agent
				// reported it — just converge observed state (once).
				if err := s.SetObservedState(ctx, s.DB, srv.ID, "failed", now); err != nil {
					return err
				}
			}
			return nil
		}
		// Recovery rules (committed only, falls back to an older save) are
		// only for a lost host. Anything else uses the normal rules, so this
		// loop never silently picks an older save.
		if lastServingEndedLost(execs) {
			return r.activate(ctx, srv, true)
		}
		err = r.activate(ctx, srv, false)
		if ae := ActivationError(err); ae != nil &&
			(ae.Code == "latest_save_unavailable" || ae.Code == "no_reachable_save") {
			// Wait for the save's machine; the next pass retries. Only a
			// stale exec-derived state is replaced — these already say the
			// server isn't up.
			if !slices.Contains([]string{"stopped", "failed", "recovering"}, srv.ObservedState) {
				return s.SetObservedState(ctx, s.DB, srv.ID, "recovering", now)
			}
			return nil
		}
		return err
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

// lastServingEndedLost reports whether the newest serving execution (execs
// newest first) ended by lease expiry. Aborted warm-migration incoming execs
// are skipped: they never served.
func lastServingEndedLost(execs []store.Execution) bool {
	for _, ex := range execs {
		if ex.Role != "" && ex.Role != "serving" {
			continue
		}
		return ex.EndReason != nil && *ex.EndReason == "lost"
	}
	return false
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
func (r *Reconciler) activate(ctx context.Context, srv *store.Server, recovery bool) error {
	return r.activateWith(ctx, srv, recovery, "", nil)
}

// ActivateNormal is used by the start API: fresh start rules, not recovery.
// allowOlderSnapshot relaxes the "newest save must be online" rule. It sets
// desired=running in the activation's own transaction, so a refused start
// leaves desired untouched.
func (r *Reconciler) ActivateNormal(ctx context.Context, srv *store.Server, preferredNodeID string, allowOlder bool) error {
	return r.activateWith(ctx, srv, false, preferredNodeID, &allowOlder)
}

// activateWith places and activates srv. A non-nil allowOlder marks an
// explicit start (ActivateNormal).
func (r *Reconciler) activateWith(ctx context.Context, srv *store.Server, recovery bool, preferredOverride string, allowOlder *bool) error {
	s := r.Store
	now := s.NowMs()

	// --- restore selection ---
	restore, sources, err := r.chooseRestore(ctx, srv, recovery, allowOlder)
	if err != nil {
		if recovery {
			if ae := ActivationError(err); ae != nil && ae.Code == "latest_save_unavailable" {
				if srv.DesiredState == "running" {
					_ = s.SetObservedState(ctx, s.DB, srv.ID, "recovering", now)
				}
				_ = s.EmitEvent(ctx, s.DB, srv.GroupID, &srv.ID, nil, "server.save_unavailable",
					map[string]any{
						"snapshot_id": ae.Details["snapshot_id"],
						"nodes":       ae.Details["nodes"],
					})
			}
		}
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
	var silent map[string]bool
	if recovery {
		silent = r.silentSinceLost(ctx, srv.ID, nodes)
	}
	for i := range nodes {
		v := r.placementView(ctx, srv, &nodes[i], restore, sources)
		if silent[nodes[i].ID] {
			// Its lease just ran out and it hasn't checked in since: it may
			// still look online, but it's the machine that died.
			v.Online = false
		}
		views = append(views, v)
	}
	preferred := preferredOverride
	if preferred == "" && srv.PreferredNodeID != nil {
		preferred = *srv.PreferredNodeID
	}
	var snapSize, depSize int64
	if restore != nil {
		snapSize = restore.StoredBytes
	}
	sv := scheduler.ServerView{ID: srv.ID, GameID: srv.GameID, PreferredNodeID: preferred}
	gv := scheduler.GameView{ID: game.ID, MinMemoryMB: game.MinMemoryMB, OS: game.OS, Arch: game.Arch, Runtimes: game.Runtimes}
	snapView := scheduler.SnapView{RestoreBytes: snapSize, DeploymentSize: depSize}
	var dec *scheduler.Decision
	// recovery goes to the hot standby while it can take the server: it
	// holds the deployment and a replica of the save
	if recovery && srv.StandbyNodeID != nil {
		for _, v := range views {
			if v.ID == *srv.StandbyNodeID {
				if d, perr := scheduler.Place(sv, gv, []scheduler.NodeView{v}, snapView); perr == nil {
					dec = d
				}
			}
		}
	}
	if dec == nil {
		dec, err = scheduler.Place(sv, gv, views, snapView)
	}
	if err != nil {
		var serr *scheduler.Error
		if errors.As(err, &serr) {
			// emit server.unschedulable (rate-limited by caller cadence)
			_ = s.EmitEvent(ctx, s.DB, srv.GroupID, &srv.ID, nil, "server.unschedulable",
				map[string]any{"reasons": serr.Reasons})
			if srv.DesiredState == "running" {
				_ = s.SetObservedState(ctx, s.DB, srv.ID, "failed", now)
			}
			return &activationError{Code: "unschedulable", Message: err.Error(), Details: map[string]any{"reasons": serr.Reasons}}
		}
		return err
	}

	targetNode, err := s.GetNode(ctx, dec.NodeID)
	if err != nil {
		return err
	}
	leaseTTL := r.Timings.ForProfile(targetNode.LeaseProfile).LeaseTTLMs

	// --- lease + execution in one transaction ---
	exec := &store.Execution{
		ID:             ids.Must(ids.Execution),
		ServerID:       srv.ID,
		NodeID:         dec.NodeID,
		Epoch:          srv.Epoch + 1,
		State:          "preparing",
		Action:         "run",
		LeaseExpiresAt: now + leaseTTL + r.Timings.StartGraceMs,
		PlacementJSON:  mustJSON(map[string]any{"score": dec.Score, "reasons": dec.Reasons}),
		CreatedAt:      now,
	}
	if restore != nil {
		exec.RestoreSnapshotID = &restore.ID
	}
	create := s.BumpEpochAndCreateExecution
	if allowOlder != nil {
		create = s.StartExecution
	}
	if err := create(ctx, exec); err != nil {
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
//     online nodes → 409 latest_save_unavailable, unless allowOlder; with
//     allowOlder and no save reachable at all → 409 no_reachable_save.
func (r *Reconciler) chooseRestore(ctx context.Context, srv *store.Server, recovery bool, allowOlder *bool) (*store.Snapshot, []string, error) {
	s := r.Store
	snaps, err := s.SnapshotsForServer(ctx, srv.ID)
	if err != nil {
		return nil, nil, err
	}
	// newest first
	slices.Reverse(snaps)
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
	blocked := func() *activationError {
		if recovery || firstBlocker == nil || (allowOlder != nil && *allowOlder) {
			return nil
		}
		return &activationError{
			Code:    "latest_save_unavailable",
			Message: "newest save is only on offline machines",
			Details: map[string]any{
				"snapshot_id": firstBlocker.ID,
				"created_at":  firstBlocker.CreatedAt,
				"nodes":       firstBlockerOnline,
			},
		}
	}
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
			_, on := online[rep.NodeID]
			if rep.State == "ready" && on {
				ready = append(ready, rep.NodeID)
			}
		}
		if len(ready) > 0 {
			if ae := blocked(); ae != nil {
				return nil, nil, ae
			}
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

	if recovery && firstBlocker != nil {
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
	if ae := blocked(); ae != nil {
		return nil, nil, ae
	}
	if firstBlocker != nil {
		// allow_older found nothing reachable either: a fresh world here
		// would supersede the real save and retention would delete it.
		return nil, nil, &activationError{
			Code:    "no_reachable_save",
			Message: "no save is reachable on an online machine",
			Details: map[string]any{
				"snapshot_id": firstBlocker.ID,
				"created_at":  firstBlocker.CreatedAt,
				"nodes":       firstBlockerOnline,
			},
		}
	}
	return nil, nil, nil // no save yet: start fresh
}

func (r *Reconciler) onlineNodeIDs(ctx context.Context, groupID string) (map[string]struct{}, error) {
	nodes, err := r.Store.ListNodes(ctx, groupID)
	if err != nil {
		return nil, err
	}
	out := map[string]struct{}{}
	for i := range nodes {
		if r.Liveness(&nodes[i]) == "online" {
			out[nodes[i].ID] = struct{}{}
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
			Draining          bool     `json:"draining"`
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
		v.Draining = caps.Draining
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
		warm := m.Mode == "warm"
		if warm && now-m.CreatedAt > warmMigrationTimeoutMs {
			r.failMigration(ctx, m)
			continue
		}
		switch m.State {
		case "stopping":
			exec, err := s.GetExecution(ctx, m.FromExecutionID)
			if err != nil {
				continue
			}
			// serial mode waits for the source exec to end; warm mode takes a
			// live 'migration' snapshot while the source keeps serving.
			if exec.EndedAt != nil {
				m.State = "await_replication"
			} else if !warm {
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
					(snaps[i].Reason == "final" || snaps[i].Reason == "migration") &&
					snaps[i].CreatedAt >= m.CreatedAt {
					if finalSnap == nil || snaps[i].CreatedAt > finalSnap.CreatedAt {
						finalSnap = &snaps[i]
					}
				}
			}
			if finalSnap == nil {
				if !warm {
					m.State = "failed"
					m.FinishedAt = &now
					_ = s.UpdateMigration(ctx, m)
				}
				continue
			}
			m.FinalSnapshotID = &finalSnap.ID
			m.State = "await_replication"
			// ensure a replica is assigned on the target node
			if err := s.UpsertReplica(ctx, s.DB, &store.Replica{
				SnapshotID: finalSnap.ID, NodeID: m.ToNodeID, State: "assigned", AssignedAt: now,
			}); err != nil {
				return err
			}
			_ = s.UpdateMigration(ctx, m)
			_ = s.EmitEvent(ctx, s.DB, finalSnap.GroupID, &m.ServerID, &m.ToNodeID,
				"migration.started", map[string]any{"snapshot_id": finalSnap.ID})

		case "await_replication":
			if m.FinalSnapshotID == nil {
				continue
			}
			if r.warmSourceSuperseded(ctx, m) {
				r.failMigration(ctx, m)
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
			if warm {
				// incoming exec stages on the target; the flip happens in
				// 'activating' once it reports running.
				continue
			}
			m.State = "done"
			m.FinishedAt = &now
			_ = s.UpdateMigration(ctx, m)
			_ = s.EmitEvent(ctx, s.DB, srv.GroupID, &srv.ID, &m.ToNodeID,
				"migration.completed", map[string]any{"snapshot_id": *m.FinalSnapshotID})

		case "activating":
			// warm mode only: wait for the incoming exec to report running,
			// then flip serving/draining roles and let the source hand off.
			if m.ToExecutionID == nil {
				continue
			}
			next, err := s.GetExecution(ctx, *m.ToExecutionID)
			if err != nil {
				continue
			}
			if next.EndedAt != nil || next.State == "failed" {
				// incoming exec died before the flip — abort; the source
				// keeps serving and unfreezes via directives.
				r.failMigration(ctx, m)
				continue
			}
			if r.warmSourceSuperseded(ctx, m) {
				// the source saved past the staged snapshot and stopped:
				// recovery restarts from its newer save instead
				r.failMigration(ctx, m)
				continue
			}
			if next.State != "running" {
				continue
			}
			prev, err := s.GetExecution(ctx, m.FromExecutionID)
			if err != nil {
				continue
			}
			r.flipMigration(ctx, m, prev, next, now)
		}
	}
	return nil
}

// warmMigrationTimeoutMs bounds staging+activation for a warm move before the
// migration aborts and the source keeps serving.
const warmMigrationTimeoutMs = 10 * 60 * 1000

// routeDrainMs is how long the mesh keeps old-epoch streams alive after a
// warm flip, so client handoffs ride their existing connections.
const routeDrainMs = 30 * 1000

func (r *Reconciler) failMigration(ctx context.Context, m *store.Migration) {
	s := r.Store
	now := s.NowMs()
	if m.ToExecutionID != nil {
		if e, err := s.GetExecution(ctx, *m.ToExecutionID); err == nil && e.EndedAt == nil {
			e.Action = "stop"
			reason := "migration"
			e.StopReason = &reason
			_ = s.UpdateExecutionState(ctx, s.DB, e)
			// End it CP-side too: if the target is dead it will never report
			// 'stopped', and reconcileServer only fences the serving lease —
			// without this the incoming exec would stay open-ended forever.
			_ = s.EndExecution(ctx, s.DB, e.ID, "aborted", now)
		}
	}
	m.State = "failed"
	m.FinishedAt = &now
	_ = s.UpdateMigration(ctx, m)
}

// flipMigration promotes the incoming exec to serving and drains the old one:
// routes point at the target while the source's existing streams stay up for
// the drain window so drivers can hand connected clients off.
func (r *Reconciler) flipMigration(ctx context.Context, m *store.Migration, prev, next *store.Execution, now int64) {
	s := r.Store
	srv, err := s.GetServer(ctx, m.ServerID)
	if err != nil {
		return
	}
	reason := "handoff"
	if err := s.Tx(ctx, func(tx *sqlx.Tx) error {
		prev.Role = "draining"
		prev.Action = "stop"
		prev.StopReason = &reason
		if err := s.UpdateExecutionState(ctx, tx, prev); err != nil {
			return err
		}
		next.Role = "serving"
		if err := s.UpdateExecutionState(ctx, tx, next); err != nil {
			return err
		}
		return s.EmitEvent(ctx, tx, srv.GroupID, &srv.ID, &m.ToNodeID, "server.assigned",
			map[string]any{"execution_id": next.ID, "epoch": next.Epoch, "migration": m.ID})
	}); err != nil {
		// role flip didn't commit — stay in 'activating' and retry next pass
		return
	}
	drain := now + routeDrainMs
	m.State = "done"
	m.FinishedAt = &now
	m.DrainUntil = &drain
	_ = s.UpdateMigration(ctx, m)
	_ = s.EmitEvent(ctx, s.DB, srv.GroupID, &srv.ID, &m.ToNodeID,
		"migration.completed", map[string]any{"snapshot_id": m.FinalSnapshotID})
}

// activateMigration creates the new execution on the target restoring the
// migration snapshot (bypasses scheduler; target was user-chosen). Warm moves
// create it with role='incoming' so the source keeps serving until the flip.
func (r *Reconciler) activateMigration(ctx context.Context, srv *store.Server, m *store.Migration) error {
	s := r.Store
	now := s.NowMs()
	targetNode, err := s.GetNode(ctx, m.ToNodeID)
	if err != nil {
		return err
	}
	leaseTTL := r.Timings.ForProfile(targetNode.LeaseProfile).LeaseTTLMs
	warm := m.Mode == "warm"
	role := "serving"
	if warm {
		role = "incoming"
	}
	exec := &store.Execution{
		ID:                ids.Must(ids.Execution),
		ServerID:          srv.ID,
		NodeID:            m.ToNodeID,
		Epoch:             srv.Epoch + 1,
		State:             "preparing",
		Action:            "run",
		LeaseExpiresAt:    now + leaseTTL + r.Timings.StartGraceMs,
		RestoreSnapshotID: m.FinalSnapshotID,
		PlacementJSON:     mustJSON(map[string]any{"reasons": []string{"migration to " + m.ToNodeID}}),
		CreatedAt:         now,
		Role:              role,
	}
	if err := s.BumpEpochAndCreateExecution(ctx, exec); err != nil {
		return err
	}
	if warm {
		m.ToExecutionID = &exec.ID
		_ = s.UpdateMigration(ctx, m)
		return nil
	}
	return s.Tx(ctx, func(tx *sqlx.Tx) error {
		if err := s.SetObservedState(ctx, tx, srv.ID, "starting", now); err != nil {
			return err
		}
		return s.EmitEvent(ctx, tx, srv.GroupID, &srv.ID, &m.ToNodeID, "server.assigned",
			map[string]any{"execution_id": exec.ID, "epoch": exec.Epoch, "migration": m.ID})
	})
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func strptr(s string) *string { return &s }

// silentSinceLost returns nodes that lost a lease on this server and have
// not been seen since, so recovery doesn't place straight back onto them.
func (r *Reconciler) silentSinceLost(ctx context.Context, serverID string, nodes []store.Node) map[string]bool {
	var lost []struct {
		NodeID  string `db:"node_id"`
		EndedAt int64  `db:"ended_at"`
	}
	if err := r.Store.DB.SelectContext(ctx, &lost, r.Store.Rebind(
		`SELECT node_id, ended_at FROM server_executions
		 WHERE server_id=? AND end_reason='lost' AND ended_at IS NOT NULL`), serverID); err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, l := range lost {
		for i := range nodes {
			n := &nodes[i]
			if n.ID == l.NodeID && (n.LastSeenAt == nil || *n.LastSeenAt <= l.EndedAt) {
				out[n.ID] = true
			}
		}
	}
	return out
}
