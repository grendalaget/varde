package api_test

import (
	"context"
	"database/sql"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

const testSteamID = "76561198000000000"

type steamOpenIDFixture struct {
	server   *httptest.Server
	requests chan url.Values
}

func newSteamOpenIDFixture(t *testing.T, status int, response string) *steamOpenIDFixture {
	t.Helper()
	fixture := &steamOpenIDFixture{requests: make(chan url.Values, 8)}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		form := make(url.Values, len(r.Form))
		for key, values := range r.Form {
			form[key] = append([]string(nil), values...)
		}
		fixture.requests <- form
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

type steamAPIFixture struct {
	server   *httptest.Server
	requests chan url.Values
}

func newSteamAPIFixture(t *testing.T, status int, response string) *steamAPIFixture {
	t.Helper()
	fixture := &steamAPIFixture{requests: make(chan url.Values, 8)}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ISteamUser/GetPlayerSummaries/v2/" {
			http.NotFound(w, r)
			return
		}
		fixture.requests <- r.URL.Query()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func beginSteamLink(t *testing.T, e *env, token string) (redirectURL, returnTo, nonce string) {
	t.Helper()
	response := e.do(http.MethodPost, "/v1/me/steam/link", nil, token)
	if response.Status != http.StatusOK {
		t.Fatalf("POST /v1/me/steam/link = %d %s, want 200", response.Status, response.Raw)
	}
	redirectURL = response.Body["redirect_url"].(string)
	parsed, err := url.Parse(redirectURL)
	if err != nil {
		t.Fatal(err)
	}
	returnTo = parsed.Query().Get("openid.return_to")
	callback, err := url.Parse(returnTo)
	if err != nil {
		t.Fatal(err)
	}
	nonce = callback.Query().Get("state")
	return redirectURL, returnTo, nonce
}

func steamCallbackValues(endpoint, returnTo, nonce string) url.Values {
	return url.Values{
		"state":              {nonce},
		"openid.ns":          {"http://specs.openid.net/auth/2.0"},
		"openid.mode":        {"id_res"},
		"openid.op_endpoint": {endpoint},
		"openid.return_to":   {returnTo},
		"openid.identity":    {"https://steamcommunity.com/openid/id/" + testSteamID},
		"openid.claimed_id":  {"https://steamcommunity.com/openid/id/" + testSteamID},
		"openid.signed":      {"op_endpoint,return_to,identity,claimed_id"},
		"openid.sig":         {"test-signature"},
	}
}

func callSteamCallback(t *testing.T, e *env, values url.Values, token string) (int, *url.URL) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.http.URL+"/v1/auth/steam/callback?"+values.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("callback Location = %q: %v", response.Header.Get("Location"), err)
	}
	return response.StatusCode, location
}

func expectSteamError(t *testing.T, e *env, values url.Values, token, want string) {
	t.Helper()
	status, location := callSteamCallback(t, e, values, token)
	if status != http.StatusSeeOther || location.Query().Get("steam_error") != want {
		t.Fatalf("callback = %d %s, want 303 steam_error=%s", status, location, want)
	}
}

func TestSteamLinkURLContainsOpenIDParameters(t *testing.T) {
	e := newEnv(t)
	e.srv.SteamOpenIDURL = "https://steam.example.test/openid/login"
	redirectURL, returnTo, nonce := beginSteamLink(t, e, e.token)
	parsed, err := url.Parse(redirectURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"openid.ns":         "http://specs.openid.net/auth/2.0",
		"openid.mode":       "checkid_setup",
		"openid.return_to":  returnTo,
		"openid.realm":      e.http.URL,
		"openid.identity":   "http://specs.openid.net/auth/2.0/identifier_select",
		"openid.claimed_id": "http://specs.openid.net/auth/2.0/identifier_select",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	raw, err := hex.DecodeString(nonce)
	if err != nil || len(raw) != 32 || len(nonce) != 64 {
		t.Fatalf("state nonce = %q, want 32 random bytes encoded as hex", nonce)
	}
	if parsed.Scheme != "https" || parsed.Host != "steam.example.test" {
		t.Fatalf("redirect URL = %s, want configured Steam endpoint", parsed)
	}
}

func TestSteamOpenIDCallbackLinksWithProfile(t *testing.T) {
	e := newEnv(t)
	openID := newSteamOpenIDFixture(t, http.StatusOK, "ns:http://specs.openid.net/auth/2.0\nis_valid:true\n")
	profile := newSteamAPIFixture(t, http.StatusOK,
		`{"response":{"players":[{"personaname":"Steam Player","avatarfull":"https://cdn.example.test/avatar.png"}]}}`)
	e.srv.SteamOpenIDURL = openID.server.URL
	e.srv.SteamAPIBase = profile.server.URL
	e.srv.Cfg.SteamAPIKey = "test-key"

	_, returnTo, nonce := beginSteamLink(t, e, e.token)
	status, location := callSteamCallback(t, e, steamCallbackValues(openID.server.URL, returnTo, nonce), e.token)
	if status != http.StatusSeeOther || location.Query().Get("steam") != "linked" {
		t.Fatalf("callback = %d %s, want 303 steam=linked", status, location)
	}
	select {
	case form := <-openID.requests:
		if form.Get("openid.mode") != "check_authentication" ||
			form.Get("openid.return_to") != returnTo ||
			form.Get("openid.sig") != "test-signature" {
			t.Errorf("OpenID verification form = %v, want received parameters and check_authentication mode", form)
		}
	default:
		t.Fatal("Steam OpenID verification request was not made")
	}
	select {
	case query := <-profile.requests:
		if query.Get("key") != "test-key" || query.Get("steamids") != testSteamID {
			t.Errorf("Steam profile query = %v, want key and Steam ID", query)
		}
	default:
		t.Fatal("Steam profile request was not made")
	}
	me := e.do(http.MethodGet, "/v1/me", nil, e.token)
	if me.Status != http.StatusOK {
		t.Fatalf("GET /v1/me = %d %s, want 200", me.Status, me.Raw)
	}
	user := me.Body["user"].(map[string]any)
	if user["steam_id"] != testSteamID ||
		user["steam_persona_name"] != "Steam Player" ||
		user["steam_avatar_url"] != "https://cdn.example.test/avatar.png" {
		t.Fatalf("linked Steam profile = %v", user)
	}
}

func TestSteamOpenIDCallbackWithoutAPIKeyStoresIDOnly(t *testing.T) {
	e := newEnv(t)
	openID := newSteamOpenIDFixture(t, http.StatusOK, "is_valid:true\n")
	e.srv.SteamOpenIDURL = openID.server.URL
	_, returnTo, nonce := beginSteamLink(t, e, e.token)
	status, location := callSteamCallback(t, e, steamCallbackValues(openID.server.URL, returnTo, nonce), e.token)
	if status != http.StatusSeeOther || location.Query().Get("steam") != "linked" {
		t.Fatalf("callback = %d %s, want 303 steam=linked", status, location)
	}
	me := e.do(http.MethodGet, "/v1/me", nil, e.token)
	user := me.Body["user"].(map[string]any)
	if user["steam_id"] != testSteamID || user["steam_persona_name"] != nil || user["steam_avatar_url"] != nil {
		t.Fatalf("linked Steam ID without API key = %v", user)
	}
}

func TestSteamOpenIDCallbackRejectsInvalidAssertions(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		mutate    func(url.Values)
		wantError string
	}{
		{name: "is_valid false", response: "is_valid:false\n", wantError: "invalid"},
		{name: "tampered return_to", response: "is_valid:true\n", mutate: func(q url.Values) {
			q.Set("openid.return_to", q.Get("openid.return_to")+"x")
		}, wantError: "invalid"},
		{name: "tampered endpoint", response: "is_valid:true\n", mutate: func(q url.Values) {
			q.Set("openid.op_endpoint", q.Get("openid.op_endpoint")+"/other")
		}, wantError: "invalid"},
		{name: "identity mismatch", response: "is_valid:true\n", mutate: func(q url.Values) {
			q.Set("openid.identity", "https://steamcommunity.com/openid/id/76561198000000001")
		}, wantError: "invalid"},
		{name: "non-Steam claimed ID", response: "is_valid:true\n", mutate: func(q url.Values) {
			q.Set("openid.claimed_id", "https://example.test/openid/id/"+testSteamID)
			q.Set("openid.identity", "https://example.test/openid/id/"+testSteamID)
		}, wantError: "invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := newEnv(t)
			openID := newSteamOpenIDFixture(t, http.StatusOK, test.response)
			e.srv.SteamOpenIDURL = openID.server.URL
			_, returnTo, nonce := beginSteamLink(t, e, e.token)
			values := steamCallbackValues(openID.server.URL, returnTo, nonce)
			if test.mutate != nil {
				test.mutate(values)
			}
			expectSteamError(t, e, values, e.token, test.wantError)
		})
	}
}

func TestSteamOpenIDCallbackCancellation(t *testing.T) {
	e := newEnv(t)
	openID := newSteamOpenIDFixture(t, http.StatusOK, "is_valid:true\n")
	e.srv.SteamOpenIDURL = openID.server.URL
	_, returnTo, nonce := beginSteamLink(t, e, e.token)
	values := steamCallbackValues(openID.server.URL, returnTo, nonce)
	values.Set("openid.mode", "cancel")
	expectSteamError(t, e, values, e.token, "cancelled")
}

func TestSteamOpenIDCallbackStateAndSessionErrors(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		e := newEnv(t)
		openID := newSteamOpenIDFixture(t, http.StatusOK, "is_valid:true\n")
		e.srv.SteamOpenIDURL = openID.server.URL
		_, returnTo, nonce := beginSteamLink(t, e, e.token)
		if _, err := e.st.DB.Exec(`UPDATE steam_link_states SET expires_at=0 WHERE nonce=?`, nonce); err != nil {
			t.Fatal(err)
		}
		expectSteamError(t, e, steamCallbackValues(openID.server.URL, returnTo, nonce), e.token, "expired")
	})
	t.Run("another user", func(t *testing.T) {
		e := newEnv(t)
		openID := newSteamOpenIDFixture(t, http.StatusOK, "is_valid:true\n")
		e.srv.SteamOpenIDURL = openID.server.URL
		_, returnTo, nonce := beginSteamLink(t, e, e.token)
		otherToken := e.signup("other@example.com", "password123")
		expectSteamError(t, e, steamCallbackValues(openID.server.URL, returnTo, nonce), otherToken, "expired")
	})
	t.Run("no session", func(t *testing.T) {
		e := newEnv(t)
		openID := newSteamOpenIDFixture(t, http.StatusOK, "is_valid:true\n")
		e.srv.SteamOpenIDURL = openID.server.URL
		_, returnTo, nonce := beginSteamLink(t, e, e.token)
		expectSteamError(t, e, steamCallbackValues(openID.server.URL, returnTo, nonce), "", "session")
	})
	t.Run("reused nonce", func(t *testing.T) {
		e := newEnv(t)
		openID := newSteamOpenIDFixture(t, http.StatusOK, "is_valid:true\n")
		e.srv.SteamOpenIDURL = openID.server.URL
		_, returnTo, nonce := beginSteamLink(t, e, e.token)
		values := steamCallbackValues(openID.server.URL, returnTo, nonce)
		status, location := callSteamCallback(t, e, values, e.token)
		if status != http.StatusSeeOther || location.Query().Get("steam") != "linked" {
			t.Fatalf("first callback = %d %s, want linked", status, location)
		}
		expectSteamError(t, e, values, e.token, "expired")
	})
}

func TestSteamIDAlreadyLinkedRedirectsTaken(t *testing.T) {
	e := newEnv(t)
	openID := newSteamOpenIDFixture(t, http.StatusOK, "is_valid:true\n")
	e.srv.SteamOpenIDURL = openID.server.URL

	_, returnTo, nonce := beginSteamLink(t, e, e.token)
	status, location := callSteamCallback(t, e, steamCallbackValues(openID.server.URL, returnTo, nonce), e.token)
	if status != http.StatusSeeOther || location.Query().Get("steam") != "linked" {
		t.Fatalf("first callback = %d %s, want linked", status, location)
	}

	otherToken := e.signup("second@example.com", "password123")
	_, returnTo, nonce = beginSteamLink(t, e, otherToken)
	expectSteamError(t, e, steamCallbackValues(openID.server.URL, returnTo, nonce), otherToken, "taken")
}

func TestSteamProfileFetchFailureDoesNotBlockLink(t *testing.T) {
	e := newEnv(t)
	openID := newSteamOpenIDFixture(t, http.StatusOK, "is_valid:true\n")
	profile := newSteamAPIFixture(t, http.StatusInternalServerError, "temporarily unavailable")
	e.srv.SteamOpenIDURL = openID.server.URL
	e.srv.SteamAPIBase = profile.server.URL
	e.srv.Cfg.SteamAPIKey = "test-key"

	_, returnTo, nonce := beginSteamLink(t, e, e.token)
	status, location := callSteamCallback(t, e, steamCallbackValues(openID.server.URL, returnTo, nonce), e.token)
	if status != http.StatusSeeOther || location.Query().Get("steam") != "linked" {
		t.Fatalf("callback = %d %s, want 303 steam=linked", status, location)
	}
	me := e.do(http.MethodGet, "/v1/me", nil, e.token)
	user := me.Body["user"].(map[string]any)
	if user["steam_id"] != testSteamID || user["steam_persona_name"] != nil || user["steam_avatar_url"] != nil {
		t.Fatalf("linked Steam profile after API failure = %v", user)
	}
}

func TestUnlinkSteamClearsAllProfileFields(t *testing.T) {
	e := newEnv(t)
	me := e.do(http.MethodGet, "/v1/me", nil, e.token)
	userID := me.Body["user"].(map[string]any)["id"].(string)
	steamID := testSteamID
	persona := "Steam Player"
	avatar := "https://cdn.example.test/avatar.png"
	if err := e.st.SetUserSteam(context.Background(), userID, &steamID, &persona, &avatar); err != nil {
		t.Fatal(err)
	}
	response := e.do(http.MethodDelete, "/v1/me/steam", nil, e.token)
	if response.Status != http.StatusNoContent {
		t.Fatalf("DELETE /v1/me/steam = %d %s, want 204", response.Status, response.Raw)
	}
	stored, err := e.st.UserByID(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SteamID != nil || stored.SteamPersonaName != nil || stored.SteamAvatarURL != nil {
		t.Fatalf("unlinked Steam fields remain: id=%v persona=%v avatar=%v", stored.SteamID, stored.SteamPersonaName, stored.SteamAvatarURL)
	}
	var id, name, image sql.NullString
	if err := e.st.DB.QueryRow(`SELECT steam_id, steam_persona_name, steam_avatar_url FROM users WHERE id=?`, userID).
		Scan(&id, &name, &image); err != nil {
		t.Fatal(err)
	}
	if id.Valid || name.Valid || image.Valid {
		t.Fatalf("database Steam fields after unlink = (%v,%v,%v), want NULLs", id, name, image)
	}
}

func TestSteamLinkStateConsumptionIsSingleUse(t *testing.T) {
	e := newEnv(t)
	if err := e.st.CreateSteamLinkState(context.Background(), "nonce-once", "user", 2); err != nil {
		t.Fatal(err)
	}
	userID, err := e.st.ConsumeSteamLinkState(context.Background(), "nonce-once", 1)
	if err != nil || userID != "user" {
		t.Fatalf("ConsumeSteamLinkState() = %q, %v, want user", userID, err)
	}
	if _, err := e.st.ConsumeSteamLinkState(context.Background(), "nonce-once", 1); err != sql.ErrNoRows {
		t.Fatalf("second ConsumeSteamLinkState() error = %v, want sql.ErrNoRows", err)
	}
	if err := e.st.CreateSteamLinkState(context.Background(), "nonce-expired", "user", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.ConsumeSteamLinkState(context.Background(), "nonce-expired", 1); err != sql.ErrNoRows {
		t.Fatalf("expired ConsumeSteamLinkState() error = %v, want sql.ErrNoRows", err)
	}
}
