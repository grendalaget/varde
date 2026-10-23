package api_test

import (
	"context"
	"strings"
	"testing"
)

func TestPatchMeSteamIDDoesNotChangeLinkedIdentity(t *testing.T) {
	e := newEnv(t)
	me := e.do("GET", "/v1/me", nil, e.token)
	if me.Status != 200 {
		t.Fatalf("GET /v1/me status = %d, want 200", me.Status)
	}
	userID := me.Body["user"].(map[string]any)["id"].(string)
	linkedSteamID := "76561198000000000"
	if err := e.st.SetUserSteam(context.Background(), userID, &linkedSteamID, nil, nil); err != nil {
		t.Fatal(err)
	}

	response := e.do("PATCH", "/v1/me", map[string]any{"steam_id": "76561198000000001"}, e.token)
	if response.Status != 200 {
		t.Fatalf("PATCH /v1/me with steam_id status = %d, want 200: %s", response.Status, response.Raw)
	}
	updated := e.do("GET", "/v1/me", nil, e.token)
	if updated.Status != 200 {
		t.Fatalf("GET /v1/me after PATCH status = %d, want 200", updated.Status)
	}
	if got := updated.Body["user"].(map[string]any)["steam_id"]; got != linkedSteamID {
		t.Errorf("PATCH /v1/me changed steam_id to %v, want %q", got, linkedSteamID)
	}
}

func TestFederatedProfileDisplayNameIsManaged(t *testing.T) {
	e := newEnv(t)
	me := e.do("GET", "/v1/me", nil, e.token)
	if me.Status != 200 {
		t.Fatalf("GET /v1/me status = %d, want 200", me.Status)
	}
	user := me.Body["user"].(map[string]any)
	if _, err := e.st.DB.Exec(`UPDATE users SET website_user_id=? WHERE id=?`, "website-user", user["id"]); err != nil {
		t.Fatal(err)
	}
	response := e.do("PATCH", "/v1/me", map[string]any{"display_name": "changed"}, e.token)
	if response.Status != 400 || response.Body["message"] != "display name is managed by varde.games" {
		t.Fatalf("PATCH /v1/me federated display_name = %d %v, want managed-name validation", response.Status, response.Body)
	}
}

func TestUpdateMeRejectsOversizedBody(t *testing.T) {
	e := newEnv(t)
	response := e.do("PATCH", "/v1/me", map[string]any{"display_name": strings.Repeat("x", 65<<10)}, e.token)
	if response.Status != 413 {
		t.Fatalf("PATCH /v1/me oversized body status = %d, want 413", response.Status)
	}
}
