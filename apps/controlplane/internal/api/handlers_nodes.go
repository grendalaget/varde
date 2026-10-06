package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/grendalaget/varde/go/ids"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/enroll"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

const deviceLinkTTLMs = 900_000 // 15 min
const devicePollIntervalS = 3

func genNode(s *Server, ctx context.Context, n *store.Node) (gen.Node, error) {
	liveness := s.Recon.Liveness(n)
	var caps *gen.Capabilities
	connections := []gen.NodeConnection{}
	if st, err := s.Store.GetNodeStatus(ctx, n.ID); err == nil {
		var c gen.Capabilities
		if err := jsonUnmarshal(st.StatusJSON, &c); err == nil {
			caps = &c
		}
		var mesh struct {
			Peers []struct {
				NodeID string `json:"node_id"`
				Path   string `json:"path"`
				RttUs  int64  `json:"rtt_us"`
			} `json:"peers"`
		}
		if err := jsonUnmarshal(st.MeshJSON, &mesh); err == nil && len(mesh.Peers) > 0 {
			// name lookup is only over this node's group; peers that aren't
			// group members are omitted
			if peers, err := s.Store.ListNodes(ctx, n.GroupID); err == nil {
				names := map[string]string{}
				for _, p := range peers {
					names[p.ID] = p.Name
				}
				for _, p := range mesh.Peers {
					name, ok := names[p.NodeID]
					if !ok {
						continue
					}
					connections = append(connections, gen.NodeConnection{
						NodeId: p.NodeID,
						Name:   name,
						Path:   gen.NodeConnectionPath(p.Path),
						RttUs:  p.RttUs,
					})
				}
			}
		}
	}
	return gen.Node{
		Id:              n.ID,
		GroupId:         n.GroupID,
		Name:            n.Name,
		PublicKey:       n.PublicKey,
		Os:              n.OS,
		Arch:            n.Arch,
		AgentVersion:    nilIfEmpty(n.AgentVersion),
		HostingEnabled:  n.HostingEnabled == 1,
		Anchor:          n.Anchor == 1,
		Priority:        intPtrToPtrInt(ptr(int64(n.Priority))),
		MaxMemoryMb:     intPtrToPtrInt(n.MaxMemoryMB),
		MaxCpuPercent:   intPtrToPtrInt(n.MaxCPUPercent),
		MaxStorageBytes: n.MaxStorageBytes,
		AdminState:      gen.NodeAdminState(n.AdminState),
		Online:          liveness == "online",
		Liveness:        ptr(gen.NodeLiveness(liveness)),
		LastSeenAt:      n.LastSeenAt,
		Capabilities:    caps,
		Connections:     connections,
		CreatedAt:       n.CreatedAt,
	}, nil
}

func intPtrToPtrInt(v *int64) *int {
	if v == nil {
		return nil
	}
	i := int(*v)
	return &i
}

func int64PtrToIntPtr(v *int64) *int {
	if v == nil {
		return nil
	}
	i := int(*v)
	return &i
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptr[T any](v T) *T { return &v }

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// ---- enrollment tokens (user side) ----

func (s *Server) ListEnrollmentTokens(ctx context.Context, req gen.ListEnrollmentTokensRequestObject) (gen.ListEnrollmentTokensResponseObject, error) {
	if _, _, err := s.requireRole(ctx, req.GroupId, "admin"); err != nil {
		return nil, err
	}
	ts, err := s.Store.ListEnrollmentTokens(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	out := []gen.EnrollmentToken{}
	for _, t := range ts {
		var labels map[string]string
		_ = jsonUnmarshal(t.LabelsJSON, &labels)
		out = append(out, gen.EnrollmentToken{
			Id: t.ID, GroupId: t.GroupID, Anchor: t.Anchor == 1,
			HostingEnabled: t.HostingEnabled == 1, Labels: &labels,
			ExpiresAt: derefInt64(t.ExpiresAt), MaxUses: int64PtrToIntPtr(t.MaxUses), Uses: int(t.Uses),
		})
	}
	return gen.ListEnrollmentTokens200JSONResponse{Tokens: out}, nil
}

func (s *Server) CreateEnrollmentToken(ctx context.Context, req gen.CreateEnrollmentTokenRequestObject) (gen.CreateEnrollmentTokenResponseObject, error) {
	u, _, err := s.requireRole(ctx, req.GroupId, "admin")
	if err != nil {
		return nil, err
	}
	secret, err := enroll.EnrollmentToken()
	if err != nil {
		return nil, err
	}
	anchor, hosting := int64(0), int64(1)
	var labels map[string]string
	var exp, maxUses *int64
	if req.Body != nil {
		if req.Body.Anchor != nil {
			anchor = b2i(*req.Body.Anchor)
		}
		if req.Body.HostingEnabled != nil {
			hosting = b2i(*req.Body.HostingEnabled)
		}
		if req.Body.Labels != nil {
			labels = *req.Body.Labels
		}
		exp = req.Body.ExpiresAt
		if req.Body.MaxUses != nil {
			v := int64(*req.Body.MaxUses)
			maxUses = &v
		}
	}
	t := &store.EnrollmentToken{
		ID:             ids.Must("ent_"),
		GroupID:        req.GroupId,
		TokenHash:      enroll.Hash(secret),
		Anchor:         anchor,
		HostingEnabled: hosting,
		LabelsJSON:     mustJSON(labels),
		ExpiresAt:      exp,
		MaxUses:        maxUses,
		CreatedBy:      u.ID,
	}
	if err := s.Store.CreateEnrollmentToken(ctx, t); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &req.GroupId, "user", u.ID, "enrollment_token.create", t.ID, nil)
	var labelsPtr *map[string]string
	if labels != nil {
		labelsPtr = &labels
	}
	return gen.CreateEnrollmentToken201JSONResponse{
		Id: t.ID, GroupId: t.GroupID, Anchor: anchor == 1, HostingEnabled: hosting == 1,
		Labels: labelsPtr, ExpiresAt: derefInt64(exp), MaxUses: int64PtrToIntPtr(maxUses), Uses: 0, Token: secret,
	}, nil
}

func (s *Server) DeleteEnrollmentToken(ctx context.Context, req gen.DeleteEnrollmentTokenRequestObject) (gen.DeleteEnrollmentTokenResponseObject, error) {
	u, _, err := s.requireRole(ctx, req.GroupId, "admin")
	if err != nil {
		return nil, err
	}
	if err := s.Store.DeleteEnrollmentToken(ctx, req.GroupId, req.TokenId); err != nil {
		return nil, errResp(gen.NotFound, "token not found", nil)
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &req.GroupId, "user", u.ID, "enrollment_token.delete", req.TokenId, nil)
	return gen.DeleteEnrollmentToken204Response{}, nil
}

// ---- device links (user side) ----

func (s *Server) GetDeviceLink(ctx context.Context, req gen.GetDeviceLinkRequestObject) (gen.GetDeviceLinkResponseObject, error) {
	if _, err := s.requireUser(ctx); err != nil {
		return nil, err
	}
	d, err := s.Store.DeviceLinkByUserCode(ctx, enroll.NormalizeUserCode(req.UserCode))
	if err != nil {
		return nil, errResp(gen.NotFound, "device link not found", nil)
	}
	return gen.GetDeviceLink200JSONResponse{
		UserCode: d.UserCode, Hostname: d.Hostname, Os: d.OS, Arch: d.Arch,
		AgentVersion: nilIfEmpty(d.AgentVersion), State: gen.DeviceLinkState(d.State),
		GroupId: d.GroupID, NodeId: d.NodeID, ExpiresAt: d.ExpiresAt,
	}, nil
}

func (s *Server) ApproveDeviceLink(ctx context.Context, req gen.ApproveDeviceLinkRequestObject) (gen.ApproveDeviceLinkResponseObject, error) {
	u, _, err := s.requireRole(ctx, req.Body.GroupId, "member")
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	d, err := s.Store.DeviceLinkByUserCode(ctx, enroll.NormalizeUserCode(req.Body.UserCode))
	if err != nil {
		return nil, errResp(gen.NotFound, "device link not found", nil)
	}
	if d.ExpiresAt <= s.Store.NowMs() {
		return nil, errResp(gen.Validation, "device link expired", nil)
	}
	if d.State != "pending" {
		return nil, errResp(gen.Conflict, "device link already "+d.State, nil)
	}
	if err := s.checkNodeLimit(ctx, req.Body.GroupId); err != nil {
		return nil, err
	}
	nodeID := ids.Must(ids.Node)
	d.State = "approved"
	d.GroupID = &req.Body.GroupId
	d.NodeID = &nodeID
	d.ApprovedBy = &u.ID
	if err := s.Store.UpdateDeviceLink(ctx, d); err != nil {
		return nil, err
	}
	now := s.Store.NowMs()
	n := &store.Node{
		ID: nodeID, GroupID: req.Body.GroupId, Name: req.Body.Name,
		PublicKey: d.PublicKey, OS: d.OS, Arch: d.Arch, AgentVersion: d.AgentVersion,
		HostingEnabled: 1, AdminState: "active", CreatedAt: now,
		OwnerUserID: d.ApprovedBy,
	}
	if err := s.Store.CreateNode(ctx, n); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &req.Body.GroupId, "user", u.ID, "device_link.approve", nodeID, nil)
	_ = s.Store.EmitEvent(ctx, s.Store.DB, req.Body.GroupId, nil, &nodeID, "node.enrolled", map[string]any{"name": req.Body.Name})
	return gen.ApproveDeviceLink200JSONResponse{
		UserCode: d.UserCode, Hostname: d.Hostname, Os: d.OS, Arch: d.Arch,
		AgentVersion: nilIfEmpty(d.AgentVersion), State: gen.Approved,
		GroupId: d.GroupID, NodeId: d.NodeID, ExpiresAt: d.ExpiresAt,
	}, nil
}

// ---- enrollment (agent side, unsigned) ----

func (s *Server) AgentEnrollDevice(ctx context.Context, req gen.AgentEnrollDeviceRequestObject) (gen.AgentEnrollDeviceResponseObject, error) {
	b := req.Body
	if b == nil || b.PublicKey == "" {
		return nil, errResp(gen.Validation, "public_key required", nil)
	}
	if raw, err := base64.StdEncoding.DecodeString(b.PublicKey); err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errResp(gen.Validation, "public_key must be base64 ed25519", nil)
	}
	userCode, err := enroll.UserCode()
	if err != nil {
		return nil, err
	}
	devCode, err := enroll.DeviceCode()
	if err != nil {
		return nil, err
	}
	now := s.Store.NowMs()
	hostname := strv(b.Hostname)
	d := &store.DeviceLink{
		UserCode: userCode, DeviceCodeHash: enroll.Hash(devCode),
		PublicKey: b.PublicKey, Hostname: hostname,
		OS: strv(b.Os), Arch: strv(b.Arch), AgentVersion: strv(b.AgentVersion),
		ExpiresAt: now + deviceLinkTTLMs, CreatedAt: now,
	}
	if err := s.Store.CreateDeviceLink(ctx, d); err != nil {
		return nil, err
	}
	base := s.publicBase(ctx)
	return gen.AgentEnrollDevice200JSONResponse{
		DeviceCode:      devCode,
		UserCode:        userCode,
		VerificationUrl: fmt.Sprintf("%s/link?code=%s", base, userCode),
		ExpiresIn:       deviceLinkTTLMs / 1000,
		Interval:        devicePollIntervalS,
	}, nil
}

func (s *Server) AgentEnrollDevicePoll(ctx context.Context, req gen.AgentEnrollDevicePollRequestObject) (gen.AgentEnrollDevicePollResponseObject, error) {
	b := req.Body
	if b == nil || b.DeviceCode == "" {
		return nil, errResp(gen.Validation, "device_code required", nil)
	}
	d, err := s.Store.DeviceLinkByDeviceHash(ctx, enroll.Hash(b.DeviceCode))
	if err != nil {
		return nil, errResp(gen.NotFound, "unknown device_code", nil)
	}
	if d.ExpiresAt <= s.Store.NowMs() {
		return gen.AgentEnrollDevicePoll410Response{}, nil
	}
	if d.State != "approved" || d.NodeID == nil || d.GroupID == nil {
		return gen.AgentEnrollDevicePoll202Response{}, nil
	}
	d.State = "consumed"
	if err := s.Store.UpdateDeviceLink(ctx, d); err != nil {
		return nil, err
	}
	pub := s.RelayKey.Public().(ed25519.PublicKey)
	return gen.AgentEnrollDevicePoll200JSONResponse{
		NodeId:                *d.NodeID,
		GroupId:               *d.GroupID,
		ControlPlanePublicKey: base64.StdEncoding.EncodeToString(pub),
	}, nil
}

func (s *Server) AgentEnrollToken(ctx context.Context, req gen.AgentEnrollTokenRequestObject) (gen.AgentEnrollTokenResponseObject, error) {
	b := req.Body
	if b == nil || b.Token == "" || b.PublicKey == "" {
		return nil, errResp(gen.Validation, "token and public_key required", nil)
	}
	if raw, err := base64.StdEncoding.DecodeString(b.PublicKey); err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errResp(gen.Validation, "public_key must be base64 ed25519", nil)
	}
	t, err := s.Store.ConsumeEnrollmentToken(ctx, enroll.Hash(b.Token))
	if err != nil {
		return nil, errResp(gen.Forbidden, "invalid or expired token", nil)
	}
	if err := s.checkNodeLimit(ctx, t.GroupID); err != nil {
		return nil, err
	}
	now := s.Store.NowMs()
	name := strv(b.Hostname)
	if name == "" {
		name = "node-" + t.ID[len("ent_"):min(10, len(t.ID))]
	}
	var owner *string
	if t.CreatedBy != "" {
		owner = &t.CreatedBy
	}
	n := &store.Node{
		ID: ids.Must(ids.Node), GroupID: t.GroupID, Name: name,
		PublicKey: b.PublicKey, OS: strv(b.Os), Arch: strv(b.Arch),
		AgentVersion: strv(b.AgentVersion), HostingEnabled: t.HostingEnabled,
		Anchor: t.Anchor, AdminState: "active", CreatedAt: now,
		OwnerUserID: owner,
	}
	if err := s.Store.CreateNode(ctx, n); err != nil {
		if store.IsUniqueViolation(err) {
			return nil, errResp(gen.Conflict, "public key already enrolled", nil)
		}
		return nil, err
	}
	pub := s.RelayKey.Public().(ed25519.PublicKey)
	_ = s.Store.Audit(ctx, s.Store.DB, &t.GroupID, "node", n.ID, "node.enroll", n.ID, map[string]any{"token_id": t.ID})
	_ = s.Store.EmitEvent(ctx, s.Store.DB, t.GroupID, nil, &n.ID, "node.enrolled", map[string]any{"name": n.Name})
	return gen.AgentEnrollToken200JSONResponse{
		NodeId:                n.ID,
		GroupId:               t.GroupID,
		ControlPlanePublicKey: base64.StdEncoding.EncodeToString(pub),
	}, nil
}

func (s *Server) checkNodeLimit(ctx context.Context, groupID string) error {
	lim, err := s.Store.GetLimits(ctx, groupID)
	if err != nil {
		return err
	}
	if lim.MaxNodes != nil {
		n, err := s.Store.NodeCount(ctx, groupID)
		if err != nil {
			return err
		}
		if n >= *lim.MaxNodes {
			return errResp(gen.LimitExceeded, "group node limit reached", nil)
		}
	}
	return nil
}

// ---- nodes ----

func (s *Server) ListNodes(ctx context.Context, req gen.ListNodesRequestObject) (gen.ListNodesResponseObject, error) {
	if _, _, err := s.requireRole(ctx, req.GroupId, "member"); err != nil {
		return nil, err
	}
	ns, err := s.Store.ListNodes(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	out := []gen.Node{}
	for i := range ns {
		gn, err := genNode(s, ctx, &ns[i])
		if err != nil {
			return nil, err
		}
		out = append(out, gn)
	}
	return gen.ListNodes200JSONResponse{Nodes: out}, nil
}

func (s *Server) GetNode(ctx context.Context, req gen.GetNodeRequestObject) (gen.GetNodeResponseObject, error) {
	n, err := s.Store.GetNode(ctx, req.NodeId)
	if err != nil {
		return nil, errResp(gen.NotFound, "node not found", nil)
	}
	if _, _, err := s.requireRole(ctx, n.GroupID, "member"); err != nil {
		return nil, err
	}
	gn, err := genNode(s, ctx, n)
	if err != nil {
		return nil, err
	}
	return gen.GetNode200JSONResponse(gn), nil
}

func (s *Server) UpdateNode(ctx context.Context, req gen.UpdateNodeRequestObject) (gen.UpdateNodeResponseObject, error) {
	u, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.Store.GetNode(ctx, req.NodeId)
	if err != nil {
		return nil, errResp(gen.NotFound, "node not found", nil)
	}
	// members may manage "own" nodes (name/hosting toggle); admin+ for admin_state
	role, rerr := s.Store.MemberRole(ctx, n.GroupID, u.ID)
	if rerr != nil && u.IsOperator != 1 {
		return nil, errResp(gen.Forbidden, "not a member of this group", nil)
	}
	// node ownership: the enrolling user, or admin/owner of the group, or an
	// operator. Nodes with no owner (pre-column enrollments) are admin-only.
	adminish := role == "admin" || role == "owner" || u.IsOperator == 1
	owned := n.OwnerUserID != nil && *n.OwnerUserID == u.ID
	if !adminish && !owned {
		return nil, errResp(gen.Forbidden, "not the node owner", nil)
	}
	b := req.Body
	if b == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	if b.AdminState != nil && role != "admin" && role != "owner" && u.IsOperator != 1 {
		return nil, errResp(gen.Forbidden, "admin_state requires admin", nil)
	}
	if b.Name != nil {
		n.Name = *b.Name
	}
	if b.HostingEnabled != nil {
		n.HostingEnabled = b2i(*b.HostingEnabled)
	}
	if b.Anchor != nil {
		if role != "admin" && role != "owner" && u.IsOperator != 1 {
			return nil, errResp(gen.Forbidden, "anchor requires admin", nil)
		}
		n.Anchor = b2i(*b.Anchor)
	}
	if b.Priority != nil {
		n.Priority = int64(*b.Priority)
	}
	if b.MaxMemoryMb != nil {
		v := int64(*b.MaxMemoryMb)
		n.MaxMemoryMB = &v
	}
	if b.MaxCpuPercent != nil {
		v := int64(*b.MaxCpuPercent)
		n.MaxCPUPercent = &v
	}
	if b.MaxStorageBytes != nil {
		n.MaxStorageBytes = b.MaxStorageBytes
	}
	if b.AdminState != nil {
		n.AdminState = string(*b.AdminState)
	}
	if err := s.Store.UpdateNode(ctx, n); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &n.GroupID, "user", u.ID, "node.update", n.ID, nil)
	gn, err := genNode(s, ctx, n)
	if err != nil {
		return nil, err
	}
	return gen.UpdateNode200JSONResponse(gn), nil
}

func (s *Server) DeleteNode(ctx context.Context, req gen.DeleteNodeRequestObject) (gen.DeleteNodeResponseObject, error) {
	n, err := s.Store.GetNode(ctx, req.NodeId)
	if err != nil {
		return nil, errResp(gen.NotFound, "node not found", nil)
	}
	u, _, err := s.requireRole(ctx, n.GroupID, "admin")
	if err != nil {
		return nil, err
	}
	if err := s.Store.DeleteNode(ctx, req.NodeId); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &n.GroupID, "user", u.ID, "node.delete", n.ID, nil)
	return gen.DeleteNode204Response{}, nil
}

// helpers

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func strv(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func jsonUnmarshal(data string, v any) error {
	if data == "" {
		return nil
	}
	return json.Unmarshal([]byte(data), v)
}

var _ = time.Now
