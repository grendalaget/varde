package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/autotournament"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

const autoTournamentKind = "autotournament"

type tournamentPlayer struct {
	steamID string
	name    string
}

func (s *Server) GetAutoTournamentLink(ctx context.Context, req gen.GetAutoTournamentLinkRequestObject) (gen.GetAutoTournamentLinkResponseObject, error) {
	if _, _, err := s.requireRole(ctx, req.GroupId, "member"); err != nil {
		return nil, err
	}
	integration, err := s.Store.GroupIntegration(ctx, req.GroupId, autoTournamentKind)
	if errors.Is(err, store.ErrNotFound) {
		return gen.GetAutoTournamentLink200JSONResponse{Linked: false}, nil
	}
	if err != nil {
		return nil, err
	}
	baseURL := integration.BaseURL
	linkedAt := integration.CreatedAt
	return gen.GetAutoTournamentLink200JSONResponse{
		Linked: true, BaseUrl: &baseURL, LinkedAt: &linkedAt,
	}, nil
}

func (s *Server) PutAutoTournamentLink(ctx context.Context, req gen.PutAutoTournamentLinkRequestObject) (gen.PutAutoTournamentLinkResponseObject, error) {
	user, _, err := s.requireRole(ctx, req.GroupId, "admin")
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	baseURL, err := autotournament.NormalizeBaseURL(req.Body.BaseUrl)
	if err != nil {
		return nil, errResp(gen.Validation, err.Error(), nil)
	}
	client, err := s.autoTournamentClient(baseURL, req.Body.ApiToken)
	if err != nil {
		return nil, errResp(gen.Validation, err.Error(), nil)
	}
	if err := client.Ping(ctx); err != nil {
		if errors.Is(err, autotournament.ErrUnauthorized) {
			return nil, errResp(gen.Validation, "auto tournament rejected the token", nil)
		}
		return nil, errResp(gen.AutoTournamentUnavailable, "auto tournament unreachable", nil)
	}
	group, err := s.Store.GetGroup(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	fleetKey, err := client.CreateFleetKey(ctx, "varde-"+group.Name)
	if err != nil {
		if errors.Is(err, autotournament.ErrUnauthorized) {
			return nil, errResp(gen.Validation, "auto tournament rejected the token", nil)
		}
		return nil, errResp(gen.AutoTournamentUnavailable, err.Error(), nil)
	}
	if s.Secrets == nil {
		return nil, errResp(gen.Internal, "secrets encryption is unavailable", nil)
	}
	tokenSealed, err := s.Secrets.Seal(req.Body.ApiToken, integrationAAD(req.GroupId, "api_token"))
	if err != nil {
		return nil, err
	}
	fleetKeySealed, err := s.Secrets.Seal(fleetKey, integrationAAD(req.GroupId, "fleet_key"))
	if err != nil {
		return nil, err
	}
	now := s.Store.NowMs()
	integration := &store.GroupIntegration{
		GroupID: req.GroupId, Kind: autoTournamentKind, BaseURL: baseURL,
		APITokenSealed: tokenSealed, FleetKeySealed: fleetKeySealed,
		CreatedBy: &user.ID, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.Store.UpsertGroupIntegration(ctx, integration); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &req.GroupId, "user", user.ID,
		"group.integration.link", autoTournamentKind, map[string]any{"base_url": baseURL})
	return gen.PutAutoTournamentLink200JSONResponse{
		Linked: true, BaseUrl: &baseURL, LinkedAt: &now,
	}, nil
}

func (s *Server) DeleteAutoTournamentLink(ctx context.Context, req gen.DeleteAutoTournamentLinkRequestObject) (gen.DeleteAutoTournamentLinkResponseObject, error) {
	user, _, err := s.requireRole(ctx, req.GroupId, "admin")
	if err != nil {
		return nil, err
	}
	if err := s.Store.DeleteGroupIntegration(ctx, req.GroupId, autoTournamentKind); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &req.GroupId, "user", user.ID,
		"group.integration.unlink", autoTournamentKind, nil)
	return gen.DeleteAutoTournamentLink204Response{}, nil
}

func (s *Server) GetGroupTournaments(ctx context.Context, req gen.GetGroupTournamentsRequestObject) (gen.GetGroupTournamentsResponseObject, error) {
	if _, _, err := s.requireRole(ctx, req.GroupId, "member"); err != nil {
		return nil, err
	}
	integration, client, err := s.linkedAutoTournament(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	tournaments, err := client.ListTournaments(ctx)
	if err != nil {
		return nil, errResp(gen.AutoTournamentUnavailable, err.Error(), nil)
	}
	out := make([]gen.AutoTournamentTournament, 0, len(tournaments))
	for _, tournament := range tournaments {
		out = append(out, gen.AutoTournamentTournament{
			Id: tournament.ID, Name: tournament.Name, Type: tournament.Type,
			Status: tournament.Status,
			Url:    strings.TrimRight(integration.BaseURL, "/") + "/tournament/" + tournament.ID,
		})
	}
	return gen.GetGroupTournaments200JSONResponse(out), nil
}

func (s *Server) CreateGroupTournament(ctx context.Context, req gen.CreateGroupTournamentRequestObject) (gen.CreateGroupTournamentResponseObject, error) {
	user, _, err := s.requireRole(ctx, req.GroupId, "member")
	if err != nil {
		return nil, err
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Name) == "" {
		return nil, errResp(gen.Validation, "name required", nil)
	}
	integration, client, err := s.linkedAutoTournament(ctx, req.GroupId)
	if err != nil {
		return nil, err
	}
	format := "shuffle"
	if req.Body.Format != nil {
		format = string(*req.Body.Format)
	}
	if !validTournamentFormat(format) {
		return nil, errResp(gen.Validation, "invalid tournament format", nil)
	}
	teamSize := 5
	if req.Body.TeamSize != nil {
		teamSize = *req.Body.TeamSize
	}
	if teamSize < 2 || teamSize > 5 {
		return nil, errResp(gen.Validation, "team size must be 2–5", nil)
	}
	maps := []string{"de_mirage", "de_inferno", "de_nuke"}
	if req.Body.Maps != nil {
		maps = *req.Body.Maps
	}
	if len(maps) < 1 || len(maps) > 7 {
		return nil, errResp(gen.Validation, "maps must contain between 1 and 7 entries", nil)
	}
	matchFormat := "bo1"
	if req.Body.MatchFormat != nil {
		matchFormat = string(*req.Body.MatchFormat)
	}
	if format != "shuffle" && matchFormat != "bo1" && matchFormat != "bo3" && matchFormat != "bo5" {
		return nil, errResp(gen.Validation, "invalid match_format", nil)
	}
	players, skipped, err := s.tournamentPlayers(ctx, req.GroupId, req.Body.UserIds)
	if err != nil {
		return nil, err
	}
	minPlayers := 2 * teamSize
	if len(players) < minPlayers {
		return nil, errResp(gen.Validation,
			fmt.Sprintf("need at least %d players with a Steam ID", minPlayers), nil)
	}
	if format != "shuffle" && len(players)%teamSize != 0 {
		return nil, errResp(gen.Validation,
			fmt.Sprintf("fixed-team tournament requires player count divisible by team_size (%d)", teamSize), nil)
	}
	imported := make([]autotournament.Player, 0, len(players))
	steamIDs := make([]string, 0, len(players))
	for _, player := range players {
		imported = append(imported, autotournament.Player{ID: player.steamID, Name: player.name})
		steamIDs = append(steamIDs, player.steamID)
	}
	if err := client.ImportPlayers(ctx, imported); err != nil {
		return nil, errResp(gen.AutoTournamentUnavailable, err.Error(), nil)
	}

	var tournament autotournament.Tournament
	var tournamentErr error
	if format == "shuffle" {
		tournament, tournamentErr = client.CreateShuffle(ctx, req.Body.Name, teamSize, maps)
		if tournamentErr == nil {
			tournamentErr = client.RegisterPlayers(ctx, tournament.ID, steamIDs)
		}
	} else {
		if err := shuffleTournamentPlayers(players); err != nil {
			return nil, err
		}
		suffix, err := randomTeamSuffix()
		if err != nil {
			return nil, err
		}
		teams, teamIDs := chunkTournamentTeams(players, teamSize, suffix)
		if err := client.CreateTeams(ctx, teams); err != nil {
			return nil, errResp(gen.AutoTournamentUnavailable, err.Error(), nil)
		}
		tournament, tournamentErr = client.CreateBracket(ctx, req.Body.Name, format, matchFormat, teamSize, maps, teamIDs)
	}
	if tournamentErr != nil {
		return nil, errResp(gen.AutoTournamentUnavailable, tournamentErr.Error(), nil)
	}
	started := req.Body.Start == nil || *req.Body.Start
	if started {
		if err := client.Start(ctx, tournament.ID); err != nil {
			return nil, errResp(gen.AutoTournamentUnavailable, err.Error(), nil)
		}
	}
	baseURL := strings.TrimRight(integration.BaseURL, "/")
	tournamentURL := baseURL + "/tournament/" + tournament.ID
	_ = s.Store.Audit(ctx, s.Store.DB, &req.GroupId, "user", user.ID,
		"group.tournament.create", tournament.ID, map[string]any{
			"format": format, "players": len(players), "started": started,
		})
	return gen.CreateGroupTournament201JSONResponse{
		TournamentId: tournament.ID, Url: tournamentURL,
		BracketUrl: tournamentURL + "/bracket", Started: started,
		Players: len(players), Skipped: skipped,
	}, nil
}

func (s *Server) autoTournamentClient(baseURL, token string) (*autotournament.Client, error) {
	client, err := autotournament.NewClientWithOptions(baseURL, token, s.AutoTournamentAllowPrivate)
	if err != nil {
		return nil, err
	}
	if s.AutoTournamentHTTP != nil {
		client.HTTP = s.AutoTournamentHTTP
	}
	return client, nil
}

func (s *Server) linkedAutoTournament(ctx context.Context, groupID string) (*store.GroupIntegration, *autotournament.Client, error) {
	integration, err := s.Store.GroupIntegration(ctx, groupID, autoTournamentKind)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, errResp(gen.Conflict, "auto tournament not linked", nil)
	}
	if err != nil {
		return nil, nil, err
	}
	if s.Secrets == nil {
		return nil, nil, errResp(gen.Internal, "secrets encryption is unavailable", nil)
	}
	token, err := s.Secrets.Open(integration.APITokenSealed, integrationAAD(groupID, "api_token"))
	if err != nil {
		return nil, nil, errResp(gen.Internal, "auto tournament credentials could not be opened", nil)
	}
	client, err := s.autoTournamentClient(integration.BaseURL, token)
	if err != nil {
		return nil, nil, errResp(gen.Internal, "stored auto tournament URL is invalid", nil)
	}
	return integration, client, nil
}

func (s *Server) addAutoTournamentDirective(ctx context.Context, srv *store.Server, cfg map[string]any) {
	if srv.GameID != "cs2" {
		return
	}
	integration, err := s.Store.GroupIntegration(ctx, srv.GroupID, autoTournamentKind)
	if err != nil || s.Secrets == nil {
		return
	}
	fleetKey, err := s.Secrets.Open(integration.FleetKeySealed, integrationAAD(srv.GroupID, "fleet_key"))
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("could not open Auto Tournament fleet key", "group_id", srv.GroupID)
		}
		return
	}
	if cfg == nil {
		cfg = make(map[string]any)
	}
	directive := map[string]any{
		"fleet_url": integration.BaseURL, "fleet_enroll_key": fleetKey,
	}
	if service, err := s.Store.ServiceByServer(ctx, srv.ID); err == nil {
		var ports []gen.GamePort
		if json.Unmarshal([]byte(service.PortsJSON), &ports) == nil {
			for _, port := range ports {
				if strings.EqualFold(string(port.Protocol), "udp") {
					directive["public_addr"] = fmt.Sprintf("%s:%d", service.LoopbackIP, port.Port)
					break
				}
			}
		}
	}
	// The CS2 driver consumes this directive to write Ready Up's fleet.cfg.
	cfg["_autotournament"] = directive
}

func integrationAAD(groupID, field string) string {
	return groupID + "/" + autoTournamentKind + "/" + field
}

func validTournamentFormat(format string) bool {
	switch format {
	case "shuffle", "single_elimination", "double_elimination", "round_robin", "swiss":
		return true
	default:
		return false
	}
}

func (s *Server) tournamentPlayers(ctx context.Context, groupID string, requested *[]string) ([]tournamentPlayer, []string, error) {
	members, err := s.Store.ListMembers(ctx, groupID)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]store.Member, len(members))
	for _, member := range members {
		byID[member.UserID] = member
	}
	selected := make([]store.Member, 0, len(members))
	if requested == nil {
		selected = members
	} else {
		seen := make(map[string]struct{}, len(*requested))
		for _, userID := range *requested {
			if _, ok := seen[userID]; ok {
				continue
			}
			seen[userID] = struct{}{}
			member, ok := byID[userID]
			if !ok {
				return nil, nil, errResp(gen.Validation, "selected user is not a group member", nil)
			}
			selected = append(selected, member)
		}
	}
	players := make([]tournamentPlayer, 0, len(selected))
	skipped := make([]string, 0)
	missing := make([]string, 0)
	for _, member := range selected {
		if member.SteamID == nil || *member.SteamID == "" {
			if requested != nil {
				missing = append(missing, member.DisplayName)
			} else {
				skipped = append(skipped, member.DisplayName)
			}
			continue
		}
		players = append(players, tournamentPlayer{steamID: *member.SteamID, name: member.DisplayName})
	}
	if len(missing) > 0 {
		return nil, nil, errResp(gen.Validation,
			"missing Steam ID for: "+strings.Join(missing, ", "), nil)
	}
	return players, skipped, nil
}

func shuffleTournamentPlayers(players []tournamentPlayer) error {
	for i := len(players) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return fmt.Errorf("shuffle tournament players: %w", err)
		}
		j := int(n.Int64())
		players[i], players[j] = players[j], players[i]
	}
	return nil
}

func randomTeamSuffix() (string, error) {
	value := make([]byte, 4)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate team IDs: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func chunkTournamentTeams(players []tournamentPlayer, teamSize int, suffix string) ([]autotournament.Team, []string) {
	teams := make([]autotournament.Team, 0, len(players)/teamSize)
	teamIDs := make([]string, 0, len(players)/teamSize)
	for start := 0; start < len(players); start += teamSize {
		n := len(teams) + 1
		id := fmt.Sprintf("varde-%s-%d", suffix, n)
		teamIDs = append(teamIDs, id)
		roster := make([]autotournament.TeamPlayer, 0, teamSize)
		for _, player := range players[start : start+teamSize] {
			roster = append(roster, autotournament.TeamPlayer{SteamID: player.steamID, Name: player.name})
		}
		teams = append(teams, autotournament.Team{
			ID: id, Name: fmt.Sprintf("Team %d", n), Tag: fmt.Sprintf("T%d", n), Players: roster,
		})
	}
	return teams, teamIDs
}
