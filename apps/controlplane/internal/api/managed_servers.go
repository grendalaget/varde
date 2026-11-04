package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/catalog"
	"github.com/grendalaget/varde/apps/controlplane/internal/plugins"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func (s *Server) managedPrimary() bool {
	return s.ManagedLeader == nil || s.ManagedLeader.IsLeader()
}

func managedSecretAAD(row *store.ManagedPluginServer) string {
	return "managed-server:" + row.GroupID + "/" + row.PluginID
}

func (s *Server) managedSecrets(row *store.ManagedPluginServer) (map[string]string, error) {
	if s.Secrets == nil {
		return nil, errResp(gen.SecretsUnconfigured, "managed server secrets are not configured", nil)
	}
	opened, err := s.Secrets.Open(row.SecretsJSON, managedSecretAAD(row))
	if err != nil {
		return nil, err
	}
	var values map[string]string
	err = json.Unmarshal([]byte(opened), &values)
	return values, err
}

func (s *Server) managedServerConfig(cfg map[string]any, row *store.ManagedPluginServer) (map[string]any, error) {
	if row == nil {
		return cfg, nil
	}
	values, err := s.managedSecrets(row)
	if err != nil {
		return nil, err
	}
	merged := maps.Clone(cfg)
	for key, value := range values {
		merged[key] = value
	}
	return merged, nil
}

func (s *Server) injectManagedSecrets(ctx context.Context, serverID string, cfg *map[string]any) error {
	row, err := s.Store.ManagedPluginServerByServer(ctx, serverID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	values, err := s.managedSecrets(row)
	if err != nil {
		return err
	}
	if *cfg == nil {
		*cfg = make(map[string]any)
	}
	for key, value := range values {
		(*cfg)[key] = value
	}
	return nil
}

func (s *Server) authorizeManagedPlugin(ctx context.Context, groupID, pluginID string) (*plugins.Plugin, error) {
	plugin, ok := s.Plugins.Get(pluginID)
	if !ok || plugin.ManagedServer == nil {
		return nil, errResp(gen.NotFound, "managed plugin not found", nil)
	}
	if _, _, err := s.authorizePluginGroup(ctx, groupID, plugin, "admin"); err != nil {
		return nil, err
	}
	return plugin, nil
}

func (s *Server) StartGroupPluginManagedServer(ctx context.Context, req gen.StartGroupPluginManagedServerRequestObject) (gen.StartGroupPluginManagedServerResponseObject, error) {
	s.managedMu.Lock()
	defer s.managedMu.Unlock()
	plugin, err := s.authorizeManagedPlugin(ctx, req.GroupId, req.PluginId)
	if err != nil {
		return nil, err
	}
	if !s.managedPrimary() {
		return nil, errResp(gen.PluginUnavailable, "managed servers are available only on the primary control plane", nil)
	}
	if s.Secrets == nil {
		return nil, errResp(gen.SecretsUnconfigured, "set VARDE_SECRETS_KEY to use managed servers", nil)
	}
	group, err := s.Store.GroupPlugin(ctx, req.GroupId, plugin.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errResp(gen.Validation, "enable the plugin and choose its required settings first", nil)
	}
	if err != nil {
		return nil, err
	}
	settings, err := s.openPluginSettings(req.GroupId, plugin, group.SettingsJSON)
	if err != nil {
		return nil, err
	}
	settings, err = mergePluginSettings(plugin, settings, nil)
	if err != nil {
		return nil, errResp(gen.Validation, err.Error(), nil)
	}
	if !plugin.Managed(settings) {
		return nil, errResp(gen.Validation, "select managed mode before starting this server", nil)
	}
	cfg := make(map[string]any)
	for target, source := range plugin.ManagedServer.Config {
		cfg[target] = settings[source]
	}
	game := catalog.Get(plugin.ManagedServer.Game)
	if err := s.requireGateway(game); err != nil {
		return nil, err
	}
	row, err := s.Store.ManagedPluginServer(ctx, req.GroupId, plugin.ID)
	create := errors.Is(err, sql.ErrNoRows)
	if create {
		row = &store.ManagedPluginServer{GroupID: req.GroupId, PluginID: plugin.ID}
		values := make(map[string]string)
		for _, source := range plugin.ManagedServer.Bindings {
			if field, ok := strings.CutPrefix(source, "secret:"); ok {
				var secret [32]byte
				if _, err := rand.Read(secret[:]); err != nil {
					return nil, err
				}
				values[field] = base64.RawURLEncoding.EncodeToString(secret[:])
			}
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		row.SecretsJSON, err = s.Secrets.Seal(string(encoded), managedSecretAAD(row))
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	validationConfig, err := s.managedServerConfig(cfg, row)
	if err != nil {
		return nil, err
	}
	if problems := game.ValidateConfig(validationConfig); len(problems) != 0 {
		return nil, errResp(gen.Validation, strings.Join(problems, "; "), nil)
	}
	if create {
		_, err = s.createServer(ctx, gen.CreateServerRequestObject{GroupId: req.GroupId, Body: &gen.CreateServerJSONRequestBody{
			Name: plugin.Name, GameId: game.ID, Config: &cfg,
		}}, row)
		if err != nil {
			return nil, err
		}
	} else {
		srv, err := s.Store.GetServer(ctx, row.ServerID)
		if err != nil {
			return nil, errResp(gen.Validation, "the managed server has been removed", nil)
		}
		if srv.GameID != game.ID {
			return nil, errResp(gen.Validation, "the managed server game cannot be changed", nil)
		}
		var existing map[string]any
		if err := json.Unmarshal([]byte(srv.ConfigJSON), &existing); err != nil {
			return nil, err
		}
		for key, value := range cfg {
			existing[key] = value
		}
		updated := mustJSON(existing)
		if updated != srv.ConfigJSON {
			row.BindingDigest = ""
		}
		srv.ConfigJSON, srv.UpdatedAt = updated, s.Store.NowMs()
		if err := s.Store.UpdateServerMeta(ctx, srv); err != nil {
			return nil, err
		}
	}
	if _, err := s.StartServer(ctx, gen.StartServerRequestObject{ServerId: row.ServerID}); err != nil {
		return nil, err
	}
	row.RetryAt, row.Attempts, row.BindingError = 0, 0, ""
	if err := s.Store.UpdateManagedPluginBinding(ctx, row); err != nil {
		return nil, err
	}
	status, err := s.managedServerStatus(ctx, plugin, req.GroupId)
	return gen.StartGroupPluginManagedServer200JSONResponse(status), err
}

func (s *Server) GetGroupPluginManagedServer(ctx context.Context, req gen.GetGroupPluginManagedServerRequestObject) (gen.GetGroupPluginManagedServerResponseObject, error) {
	plugin, err := s.authorizeManagedPlugin(ctx, req.GroupId, req.PluginId)
	if err != nil {
		return nil, err
	}
	status, err := s.managedServerStatus(ctx, plugin, req.GroupId)
	return gen.GetGroupPluginManagedServer200JSONResponse(status), err
}

func (s *Server) StopGroupPluginManagedServer(ctx context.Context, req gen.StopGroupPluginManagedServerRequestObject) (gen.StopGroupPluginManagedServerResponseObject, error) {
	s.managedMu.Lock()
	defer s.managedMu.Unlock()
	plugin, err := s.authorizeManagedPlugin(ctx, req.GroupId, req.PluginId)
	if err != nil {
		return nil, err
	}
	if !s.managedPrimary() {
		return nil, errResp(gen.PluginUnavailable, "managed servers are available only on the primary control plane", nil)
	}
	if err := s.stopManagedPluginServer(ctx, req.GroupId, plugin.ID); err != nil {
		return nil, err
	}
	status, err := s.managedServerStatus(ctx, plugin, req.GroupId)
	return gen.StopGroupPluginManagedServer200JSONResponse(status), err
}

func (s *Server) stopManagedPluginServer(ctx context.Context, groupID, pluginID string) error {
	row, err := s.Store.ManagedPluginServer(ctx, groupID, pluginID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.Store.UpdateServerDesired(ctx, row.ServerID, "stopped", s.Store.NowMs()); err != nil {
		return err
	}
	s.Recon.WakeGroup(groupID)
	return nil
}

func (s *Server) stopPluginManagedServers(ctx context.Context, pluginID string) error {
	rows, err := s.Store.ManagedPluginServers(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.PluginID == pluginID {
			if err := s.stopManagedPluginServer(ctx, row.GroupID, pluginID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) managedServerStatus(ctx context.Context, plugin *plugins.Plugin, groupID string) (gen.ManagedServerStatus, error) {
	status := gen.ManagedServerStatus{Phase: "stopped"}
	row, err := s.Store.ManagedPluginServer(ctx, groupID, plugin.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	status.ServerId = &row.ServerID
	srv, err := s.Store.GetServer(ctx, row.ServerID)
	if errors.Is(err, store.ErrNotFound) {
		status.Phase, status.Reason = "failed", ptr("the managed server has been removed")
		return status, nil
	}
	if err != nil {
		return status, err
	}
	status.PublicUrl = ptr(s.gatewayPublicURL(srv))
	if srv.DesiredState != "running" {
		return status, nil
	}
	status.Phase = "starting"
	execution, err := s.Store.ActiveExecution(ctx, srv.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return status, err
	}
	if execution != nil {
		if node, err := s.Store.GetNode(ctx, execution.NodeID); err == nil {
			status.NodeName = &node.Name
		}
		status.SetupCode, err = s.Store.ExecutionJoinCode(ctx, execution.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return status, err
		}
		if execution.State == "preparing" || execution.State == "restoring" {
			status.Phase = "preparing"
		}
		if execution.State == "failed" {
			status.Phase, status.Reason = "failed", ptr("server execution failed")
		}
		if s.managedHealthy(srv, execution) && row.BindingDigest != "" {
			group, err := s.Store.GroupPlugin(ctx, groupID, plugin.ID)
			if err != nil {
				return status, err
			}
			settings, err := s.openPluginSettings(groupID, plugin, group.SettingsJSON)
			if err != nil {
				return status, err
			}
			secrets, err := s.managedSecrets(row)
			if err != nil {
				return status, err
			}
			known, err := s.applyManagedBindings(ctx, plugin, srv, execution, settings, secrets)
			if err != nil {
				return status, err
			}
			digest, err := managedBindingDigest(plugin, settings)
			if err != nil {
				return status, err
			}
			if known && plugin.Managed(settings) && row.BindingDigest == digest {
				status.Phase = "ready"
			}
		}
	}
	if srv.ObservedState == "failed" {
		status.Phase, status.Reason = "failed", ptr("server execution failed")
	}
	if row.BindingError != "" {
		status.Phase, status.Reason = "failed", &row.BindingError
	}
	return status, nil
}

func (s *Server) managedHealthy(srv *store.Server, execution *store.Execution) bool {
	return execution != nil && execution.State == "running" && execution.Health != nil && *execution.Health == "ok" &&
		execution.Role == "serving" && execution.Action == "run" && execution.Epoch == srv.Epoch && execution.LeaseExpiresAt > s.Store.NowMs()
}

func (s *Server) applyPluginSettingsHook(ctx context.Context, groupID string, plugin *plugins.Plugin, user, settings map[string]any, key string) error {
	reply, err := s.callPluginIdempotent(ctx, groupID, plugin, user, "settings", plugin.BaseURL+"/varde/v1/settings", settings, nil, key)
	if err != nil {
		return err
	}
	var hook struct {
		Store map[string]any `json:"store"`
	}
	if err := json.Unmarshal(reply, &hook); err != nil {
		return pluginUnavailable(plugin)
	}
	for name, value := range hook.Store {
		field, found := findPluginField(plugin.Settings, name)
		if !found || !field.Hidden || field.Validate(value) != nil {
			return pluginUnavailable(plugin)
		}
		settings[name] = value
	}
	return nil
}

func (s *Server) RunManagedServers(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.ReconcileManagedServers(ctx); err != nil && ctx.Err() == nil && s.Log != nil {
			s.Log.Error("managed server reconciliation failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) ReconcileManagedServers(ctx context.Context) error {
	if !s.managedPrimary() {
		return nil
	}
	rows, err := s.Store.ManagedPluginServers(ctx)
	if err != nil {
		return err
	}
	var failures error
	for _, row := range rows {
		s.managedMu.Lock()
		err := s.reconcileManagedServer(ctx, row.GroupID, row.PluginID)
		s.managedMu.Unlock()
		if err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

func (s *Server) reconcileManagedServer(ctx context.Context, groupID, pluginID string) error {
	if !s.managedPrimary() {
		return nil
	}
	row, err := s.Store.ManagedPluginServer(ctx, groupID, pluginID)
	if err != nil {
		return err
	}
	plugin, ok := s.Plugins.Get(pluginID)
	if !ok || plugin.ManagedServer == nil {
		return s.stopManagedPluginServer(ctx, groupID, pluginID)
	}
	group, err := s.Store.GroupPlugin(ctx, groupID, pluginID)
	if errors.Is(err, sql.ErrNoRows) {
		return s.stopManagedPluginServer(ctx, groupID, pluginID)
	}
	if err != nil {
		return err
	}
	settings, err := s.openPluginSettings(groupID, plugin, group.SettingsJSON)
	if err != nil {
		return err
	}
	if !plugin.Managed(settings) {
		return s.stopManagedPluginServer(ctx, groupID, pluginID)
	}
	srv, err := s.Store.GetServer(ctx, row.ServerID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if srv.DesiredState != "running" || row.RetryAt > s.Store.NowMs() {
		return nil
	}
	execution, err := s.Store.ActiveExecution(ctx, srv.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil || !s.managedHealthy(srv, execution) {
		return err
	}
	secrets, err := s.managedSecrets(row)
	if err != nil {
		return err
	}
	known, err := s.applyManagedBindings(ctx, plugin, srv, execution, settings, secrets)
	if err != nil {
		return s.managedBindingFailed(ctx, row, err, secrets)
	}
	if !known {
		return nil
	}
	key, err := managedBindingDigest(plugin, settings)
	if err != nil {
		return err
	}
	if row.BindingDigest == key {
		if row.BindingError != "" {
			row.BindingError, row.Attempts, row.RetryAt = "", 0, 0
			return s.Store.UpdateManagedPluginBinding(ctx, row)
		}
		return nil
	}
	if !s.managedPrimary() {
		return nil
	}
	group.SettingsJSON, err = s.sealPluginSettings(groupID, plugin, maps.Clone(settings))
	if err != nil {
		return err
	}
	group.UpdatedAt = s.Store.NowMs()
	if err := s.Store.UpsertGroupPlugin(ctx, *group); err != nil {
		return err
	}
	user := map[string]any{"id": group.EnabledBy, "role": "admin", "display_name": "Varde managed server"}
	for _, field := range plugin.Settings {
		if field.Secret || field.Type == "secret" || field.Hidden {
			if value, ok := settings[field.Name].(string); ok {
				secrets["setting:"+field.Name] = value
			}
		}
	}
	if err := s.applyPluginSettingsHook(ctx, groupID, plugin, user, settings, "managed-server:"+row.ServerID+":"+key); err != nil {
		return s.managedBindingFailed(ctx, row, err, secrets)
	}
	if !s.managedPrimary() {
		return nil
	}
	group.SettingsJSON, err = s.sealPluginSettings(groupID, plugin, maps.Clone(settings))
	if err != nil {
		return err
	}
	row.BindingDigest, row.BindingError, row.Attempts, row.RetryAt = key, "", 0, 0
	return s.Store.CommitManagedPluginBinding(ctx, row, group)
}

func (s *Server) applyManagedBindings(ctx context.Context, plugin *plugins.Plugin, srv *store.Server, execution *store.Execution, settings map[string]any, secrets map[string]string) (bool, error) {
	for target, source := range plugin.ManagedServer.Bindings {
		var value any
		switch source {
		case "public_url":
			value = s.gatewayPublicURL(srv)
		case "setup_code":
			code, err := s.Store.ExecutionJoinCode(ctx, execution.ID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
			if code == nil {
				return false, nil
			}
			value = *code
		default:
			value = secrets[strings.TrimPrefix(source, "secret:")]
		}
		field, _ := findPluginField(plugin.Settings, target)
		if err := field.Validate(value); err != nil {
			return false, err
		}
		settings[target] = value
	}
	return true, nil
}

func managedBindingDigest(plugin *plugins.Plugin, settings map[string]any) (string, error) {
	digestInput := maps.Clone(settings)
	for _, field := range plugin.Settings {
		if field.Hidden {
			delete(digestInput, field.Name)
		}
	}
	encoded, err := json.Marshal(digestInput)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Server) managedBindingFailed(ctx context.Context, row *store.ManagedPluginServer, err error, secrets map[string]string) error {
	if !s.managedPrimary() {
		return nil
	}
	message := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	row.BindingError = fmt.Sprintf("plugin settings hook: %s", message)
	row.Attempts++
	delay := time.Second << min(row.Attempts-1, 6)
	row.RetryAt = s.Store.NowMs() + delay.Milliseconds()
	return s.Store.UpdateManagedPluginBinding(ctx, row)
}
