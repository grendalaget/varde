package autotournament

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

var ErrUnauthorized = errors.New("auto tournament unauthorized")

const maxResponseBodyBytes = 1 << 20

const privateNetworkError = "auto tournament address is on a private network; this control plane doesn't allow that"

var errPrivateNetwork = errors.New(privateNetworkError)

type Client struct {
	BaseURL      string
	Token        string
	HTTP         *http.Client
	allowPrivate bool
}

type Player struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Tournament struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

type rawTournament struct {
	ID     json.RawMessage `json:"id"`
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Status string          `json:"status"`
}

type TeamPlayer struct {
	SteamID string `json:"steamId"`
	Name    string `json:"name"`
}

type Team struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Tag     string       `json:"tag"`
	Players []TeamPlayer `json:"players"`
}

type CreateShuffleRequest struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	TeamSize     int      `json:"teamSize"`
	MapSequence  []string `json:"mapSequence"`
	MaxRounds    int      `json:"maxRounds"`
	OvertimeMode string   `json:"overtimeMode"`
}

type CreateBracketRequest struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Format   string   `json:"format"`
	TeamSize int      `json:"teamSize"`
	Maps     []string `json:"maps"`
	TeamIDs  []string `json:"teamIds"`
}

func NormalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("base URL must use http or https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("base URL must not include credentials, query, or fragment")
	}
	return strings.TrimRight(trimmed, "/"), nil
}

func NewClient(baseURL, token string) (*Client, error) {
	return NewClientWithOptions(baseURL, token, true)
}

// NewClientWithOptions constructs an AT client with an explicit private-network policy.
func NewClientWithOptions(baseURL, token string, allowPrivate bool) (*Client, error) {
	normalized, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{
		BaseURL:      normalized,
		Token:        token,
		HTTP:         newHTTPClient(allowPrivate),
		allowPrivate: allowPrivate,
	}, nil
}

func newHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = rejectPrivateAddress
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       dialer.DialContext,
			ForceAttemptHTTP2: true,
		},
		CheckRedirect: rejectRedirect,
	}
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func rejectPrivateAddress(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errPrivateNetwork
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || isPrivateAddress(addr) {
		return errPrivateNetwork
	}
	return nil
}

func isPrivateAddress(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	addr = addr.Unmap()
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsUnspecified() || addr.IsMulticast() {
		return true
	}
	if addr.Is4() {
		octets := addr.As4()
		return octets[0] == 100 && octets[1] >= 64 && octets[1] <= 127
	}
	return false
}

func (c *Client) Ping(ctx context.Context) error {
	_, status, err := c.do(ctx, http.MethodGet, "/api/tournaments", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return responseError(status, nil)
	}
	return nil
}

func (c *Client) CreateFleetKey(ctx context.Context, name string) (string, error) {
	body, status, err := c.do(ctx, http.MethodPost, "/api/fleet/keys",
		map[string]any{"name": name, "autoLink": true}, nil)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", responseError(status, body)
	}
	var result struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("auto tournament decode fleet key: %w", err)
	}
	if result.Value == "" {
		return "", errors.New("auto tournament response did not include fleet key value")
	}
	return result.Value, nil
}

func (c *Client) ImportPlayers(ctx context.Context, players []Player) error {
	body, status, err := c.do(ctx, http.MethodPost, "/api/players/bulk-import", players, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return responseError(status, body)
	}
	return nil
}

func (c *Client) CreateShuffle(ctx context.Context, name string, teamSize int, maps []string) (Tournament, error) {
	request := CreateShuffleRequest{
		Name: name, Type: "shuffle", TeamSize: teamSize, MapSequence: maps,
		MaxRounds: 24, OvertimeMode: "enabled",
	}
	return c.createTournament(ctx, request)
}

func (c *Client) CreateTeams(ctx context.Context, teams []Team) error {
	body, status, err := c.do(ctx, http.MethodPost, "/api/teams", teams, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return responseError(status, body)
	}
	return nil
}

func (c *Client) CreateBracket(ctx context.Context, name, tournamentType, format string, teamSize int, maps, teamIDs []string) (Tournament, error) {
	request := CreateBracketRequest{
		Name: name, Type: tournamentType, Format: format, TeamSize: teamSize,
		Maps: maps, TeamIDs: teamIDs,
	}
	return c.createTournament(ctx, request)
}

func (c *Client) RegisterPlayers(ctx context.Context, id string, steamIDs []string) error {
	body, status, err := c.do(ctx, http.MethodPost, "/api/tournament/"+url.PathEscape(id)+"/register-players",
		map[string][]string{"playerIds": steamIDs}, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return responseError(status, body)
	}
	return nil
}

func (c *Client) Start(ctx context.Context, id string) error {
	headers := http.Header{"X-Tournament-Id": []string{id}}
	body, status, err := c.do(ctx, http.MethodPost, "/api/tournament/start", map[string]any{}, headers)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return responseError(status, body)
	}
	return nil
}

func (c *Client) ListTournaments(ctx context.Context) ([]Tournament, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/api/tournaments", nil, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, responseError(status, body)
	}
	var result struct {
		Tournaments []rawTournament `json:"tournaments"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("auto tournament decode tournaments: %w", err)
	}
	tournaments := make([]Tournament, 0, len(result.Tournaments))
	for _, tournament := range result.Tournaments {
		id, err := decodeID(tournament.ID)
		if err != nil {
			return nil, fmt.Errorf("auto tournament decode tournament id: %w", err)
		}
		tournaments = append(tournaments, Tournament{
			ID: id, Name: tournament.Name, Type: tournament.Type, Status: tournament.Status,
		})
	}
	return tournaments, nil
}

func (c *Client) createTournament(ctx context.Context, payload any) (Tournament, error) {
	body, status, err := c.do(ctx, http.MethodPost, "/api/tournaments", payload, nil)
	if err != nil {
		return Tournament{}, err
	}
	if status < 200 || status >= 300 {
		return Tournament{}, responseError(status, body)
	}
	var result struct {
		Tournament rawTournament `json:"tournament"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return Tournament{}, fmt.Errorf("auto tournament decode created tournament: %w", err)
	}
	if len(result.Tournament.ID) == 0 {
		return Tournament{}, errors.New("auto tournament response did not include tournament id")
	}
	id, err := decodeID(result.Tournament.ID)
	if err != nil {
		return Tournament{}, fmt.Errorf("auto tournament decode created tournament id: %w", err)
	}
	if id == "" {
		return Tournament{}, errors.New("auto tournament response did not include tournament id")
	}
	return Tournament{ID: id, Name: result.Tournament.Name, Type: result.Tournament.Type}, nil
}

func decodeID(raw json.RawMessage) (string, error) {
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return "", err
	}
	return number.String(), nil
}

func (c *Client) do(ctx context.Context, method, path string, payload any, headers http.Header) ([]byte, int, error) {
	baseURL, err := NormalizeBaseURL(c.BaseURL)
	if err != nil {
		return nil, 0, err
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, fmt.Errorf("auto tournament encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		return nil, 0, fmt.Errorf("auto tournament create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	client := c.HTTP
	if client == nil {
		client = newHTTPClient(c.allowPrivate)
	}
	clientCopy := *client
	clientCopy.CheckRedirect = rejectRedirect
	response, err := clientCopy.Do(req)
	if err != nil {
		if errors.Is(err, errPrivateNetwork) {
			return nil, 0, errPrivateNetwork
		}
		return nil, 0, fmt.Errorf("auto tournament request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		return nil, response.StatusCode, errors.New("auto tournament redirected; use the final URL")
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBodyBytes))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("auto tournament read response: %w", err)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return responseBody, response.StatusCode, fmt.Errorf("%w: %s", ErrUnauthorized, responseMessage(responseBody))
	}
	return responseBody, response.StatusCode, nil
}

func responseMessage(body []byte) string {
	var response struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &response)
	return response.Error
}

func responseError(status int, body []byte) error {
	message := responseMessage(body)
	if message == "" {
		return fmt.Errorf("auto tournament HTTP %d", status)
	}
	return fmt.Errorf("auto tournament HTTP %d: %s", status, message)
}
