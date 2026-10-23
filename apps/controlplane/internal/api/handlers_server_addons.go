package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/catalog"
	"github.com/grendalaget/varde/apps/controlplane/internal/plugins"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

var (
	serverAddonSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	serverAddonTarget = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	serverAddonID     = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
)

func (s *Server) ListServerAddons(ctx context.Context, req gen.ListServerAddonsRequestObject) (gen.ListServerAddonsResponseObject, error) {
	srv, err := s.Store.GetServer(ctx, string(req.ServerId))
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	if _, _, err := s.scopedActor(ctx, srv.GroupID, "servers", "member"); err != nil {
		return nil, err
	}
	if s.Secrets == nil {
		return nil, errResp(gen.SecretsUnconfigured, "server addons are unavailable because secrets are not configured", nil)
	}
	rows, err := s.Store.ListServerAddons(ctx, srv.ID)
	if err != nil {
		return nil, err
	}
	addons := make([]gen.ServerAddonSummary, 0, len(rows))
	for _, row := range rows {
		plaintext, err := s.Secrets.Open(row.Spec, serverAddonAAD(srv.GroupID, row.ServerID, row.PluginID, row.AddonID))
		if err != nil {
			return nil, err
		}
		var spec gen.ServerAddonSpec
		if err := json.Unmarshal([]byte(plaintext), &spec); err != nil {
			return nil, err
		}
		names := make([]string, 0, len(spec.Files))
		for name := range spec.Files {
			names = append(names, name)
		}
		sort.Strings(names)
		addons = append(addons, gen.ServerAddonSummary{
			PluginId: row.PluginID, AddonId: row.AddonID, Url: spec.Url,
			Sha256: spec.Sha256, Archive: gen.ServerAddonSummaryArchive(spec.Archive),
			Target: spec.Target, FileNames: names, UpdatedAt: row.UpdatedAt,
		})
	}
	return gen.ListServerAddons200JSONResponse{Addons: addons}, nil
}

func (s *Server) PutServerAddon(ctx context.Context, req gen.PutServerAddonRequestObject) (gen.PutServerAddonResponseObject, error) {
	if !serverAddonID.MatchString(req.AddonId) {
		return nil, errResp(gen.Validation, "addon id must match ^[a-z][a-z0-9-]{1,31}$", nil)
	}
	srv, err := s.Store.GetServer(ctx, string(req.ServerId))
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	actorType, actorID, err := s.scopedActor(ctx, srv.GroupID, "server_addons", "member")
	if err != nil {
		return nil, err
	}
	plugin, err := s.enabledAddonPlugin(ctx, srv.GroupID, req.PluginId)
	if err != nil {
		return nil, err
	}
	service := serviceFrom(ctx)
	if actorType != "system" || service == nil || plugin.ServiceToken == "" || service.Name != plugin.ServiceToken {
		return nil, errResp(gen.Forbidden, "server addons require the plugin service token", nil)
	}
	if s.Secrets == nil {
		return nil, errResp(gen.SecretsUnconfigured, "server addons are unavailable because secrets are not configured", nil)
	}
	if req.Body == nil {
		return nil, errResp(gen.Validation, "server addon specification is required", nil)
	}
	if err := s.validateServerAddonSpec(*req.Body); err != nil {
		return nil, errResp(gen.Validation, err.Error(), nil)
	}
	game := catalog.Get(srv.GameID)
	if game == nil || !containsServerAddonTarget(game.AddonTargets, req.Body.Target) {
		return nil, errResp(gen.Validation, "server game does not support addon target "+req.Body.Target, nil)
	}
	raw, err := json.Marshal(req.Body)
	if err != nil {
		return nil, err
	}
	sealed, err := s.Secrets.Seal(string(raw), serverAddonAAD(srv.GroupID, srv.ID, req.PluginId, req.AddonId))
	if err != nil {
		return nil, err
	}
	now := s.Store.NowMs()
	if err := s.Store.UpsertServerAddon(ctx, store.ServerAddon{
		ServerID: srv.ID, PluginID: req.PluginId, AddonID: req.AddonId,
		Spec: sealed, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &srv.GroupID, actorType, actorID, "server_addon.put",
		srv.ID+"/"+req.PluginId+"/"+req.AddonId,
		map[string]any{"plugin_id": req.PluginId, "addon_id": req.AddonId})
	return gen.PutServerAddon204Response{}, nil
}

func (s *Server) DeleteServerAddon(ctx context.Context, req gen.DeleteServerAddonRequestObject) (gen.DeleteServerAddonResponseObject, error) {
	if !serverAddonID.MatchString(req.AddonId) {
		return nil, errResp(gen.Validation, "addon id must match ^[a-z][a-z0-9-]{1,31}$", nil)
	}
	srv, err := s.Store.GetServer(ctx, string(req.ServerId))
	if err != nil {
		return nil, errResp(gen.NotFound, "server not found", nil)
	}
	actorType, actorID, err := s.scopedActor(ctx, srv.GroupID, "server_addons", "admin")
	if err != nil {
		return nil, err
	}
	plugin, ok := s.Plugins.Get(req.PluginId)
	if !ok {
		return nil, errResp(gen.NotFound, "plugin not found", nil)
	}
	if actorType == "system" {
		service := serviceFrom(ctx)
		if service == nil || plugin.ServiceToken == "" || service.Name != plugin.ServiceToken {
			return nil, errResp(gen.Forbidden, "only the addon plugin token may delete this addon", nil)
		}
	}
	if err := s.Store.DeleteServerAddon(ctx, srv.ID, req.PluginId, req.AddonId); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &srv.GroupID, actorType, actorID, "server_addon.delete",
		srv.ID+"/"+req.PluginId+"/"+req.AddonId,
		map[string]any{"plugin_id": req.PluginId, "addon_id": req.AddonId})
	return gen.DeleteServerAddon204Response{}, nil
}

func (s *Server) enabledAddonPlugin(ctx context.Context, groupID, pluginID string) (*plugins.Plugin, error) {
	if !serverAddonID.MatchString(pluginID) || s.Plugins == nil {
		return nil, errResp(gen.NotFound, "plugin not found", nil)
	}
	plugin, ok := s.Plugins.Get(pluginID)
	if !ok {
		return nil, errResp(gen.NotFound, "plugin not found", nil)
	}
	if _, err := s.Store.GroupPlugin(ctx, groupID, pluginID); err != nil {
		return nil, errResp(gen.Forbidden, "plugin is not enabled for this group", nil)
	}
	return plugin, nil
}

func (s *Server) validateServerAddonSpec(spec gen.ServerAddonSpec) error {
	parsed, err := url.Parse(spec.Url)
	if err != nil || parsed.Hostname() == "" {
		return errors.New("url must be an absolute HTTP or HTTPS URL")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !s.Cfg.Dev && !serverAddonLoopback(parsed.Hostname()) {
			return errors.New("HTTP addon URLs require --dev or a loopback host")
		}
	default:
		return errors.New("url must use HTTPS")
	}
	if !serverAddonSHA256.MatchString(spec.Sha256) {
		return errors.New("sha256 must be 64 lowercase hexadecimal characters")
	}
	if spec.Archive != gen.ServerAddonSpecArchiveZip && spec.Archive != gen.ServerAddonSpecArchiveTarGz {
		return errors.New("archive must be zip or tar_gz")
	}
	if !serverAddonTarget.MatchString(spec.Target) {
		return errors.New("target must match ^[a-z][a-z0-9_]{0,31}$")
	}
	if spec.DriverOptions == nil {
		return errors.New("driver_options must be an object")
	}
	options, err := json.Marshal(spec.DriverOptions)
	if err != nil || len(options) > 4<<10 {
		return errors.New("driver_options must serialize to at most 4 KiB")
	}
	if spec.Files == nil {
		return errors.New("files must be an object")
	}
	if len(spec.Files) > 32 {
		return errors.New("files may contain at most 32 entries")
	}
	total := 0
	for key, content := range spec.Files {
		if !validServerAddonFileKey(key) {
			return fmt.Errorf("invalid addon file path %q", key)
		}
		size := len([]byte(content))
		if size > 64<<10 {
			return fmt.Errorf("addon file %q exceeds 64 KiB", key)
		}
		total += size
	}
	if total > 2<<20 {
		return errors.New("addon files exceed 2 MiB total")
	}
	return nil
}

func serverAddonLoopback(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

func containsServerAddonTarget(targets []string, target string) bool {
	for _, candidate := range targets {
		if candidate == target {
			return true
		}
	}
	return false
}

func validServerAddonFileKey(key string) bool {
	if key == "" || strings.HasPrefix(key, "/") || strings.ContainsAny(key, `\\:`) {
		return false
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == ".." {
			return false
		}
	}
	return true
}

func serverAddonAAD(groupID, serverID, pluginID, addonID string) string {
	return fmt.Sprintf("%s/addon:%s/%s/%s", groupID, serverID, pluginID, addonID)
}

func (s *Server) directiveServerAddons(ctx context.Context, srv *store.Server) *[]gen.ServerAddon {
	addons := []gen.ServerAddon{}
	rows, err := s.Store.ListServerAddons(ctx, srv.ID)
	if err != nil {
		if s.Log != nil {
			s.Log.Error("could not list server addons for execution directive", "server_id", srv.ID, "error", err)
		}
		return &addons
	}
	for _, row := range rows {
		plaintext, err := s.Secrets.Open(row.Spec, serverAddonAAD(srv.GroupID, row.ServerID, row.PluginID, row.AddonID))
		if err != nil {
			if s.Log != nil {
				s.Log.Error("could not unseal server addon for execution directive",
					"server_id", srv.ID, "plugin_id", row.PluginID, "addon_id", row.AddonID)
			}
			continue
		}
		var spec gen.ServerAddonSpec
		if err := json.Unmarshal([]byte(plaintext), &spec); err != nil {
			if s.Log != nil {
				s.Log.Error("could not decode server addon for execution directive",
					"server_id", srv.ID, "plugin_id", row.PluginID, "addon_id", row.AddonID)
			}
			continue
		}
		addons = append(addons, gen.ServerAddon{
			PluginId: row.PluginID, AddonId: row.AddonID, Url: spec.Url, Sha256: spec.Sha256,
			Archive: gen.ServerAddonArchive(spec.Archive), Target: spec.Target,
			DriverOptions: spec.DriverOptions, Files: spec.Files,
		})
	}
	return &addons
}
