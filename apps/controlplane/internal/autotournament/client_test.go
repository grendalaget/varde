package autotournament

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
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
	if failures, err := client.ImportPlayers(ctx, players); err != nil {
		t.Fatal(err)
	} else if len(failures) != 0 {
		t.Fatalf("ImportPlayers() failures = %+v, want none", failures)
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
	if failures, err := client.RegisterPlayers(ctx, "42", []string{players[0].ID}); err != nil {
		t.Fatal(err)
	} else if len(failures) != 0 {
		t.Fatalf("RegisterPlayers() failures = %+v, want none", failures)
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

func TestPlayerFailuresAreReturned(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/players/bulk-import", "/api/tournament/42/register-players":
			_, _ = w.Write([]byte(`{"errors":[{"playerId":"76561198000000001","name":"Player One","error":"already exists"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}

	importFailures, err := client.ImportPlayers(context.Background(), []Player{{ID: "76561198000000001", Name: "Player One"}})
	if err != nil || !reflect.DeepEqual(importFailures, []PlayerFailure{{
		ID: "76561198000000001", Name: "Player One", Message: "already exists",
	}}) {
		t.Fatalf("ImportPlayers() = %+v, %v", importFailures, err)
	}
	registrationFailures, err := client.RegisterPlayers(context.Background(), "42", []string{"76561198000000001"})
	if err != nil || !reflect.DeepEqual(registrationFailures, importFailures) {
		t.Fatalf("RegisterPlayers() = %+v, %v; want %+v", registrationFailures, err, importFailures)
	}
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

func TestPrivateAddressPolicy(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"tournaments":[]}`))
	}))
	defer server.Close()

	blocked, err := NewClientWithOptions(server.URL, "token", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := blocked.Ping(context.Background()); err == nil || err.Error() != privateNetworkError {
		t.Fatalf("Ping() error = %v, want private-address policy error", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("blocked request reached the server %d times", got)
	}

	allowed, err := NewClientWithOptions(server.URL, "token", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := allowed.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() with private addresses enabled: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("allowed request reached the server %d times, want 1", got)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var destinationRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationRequests.Add(1)
		_, _ = w.Write([]byte(`{"tournaments":[]}`))
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer redirect.Close()

	client, err := NewClientWithOptions(redirect.URL, "token", true)
	if err != nil {
		t.Fatal(err)
	}
	err = client.Ping(context.Background())
	if err == nil || err.Error() != "auto tournament redirected; use the final URL" {
		t.Fatalf("Ping() error = %v, want redirect error", err)
	}
	if got := destinationRequests.Load(); got != 0 {
		t.Fatalf("redirect destination received %d requests, want 0", got)
	}
}

func TestIsPrivateAddress(t *testing.T) {
	for _, test := range []struct {
		address string
		want    bool
	}{
		{address: "127.0.0.1", want: true},
		{address: "10.1.2.3", want: true},
		{address: "192.168.1.1", want: true},
		{address: "172.31.1.1", want: true},
		{address: "224.0.0.1", want: true},
		{address: "fc00::1", want: true},
		{address: "fe80::1", want: true},
		{address: "ff02::1", want: true},
		{address: "0.0.0.0", want: true},
		{address: "::", want: true},
		{address: "100.64.0.1", want: true},
		{address: "100.127.255.254", want: true},
		{address: "::ffff:127.0.0.1", want: true},
		{address: "0.1.2.3", want: true},
		{address: "192.0.0.1", want: true},
		{address: "192.0.2.1", want: true},
		{address: "198.18.0.1", want: true},
		{address: "198.51.100.1", want: true},
		{address: "203.0.113.1", want: true},
		{address: "240.0.0.1", want: true},
		{address: "255.255.255.255", want: true},
		{address: "64:ff9b::1", want: true},
		{address: "64:ff9b:1::1", want: true},
		{address: "2001:db8::1", want: true},
		{address: "2002::1", want: true},
		{address: "100::1", want: true},
		{address: "100.63.255.255", want: false},
		{address: "100.128.0.1", want: false},
		{address: "1.1.1.1", want: false},
		{address: "8.8.8.8", want: false},
		{address: "2606:4700::1111", want: false},
		{address: "2606:4700:4700::1111", want: false},
	} {
		t.Run(test.address, func(t *testing.T) {
			addr, err := netip.ParseAddr(test.address)
			if err != nil {
				t.Fatal(err)
			}
			if got := isPrivateAddress(addr); got != test.want {
				t.Errorf("isPrivateAddress(%s) = %t, want %t", addr, got, test.want)
			}
		})
	}
}

func TestListTournamentsResponseBodyIsLimited(t *testing.T) {
	body := &countingBody{
		reader: strings.NewReader(`{"tournaments":[]}` + strings.Repeat(" ", maxResponseBodyBytes)),
	}
	client, err := NewClientWithOptions("https://at.example.test", "token", true)
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP = &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: request,
		}, nil
	})}
	if _, err := client.ListTournaments(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body.read != maxResponseBodyBytes {
		t.Fatalf("read %d response bytes, want limit %d", body.read, maxResponseBodyBytes)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type countingBody struct {
	reader io.Reader
	read   int
}

func (b *countingBody) Read(buffer []byte) (int, error) {
	n, err := b.reader.Read(buffer)
	b.read += n
	return n, err
}

func (*countingBody) Close() error { return nil }
