package api

import (
	"context"
	"errors"
	"strings"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/auth"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func apiErrFromMsg(err error) error {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "validation: "):
		return errResp(gen.Validation, strings.TrimPrefix(msg, "validation: "), nil)
	case strings.HasPrefix(msg, "forbidden: "):
		return errResp(gen.Forbidden, strings.TrimPrefix(msg, "forbidden: "), nil)
	case strings.HasPrefix(msg, "conflict: "):
		return errResp(gen.Conflict, strings.TrimPrefix(msg, "conflict: "), nil)
	case errors.Is(err, auth.ErrBadCredentials):
		return errResp(gen.Unauthorized, "invalid email or password", nil)
	}
	return err
}

func genUser(u *store.User) gen.User {
	return gen.User{
		Id:          u.ID,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		IsOperator:  u.IsOperator == 1,
		CreatedAt:   u.CreatedAt,
	}
}

func (s *Server) Signup(ctx context.Context, req gen.SignupRequestObject) (gen.SignupResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	invite := ""
	if b.InviteCode != nil {
		invite = *b.InviteCode
	}
	u, token, err := s.Auth.Signup(ctx, string(b.Email), b.Password, b.DisplayName, invite)
	if err != nil {
		return nil, apiErrFromMsg(err)
	}
	_ = s.Store.Audit(ctx, s.Store.DB, nil, "user", u.ID, "user.signup", u.ID, map[string]any{"email": u.Email})
	return gen.Signup201JSONResponse{
		User:  genUser(u),
		Token: token,
	}, nil
}

func (s *Server) Login(ctx context.Context, req gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	u, token, err := s.Auth.Login(ctx, string(b.Email), b.Password)
	if err != nil {
		return nil, apiErrFromMsg(err)
	}
	return gen.Login200JSONResponse{
		User:  genUser(u),
		Token: token,
	}, nil
}

func (s *Server) Logout(ctx context.Context, _ gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	u, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	_ = u
	if tok := tokenFromCtx(ctx); tok != "" {
		_ = s.Auth.Logout(ctx, tok)
	}
	return gen.Logout204Response{}, nil
}

// tokenFromCtx recovers the raw session token stashed by middleware.
func tokenFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(ctxToken).(string)
	return v
}

func (s *Server) GetMe(ctx context.Context, _ gen.GetMeRequestObject) (gen.GetMeResponseObject, error) {
	u, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	ms, err := s.Store.GroupsForUser(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	groups := []struct {
		GroupId string   `json:"group_id"`
		Name    string   `json:"name"`
		Role    gen.Role `json:"role"`
	}{}
	for _, m := range ms {
		g, err := s.Store.GetGroup(ctx, m.GroupID)
		if err != nil {
			continue
		}
		groups = append(groups, struct {
			GroupId string   `json:"group_id"`
			Name    string   `json:"name"`
			Role    gen.Role `json:"role"`
		}{GroupId: g.ID, Name: g.Name, Role: gen.Role(m.Role)})
	}
	return gen.GetMe200JSONResponse{
		User:   genUser(u),
		Groups: groups,
	}, nil
}
