package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/grendalaget/varde/go/ids"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/catalog"
	"github.com/grendalaget/varde/apps/controlplane/internal/services"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func (s *Server) ListGames(ctx context.Context, _ gen.ListGamesRequestObject) (gen.ListGamesResponseObject, error) {
	if _, err := s.requireUser(ctx); err != nil {
		return nil, err
	}
	out := []gen.Game{}
	for _, g := range catalog.All() {
		out = append(out, genGame(&g))
	}
	return gen.ListGames200JSONResponse{Games: out}, nil
}

func genGame(g *catalog.Game) gen.Game {
	ports := []gen.GamePort{}
	for _, p := range g.Ports {
		ports = append(ports, gen.GamePort{Port: p.Port, Protocol: gen.Protocol(p.Protocol)})
	}
	fields := []gen.GameConfigField{}
	for _, f := range g.ConfigFields {
		fields = append(fields, gen.GameConfigField{
			Name: f.Name, Type: gen.GameConfigFieldType(f.Type),
			Default: &f.Default, Required: &f.Required, Secret: &f.Secret,
			Options: &f.Options, Help: &f.Help,
		})
	}
	oss := []gen.GameOs{}
	for _, o := range g.OS {
		oss = append(oss, gen.GameOs(o))
	}
	return gen.Game{
		Id: g.ID, Name: g.Name, Ports: ports, Os: oss, Arch: g.Arch,
		MinMemoryMb: int(g.MinMemoryMB), Runtimes: &g.Runtimes, ConfigFields: fields,
	}
}

// serverView builds the API representation including computed summary.
func (s *Server) serverView(ctx context.Context, srv *store.Server) (gen.Server, error) {
	var cfg map[string]any
	_ = json.Unmarshal([]byte(srv.ConfigJSON), &cfg)
	v := gen.Server{
		Id:                srv.ID,
		GroupId:           srv.GroupID,
		Name:              srv.Name,
		GameId:            srv.GameID,
		DeploymentId:      srv.DeploymentID,
		DesiredState:      gen.DesiredState(srv.DesiredState),
		ObservedState:     gen.ObservedState(srv.ObservedState),
		Epoch:             srv.Epoch,
		ReplicationFactor: int(srv.ReplicationFactor),
		MinCommitReplicas: int(srv.MinCommitReplicas),
		SnapshotIntervalS: int(srv.SnapshotIntervalS),
		PreferredNodeId:   srv.PreferredNodeID,
		Config:            cfg,
		CreatedAt:         srv.CreatedAt,
		UpdatedAt:         srv.UpdatedAt,
	}

	// service
	if svc, err := s.Store.ServiceByServer(ctx, srv.ID); err == nil {
		var ports []gen.GamePort
		_ = json.Unmarshal([]byte(svc.PortsJSON), &ports)
		v.Service = &gen.Service{ServiceId: svc.ID, LoopbackIp: svc.LoopbackIP, Ports: ports}
		if len(ports) > 0 {
			v.Summary.Address = ptr(fmt.Sprintf("%s:%d", svc.LoopbackIP, ports[0].Port))
		}
	}

	// summary: hosting_on
	if e, err := s.Store.ActiveExecution(ctx, srv.ID); err == nil && e.State == "running" {
		if n, err := s.Store.GetNode(ctx, e.NodeID); err == nil {
			v.Summary.HostingOn = &struct {
				Name   string `json:"name"`
				NodeId string `json:"node_id"`
			}{Name: n.Name, NodeId: n.ID}
		}
	}

	// summary: latest safe save + latest save + one-machine flag
	snaps, _ := s.Store.SnapshotsForServer(ctx, srv.ID)
	var latestSafe, latest *store.Snapshot
	for i := range snaps {
		sn := &snaps[i]
		if sn.State == "invalid" {
			continue
		}
		if latest == nil || sn.CreatedAt > latest.CreatedAt {
			latest = sn
		}
		if sn.State == "committed" && (latestSafe == nil || sn.CreatedAt > latestSafe.CreatedAt) {
			latestSafe = sn
		}
	}
	if latest != nil {
		v.Summary.LatestSave = &struct {
			CreatedAt  int64  `json:"created_at"`
			SnapshotId string `json:"snapshot_id"`
		}{CreatedAt: latest.CreatedAt, SnapshotId: latest.ID}
	}
	if latestSafe != nil {
		reps, _ := s.Store.ReplicasForSnapshot(ctx, latestSafe.ID)
		repList := []struct {
			Anchor bool   `json:"anchor"`
			Name   string `json:"name"`
			NodeId string `json:"node_id"`
		}{}
		ready := 0
		for _, r := range reps {
			if r.State != "ready" {
				continue
			}
			ready++
			n, _ := s.Store.GetNode(ctx, r.NodeID)
			name, anchor := r.NodeID, false
			if n != nil {
				name = n.Name
				anchor = n.Anchor == 1
			}
			repList = append(repList, struct {
				Anchor bool   `json:"anchor"`
				Name   string `json:"name"`
				NodeId string `json:"node_id"`
			}{Anchor: anchor, Name: name, NodeId: r.NodeID})
		}
		v.Summary.LatestSafeSave = &struct {
			CreatedAt int64 `json:"created_at"`
			Replicas  *[]struct {
				Anchor bool   `json:"anchor"`
				Name   string `json:"name"`
				NodeId string `json:"node_id"`
			} `json:"replicas,omitempty"`
			SnapshotId string `json:"snapshot_id"`
		}{CreatedAt: latestSafe.CreatedAt, SnapshotId: latestSafe.ID, Replicas: &repList}
		v.Summary.OnlyOnOneMachine = ptr(ready <= 1)
	} else if latest != nil {
		reps, _ := s.Store.ReplicasForSnapshot(ctx, latest.ID)
		ready := 0
		for _, r := range reps {
			if r.State == "ready" {
				ready++
			}
		}
		v.Summary.OnlyOnOneMachine = ptr(ready <= 1)
	}
	return v, nil
}

func (s *Server) ListServers(ctx context.Context, req gen.ListServersRequestObject) (gen.ListServersResponseObject, error) {
	if _, _, err := s.requireRole(ctx, req.GroupId, "member"); err != nil {
		return nil, err
	}
	ss, err := s.Store.ListServers(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	out := []gen.Server{}
	for i := range ss {
		v, err := s.serverView(ctx, &ss[i])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return gen.ListServers200JSONResponse{Servers: out}, nil
}

func (s *Server) CreateServer(ctx context.Context, req gen.CreateServerRequestObject) (gen.CreateServerResponseObject, error) {
	u, _, err := s.requireRole(ctx, req.GroupId, "admin")
	if err != nil {
		return nil, err
	}
	b := req.Body
	if b == nil || b.Name == "" || b.GameId == "" {
		return nil, errResp(gen.Validation, "name and game_id required", nil)
	}
	game := catalog.Get(b.GameId)
	if game == nil {
		return nil, errResp(gen.Validation, "unknown game_id", map[string]any{"game_id": b.GameId})
	}
	cfg := map[string]any{}
	if b.Config != nil {
		cfg = *b.Config
	}
	if verr := game.ValidateConfig(cfg); len(verr) > 0 {
		return nil, errResp(gen.Validation, "invalid config: "+strings.Join(verr, "; "), map[string]any{"fields": verr})
	}
	lim, err := s.Store.GetLimits(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	if lim.MaxServers != nil {
		n, _ := s.Store.ServerCount(ctx, req.GroupId)
		if n >= *lim.MaxServers {
			return nil, errResp(gen.LimitExceeded, "group server limit reached", nil)
		}
	}

	grp, err := s.Store.GetGroup(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	sets := grp.Settings()
	now := s.Store.NowMs()
	srv := &store.Server{
		ID:                ids.Must(ids.Server),
		GroupID:           req.GroupId,
		Name:              b.Name,
		GameID:            b.GameId,
		DesiredState:      "stopped",
		ObservedState:     "stopped",
		ReplicationFactor: sets.DefaultReplicationFactor,
		MinCommitReplicas: sets.DefaultMinCommitReplicas,
		SnapshotIntervalS: sets.SnapshotIntervalS,
		ConfigJSON:        mustJSON(cfg),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if b.ReplicationFactor != nil {
		srv.ReplicationFactor = int64(*b.ReplicationFactor)
	}
	if b.MinCommitReplicas != nil {
		srv.MinCommitReplicas = int64(*b.MinCommitReplicas)
	}
	if b.SnapshotIntervalS != nil {
		srv.SnapshotIntervalS = int64(*b.SnapshotIntervalS)
	}
	if b.PreferredNodeId != nil {
		srv.PreferredNodeID = b.PreferredNodeId
	}

	// deployment: one per (group, game) for now — drivers evolve later
	depID := ids.Must("dep_")
	dep := &store.Deployment{
		ID: depID, GroupID: req.GroupId, GameID: b.GameId,
		SpecJSON: mustJSON(map[string]any{"game_id": b.GameId, "config": cfg}),
		Digest:   "", CreatedAt: now,
	}
	_ = dep // stored below via CreateDeployment
	if err := s.createDeploymentIfMissing(ctx, dep); err != nil {
		return nil, err
	}
	srv.DeploymentID = &depID

	svcID := ids.Must(ids.Service)
	ports := []gen.GamePort{}
	for _, p := range game.Ports {
		ports = append(ports, gen.GamePort{Port: p.Port, Protocol: gen.Protocol(p.Protocol)})
	}
	existing, err := s.Store.ServicesInGroup(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, e := range existing {
		used[e.LoopbackIP] = true
	}
	ip, err := services.Allocate(svcID, func(ip string) bool { return used[ip] })
	if err != nil {
		return nil, err
	}
	svc := &store.Service{
		ID: svcID, ServerID: srv.ID, GroupID: req.GroupId,
		LoopbackIP: ip, PortsJSON: mustJSON(ports),
	}
	if err := s.Store.CreateServer(ctx, srv, svc); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &req.GroupId, "user", u.ID, "server.create", srv.ID, map[string]any{"name": srv.Name, "game": srv.GameID})
	_ = s.Store.EmitEvent(ctx, s.Store.DB, req.GroupId, &srv.ID, nil, "server.created", map[string]any{"name": srv.Name, "game": srv.GameID})
	v, err := s.serverView(ctx, srv)
	if err != nil {
		return nil, err
	}
	return gen.CreateServer201JSONResponse(v), nil
}

func (s *Server) createDeploymentIfMissing(ctx context.Context, d *store.Deployment) error {
	_, err := s.Store.DB.ExecContext(ctx, s.Store.Rebind(
		`INSERT INTO deployments (id,group_id,game_id,spec_json,digest,created_at) VALUES (?,?,?,?,?,?)`),
		d.ID, d.GroupID, d.GameID, d.SpecJSON, d.Digest, d.CreatedAt)
	return err
}

func (s *Server) GetServer(ctx context.Context, req gen.GetServerRequestObject) (gen.GetServerResponseObject, error) {
	srv, err := s.Store.GetServer(ctx, req.ServerId)
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	if _, _, err := s.requireRole(ctx, srv.GroupID, "member"); err != nil {
		return nil, err
	}
	v, err := s.serverView(ctx, srv)
	if err != nil {
		return nil, err
	}
	return gen.GetServer200JSONResponse(v), nil
}

func (s *Server) UpdateServer(ctx context.Context, req gen.UpdateServerRequestObject) (gen.UpdateServerResponseObject, error) {
	srv, err := s.Store.GetServer(ctx, req.ServerId)
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	u, _, err := s.requireRole(ctx, srv.GroupID, "admin")
	if err != nil {
		return nil, err
	}
	b := req.Body
	if b == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	if b.Config != nil {
		game := catalog.Get(srv.GameID)
		if game != nil {
			if verr := game.ValidateConfig(*b.Config); len(verr) > 0 {
				return nil, errResp(gen.Validation, "invalid config: "+strings.Join(verr, "; "), nil)
			}
		}
		srv.ConfigJSON = mustJSON(*b.Config)
	}
	if b.Name != nil {
		srv.Name = *b.Name
	}
	if b.ReplicationFactor != nil {
		srv.ReplicationFactor = int64(*b.ReplicationFactor)
	}
	if b.MinCommitReplicas != nil {
		srv.MinCommitReplicas = int64(*b.MinCommitReplicas)
	}
	if b.SnapshotIntervalS != nil {
		srv.SnapshotIntervalS = int64(*b.SnapshotIntervalS)
	}
	if b.PreferredNodeId != nil {
		srv.PreferredNodeID = b.PreferredNodeId
	}
	srv.UpdatedAt = s.Store.NowMs()
	if err := s.Store.UpdateServerMeta(ctx, srv); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &srv.GroupID, "user", u.ID, "server.update", srv.ID, nil)
	v, err := s.serverView(ctx, srv)
	if err != nil {
		return nil, err
	}
	return gen.UpdateServer200JSONResponse(v), nil
}

func (s *Server) DeleteServer(ctx context.Context, req gen.DeleteServerRequestObject) (gen.DeleteServerResponseObject, error) {
	srv, err := s.Store.GetServer(ctx, req.ServerId)
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	u, _, err := s.requireRole(ctx, srv.GroupID, "admin")
	if err != nil {
		return nil, err
	}
	if err := s.Store.DeleteServer(ctx, req.ServerId); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &srv.GroupID, "user", u.ID, "server.delete", srv.ID, nil)
	return gen.DeleteServer204Response{}, nil
}

func (s *Server) StartServer(ctx context.Context, req gen.StartServerRequestObject) (gen.StartServerResponseObject, error) {
	srv, err := s.Store.GetServer(ctx, req.ServerId)
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	u, _, err := s.requireRole(ctx, srv.GroupID, "member")
	if err != nil {
		return nil, err
	}
	if srv.DesiredState == "running" {
		if _, err := s.Store.ActiveExecution(ctx, srv.ID); err == nil {
			v, err := s.serverView(ctx, srv)
			if err != nil {
				return nil, err
			}
			return gen.StartServer200JSONResponse(v), nil // idempotent
		}
		// No active execution despite desired=running: the previous run
		// ended 'failed'. An explicit start clears the crash-loop guard
		// and activates normally below.
	}
	if err := s.Store.UpdateServerDesired(ctx, srv.ID, "running", s.Store.NowMs()); err != nil {
		return nil, err
	}
	_ = s.Store.EmitEvent(ctx, s.Store.DB, srv.GroupID, &srv.ID, nil, "server.start_requested", nil)
	_ = s.Store.Audit(ctx, s.Store.DB, &srv.GroupID, "user", u.ID, "server.start", srv.ID, nil)

	var pref string
	allowOlder := false
	if req.Body != nil {
		if req.Body.PreferredNodeId != nil {
			pref = *req.Body.PreferredNodeId
		}
		if req.Body.AllowOlderSnapshot != nil {
			allowOlder = *req.Body.AllowOlderSnapshot
		}
	}
	srv.DesiredState = "running"
	if err := s.Recon.ActivateNormal(ctx, srv, pref, allowOlder); err != nil {
		// roll desired back to stopped on hard failure
		if ae := apiActivationErr(err); ae != nil {
			_ = s.Store.UpdateServerDesired(ctx, srv.ID, "stopped", s.Store.NowMs())
			return nil, ae
		}
		return nil, err
	}
	s.Recon.Wake()
	// re-read for the fresh epoch/observed state
	srv2, err := s.Store.GetServer(ctx, srv.ID)
	if err != nil {
		return nil, err
	}
	v, err := s.serverView(ctx, srv2)
	if err != nil {
		return nil, err
	}
	return gen.StartServer200JSONResponse(v), nil
}

func apiActivationErr(err error) error {
	if ae := activationErr(err); ae != nil {
		return errResp(gen.ErrorCode(ae.Code), ae.Message, ae.Details)
	}
	return nil
}

func (s *Server) StopServer(ctx context.Context, req gen.StopServerRequestObject) (gen.StopServerResponseObject, error) {
	srv, err := s.Store.GetServer(ctx, req.ServerId)
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	u, _, err := s.requireRole(ctx, srv.GroupID, "member")
	if err != nil {
		return nil, err
	}
	if err := s.Store.UpdateServerDesired(ctx, srv.ID, "stopped", s.Store.NowMs()); err != nil {
		return nil, err
	}
	_ = s.Store.EmitEvent(ctx, s.Store.DB, srv.GroupID, &srv.ID, nil, "server.stop_requested", nil)
	_ = s.Store.Audit(ctx, s.Store.DB, &srv.GroupID, "user", u.ID, "server.stop", srv.ID, nil)
	s.Recon.Wake()
	v, err := s.serverView(ctx, srv)
	if err != nil {
		return nil, err
	}
	return gen.StopServer200JSONResponse(v), nil
}

func (s *Server) MoveServer(ctx context.Context, req gen.MoveServerRequestObject) (gen.MoveServerResponseObject, error) {
	srv, err := s.Store.GetServer(ctx, req.ServerId)
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	u, _, err := s.requireRole(ctx, srv.GroupID, "member")
	if err != nil {
		return nil, err
	}
	if req.Body == nil || req.Body.TargetNodeId == "" {
		return nil, errResp(gen.Validation, "target_node_id required", nil)
	}
	target, err := s.Store.GetNode(ctx, req.Body.TargetNodeId)
	if err != nil || target.GroupID != srv.GroupID {
		return nil, errResp(gen.Validation, "target node not in group", nil)
	}
	active, err := s.Store.ActiveExecution(ctx, srv.ID)
	if err != nil {
		return nil, errResp(gen.Conflict, "server has no active execution", nil)
	}
	now := s.Store.NowMs()
	m := &store.Migration{
		ID: ids.Must("mig_"), ServerID: srv.ID, FromExecutionID: active.ID,
		ToNodeID: target.ID, State: "stopping", CreatedAt: now,
	}
	if err := s.Store.CreateMigration(ctx, m); err != nil {
		return nil, err
	}
	// mark the active exec stop reason=migration
	active.Action = "stop"
	reason := "migration"
	active.StopReason = &reason
	if err := s.Store.UpdateExecutionState(ctx, s.Store.DB, active); err != nil {
		return nil, err
	}
	_ = s.Store.SetObservedState(ctx, s.Store.DB, srv.ID, "migrating", now)
	_ = s.Store.Audit(ctx, s.Store.DB, &srv.GroupID, "user", u.ID, "server.move", srv.ID, map[string]any{"to": target.ID})
	s.Recon.Wake()
	v, err := s.serverView(ctx, srv)
	if err != nil {
		return nil, err
	}
	return gen.MoveServer200JSONResponse(v), nil
}

// ---- snapshots ----

func (s *Server) ListSnapshots(ctx context.Context, req gen.ListSnapshotsRequestObject) (gen.ListSnapshotsResponseObject, error) {
	srv, err := s.Store.GetServer(ctx, req.ServerId)
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	if _, _, err := s.requireRole(ctx, srv.GroupID, "member"); err != nil {
		return nil, err
	}
	snaps, err := s.Store.SnapshotsForServer(ctx, srv.ID)
	if err != nil {
		return nil, err
	}
	out := []gen.Snapshot{}
	for i := range snaps {
		v, err := s.snapshotView(ctx, &snaps[i])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return gen.ListSnapshots200JSONResponse{Snapshots: out}, nil
}

func (s *Server) snapshotView(ctx context.Context, sn *store.Snapshot) (gen.Snapshot, error) {
	reps, err := s.Store.ReplicasForSnapshot(ctx, sn.ID)
	if err != nil {
		return gen.Snapshot{}, err
	}
	outReps := []struct {
		NodeId string                    `json:"node_id"`
		State  gen.SnapshotReplicasState `json:"state"`
	}{}
	for _, r := range reps {
		outReps = append(outReps, struct {
			NodeId string                    `json:"node_id"`
			State  gen.SnapshotReplicasState `json:"state"`
		}{NodeId: r.NodeID, State: gen.SnapshotReplicasState(r.State)})
	}
	return gen.Snapshot{
		Id: sn.ID, ServerId: sn.ServerID, Epoch: sn.Epoch, ExecutionId: ptr(sn.ExecutionID),
		NodeId: sn.NodeID, ParentId: sn.ParentID, ManifestDigest: ptr(sn.ManifestDigest),
		DeploymentId: ptr(sn.DeploymentID), Reason: gen.SnapshotReason(sn.Reason),
		State: gen.SnapshotState(sn.State), SizeBytes: ptr(sn.SizeBytes),
		StoredBytes: ptr(sn.StoredBytes), FileCount: ptr(int(sn.FileCount)),
		ChunkCount: ptr(int(sn.ChunkCount)), Pinned: sn.Pinned == 1,
		CreatedAt: sn.CreatedAt, CommittedAt: sn.CommittedAt, Replicas: outReps,
	}, nil
}

func (s *Server) RequestSnapshot(ctx context.Context, req gen.RequestSnapshotRequestObject) (gen.RequestSnapshotResponseObject, error) {
	srv, err := s.Store.GetServer(ctx, req.ServerId)
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	u, _, err := s.requireRole(ctx, srv.GroupID, "member")
	if err != nil {
		return nil, err
	}
	active, err := s.Store.ActiveExecution(ctx, srv.ID)
	if err != nil {
		return nil, errResp(gen.Conflict, "server not running", nil)
	}
	r := &store.SnapshotRequest{
		ID: ids.Must("snr_"), ServerID: srv.ID, Reason: "manual", CreatedAt: s.Store.NowMs(),
	}
	if err := s.Store.CreateSnapshotRequest(ctx, r); err != nil {
		return nil, err
	}
	_ = active
	_ = s.Store.Audit(ctx, s.Store.DB, &srv.GroupID, "user", u.ID, "snapshot.request", srv.ID, nil)
	s.Recon.Wake()
	return gen.RequestSnapshot202JSONResponse{RequestId: r.ID}, nil
}

func (s *Server) UpdateSnapshot(ctx context.Context, req gen.UpdateSnapshotRequestObject) (gen.UpdateSnapshotResponseObject, error) {
	sn, err := s.Store.GetSnapshot(ctx, req.SnapshotId)
	if err != nil {
		return nil, errResp(gen.NotFound, "snapshot not found", nil)
	}
	if _, _, err := s.requireRole(ctx, sn.GroupID, "member"); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	if err := s.Store.UpdateSnapshotPinned(ctx, sn.ID, req.Body.Pinned); err != nil {
		return nil, err
	}
	sn.Pinned = b2i(req.Body.Pinned)
	v, err := s.snapshotView(ctx, sn)
	if err != nil {
		return nil, err
	}
	return gen.UpdateSnapshot200JSONResponse(v), nil
}

func (s *Server) ListExecutions(ctx context.Context, req gen.ListExecutionsRequestObject) (gen.ListExecutionsResponseObject, error) {
	srv, err := s.Store.GetServer(ctx, req.ServerId)
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	if _, _, err := s.requireRole(ctx, srv.GroupID, "member"); err != nil {
		return nil, err
	}
	es, err := s.Store.ExecutionsForServer(ctx, srv.ID)
	if err != nil {
		return nil, err
	}
	out := []gen.Execution{}
	for i := range es {
		out = append(out, genExecution(&es[i]))
	}
	return gen.ListExecutions200JSONResponse{Executions: out}, nil
}

func genExecution(e *store.Execution) gen.Execution {
	var placement *struct {
		Reasons *[]string `json:"reasons,omitempty"`
		Score   *float32  `json:"score,omitempty"`
	}
	var p struct {
		Score   float64  `json:"score"`
		Reasons []string `json:"reasons"`
	}
	if json.Unmarshal([]byte(e.PlacementJSON), &p) == nil && (p.Reasons != nil || p.Score != 0) {
		placement = &struct {
			Reasons *[]string `json:"reasons,omitempty"`
			Score   *float32  `json:"score,omitempty"`
		}{Score: ptr(float32(p.Score)), Reasons: &p.Reasons}
	}
	var endReason *gen.ExecutionEndReason
	if e.EndReason != nil {
		er := gen.ExecutionEndReason(*e.EndReason)
		endReason = &er
	}
	return gen.Execution{
		Id: e.ID, ServerId: e.ServerID, NodeId: e.NodeID, Epoch: e.Epoch,
		State: gen.ExecutionState(e.State), Action: gen.ExecutionAction(e.Action),
		StopReason: e.StopReason, LeaseExpiresAt: ptr(e.LeaseExpiresAt),
		RestoreSnapshotId: e.RestoreSnapshotID, Health: e.Health, Message: e.Message,
		Placement: placement, StartedAt: e.StartedAt, EndedAt: e.EndedAt,
		EndReason: endReason,
	}
}

func (s *Server) GetServerLogs(ctx context.Context, req gen.GetServerLogsRequestObject) (gen.GetServerLogsResponseObject, error) {
	srv, err := s.Store.GetServer(ctx, req.ServerId)
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	if _, _, err := s.requireRole(ctx, srv.GroupID, "member"); err != nil {
		return nil, err
	}
	execID := ""
	if req.Params.ExecutionId != nil {
		ex, err := s.Store.GetExecution(ctx, *req.Params.ExecutionId)
		if err != nil || ex.ServerID != srv.ID {
			return nil, errResp(gen.NotFound, "execution not found", nil)
		}
		execID = ex.ID
	} else if e, err := s.Store.ActiveExecution(ctx, srv.ID); err == nil {
		execID = e.ID
	}
	var lines []gen.LogLine
	if execID != "" {
		ls, err := s.Store.LogsForExecution(ctx, execID)
		if err != nil {
			return nil, err
		}
		for _, l := range ls {
			lines = append(lines, gen.LogLine{
				At: l.At, Stream: gen.LogLineStream(l.Stream), Line: l.Line,
			})
		}
	}
	if lines == nil {
		lines = []gen.LogLine{}
	}
	return gen.GetServerLogs200JSONResponse{Lines: lines}, nil
}
