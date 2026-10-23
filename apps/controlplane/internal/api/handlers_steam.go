package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/outbound"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

const (
	defaultSteamOpenIDURL = "https://steamcommunity.com/openid/login"
	defaultSteamAPIBase   = "https://api.steampowered.com"
	steamOpenIDNamespace  = "http://specs.openid.net/auth/2.0"
	steamLinkStateTTL     = 10 * time.Minute
)

var steamIDPattern = regexp.MustCompile(`^https://steamcommunity\.com/openid/id/(7656119\d{10})$`)

func (s *Server) steamOpenIDEndpoint() string {
	if s.SteamOpenIDURL != "" {
		return s.SteamOpenIDURL
	}
	return defaultSteamOpenIDURL
}

func (s *Server) steamWebAPIBase() string {
	if s.SteamAPIBase != "" {
		return s.SteamAPIBase
	}
	return defaultSteamAPIBase
}

func steamReturnTo(base, nonce string) string {
	return strings.TrimRight(base, "/") + "/v1/auth/steam/callback?state=" + url.QueryEscape(nonce)
}

func (s *Server) CreateSteamLink(ctx context.Context, _ gen.CreateSteamLinkRequestObject) (gen.CreateSteamLinkResponseObject, error) {
	u, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	nonce := hex.EncodeToString(random)
	expiresAt := s.Store.NowMs()/1000 + int64(steamLinkStateTTL.Seconds())
	if err := s.Store.CreateSteamLinkState(ctx, nonce, u.ID, expiresAt); err != nil {
		return nil, err
	}
	base := s.publicBase(ctx)
	endpoint, err := url.Parse(s.steamOpenIDEndpoint())
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("openid.ns", steamOpenIDNamespace)
	query.Set("openid.mode", "checkid_setup")
	query.Set("openid.return_to", steamReturnTo(base, nonce))
	query.Set("openid.realm", base)
	query.Set("openid.identity", steamOpenIDNamespace+"/identifier_select")
	query.Set("openid.claimed_id", steamOpenIDNamespace+"/identifier_select")
	endpoint.RawQuery = query.Encode()
	return gen.CreateSteamLink200JSONResponse{RedirectUrl: endpoint.String()}, nil
}

func (s *Server) UnlinkSteam(ctx context.Context, _ gen.UnlinkSteamRequestObject) (gen.UnlinkSteamResponseObject, error) {
	u, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetUserSteam(ctx, u.ID, nil, nil, nil); err != nil {
		return nil, err
	}
	return gen.UnlinkSteam204Response{}, nil
}

func (s *Server) steamCallback(w http.ResponseWriter, r *http.Request) {
	redirect := func(code string) {
		query := url.Values{}
		query.Set("steam_error", code)
		http.Redirect(w, r, s.publicBase(r.Context())+"/settings?"+query.Encode(), http.StatusSeeOther)
	}
	success := func() {
		query := url.Values{}
		query.Set("steam", "linked")
		http.Redirect(w, r, s.publicBase(r.Context())+"/settings?"+query.Encode(), http.StatusSeeOther)
	}

	u := userFrom(r.Context())
	if u == nil {
		redirect("session")
		return
	}
	nonce := r.URL.Query().Get("state")
	stateUserID, err := s.Store.ConsumeSteamLinkState(r.Context(), nonce, s.Store.NowMs()/1000)
	if err != nil || stateUserID != u.ID {
		redirect("expired")
		return
	}
	values := r.URL.Query()
	if values.Get("openid.mode") != "id_res" {
		redirect("cancelled")
		return
	}
	endpoint := s.steamOpenIDEndpoint()
	if values.Get("openid.op_endpoint") != endpoint {
		redirect("invalid")
		return
	}
	base := s.publicBase(r.Context())
	if values.Get("openid.return_to") != steamReturnTo(base, nonce) {
		redirect("invalid")
		return
	}
	claimedID := values.Get("openid.claimed_id")
	identity := values.Get("openid.identity")
	match := steamIDPattern.FindStringSubmatch(claimedID)
	if claimedID != identity || len(match) != 2 {
		redirect("invalid")
		return
	}
	verified, err := s.verifySteamOpenID(r.Context(), endpoint, values)
	if err != nil {
		redirect("unavailable")
		return
	}
	if !verified {
		redirect("invalid")
		return
	}

	steamID := match[1]
	persona, avatar := s.fetchSteamProfile(r.Context(), steamID)
	if err := s.Store.SetUserSteam(r.Context(), u.ID, &steamID, persona, avatar); err != nil {
		if errors.Is(err, store.ErrSteamIDTaken) {
			redirect("taken")
			return
		}
		redirect("unavailable")
		return
	}
	success()
}

func (s *Server) verifySteamOpenID(ctx context.Context, endpoint string, received url.Values) (bool, error) {
	form := make(url.Values)
	for key, values := range received {
		if strings.HasPrefix(key, "openid.") {
			form[key] = append([]string(nil), values...)
		}
	}
	form.Set("openid.mode", "check_authentication")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return false, nil
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := s.steamHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := outbound.ReadLimited(resp.Body)
	if readErr != nil || resp.StatusCode != http.StatusOK {
		return false, nil
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSuffix(line, "\r") == "is_valid:true" {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) fetchSteamProfile(ctx context.Context, steamID string) (*string, *string) {
	if s.Cfg.SteamAPIKey == "" {
		return nil, nil
	}
	profileURL, err := url.Parse(strings.TrimRight(s.steamWebAPIBase(), "/") + "/ISteamUser/GetPlayerSummaries/v2/")
	if err != nil {
		s.warnSteamProfileFailure()
		return nil, nil
	}
	query := profileURL.Query()
	query.Set("key", s.Cfg.SteamAPIKey)
	query.Set("steamids", steamID)
	profileURL.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, profileURL.String(), nil)
	if err != nil {
		s.warnSteamProfileFailure()
		return nil, nil
	}
	resp, err := s.steamHTTPClient().Do(req)
	if err != nil {
		s.warnSteamProfileFailure()
		return nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := outbound.ReadLimited(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		s.warnSteamProfileFailure()
		return nil, nil
	}
	var result struct {
		Response struct {
			Players []struct {
				PersonaName string `json:"personaname"`
				AvatarFull  string `json:"avatarfull"`
			} `json:"players"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		s.warnSteamProfileFailure()
		return nil, nil
	}
	if len(result.Response.Players) == 0 {
		return nil, nil
	}
	player := result.Response.Players[0]
	persona := truncateRunes(player.PersonaName, 256)
	var personaValue *string
	if persona != "" {
		personaValue = &persona
	}
	avatar := truncateRunes(player.AvatarFull, 256)
	avatarURL, err := url.Parse(avatar)
	if err != nil || avatarURL.Scheme != "https" || avatarURL.Host == "" {
		return personaValue, nil
	}
	return personaValue, &avatar
}

func (s *Server) steamHTTPClient() *http.Client {
	if s.PluginHTTP != nil {
		return s.PluginHTTP
	}
	return outbound.NewClient(10 * time.Second)
}

func (s *Server) warnSteamProfileFailure() {
	if s.Log != nil {
		s.Log.Warn("Steam profile lookup failed")
	}
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) > max {
		return string(runes[:max])
	}
	return value
}
