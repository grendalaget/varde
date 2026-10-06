package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/grendalaget/varde/go/ids"
	"github.com/grendalaget/varde/go/relaytoken"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/reconciler"
	"github.com/grendalaget/varde/apps/controlplane/internal/scheduler"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

// activationErr bridges reconciler errors to the API layer.
type actErrData struct {
	Code    string
	Message string
	Details map[string]any
}

func activationErr(err error) *actErrData {
	if ae := reconciler.ActivationError(err); ae != nil {
		return &actErrData{Code: ae.Code, Message: ae.Message, Details: ae.Details}
	}
	return nil
}

func filterDialEndpoints(endpoints []string) []string {
	filtered := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if addrPort, err := netip.ParseAddrPort(endpoint); err == nil {
			if addrPort.Addr().IsUnspecified() {
				continue
			}
		} else if addr, err := netip.ParseAddr(strings.Trim(endpoint, "[]")); err == nil && addr.IsUnspecified() {
			continue
		}
		filtered = append(filtered, endpoint)
	}
	return filtered
}

// ---- heartbeat ----

func (s *Server) AgentHeartbeat(ctx context.Context, req gen.AgentHeartbeatRequestObject) (gen.AgentHeartbeatResponseObject, error) {
	node := nodeFrom(ctx)
	if node == nil {
		return nil, errResp(gen.Unauthorized, "no node", nil)
	}
	b := req.Body
	now := s.Store.NowMs()

	// update status + last seen
	caps := b.Capabilities
	statusJSON := "{}"
	if caps != nil {
		statusJSON = mustJSON(caps)
	}
	meshJSON := "{}"
	if b.Mesh != nil {
		meshJSON = mustJSON(b.Mesh)
	}
	if b.AgentVersion != nil && *b.AgentVersion != "" {
		node.AgentVersion = *b.AgentVersion
		_, _ = s.Store.DB.ExecContext(ctx, s.Store.Rebind(
			`UPDATE nodes SET agent_version=? WHERE id=?`), node.AgentVersion, node.ID)
	}
	if err := s.Store.PutNodeStatus(ctx, &store.NodeStatus{
		NodeID: node.ID, StatusJSON: statusJSON, MeshJSON: meshJSON, UpdatedAt: now,
	}); err != nil {
		return nil, err
	}
	wasOffline := s.Recon.Liveness(node) != "online"
	if err := s.Store.TouchNodeSeen(ctx, node.ID, now); err != nil {
		return nil, err
	}
	if wasOffline {
		_ = s.Store.EmitEvent(ctx, s.Store.DB, node.GroupID, nil, &node.ID, "node.online",
			map[string]any{"name": node.Name})
	}

	if b.DeletedSnapshots != nil {
		for _, snapshotID := range *b.DeletedSnapshots {
			if err := s.acknowledgeDeletedSnapshot(ctx, node.ID, snapshotID); err != nil {
				return nil, err
			}
		}
	}

	// process reported executions + implicit lease renewal
	var execReports []gen.ExecutionReport
	if b.Executions != nil {
		execReports = *b.Executions
	}
	for _, er := range execReports {
		if err := s.processExecReport(ctx, node, &er, now); err != nil {
			s.Log.Warn("exec report rejected", "node", node.ID, "exec", er.ExecutionId, "error", err)
		}
	}

	// build directives
	resp, err := s.buildDirectives(ctx, node, now)
	if err != nil {
		return nil, err
	}
	s.Recon.Wake()
	return gen.AgentHeartbeat200JSONResponse(resp), nil
}

func (s *Server) acknowledgeDeletedSnapshot(ctx context.Context, nodeID, snapshotID string) error {
	return s.Store.Tx(ctx, func(tx *sqlx.Tx) error {
		var state string
		err := tx.GetContext(ctx, &state, s.Store.Rebind(
			`SELECT state FROM snapshot_replicas WHERE snapshot_id=? AND node_id=?`),
			snapshotID, nodeID)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if state != "deleting" {
			return nil
		}
		if err := s.Store.DeleteReplica(ctx, tx, snapshotID, nodeID); err != nil {
			return err
		}
		var replicas int
		if err := tx.GetContext(ctx, &replicas, s.Store.Rebind(
			`SELECT COUNT(*) FROM snapshot_replicas WHERE snapshot_id=?`), snapshotID); err != nil {
			return err
		}
		if replicas == 0 {
			_, err := tx.ExecContext(ctx, s.Store.Rebind(
				`DELETE FROM snapshots WHERE id=?`), snapshotID)
			return err
		}
		return nil
	})
}

// processExecReport handles one ExecutionReport inside a heartbeat: validates
// (server_id, epoch, node_id) against the active execution, renews the lease,
// records state transitions.
func (s *Server) processExecReport(ctx context.Context, node *store.Node, er *gen.ExecutionReport, now int64) error {
	exec, err := s.Store.GetExecution(ctx, er.ExecutionId)
	if err != nil {
		return fmt.Errorf("unknown execution")
	}
	active, err := s.Store.ActiveExecution(ctx, er.ServerId)
	if err != nil || active.ID != exec.ID || exec.NodeID != node.ID ||
		exec.Epoch != er.Epoch || er.Epoch != active.Epoch {
		return fmt.Errorf("stale_epoch") // logged; not returned to agent as error
	}
	// An expired lease is terminal: never resurrect it. The reconciler reaps
	// it as 'lost' on the next pass; until then reports change nothing.
	if exec.LeaseExpiresAt <= now {
		return fmt.Errorf("stale: lease expired")
	}

	previousState := exec.State
	state := string(er.State)
	exec.State = state
	if er.Health != nil {
		exec.Health = er.Health
	}
	if er.Message != nil {
		exec.Message = er.Message
	}
	leaseExp := now + s.Cfg.Timings.LeaseTTLMs

	return s.Store.Tx(ctx, func(tx *sqlx.Tx) error {
		if er.JoinCode != nil {
			changed, err := s.Store.SetExecutionJoinCode(ctx, tx, exec.ID, *er.JoinCode, now)
			if err != nil {
				return err
			}
			if changed && *er.JoinCode != "" {
				if err := s.Store.EmitEvent(ctx, tx, node.GroupID, &exec.ServerID, &node.ID,
					"server.join_code", map[string]any{"join_code": *er.JoinCode}); err != nil {
					return err
				}
			}
		}
		switch state {
		case "running":
			if exec.StartedAt == nil {
				exec.StartedAt = &now
			}
			exec.LeaseExpiresAt = leaseExp
			if err := s.Store.UpdateExecutionState(ctx, tx, exec); err != nil {
				return err
			}
			return s.Store.EmitEvent(ctx, tx, node.GroupID, &exec.ServerID, &node.ID,
				"execution.state", map[string]any{"execution_id": exec.ID, "state": "running"})
		case "stopped", "failed":
			exec.State = state
			if err := s.Store.UpdateExecutionState(ctx, tx, exec); err != nil {
				return err
			}
			reason := "stopped"
			typ := "server.stopped"
			data := map[string]any{"execution_id": exec.ID, "epoch": exec.Epoch}
			if state == "failed" {
				reason = "failed"
				typ = "server.failed"
				if previousState == "restoring" {
					reason = "restore_failed"
					typ = "server.restore_failed"
					message := ""
					if exec.Message != nil {
						message = *exec.Message
					}
					data["message"] = message
				}
			}
			if err := s.Store.EndExecution(ctx, tx, exec.ID, reason, now); err != nil {
				return err
			}
			return s.Store.EmitEvent(ctx, tx, node.GroupID, &exec.ServerID, &node.ID,
				typ, data)
		default:
			// preparing/restoring/starting/stopping: renew lease, keep state
			exec.LeaseExpiresAt = leaseExp
			return s.Store.UpdateExecutionState(ctx, tx, exec)
		}
	})
}

// buildDirectives composes the full desired-state response for the node.
func (s *Server) buildDirectives(ctx context.Context, node *store.Node, now int64) (gen.AgentDirectives, error) {
	d := gen.AgentDirectives{
		ServerTimeUnixMs:    now,
		HeartbeatIntervalMs: int(s.Cfg.Timings.HeartbeatIntervalMs),
		LeaseTtlMs:          int(s.Cfg.Timings.LeaseTTLMs),
		Executions:          &[]gen.ExecutionDirective{},
		Peers:               &[]gen.DirectivePeer{},
		Routes:              &[]gen.DirectiveRoute{},
		Relays:              &[]gen.DirectiveRelay{},
		ReplicationTasks:    &[]gen.ReplicationTask{},
		DeleteSnapshots:     &[]string{},
	}
	d.Node.Name = node.Name
	d.Node.HostingEnabled = node.HostingEnabled == 1
	d.Node.Anchor = node.Anchor == 1
	d.Node.AdminState = gen.AgentDirectivesNodeAdminState(node.AdminState)
	if g, err := s.Store.GetGroup(ctx, node.GroupID); err == nil {
		d.Node.GroupName = &g.Name
	}

	// executions held by this node
	execs, err := s.Store.ActiveExecutionsForNode(ctx, node.ID)
	if err != nil {
		return d, err
	}
	out := []gen.ExecutionDirective{}
	for i := range execs {
		e := &execs[i]
		srv, err := s.Store.GetServer(ctx, e.ServerID)
		if err != nil {
			continue
		}
		ed := gen.ExecutionDirective{
			ExecutionId:          e.ID,
			ServerId:             e.ServerID,
			ServerName:           srv.Name,
			Epoch:                e.Epoch,
			LeaseExpiresAtUnixMs: e.LeaseExpiresAt,
			Action:               gen.ExecutionDirectiveAction(e.Action),
			SnapshotIntervalS:    ptr(int(srv.SnapshotIntervalS)),
		}
		if at, err := s.Store.LatestCommittedSnapshotAt(ctx, srv.ID); err == nil {
			ed.LatestSafeSaveAtUnixMs = at
		}
		if e.StopReason != nil {
			ed.StopReason = ptr(gen.ExecutionDirectiveStopReason(*e.StopReason))
		}
		var cfg map[string]any
		_ = json.Unmarshal([]byte(srv.ConfigJSON), &cfg)
		ed.Config = &cfg
		if srv.DeploymentID != nil {
			if dep, err := s.getDeployment(ctx, *srv.DeploymentID); err == nil {
				var spec map[string]any
				_ = json.Unmarshal([]byte(dep.SpecJSON), &spec)
				ed.Deployment = &struct {
					GameId string                 `json:"game_id"`
					Id     string                 `json:"id"`
					Spec   map[string]interface{} `json:"spec"`
				}{GameId: dep.GameID, Id: dep.ID, Spec: spec}
			}
		}
		if svc, err := s.Store.ServiceByServer(ctx, srv.ID); err == nil {
			var ports []gen.GamePort
			_ = json.Unmarshal([]byte(svc.PortsJSON), &ports)
			ed.Service = &gen.Service{ServiceId: svc.ID, LoopbackIp: svc.LoopbackIP, Ports: ports}
		}
		if e.RestoreSnapshotID != nil {
			snap, err := s.Store.GetSnapshot(ctx, *e.RestoreSnapshotID)
			if err == nil {
				sources, _ := s.readyOnlineSources(ctx, snap)
				ed.Restore = &struct {
					ManifestDigest string   `json:"manifest_digest"`
					SnapshotId     string   `json:"snapshot_id"`
					SourceNodeIds  []string `json:"source_node_ids"`
				}{
					ManifestDigest: snap.ManifestDigest,
					SnapshotId:     snap.ID,
					SourceNodeIds:  sources,
				}
			}
		}
		// pending snapshot requests
		reqs, _ := s.Store.PendingSnapshotRequests(ctx, srv.ID)
		if len(reqs) > 0 {
			sr := []gen.SnapshotRequest{}
			for _, rq := range reqs {
				sr = append(sr, gen.SnapshotRequest{
					RequestId: rq.ID, Reason: gen.SnapshotRequestReason(rq.Reason),
				})
				_ = s.Store.ClaimSnapshotRequest(ctx, rq.ID)
			}
			ed.SnapshotRequests = &sr
		}
		out = append(out, ed)
	}
	d.Executions = &out

	// peers: other nodes in the group
	nodes, err := s.Store.ListNodes(ctx, node.GroupID)
	if err != nil {
		return d, err
	}
	peers := []gen.DirectivePeer{}
	for i := range nodes {
		n := &nodes[i]
		if n.ID == node.ID {
			continue
		}
		p := gen.DirectivePeer{
			NodeId: n.ID, Name: n.Name, PublicKey: n.PublicKey,
			Endpoints: &[]string{}, RelayIds: &[]string{},
		}
		if st, err := s.Store.GetNodeStatus(ctx, n.ID); err == nil {
			var mesh struct {
				LocalEndpoints    []string `json:"local_endpoints"`
				ObservedEndpoints []string `json:"observed_endpoints"`
				RelayIds          []string `json:"relay_ids"`
			}
			if json.Unmarshal([]byte(st.MeshJSON), &mesh) == nil {
				eps := filterDialEndpoints(append(mesh.LocalEndpoints, mesh.ObservedEndpoints...))
				p.Endpoints = &eps
				p.RelayIds = &mesh.RelayIds
			}
		}
		peers = append(peers, p)
	}
	d.Peers = &peers

	// routes: all services in the group
	svcs, err := s.Store.ServicesInGroup(ctx, node.GroupID)
	if err != nil {
		return d, err
	}
	routes := []gen.DirectiveRoute{}
	for _, svc := range svcs {
		srv, err := s.Store.GetServer(ctx, svc.ServerID)
		if err != nil {
			continue
		}
		var config map[string]any
		if json.Unmarshal([]byte(srv.ConfigJSON), &config) == nil &&
			crossplayEnabled(srv.GameID, config) {
			continue
		}
		var ports []gen.GamePort
		_ = json.Unmarshal([]byte(svc.PortsJSON), &ports)
		rt := gen.DirectiveRoute{
			ServiceId: svc.ID, ServerId: srv.ID, ServerName: srv.Name,
			LoopbackIp: svc.LoopbackIP, Ports: ports, Epoch: ptr(srv.Epoch),
		}
		if e, err := s.Store.ActiveExecution(ctx, srv.ID); err == nil {
			rt.HostNodeId = ptr(e.NodeID)
			rt.Epoch = ptr(e.Epoch)
		}
		routes = append(routes, rt)
	}
	d.Routes = &routes

	// relays: configured relays get fresh signed tokens for this node
	relays := []gen.DirectiveRelay{}
	for _, rc := range s.Cfg.Relays {
		tok, err := relaytoken.Sign(s.RelayKey, relaytoken.Claims{
			NodeID: node.ID, GroupID: node.GroupID, RelayID: rc.ID,
			PublicKey: node.PublicKey, ExpUnixMs: now + 3600_000,
		})
		if err != nil {
			return d, err
		}
		relays = append(relays, gen.DirectiveRelay{RelayId: rc.ID, Addr: rc.Addr, Token: tok})
	}
	d.Relays = &relays

	// replication tasks for this node
	tasks := []gen.ReplicationTask{}
	assigned, err := s.Store.AssignedReplicasForNode(ctx, node.ID)
	if err != nil {
		return d, err
	}
	for _, r := range assigned {
		snap, err := s.Store.GetSnapshot(ctx, r.SnapshotID)
		if err != nil {
			continue
		}
		sources, _ := s.readyOnlineSources(ctx, snap)
		tasks = append(tasks, gen.ReplicationTask{
			SnapshotId: snap.ID, ServerId: snap.ServerID,
			ManifestDigest: snap.ManifestDigest, SourceNodeIds: sources,
		})
	}
	d.ReplicationTasks = &tasks

	// explicit deletions: replicas marked 'deleting' for this node
	del, err := s.Store.DeletingReplicasForNode(ctx, node.ID)
	if err != nil {
		return d, err
	}
	delIDs := []string{}
	for _, r := range del {
		delIDs = append(delIDs, r.SnapshotID)
	}
	d.DeleteSnapshots = &delIDs

	return d, nil
}

func (s *Server) readyOnlineSources(ctx context.Context, snap *store.Snapshot) ([]string, error) {
	reps, err := s.Store.ReplicasForSnapshot(ctx, snap.ID)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, r := range reps {
		if r.State != "ready" {
			continue
		}
		n, err := s.Store.GetNode(ctx, r.NodeID)
		if err != nil {
			continue
		}
		if s.Recon.Liveness(n) == "online" {
			out = append(out, r.NodeID)
		}
	}
	return out, nil
}

func (s *Server) getDeployment(ctx context.Context, id string) (*store.Deployment, error) {
	var d store.Deployment
	err := s.Store.DB.GetContext(ctx, &d, s.Store.Rebind(`SELECT * FROM deployments WHERE id=?`), id)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ---- immediate status / logs / snapshots / replicas ----

func (s *Server) AgentExecutionStatus(ctx context.Context, req gen.AgentExecutionStatusRequestObject) (gen.AgentExecutionStatusResponseObject, error) {
	node := nodeFrom(ctx)
	b := req.Body
	if b == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	exec, err := s.Store.GetExecution(ctx, req.ExecutionId)
	if err != nil {
		return nil, errResp(gen.NotFound, "execution not found", nil)
	}
	active, aerr := s.Store.ActiveExecution(ctx, b.ServerId)
	if aerr != nil || active.ID != exec.ID || exec.NodeID != node.ID || exec.Epoch != b.Epoch {
		return nil, errResp(gen.StaleEpoch, "stale execution", map[string]any{
			"server_id": b.ServerId, "epoch": b.Epoch,
		})
	}
	now := s.Store.NowMs()
	if err := s.processExecReport(ctx, node, &gen.ExecutionReport{
		ExecutionId: exec.ID, ServerId: b.ServerId, Epoch: b.Epoch,
		State: b.State, Health: b.Health, Message: b.Message, JoinCode: b.JoinCode,
	}, now); err != nil {
		return nil, errResp(gen.StaleEpoch, "stale execution", nil)
	}
	s.Recon.Wake()
	return gen.AgentExecutionStatus204Response{}, nil
}

func (s *Server) AgentExecutionLogs(ctx context.Context, req gen.AgentExecutionLogsRequestObject) (gen.AgentExecutionLogsResponseObject, error) {
	node := nodeFrom(ctx)
	exec, err := s.Store.GetExecution(ctx, req.ExecutionId)
	if err != nil {
		return nil, errResp(gen.NotFound, "execution not found", nil)
	}
	if exec.NodeID != node.ID {
		return nil, errResp(gen.Forbidden, "not your execution", nil)
	}
	if req.Body == nil {
		return gen.AgentExecutionLogs204Response{}, nil
	}
	var rows []store.LogLineRow
	for _, l := range req.Body.Lines {
		rows = append(rows, store.LogLineRow{
			ExecutionID: exec.ID, At: l.At, Stream: string(l.Stream), Line: l.Line,
		})
	}
	if len(rows) > 0 {
		if err := s.Store.AppendLogLines(ctx, exec.ID, rows); err != nil {
			return nil, err
		}
	}
	return gen.AgentExecutionLogs204Response{}, nil
}

func (s *Server) AgentCreateSnapshot(ctx context.Context, req gen.AgentCreateSnapshotRequestObject) (gen.AgentCreateSnapshotResponseObject, error) {
	node := nodeFrom(ctx)
	b := req.Body
	if b == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	exec, err := s.Store.GetExecution(ctx, b.ExecutionId)
	if err != nil {
		return nil, errResp(gen.NotFound, "execution not found", nil)
	}
	// The snapshot is accepted only if the execution is the server's active
	// one, held by the caller, epoch matches, and the lease is still valid.
	// Stopping executions keep their lease alive via heartbeats.
	active, aerr := s.Store.ActiveExecution(ctx, b.ServerId)
	now := s.Store.NowMs()
	valid := aerr == nil && active.ID == exec.ID && exec.NodeID == node.ID &&
		exec.Epoch == b.Epoch && exec.LeaseExpiresAt > now
	if !valid {
		return nil, errResp(gen.StaleEpoch, "stale epoch or expired lease", map[string]any{
			"server_id": b.ServerId, "epoch": b.Epoch,
		})
	}

	snap := &store.Snapshot{
		ID: b.SnapshotId, ServerID: b.ServerId, GroupID: node.GroupID,
		Epoch: b.Epoch, ExecutionID: exec.ID, NodeID: node.ID,
		ParentID: b.ParentId, ManifestDigest: b.ManifestDigest,
		DeploymentID: b.DeploymentId, Reason: string(b.Reason), State: "local",
		SizeBytes: derefInt64(b.SizeBytes), StoredBytes: derefInt64(b.StoredBytes),
		FileCount: int64(derefInt(b.FileCount)), ChunkCount: int64(derefInt(b.ChunkCount)),
		CreatedAt: now,
	}
	if err := s.Store.Tx(ctx, func(tx *sqlx.Tx) error {
		if err := s.Store.CreateSnapshotTx(ctx, tx, snap); err != nil {
			return err
		}
		// producer's own copy counts as a ready replica
		return s.Store.UpsertReplica(ctx, tx, &store.Replica{
			SnapshotID: snap.ID, NodeID: node.ID, State: "ready",
			AssignedAt: now, ReadyAt: &now,
		})
	}); err != nil {
		if store.IsUniqueViolation(err) {
			return nil, errResp(gen.Conflict, "snapshot already exists", nil)
		}
		return nil, err
	}
	if b.RequestId != nil && *b.RequestId != "" {
		_ = s.Store.ClaimSnapshotRequest(ctx, *b.RequestId)
	}

	// assign replication targets
	srv, err := s.Store.GetServer(ctx, b.ServerId)
	if err != nil {
		return nil, err
	}
	targets, err := s.assignReplication(ctx, srv, snap)
	if err != nil {
		return nil, err
	}
	_ = s.Store.EmitEvent(ctx, s.Store.DB, snap.GroupID, &snap.ServerID, &node.ID,
		"snapshot.created", map[string]any{
			"snapshot_id": snap.ID, "reason": snap.Reason, "targets": targets,
		})
	if err := s.evaluateCommit(ctx, srv, snap); err != nil {
		return nil, err
	}
	s.Recon.Wake()
	v, err := s.snapshotView(ctx, snap)
	if err != nil {
		return nil, err
	}
	return gen.AgentCreateSnapshot201JSONResponse(v), nil
}

// assignReplication picks targets per storage.md and inserts 'assigned' rows.
func (s *Server) assignReplication(ctx context.Context, srv *store.Server, snap *store.Snapshot) ([]string, error) {
	nodes, err := s.Store.ListNodes(ctx, srv.GroupID)
	if err != nil {
		return nil, err
	}
	var parentNode = ""
	if snap.ParentID != nil {
		parentNode = "" // HasParent checked below per node
	}
	var cands []scheduler.ReplCandidate
	for i := range nodes {
		n := &nodes[i]
		hasParent := false
		if snap.ParentID != nil {
			reps, err := s.Store.ReplicasForSnapshot(ctx, *snap.ParentID)
			if err == nil {
				for _, r := range reps {
					if r.NodeID == n.ID && r.State == "ready" {
						hasParent = true
					}
				}
			}
		}
		var diskFree int64
		if st, err := s.Store.GetNodeStatus(ctx, n.ID); err == nil {
			var c struct {
				DiskFreeBytes int64 `json:"disk_free_bytes"`
			}
			_ = json.Unmarshal([]byte(st.StatusJSON), &c)
			diskFree = c.DiskFreeBytes
		}
		cands = append(cands, scheduler.ReplCandidate{
			NodeID: n.ID, Online: s.Recon.Liveness(n) == "online",
			Anchor: n.Anchor == 1, HostingEnabled: n.HostingEnabled == 1,
			HasParent: hasParent, DiskFreeBytes: diskFree,
		})
	}
	_ = parentNode
	targets := scheduler.SelectReplicationTargets(snap.NodeID, int(srv.ReplicationFactor), cands)
	now := s.Store.NowMs()
	for _, t := range targets {
		if err := s.Store.UpsertReplica(ctx, s.Store.DB, &store.Replica{
			SnapshotID: snap.ID, NodeID: t, State: "assigned", AssignedAt: now,
		}); err != nil {
			return nil, err
		}
	}
	if len(targets) > 0 {
		if err := s.Store.SetSnapshotState(ctx, s.Store.DB, snap.ID, "replicating", nil); err != nil {
			return nil, err
		}
		snap.State = "replicating"
	}
	return targets, nil
}

// evaluateCommit applies the commit policy and supersession.
func (s *Server) evaluateCommit(ctx context.Context, srv *store.Server, snap *store.Snapshot) error {
	if snap.State == "committed" {
		return nil
	}
	grp, err := s.Store.GetGroup(ctx, srv.GroupID)
	if err != nil {
		return err
	}
	settings := grp.Settings()
	minRep := srv.MinCommitReplicas
	if minRep <= 0 {
		minRep = settings.DefaultMinCommitReplicas
	}
	// single-node clamp: commit within the group's enrolled node count
	nNodes, err := s.Store.NodeCount(ctx, srv.GroupID)
	if err != nil {
		return err
	}
	if nNodes > 0 && minRep > nNodes {
		minRep = nNodes
	}
	reps, err := s.Store.ReplicasForSnapshot(ctx, snap.ID)
	if err != nil {
		return err
	}
	ready := 0
	anchorReady := 0
	hasAnchors := false
	nodes, err := s.Store.ListNodes(ctx, srv.GroupID)
	if err != nil {
		return err
	}
	isAnchor := map[string]bool{}
	for _, n := range nodes {
		isAnchor[n.ID] = n.Anchor == 1
		if n.Anchor == 1 {
			hasAnchors = true
		}
	}
	for _, r := range reps {
		if r.State == "ready" {
			ready++
			if isAnchor[r.NodeID] {
				anchorReady++
			}
		}
	}
	if int64(ready) < minRep {
		return nil
	}
	if settings.RequireAnchorForCommit && hasAnchors && anchorReady == 0 {
		return nil
	}
	// supersede candidates read before the txn (single-conn pool: no nested DB use)
	snaps, err := s.Store.SnapshotsForServer(ctx, srv.ID)
	if err != nil {
		return err
	}
	now := s.Store.NowMs()
	return s.Store.Tx(ctx, func(tx *sqlx.Tx) error {
		if err := s.Store.SetSnapshotState(ctx, tx, snap.ID, "committed", &now); err != nil {
			return err
		}
		if err := s.Store.EmitEvent(ctx, tx, snap.GroupID, &snap.ServerID, &snap.NodeID,
			"snapshot.committed", map[string]any{"snapshot_id": snap.ID}); err != nil {
			return err
		}
		for i := range snaps {
			o := &snaps[i]
			if o.ID == snap.ID || o.State != "committed" || o.CreatedAt >= snap.CreatedAt {
				continue
			}
			if err := s.Store.SetSnapshotState(ctx, tx, o.ID, "superseded", nil); err != nil {
				return err
			}
			if err := s.Store.EmitEvent(ctx, tx, snap.GroupID, &srv.ID, nil,
				"snapshot.superseded", map[string]any{"snapshot_id": o.ID}); err != nil {
				return err
			}
		}
		return nil
	})
}

// retention returns snapshot ids to delete on the node holding them:
// beyond the newest N committed, not pinned, not newer-than-newest-committed.
func (s *Server) ApplyRetention(ctx context.Context, srv *store.Server) error {
	grp, err := s.Store.GetGroup(ctx, srv.GroupID)
	if err != nil {
		return err
	}
	keep := grp.Settings().SnapshotRetention
	snaps, err := s.Store.SnapshotsForServer(ctx, srv.ID)
	if err != nil {
		return err
	}
	// newest committed watermark
	var newestCommitted int64
	for _, sn := range snaps {
		if sn.State == "committed" && sn.CreatedAt > newestCommitted {
			newestCommitted = sn.CreatedAt
		}
	}
	var committed []store.Snapshot
	for _, sn := range snaps {
		if sn.State == "committed" || sn.State == "superseded" {
			committed = append(committed, sn)
		}
	}
	// sort committed desc by created_at; keep first `keep` committed
	for i, j := 0, len(committed)-1; i < j; i, j = i+1, j-1 {
		committed[i], committed[j] = committed[j], committed[i]
	}
	kept := 0
	for _, sn := range committed {
		del := false
		if sn.State == "committed" {
			if int64(kept) >= keep && sn.Pinned != 1 {
				del = true
			}
			kept++
		} else if sn.State == "superseded" && sn.Pinned != 1 {
			// superseded count against retention slots after the kept committed
			if int64(kept) >= keep {
				del = true
			}
			kept++
		}
		if !del {
			continue
		}
		reps, err := s.Store.ReplicasForSnapshot(ctx, sn.ID)
		if err != nil {
			return err
		}
		for _, r := range reps {
			if r.State != "deleting" {
				r.State = "deleting"
				if err := s.Store.UpsertReplica(ctx, s.Store.DB, &r); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Server) AgentReplicaReady(ctx context.Context, req gen.AgentReplicaReadyRequestObject) (gen.AgentReplicaReadyResponseObject, error) {
	node := nodeFrom(ctx)
	snap, err := s.Store.GetSnapshot(ctx, req.SnapshotId)
	if err != nil {
		return nil, errResp(gen.NotFound, "snapshot not found", nil)
	}
	// a node may only mark replicas it was actually assigned (or that it
	// produced as the snapshot's source)
	if snap.GroupID != node.GroupID {
		return nil, errResp(gen.Conflict, "replica not assigned", nil)
	}
	assigned := false
	if reps, err := s.Store.ReplicasForSnapshot(ctx, snap.ID); err == nil {
		for _, r := range reps {
			if r.NodeID == node.ID && r.State != "deleting" {
				assigned = true
			}
		}
	}
	if !assigned {
		return nil, errResp(gen.Conflict, "replica not assigned", nil)
	}
	now := s.Store.NowMs()
	if err := s.Store.UpsertReplica(ctx, s.Store.DB, &store.Replica{
		SnapshotID: snap.ID, NodeID: node.ID, State: "ready",
		AssignedAt: now, ReadyAt: &now,
	}); err != nil {
		return nil, err
	}
	_ = s.Store.EmitEvent(ctx, s.Store.DB, snap.GroupID, &snap.ServerID, &node.ID,
		"snapshot.replica_ready", map[string]any{"snapshot_id": snap.ID})
	srv, err := s.Store.GetServer(ctx, snap.ServerID)
	if err != nil {
		return nil, err
	}
	if err := s.evaluateCommit(ctx, srv, snap); err != nil {
		return nil, err
	}
	if err := s.ApplyRetention(ctx, srv); err != nil {
		return nil, err
	}
	s.Recon.Wake()
	return gen.AgentReplicaReady204Response{}, nil
}

var _ = base64.StdEncoding
var _ = ids.Must
