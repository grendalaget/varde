package api_test

import (
	"strings"
	"testing"
)

func TestUpdateMeSteamIDAndValidation(t *testing.T) {
	e := newEnv(t)
	valid := "76561198000000000"
	response := e.do("PATCH", "/v1/me", map[string]any{"steam_id": valid}, e.token)
	if response.Status != 200 {
		t.Fatalf("PATCH /v1/me valid steam_id status = %d, want 200: %s", response.Status, response.Raw)
	}
	user := response.Body["user"].(map[string]any)
	if user["steam_id"] != valid {
		t.Errorf("PATCH /v1/me returned steam_id = %v, want %q", user["steam_id"], valid)
	}

	response = e.do("PATCH", "/v1/me", map[string]any{"steam_id": ""}, e.token)
	if response.Status != 200 {
		t.Fatalf("PATCH /v1/me empty steam_id status = %d, want 200: %s", response.Status, response.Raw)
	}
	response = e.do("PATCH", "/v1/me", map[string]any{"steam_id": "123"}, e.token)
	if response.Status != 400 || response.Body["message"] != "invalid steam id" {
		t.Fatalf("PATCH /v1/me invalid steam_id = %d %v, want 400 invalid steam id", response.Status, response.Body)
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
	response = e.do("PATCH", "/v1/me", map[string]any{"steam_id": "76561198000000001"}, e.token)
	if response.Status != 200 {
		t.Fatalf("PATCH /v1/me federated steam_id status = %d, want 200: %s", response.Status, response.Raw)
	}
}

func TestUpdateMeRejectsOversizedBody(t *testing.T) {
	e := newEnv(t)
	response := e.do("PATCH", "/v1/me", map[string]any{"display_name": strings.Repeat("x", 65<<10)}, e.token)
	if response.Status != 413 {
		t.Fatalf("PATCH /v1/me oversized body status = %d, want 413", response.Status)
	}
}
