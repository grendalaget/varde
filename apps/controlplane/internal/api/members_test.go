package api_test

import (
	"context"
	"testing"

	"github.com/grendalaget/varde/apps/controlplane/internal/auth"
)

// memberUser signs up a user and adds them to the env group with role.
func (e *env) memberUser(email, role string) (token, userID string) {
	e.t.Helper()
	token = e.signup(email, "password123")
	u, err := e.st.UserByEmail(context.Background(), email)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.AddMember(context.Background(), e.group, u.ID, role, e.clk.ms); err != nil {
		e.t.Fatal(err)
	}
	return token, u.ID
}

func TestMemberRoleRules(t *testing.T) {
	e := newEnv(t)
	op, err := e.st.UserByEmail(context.Background(), "op@example.com")
	if err != nil {
		t.Fatal(err)
	}
	adminTok, adminID := e.memberUser("admin@example.com", "admin")
	memberTok, memberID := e.memberUser("member@example.com", "member")
	_, otherID := e.memberUser("other@example.com", "member")
	members := "/v1/groups/" + e.group + "/members/"
	expect := func(what string, r apiResp, status int) {
		t.Helper()
		if r.Status != status {
			t.Fatalf("%s: want %d, got %d %s", what, status, r.Status, r.Raw)
		}
	}
	setRole := func(tok, userID, role string) apiResp {
		return e.do("PATCH", members+userID, map[string]any{"role": role}, tok)
	}

	// admins move people between admin and member
	expect("admin promotes member", setRole(adminTok, memberID, "admin"), 200)
	expect("admin demotes admin", setRole(adminTok, memberID, "member"), 200)
	// but never touch owner
	expect("admin grants owner", setRole(adminTok, memberID, "owner"), 403)
	expect("admin demotes owner", setRole(adminTok, op.ID, "admin"), 403)
	expect("admin removes owner", e.do("DELETE", members+op.ID, nil, adminTok), 403)
	expect("member changes role", setRole(memberTok, otherID, "admin"), 403)
	expect("member removes other", e.do("DELETE", members+otherID, nil, memberTok), 403)
	expect("admin removes member", e.do("DELETE", members+otherID, nil, adminTok), 204)
	expect("removed member is gone", e.do("DELETE", members+otherID, nil, adminTok), 404)

	// the last owner can neither step down nor leave
	expect("sole owner demotes self", setRole(e.token, op.ID, "admin"), 409)
	expect("sole owner leaves", e.do("DELETE", members+op.ID, nil, e.token), 409)
	expect("owner grants owner", setRole(e.token, adminID, "owner"), 200)
	expect("owner demotes other owner", setRole(e.token, adminID, "admin"), 200)
	expect("owner re-grants owner", setRole(e.token, adminID, "owner"), 200)
	expect("co-owner can leave", e.do("DELETE", members+op.ID, nil, e.token), 204)
	expect("new sole owner demotes self", setRole(adminTok, adminID, "member"), 409)

	// anyone may leave
	expect("member leaves", e.do("DELETE", members+memberID, nil, memberTok), 204)
}

// An expired or used-up invite must not leave a usable account behind.
func TestSignupWithDeadInviteCreatesNoAccount(t *testing.T) {
	e := newEnv(t)
	expired := e.mustOK(e.do("POST", "/v1/groups/"+e.group+"/invites",
		map[string]any{"expires_at": e.clk.ms - 1}, e.token))["code"].(string)
	single := e.mustOK(e.do("POST", "/v1/groups/"+e.group+"/invites",
		map[string]any{"max_uses": 1}, e.token))["code"].(string)
	e.srv.Auth.Policy = auth.SignupInvite

	signup := func(email, code string) apiResp {
		return e.do("POST", "/v1/auth/signup", map[string]any{
			"email": email, "password": "password123", "display_name": email, "invite_code": code,
		}, "")
	}
	if r := signup("late@example.com", expired); r.Status != 403 {
		t.Fatalf("expired invite signup: want 403, got %d %s", r.Status, r.Raw)
	}
	if _, err := e.st.UserByEmail(context.Background(), "late@example.com"); err == nil {
		t.Fatal("expired invite created the account")
	}
	if r := e.do("POST", "/v1/auth/login",
		map[string]any{"email": "late@example.com", "password": "password123"}, ""); r.Status == 200 {
		t.Fatal("expired invite signup can log in")
	}

	// a failed account creation hands the use back
	if r := signup("op@example.com", single); r.Status != 409 {
		t.Fatalf("duplicate email signup: want 409, got %d %s", r.Status, r.Raw)
	}
	if inv, err := e.st.GetInvite(context.Background(), single); err != nil || inv.Uses != 0 {
		t.Fatalf("invite after failed signup: %+v %v, want 0 uses", inv, err)
	}
	e.mustOK(signup("first@example.com", single))
	if r := signup("second@example.com", single); r.Status != 403 {
		t.Fatalf("exhausted invite signup: want 403, got %d %s", r.Status, r.Raw)
	}
	if _, err := e.st.UserByEmail(context.Background(), "second@example.com"); err == nil {
		t.Fatal("exhausted invite created the account")
	}
}
