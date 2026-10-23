package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

type atCall struct {
	method string
	path   string
	auth   string
	header string
	body   []byte
}

func newATStub(t *testing.T, e *env) (*httptest.Server, *[]atCall) {
	t.Helper()
	calls := make([]atCall, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, atCall{
			method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
			header: r.Header.Get("X-Tournament-Id"), body: body,
		})
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/tournaments":
			_, _ = w.Write([]byte(`{"tournaments":[{"id":77,"name":"Existing","type":"swiss","status":"in_progress"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/fleet/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"value":"test-fleet-key"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/players/bulk-import":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"created":4,"updated":0,"errors":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/teams":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/tournaments":
			var request struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(body, &request)
			id := 101
			if request.Type != "shuffle" {
				id = 102
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"success":true,"tournament":{"id":%d,"type":%q}}`, id, request.Type)
		case r.Method == http.MethodPost &&
			(strings.HasPrefix(r.URL.Path, "/api/tournament/") && strings.HasSuffix(r.URL.Path, "/register-players")):
			_, _ = w.Write([]byte(`{"registered":4,"errors":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/tournament/start":
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"unexpected request"}`))
		}
	}))
	t.Cleanup(server.Close)
	e.srv.AutoTournamentHTTP = server.Client()
	return server, &calls
}

func newATWorkflowStub(t *testing.T, e *env, importBody, registerBody []byte, startStatus int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	if importBody == nil {
		importBody = []byte(`{"errors":[]}`)
	}
	if registerBody == nil {
		registerBody = []byte(`{"errors":[]}`)
	}
	if startStatus == 0 {
		startStatus = http.StatusOK
	}
	created := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/tournaments":
			_, _ = w.Write([]byte(`{"tournaments":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/fleet/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"value":"test-fleet-key"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/players/bulk-import":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(importBody)
		case r.Method == http.MethodPost && r.URL.Path == "/api/tournaments":
			created.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"tournament":{"id":101,"name":"Created","type":"shuffle"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/register-players"):
			_, _ = w.Write(registerBody)
		case r.Method == http.MethodPost && r.URL.Path == "/api/tournament/start":
			w.WriteHeader(startStatus)
			if startStatus >= 200 && startStatus < 300 {
				_, _ = w.Write([]byte(`{"success":true}`))
			} else {
				_, _ = w.Write([]byte(`{"error":"start failed"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"unexpected request"}`))
		}
	}))
	t.Cleanup(server.Close)
	e.srv.AutoTournamentHTTP = server.Client()
	return server, created
}

func linkAT(t *testing.T, e *env, server *httptest.Server) {
	t.Helper()
	path := fmt.Sprintf("/v1/groups/%s/integrations/autotournament", e.group)
	linked := e.do(http.MethodPut, path, map[string]any{
		"base_url": server.URL + "/", "api_token": "test-service-token",
	}, e.token)
	if linked.Status != http.StatusOK || linked.Body["linked"] != true {
		t.Fatalf("link Auto Tournament: status=%d body=%s", linked.Status, linked.Raw)
	}
	if strings.Contains(string(linked.Raw), "test-service-token") ||
		strings.Contains(string(linked.Raw), "test-fleet-key") {
		t.Fatalf("link response exposed credentials: %s", linked.Raw)
	}
}

func TestAutoTournamentLinkRejectsUnauthorizedTokens(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{
		{name: "401", status: http.StatusUnauthorized},
		{name: "403", status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := newEnv(t)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || r.URL.Path != "/api/tournaments" {
					t.Errorf("unexpected Auto Tournament request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"error":"Invalid API token"}`))
			}))
			t.Cleanup(server.Close)
			e.srv.AutoTournamentHTTP = server.Client()

			path := fmt.Sprintf("/v1/groups/%s/integrations/autotournament", e.group)
			response := e.do(http.MethodPut, path, map[string]any{
				"base_url": server.URL, "api_token": "wrong-token",
			}, e.token)
			if response.Status != http.StatusBadRequest ||
				response.Body["message"] != "auto tournament rejected the token" {
				t.Fatalf("status=%d body=%s; want 400 auto tournament rejected the token",
					response.Status, response.Raw)
			}
			if requests != 1 {
				t.Fatalf("Auto Tournament requests = %d, want one Ping request", requests)
			}
			if _, err := e.st.GroupIntegration(context.Background(), e.group, "autotournament"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("integration row exists or lookup failed: %v", err)
			}
		})
	}
}

func TestAutoTournamentLinkRejectsUnauthorizedFleetKey(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{
		{name: "401", status: http.StatusUnauthorized},
		{name: "403", status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := newEnv(t)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/tournaments":
					_, _ = w.Write([]byte(`{"tournaments":[]}`))
				case r.Method == http.MethodPost && r.URL.Path == "/api/fleet/keys":
					w.WriteHeader(test.status)
					_, _ = w.Write([]byte(`{"error":"Invalid API token"}`))
				default:
					t.Errorf("unexpected Auto Tournament request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			e.srv.AutoTournamentHTTP = server.Client()

			path := fmt.Sprintf("/v1/groups/%s/integrations/autotournament", e.group)
			response := e.do(http.MethodPut, path, map[string]any{
				"base_url": server.URL, "api_token": "wrong-token",
			}, e.token)
			if response.Status != http.StatusBadRequest ||
				response.Body["message"] != "auto tournament rejected the token" {
				t.Fatalf("status=%d body=%s; want 400 auto tournament rejected the token",
					response.Status, response.Raw)
			}
			if requests != 2 {
				t.Fatalf("Auto Tournament requests = %d, want Ping and fleet-key requests", requests)
			}
			if _, err := e.st.GroupIntegration(context.Background(), e.group, "autotournament"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("integration row exists or lookup failed: %v", err)
			}
		})
	}
}

func TestAutoTournamentEndpointsRequireSecretsConfiguration(t *testing.T) {
	e := newEnv(t)
	e.srv.Secrets = nil
	groupPath := fmt.Sprintf("/v1/groups/%s", e.group)
	requests := []struct {
		method string
		path   string
		body   any
	}{
		{method: http.MethodGet, path: groupPath + "/integrations/autotournament"},
		{method: http.MethodPut, path: groupPath + "/integrations/autotournament", body: map[string]any{
			"base_url": "https://at.example.test", "api_token": "token",
		}},
		{method: http.MethodDelete, path: groupPath + "/integrations/autotournament"},
		{method: http.MethodGet, path: groupPath + "/tournaments"},
		{method: http.MethodPost, path: groupPath + "/tournaments", body: map[string]any{
			"name": "Needs secrets",
		}},
	}
	for _, request := range requests {
		response := e.do(request.method, request.path, request.body, e.token)
		if response.Status != http.StatusServiceUnavailable ||
			response.Body["code"] != "secrets_unconfigured" ||
			response.Body["message"] != "set VARDE_SECRETS_KEY (same value on every control plane replica) to use Auto Tournament" {
			t.Errorf("%s %s: status=%d body=%s; want 503 secrets_unconfigured",
				request.method, request.path, response.Status, response.Raw)
		}
	}
}

func TestAutoTournamentPartialImportStopsBeforeTournamentCreation(t *testing.T) {
	e := newEnv(t)
	server, created := newATWorkflowStub(t, e,
		[]byte(`{"errors":[{"playerId":"76561198000000001","error":"invalid player"}]}`), nil, 0)
	linkAT(t, e, server)
	addATMembers(t, e, 4, 4)

	path := fmt.Sprintf("/v1/groups/%s/tournaments", e.group)
	response := e.do(http.MethodPost, path, map[string]any{
		"name": "Partial import", "team_size": 2,
	}, e.token)
	if response.Status != http.StatusBadGateway ||
		response.Body["code"] != "auto_tournament_unavailable" ||
		!strings.Contains(response.Body["message"].(string), "Varde Player 1") ||
		!strings.Contains(response.Body["message"].(string), "invalid player") {
		t.Fatalf("partial import response: status=%d body=%s", response.Status, response.Raw)
	}
	if count := created.Load(); count != 0 {
		t.Fatalf("created %d tournaments after a partial import failure", count)
	}
}

func TestAutoTournamentPartialRegistrationIsReportedAsSkipped(t *testing.T) {
	e := newEnv(t)
	server, _ := newATWorkflowStub(t, e, nil,
		[]byte(`{"registered":3,"errors":[{"playerId":"76561198000000001","error":"already registered"}]}`), 0)
	linkAT(t, e, server)
	addATMembers(t, e, 4, 4)

	userIDs := []string{"usr_at_1", "usr_at_2", "usr_at_3", "usr_at_4"}
	path := fmt.Sprintf("/v1/groups/%s/tournaments", e.group)
	response := e.do(http.MethodPost, path, map[string]any{
		"name": "Partial registration", "team_size": 2, "user_ids": userIDs, "start": false,
	}, e.token)
	if response.Status != http.StatusCreated || response.Body["players"] != float64(3) ||
		response.Body["started"] != false {
		t.Fatalf("partial registration response: status=%d body=%s", response.Status, response.Raw)
	}
	skipped, ok := response.Body["skipped"].([]any)
	if !ok || !reflect.DeepEqual(skipped, []any{"Varde Player 1"}) {
		t.Fatalf("skipped = %#v, want Varde Player 1", response.Body["skipped"])
	}
}

func TestAutoTournamentStartFailureReturnsCreatedTournament(t *testing.T) {
	e := newEnv(t)
	server, _ := newATWorkflowStub(t, e, nil, nil, http.StatusBadGateway)
	linkAT(t, e, server)
	addATMembers(t, e, 4, 4)

	path := fmt.Sprintf("/v1/groups/%s/tournaments", e.group)
	response := e.do(http.MethodPost, path, map[string]any{
		"name": "Start failure", "team_size": 2,
	}, e.token)
	if response.Status != http.StatusCreated || response.Body["tournament_id"] != "101" ||
		response.Body["url"] != server.URL+"/tournament/101" ||
		response.Body["bracket_url"] != server.URL+"/tournament/101/bracket" ||
		response.Body["started"] != false ||
		!strings.Contains(response.Body["start_error"].(string), "start failed") {
		t.Fatalf("start failure response: status=%d body=%s", response.Status, response.Raw)
	}
}

func addATMembers(t *testing.T, e *env, steamCount, total int) []store.Member {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < total; i++ {
		userID := fmt.Sprintf("usr_at_%d", i+1)
		member := &store.User{
			ID: userID, Email: fmt.Sprintf("at%d@example.test", i+1),
			DisplayName: fmt.Sprintf("Varde Player %d", i+1), PasswordHash: "test",
			CreatedAt: e.clk.ms,
		}
		if err := e.st.CreateUser(ctx, member); err != nil {
			t.Fatal(err)
		}
		if i < steamCount {
			value := fmt.Sprintf("765611980000000%02d", i+1)
			steamID := &value
			if err := e.st.UpdateUserProfile(ctx, userID, nil, &steamID); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.st.AddMember(ctx, e.group, userID, "member", e.clk.ms); err != nil {
			t.Fatal(err)
		}
	}
	members, err := e.st.ListMembers(ctx, e.group)
	if err != nil {
		t.Fatal(err)
	}
	return members
}

func decodeCallBody(t *testing.T, call atCall) any {
	t.Helper()
	var body any
	if err := json.Unmarshal(call.body, &body); err != nil {
		t.Fatalf("decode Auto Tournament request body %q: %v", call.body, err)
	}
	return body
}

func TestAutoTournamentLinkAndTournamentFlows(t *testing.T) {
	e := newEnv(t)
	at, calls := newATStub(t, e)
	linkAT(t, e, at)
	addATMembers(t, e, 4, 4)

	groupPath := fmt.Sprintf("/v1/groups/%s", e.group)
	linkStatus := e.do(http.MethodGet, groupPath+"/integrations/autotournament", nil, e.token)
	if linkStatus.Status != http.StatusOK || linkStatus.Body["linked"] != true ||
		linkStatus.Body["base_url"] != at.URL {
		t.Fatalf("GET link: status=%d body=%s", linkStatus.Status, linkStatus.Raw)
	}
	list := e.do(http.MethodGet, groupPath+"/tournaments", nil, e.token)
	if list.Status != http.StatusOK {
		t.Fatalf("GET tournaments: status=%d body=%s", list.Status, list.Raw)
	}
	var listed []map[string]any
	if err := json.Unmarshal(list.Raw, &listed); err != nil || len(listed) != 1 ||
		listed[0]["url"] != at.URL+"/tournament/77" {
		t.Fatalf("GET tournaments body=%s, err=%v", list.Raw, err)
	}

	shuffle := e.do(http.MethodPost, groupPath+"/tournaments", map[string]any{
		"name": "Shuffle Test", "team_size": 2, "maps": []string{"de_mirage", "de_inferno"},
	}, e.token)
	if shuffle.Status != http.StatusCreated {
		t.Fatalf("POST shuffle: status=%d body=%s", shuffle.Status, shuffle.Raw)
	}
	if shuffle.Body["tournament_id"] != "101" || shuffle.Body["players"] != float64(4) ||
		shuffle.Body["started"] != true || len(shuffle.Body["skipped"].([]any)) != 1 {
		t.Fatalf("unexpected shuffle response: %s", shuffle.Raw)
	}
	if shuffle.Body["url"] != at.URL+"/tournament/101" ||
		shuffle.Body["bracket_url"] != at.URL+"/tournament/101/bracket" {
		t.Fatalf("unexpected shuffle URLs: %s", shuffle.Raw)
	}

	fixed := e.do(http.MethodPost, groupPath+"/tournaments", map[string]any{
		"name": "Fixed Test", "format": "single_elimination", "team_size": 2,
		"maps": []string{"de_mirage"}, "match_format": "bo1",
	}, e.token)
	if fixed.Status != http.StatusCreated || fixed.Body["tournament_id"] != "102" {
		t.Fatalf("POST fixed bracket: status=%d body=%s", fixed.Status, fixed.Raw)
	}

	if len(*calls) != 11 {
		t.Fatalf("Auto Tournament request count = %d, want 11: %+v", len(*calls), *calls)
	}
	for i, call := range *calls {
		if call.auth != "Bearer test-service-token" {
			t.Errorf("request %d Authorization = %q", i, call.auth)
		}
	}
	wantEndpoints := []struct{ method, path string }{
		{http.MethodGet, "/api/tournaments"},
		{http.MethodPost, "/api/fleet/keys"},
		{http.MethodGet, "/api/tournaments"},
		{http.MethodPost, "/api/players/bulk-import"},
		{http.MethodPost, "/api/tournaments"},
		{http.MethodPost, "/api/tournament/101/register-players"},
		{http.MethodPost, "/api/tournament/start"},
		{http.MethodPost, "/api/players/bulk-import"},
		{http.MethodPost, "/api/teams"},
		{http.MethodPost, "/api/tournaments"},
		{http.MethodPost, "/api/tournament/start"},
	}
	for i, want := range wantEndpoints {
		if (*calls)[i].method != want.method || (*calls)[i].path != want.path {
			t.Errorf("request %d = %s %s, want %s %s", i, (*calls)[i].method, (*calls)[i].path, want.method, want.path)
		}
	}
	if got := decodeCallBody(t, (*calls)[1]); !jsonObjectHas(got, "autoLink", true) ||
		!jsonObjectHas(got, "name", "varde-g1") {
		t.Errorf("fleet key request = %#v", got)
	}
	shuffleCreate := decodeCallBody(t, (*calls)[4])
	if !jsonObjectHas(shuffleCreate, "type", "shuffle") ||
		!jsonObjectHas(shuffleCreate, "teamSize", float64(2)) ||
		!jsonObjectHas(shuffleCreate, "maxRounds", float64(24)) ||
		!jsonObjectHas(shuffleCreate, "overtimeMode", "enabled") {
		t.Errorf("shuffle create body = %#v", shuffleCreate)
	}
	if (*calls)[5].header != "" || (*calls)[6].header != "101" ||
		(*calls)[9].header != "" || (*calls)[10].header != "102" {
		t.Errorf("unexpected shuffle/fixed tournament headers: %+v", *calls)
	}
	if got := decodeCallBody(t, (*calls)[7]); !jsonArrayOfMaps(got, 4) {
		t.Errorf("fixed import players body = %#v", got)
	}
	teams, ok := decodeCallBody(t, (*calls)[8]).([]any)
	if !ok || len(teams) != 2 {
		t.Fatalf("fixed teams body = %#v, want two teams", teams)
	}
	for i, value := range teams {
		team := value.(map[string]any)
		if team["name"] != fmt.Sprintf("Team %d", i+1) || team["tag"] != fmt.Sprintf("T%d", i+1) ||
			len(team["players"].([]any)) != 2 {
			t.Errorf("fixed team %d = %#v", i+1, team)
		}
	}
	bracket := decodeCallBody(t, (*calls)[9])
	if !jsonObjectHas(bracket, "type", "single_elimination") ||
		!jsonObjectHas(bracket, "format", "bo1") {
		t.Errorf("fixed bracket body = %#v", bracket)
	}
}

func TestAutoTournamentValidation(t *testing.T) {
	t.Run("minimum Steam ID players", func(t *testing.T) {
		e := newEnv(t)
		at, _ := newATStub(t, e)
		linkAT(t, e, at)
		addATMembers(t, e, 1, 1)
		path := fmt.Sprintf("/v1/groups/%s/tournaments", e.group)
		response := e.do(http.MethodPost, path, map[string]any{
			"name": "Too Few", "team_size": 2,
		}, e.token)
		if response.Status != http.StatusBadRequest ||
			response.Body["message"] != "need at least 4 players with a Steam ID" {
			t.Fatalf("minimum player validation: status=%d body=%s", response.Status, response.Raw)
		}
	})

	t.Run("selection and fixed team validation", func(t *testing.T) {
		e := newEnv(t)
		at, calls := newATStub(t, e)
		linkAT(t, e, at)
		members := addATMembers(t, e, 5, 6)
		path := fmt.Sprintf("/v1/groups/%s/tournaments", e.group)

		notMember := e.do(http.MethodPost, path, map[string]any{
			"name": "Not a member", "user_ids": []string{"usr_unknown"},
		}, e.token)
		if notMember.Status != http.StatusBadRequest ||
			notMember.Body["message"] != "selected user is not a group member" {
			t.Fatalf("nonmember validation: status=%d body=%s", notMember.Status, notMember.Raw)
		}

		var noSteamID string
		for _, member := range members {
			if member.DisplayName == "Varde Player 6" && member.SteamID == nil {
				noSteamID = member.UserID
				break
			}
		}
		if noSteamID == "" {
			t.Fatal("test member Varde Player 6 has a Steam ID")
		}
		missingSteam := e.do(http.MethodPost, path, map[string]any{
			"name": "Missing ID", "user_ids": []string{noSteamID},
		}, e.token)
		if missingSteam.Status != http.StatusBadRequest ||
			!strings.Contains(missingSteam.Body["message"].(string), "Varde Player 6") {
			t.Fatalf("missing Steam ID validation: status=%d body=%s", missingSteam.Status, missingSteam.Raw)
		}

		notDivisible := e.do(http.MethodPost, path, map[string]any{
			"name": "Odd fixed", "format": "single_elimination", "team_size": 2,
		}, e.token)
		if notDivisible.Status != http.StatusBadRequest ||
			!strings.Contains(notDivisible.Body["message"].(string), "divisible by team_size") {
			t.Fatalf("fixed team divisibility: status=%d body=%s", notDivisible.Status, notDivisible.Raw)
		}
		if len(*calls) != 2 {
			t.Fatalf("validation reached AT after link ping/key creation: calls=%+v", *calls)
		}
	})
}

func TestAutoTournamentTeamSizeBounds(t *testing.T) {
	tests := []struct {
		name        string
		teamSize    int
		playerCount int
		wantStatus  int
	}{
		{name: "below minimum", teamSize: 1, wantStatus: http.StatusBadRequest},
		{name: "minimum", teamSize: 2, playerCount: 4, wantStatus: http.StatusCreated},
		{name: "maximum", teamSize: 5, playerCount: 10, wantStatus: http.StatusCreated},
		{name: "above maximum", teamSize: 6, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := newEnv(t)
			at, _ := newATStub(t, e)
			linkAT(t, e, at)
			if test.playerCount > 0 {
				addATMembers(t, e, test.playerCount, test.playerCount)
			}

			path := fmt.Sprintf("/v1/groups/%s/tournaments", e.group)
			response := e.do(http.MethodPost, path, map[string]any{
				"name": "Boundary", "format": "single_elimination", "team_size": test.teamSize,
			}, e.token)
			if response.Status != test.wantStatus {
				t.Fatalf("team_size %d: status=%d body=%s, want %d",
					test.teamSize, response.Status, response.Raw, test.wantStatus)
			}
			if test.wantStatus == http.StatusBadRequest &&
				response.Body["message"] != "team size must be 2–5" {
				t.Fatalf("team_size %d: message=%v, want team size must be 2–5",
					test.teamSize, response.Body["message"])
			}
			if test.wantStatus == http.StatusCreated &&
				response.Body["players"] != float64(test.playerCount) {
				t.Fatalf("team_size %d: players=%v, want %d",
					test.teamSize, response.Body["players"], test.playerCount)
			}
		})
	}
}

func jsonObjectHas(value any, key string, want any) bool {
	object, ok := value.(map[string]any)
	return ok && object[key] == want
}

func jsonArrayOfMaps(value any, length int) bool {
	array, ok := value.([]any)
	if !ok || len(array) != length {
		return false
	}
	for _, item := range array {
		if _, ok := item.(map[string]any); !ok {
			return false
		}
	}
	return true
}
