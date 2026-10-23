package autotournament

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	got, err := NormalizeBaseURL(" https://at.example.test/ ")
	if err != nil || got != "https://at.example.test" {
		t.Fatalf("NormalizeBaseURL() = %q, %v; want https://at.example.test", got, err)
	}
	for _, invalid := range []string{"file:///tmp/at", "ftp://at.example.test", "/relative"} {
		if _, err := NormalizeBaseURL(invalid); err == nil {
			t.Errorf("NormalizeBaseURL(%q) succeeded", invalid)
		}
	}
}

func TestClientRequests(t *testing.T) {
	requests := make([]struct {
		method string
		path   string
		auth   string
		header string
		body   []byte
	}, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, struct {
			method string
			path   string
			auth   string
			header string
			body   []byte
		}{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Tournament-Id"), body})
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/fleet/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"value":"test-fleet-key"}`))
		case "/api/tournaments":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"tournaments":[{"id":17,"name":"Existing","type":"swiss","status":"in_progress"}]}`))
			} else {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"success":true,"tournament":{"id":42,"name":"Created","type":"shuffle"}}`))
			}
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success":true}`))
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL+"/", "service-token")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	key, err := client.CreateFleetKey(ctx, "varde-test")
	if err != nil || key != "test-fleet-key" {
		t.Fatalf("CreateFleetKey() = %q, %v", key, err)
	}
	players := []Player{{ID: "76561198000000001", Name: "Player One"}}
	if err := client.ImportPlayers(ctx, players); err != nil {
		t.Fatal(err)
	}
	tournament, err := client.CreateShuffle(ctx, "Shuffle", 2, []string{"de_mirage"})
	if err != nil || tournament.ID != "42" {
		t.Fatalf("CreateShuffle() = %+v, %v", tournament, err)
	}
	teams := []Team{{
		ID: "varde-abc-1", Name: "Team 1", Tag: "T1",
		Players: []TeamPlayer{{SteamID: players[0].ID, Name: players[0].Name}},
	}}
	if err := client.CreateTeams(ctx, teams); err != nil {
		t.Fatal(err)
	}
	bracket, err := client.CreateBracket(ctx, "Bracket", "single_elimination", "bo1", 1,
		[]string{"de_mirage"}, []string{"varde-abc-1"})
	if err != nil || bracket.ID != "42" {
		t.Fatalf("CreateBracket() = %+v, %v", bracket, err)
	}
	if err := client.RegisterPlayers(ctx, "42", []string{players[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx, "42"); err != nil {
		t.Fatal(err)
	}
	listed, err := client.ListTournaments(ctx)
	if err != nil || !reflect.DeepEqual(listed, []Tournament{{
		ID: "17", Name: "Existing", Type: "swiss", Status: "in_progress",
	}}) {
		t.Fatalf("ListTournaments() = %+v, %v", listed, err)
	}

	want := []struct {
		method string
		path   string
		header string
	}{
		{http.MethodGet, "/api/tournaments", ""},
		{http.MethodPost, "/api/fleet/keys", ""},
		{http.MethodPost, "/api/players/bulk-import", ""},
		{http.MethodPost, "/api/tournaments", ""},
		{http.MethodPost, "/api/teams", ""},
		{http.MethodPost, "/api/tournaments", ""},
		{http.MethodPost, "/api/tournament/42/register-players", ""},
		{http.MethodPost, "/api/tournament/start", "42"},
		{http.MethodGet, "/api/tournaments", ""},
	}
	if len(requests) != len(want) {
		t.Fatalf("request count = %d, want %d", len(requests), len(want))
	}
	for i, expected := range want {
		if requests[i].method != expected.method || requests[i].path != expected.path ||
			requests[i].auth != "Bearer service-token" || requests[i].header != expected.header {
			t.Errorf("request %d = %+v, want method=%s path=%s bearer auth and tournament header=%q",
				i, requests[i], expected.method, expected.path, expected.header)
		}
	}
	assertRequestJSON(t, requests[1].body, map[string]any{"name": "varde-test", "autoLink": true})
	assertRequestJSON(t, requests[2].body, []map[string]any{{"id": players[0].ID, "name": "Player One"}})
	assertRequestJSON(t, requests[3].body, map[string]any{
		"name": "Shuffle", "type": "shuffle", "teamSize": float64(2),
		"mapSequence": []any{"de_mirage"}, "maxRounds": float64(24), "overtimeMode": "enabled",
	})
	assertRequestJSON(t, requests[4].body, []map[string]any{{
		"id": "varde-abc-1", "name": "Team 1", "tag": "T1",
		"players": []map[string]any{{"steamId": players[0].ID, "name": "Player One"}},
	}})
	assertRequestJSON(t, requests[5].body, map[string]any{
		"name": "Bracket", "type": "single_elimination", "format": "bo1",
		"teamSize": float64(1), "maps": []any{"de_mirage"}, "teamIds": []any{"varde-abc-1"},
	})
	assertRequestJSON(t, requests[6].body, map[string]any{"playerIds": []any{players[0].ID}})
}

func assertRequestJSON(t *testing.T, body []byte, want any) {
	t.Helper()
	var got any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode request body %q: %v", body, err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var wantDecoded any
	if err := json.Unmarshal(wantJSON, &wantDecoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantDecoded) {
		t.Errorf("request JSON = %#v, want %#v", got, wantDecoded)
	}
}

func TestUnauthorizedIsSentinelAndSurfacesATError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"invalid service token"}`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "bad-token")
	err := client.Ping(context.Background())
	if !errors.Is(err, ErrUnauthorized) || !strings.Contains(err.Error(), "invalid service token") {
		t.Fatalf("Ping() error = %v", err)
	}
}
