package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/arnemolland/p2pgames/go/ids"

	"github.com/arnemolland/p2pgames/apps/control-plane/internal/api/gen"
	"github.com/arnemolland/p2pgames/apps/control-plane/internal/enroll"
	"github.com/arnemolland/p2pgames/apps/control-plane/internal/store"
)

func genGroup(g *store.Group) gen.Group {
	set := g.Settings()
	return gen.Group{
		Id:   g.ID,
		Name: g.Name,
		Plan: g.Plan,
		Settings: gen.GroupSettings{
			SnapshotIntervalS:        ptr(int(set.SnapshotIntervalS)),
			DefaultReplicationFactor: ptr(int(set.DefaultReplicationFactor)),
			DefaultMinCommitReplicas: ptr(int(set.DefaultMinCommitReplicas)),
			RequireAnchorForCommit:   ptr(set.RequireAnchorForCommit),
			SnapshotRetention:        ptr(int(set.SnapshotRetention)),
		},
		CreatedAt: g.CreatedAt,
	}
}

func (s *Server) ListGroups(ctx context.Context, _ gen.ListGroupsRequestObject) (gen.ListGroupsResponseObject, error) {
	u, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	ms, err := s.Store.GroupsForUser(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	out := []gen.Group{}
	for _, m := range ms {
		g, err := s.Store.GetGroup(ctx, m.GroupID)
		if err == nil {
			out = append(out, genGroup(g))
		}
	}
	return gen.ListGroups200JSONResponse{Groups: out}, nil
}

func (s *Server) CreateGroup(ctx context.Context, req gen.CreateGroupRequestObject) (gen.CreateGroupResponseObject, error) {
	u, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil || req.Body.Name == "" {
		return nil, errResp(gen.Validation, "name required", nil)
	}
	now := s.Store.NowMs()
	sets := store.DefaultGroupSettings()
	g := &store.Group{
		ID:           ids.Must(ids.Group),
		Name:         req.Body.Name,
		Plan:         "free",
		SettingsJSON: mustJSON(sets),
		CreatedAt:    now,
	}
	if err := s.Store.CreateGroup(ctx, g, u.ID); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &g.ID, "user", u.ID, "group.create", g.ID, map[string]any{"name": g.Name})
	return gen.CreateGroup201JSONResponse(genGroup(g)), nil
}

func (s *Server) GetGroup(ctx context.Context, req gen.GetGroupRequestObject) (gen.GetGroupResponseObject, error) {
	if _, _, err := s.requireRole(ctx, req.GroupId, "member"); err != nil {
		return nil, err
	}
	g, err := s.Store.GetGroup(ctx, req.GroupId)
	if err != nil {
		return nil, errResp(gen.NotFound, "group not found", nil)
	}
	return gen.GetGroup200JSONResponse(genGroup(g)), nil
}

func (s *Server) UpdateGroup(ctx context.Context, req gen.UpdateGroupRequestObject) (gen.UpdateGroupResponseObject, error) {
	u, _, err := s.requireRole(ctx, req.GroupId, "owner")
	if err != nil {
		return nil, err
	}
	g, err := s.Store.GetGroup(ctx, req.GroupId)
	if err != nil {
		return nil, errResp(gen.NotFound, "group not found", nil)
	}
	if req.Body != nil {
		if req.Body.Name != nil {
			g.Name = *req.Body.Name
		}
		if req.Body.Settings != nil {
			cur := g.Settings()
			ns := req.Body.Settings
			if ns.SnapshotIntervalS != nil {
				cur.SnapshotIntervalS = int64(*ns.SnapshotIntervalS)
			}
			if ns.DefaultReplicationFactor != nil {
				cur.DefaultReplicationFactor = int64(*ns.DefaultReplicationFactor)
			}
			if ns.DefaultMinCommitReplicas != nil {
				cur.DefaultMinCommitReplicas = int64(*ns.DefaultMinCommitReplicas)
			}
			if ns.RequireAnchorForCommit != nil {
				cur.RequireAnchorForCommit = *ns.RequireAnchorForCommit
			}
			if ns.SnapshotRetention != nil {
				cur.SnapshotRetention = int64(*ns.SnapshotRetention)
			}
			g.SettingsJSON = mustJSON(cur)
		}
	}
	if err := s.Store.UpdateGroup(ctx, g); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &g.ID, "user", u.ID, "group.update", g.ID, nil)
	return gen.UpdateGroup200JSONResponse(genGroup(g)), nil
}

func (s *Server) DeleteGroup(ctx context.Context, req gen.DeleteGroupRequestObject) (gen.DeleteGroupResponseObject, error) {
	u, _, err := s.requireRole(ctx, req.GroupId, "owner")
	if err != nil {
		return nil, err
	}
	if err := s.Store.DeleteGroup(ctx, req.GroupId); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &req.GroupId, "user", u.ID, "group.delete", req.GroupId, nil)
	return gen.DeleteGroup204Response{}, nil
}

func (s *Server) ListMembers(ctx context.Context, req gen.ListMembersRequestObject) (gen.ListMembersResponseObject, error) {
	if _, _, err := s.requireRole(ctx, req.GroupId, "member"); err != nil {
		return nil, err
	}
	ms, err := s.Store.ListMembers(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	out := []gen.Member{}
	for _, m := range ms {
		out = append(out, gen.Member{
			UserId: m.UserID, Email: m.Email, DisplayName: m.DisplayName,
			Role: gen.Role(m.Role), CreatedAt: m.CreatedAt,
		})
	}
	return gen.ListMembers200JSONResponse{Members: out}, nil
}

func (s *Server) UpdateMember(ctx context.Context, req gen.UpdateMemberRequestObject) (gen.UpdateMemberResponseObject, error) {
	u, _, err := s.requireRole(ctx, req.GroupId, "owner")
	if err != nil {
		return nil, err
	}
	if req.Body == nil || req.Body.Role == "" {
		return nil, errResp(gen.Validation, "role required", nil)
	}
	if err := s.Store.SetMemberRole(ctx, req.GroupId, req.UserId, string(req.Body.Role)); err != nil {
		return nil, errResp(gen.NotFound, "member not found", nil)
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &req.GroupId, "user", u.ID, "member.update", req.UserId, map[string]any{"role": req.Body.Role})
	return gen.UpdateMember200JSONResponse{
		UserId: req.UserId, Role: req.Body.Role, CreatedAt: s.Store.NowMs(),
	}, nil
}

func (s *Server) RemoveMember(ctx context.Context, req gen.RemoveMemberRequestObject) (gen.RemoveMemberResponseObject, error) {
	u, _, err := s.requireRole(ctx, req.GroupId, "owner")
	if err != nil {
		return nil, err
	}
	// members may remove themselves
	role, _ := s.Store.MemberRole(ctx, req.GroupId, u.ID)
	if req.UserId != u.ID && role != "owner" {
		return nil, errResp(gen.Forbidden, "owner only", nil)
	}
	if err := s.Store.RemoveMember(ctx, req.GroupId, req.UserId); err != nil {
		return nil, errResp(gen.NotFound, "member not found", nil)
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &req.GroupId, "user", u.ID, "member.remove", req.UserId, nil)
	return gen.RemoveMember204Response{}, nil
}

func (s *Server) CreateInvite(ctx context.Context, req gen.CreateInviteRequestObject) (gen.CreateInviteResponseObject, error) {
	u, _, err := s.requireRole(ctx, req.GroupId, "admin")
	if err != nil {
		return nil, err
	}
	role := "member"
	var exp, maxUses *int64
	if req.Body != nil {
		if req.Body.Role != nil {
			role = string(*req.Body.Role)
		}
		exp = req.Body.ExpiresAt
		if req.Body.MaxUses != nil {
			v := int64(*req.Body.MaxUses)
			maxUses = &v
		}
	}
	inv := &store.Invite{
		Code:      ids.Must("inv_"),
		GroupID:   req.GroupId,
		Role:      role,
		CreatedBy: u.ID,
		ExpiresAt: exp,
		MaxUses:   maxUses,
	}
	if err := s.Store.CreateInvite(ctx, inv); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &req.GroupId, "user", u.ID, "invite.create", inv.Code, nil)
	g, _ := s.Store.GetGroup(ctx, req.GroupId)
	return gen.CreateInvite201JSONResponse{
		Code: inv.Code, GroupId: inv.GroupID, GroupName: g.Name,
		Role: gen.Role(inv.Role), ExpiresAt: derefInt64(inv.ExpiresAt), MaxUses: int(derefInt64(inv.MaxUses)), Uses: 0,
	}, nil
}

func (s *Server) GetInvite(ctx context.Context, req gen.GetInviteRequestObject) (gen.GetInviteResponseObject, error) {
	if _, err := s.requireUser(ctx); err != nil {
		return nil, err
	}
	inv, err := s.Store.GetInvite(ctx, req.Code)
	if err != nil {
		return nil, errResp(gen.NotFound, "invite not found", nil)
	}
	g, _ := s.Store.GetGroup(ctx, inv.GroupID)
	return gen.GetInvite200JSONResponse{
		Code: inv.Code, GroupId: inv.GroupID, GroupName: g.Name,
		Role: gen.Role(inv.Role), ExpiresAt: derefInt64(inv.ExpiresAt), MaxUses: int(derefInt64(inv.MaxUses)), Uses: int(inv.Uses),
	}, nil
}

func (s *Server) AcceptInvite(ctx context.Context, req gen.AcceptInviteRequestObject) (gen.AcceptInviteResponseObject, error) {
	u, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	inv, err := s.Store.GetInvite(ctx, req.Code)
	if err != nil {
		return nil, errResp(gen.NotFound, "invite not found", nil)
	}
	if err := s.Store.ConsumeInvite(ctx, inv.Code); err != nil {
		return nil, errResp(gen.Validation, "invite expired or exhausted", nil)
	}
	if err := s.Store.AddMember(ctx, inv.GroupID, u.ID, inv.Role, s.Store.NowMs()); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &inv.GroupID, "user", u.ID, "invite.accept", inv.Code, nil)
	return gen.AcceptInvite200JSONResponse{GroupId: inv.GroupID}, nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var _ = fmt.Sprint
var _ = enroll.UserCode
