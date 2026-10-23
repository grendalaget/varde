package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/outbound"
	"github.com/grendalaget/varde/apps/controlplane/internal/plugins"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func (s *Server) ListPlugins(ctx context.Context, _ gen.ListPluginsRequestObject) (gen.ListPluginsResponseObject, error) {
	if _, err := s.requireUser(ctx); err != nil {
		return nil, err
	}
	result := gen.ListPlugins200JSONResponse{Plugins: []gen.Plugin{}}
	for _, plugin := range s.Plugins.All() {
		result.Plugins = append(result.Plugins, pluginInfo(plugin))
	}
	return result, nil
}

func pluginInfo(plugin *plugins.Plugin) gen.Plugin {
	result := gen.Plugin{
		Id: plugin.ID, Name: plugin.Name, Version: plugin.Version,
		Settings: []gen.PluginField{}, Pages: []gen.PluginPage{},
	}
	for _, field := range plugin.Settings {
		if field.Hidden {
			continue
		}
		result.Settings = append(result.Settings, apiPluginField(field))
	}
	for _, page := range plugin.Pages {
		outPage := gen.PluginPage{Id: page.ID, Title: page.Title, Actions: []gen.PluginAction{}}
		if page.List != nil {
			outPage.ListEndpoint = &page.List.Endpoint
		}
		for _, action := range page.Actions {
			outAction := gen.PluginAction{Id: action.ID, Title: action.Title, Endpoint: action.Endpoint, Fields: []gen.PluginField{}}
			for _, field := range action.Fields {
				outAction.Fields = append(outAction.Fields, apiPluginField(field))
			}
			outPage.Actions = append(outPage.Actions, outAction)
		}
		result.Pages = append(result.Pages, outPage)
	}
	return result
}

func apiPluginField(field plugins.Field) gen.PluginField {
	result := gen.PluginField{Name: field.Name, Type: gen.PluginFieldType(field.Type)}
	if field.Default != nil {
		result.Default = field.Default
	}
	if field.Help != "" {
		result.Help = &field.Help
	}
	if field.Required {
		result.Required = &field.Required
	}
	if field.Secret {
		result.Secret = &field.Secret
	}
	if field.Hidden {
		result.Hidden = &field.Hidden
	}
	if len(field.Options) > 0 {
		options := append([]string(nil), field.Options...)
		result.Options = &options
	}
	if field.Requires != "" {
		value := gen.PluginFieldRequires(field.Requires)
		result.Requires = &value
	}
	return result
}

func (s *Server) ListGroupPlugins(ctx context.Context, req gen.ListGroupPluginsRequestObject) (gen.ListGroupPluginsResponseObject, error) {
	if serviceFrom(ctx) != nil {
		return nil, errResp(gen.Forbidden, "service tokens cannot access plugin endpoints", nil)
	}
	if _, _, err := s.requireRole(ctx, req.GroupId, "member"); err != nil {
		return nil, err
	}
	result := gen.ListGroupPlugins200JSONResponse{Plugins: []gen.GroupPlugin{}}
	loaded := s.Plugins.All()
	if len(loaded) == 0 {
		return result, nil
	}
	if s.Secrets == nil {
		return nil, errResp(gen.SecretsUnconfigured, "set VARDE_SECRETS_KEY (same value on every control plane replica) to use plugins", nil)
	}
	for _, plugin := range loaded {
		row, err := s.Store.GroupPlugin(ctx, req.GroupId, plugin.ID)
		if errors.Is(err, sql.ErrNoRows) {
			result.Plugins = append(result.Plugins, groupPluginResponse(plugin, false, nil, nil))
			continue
		}
		if err != nil {
			return nil, err
		}
		settings, err := s.openPluginSettings(req.GroupId, plugin, row.SettingsJSON)
		if err != nil {
			return nil, err
		}
		result.Plugins = append(result.Plugins, groupPluginResponse(plugin, true, settings, nil))
	}
	return result, nil
}

func groupPluginResponse(plugin *plugins.Plugin, enabled bool, settings map[string]any, secretsSet []string) gen.GroupPlugin {
	public := make(map[string]any)
	secrets := append([]string(nil), secretsSet...)
	for _, field := range plugin.Settings {
		if field.Hidden {
			continue
		}
		if field.Type == "secret" || field.Secret {
			if value, ok := settings[field.Name]; ok && value != nil && value != "" {
				secrets = append(secrets, field.Name)
			}
			public[field.Name] = nil
			continue
		}
		if value, ok := settings[field.Name]; ok {
			public[field.Name] = value
		}
	}
	return gen.GroupPlugin{Id: plugin.ID, Enabled: enabled, Settings: public, SecretsSet: uniqueStrings(secrets)}
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

func (s *Server) PutGroupPlugin(ctx context.Context, req gen.PutGroupPluginRequestObject) (gen.PutGroupPluginResponseObject, error) {
	plugin, ok := s.Plugins.Get(req.PluginId)
	if !ok {
		return nil, errResp(gen.NotFound, "plugin not found", nil)
	}
	if s.Secrets == nil {
		return nil, errResp(gen.SecretsUnconfigured, "set VARDE_SECRETS_KEY (same value on every control plane replica) to use plugins", nil)
	}
	if serviceFrom(ctx) != nil {
		return nil, errResp(gen.Forbidden, "owner or admin required", nil)
	}
	u, role, err := s.authorizePluginGroup(ctx, req.GroupId, plugin, "admin")
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	var previous map[string]any
	if row, err := s.Store.GroupPlugin(ctx, req.GroupId, plugin.ID); err == nil {
		previous, err = s.openPluginSettings(req.GroupId, plugin, row.SettingsJSON)
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	merged, err := mergePluginSettings(plugin, previous, req.Body.Settings)
	if err != nil {
		return nil, errResp(gen.Validation, err.Error(), nil)
	}
	hookReply, err := s.callPlugin(ctx, req.GroupId, plugin, pluginUser(u, role), "settings", plugin.BaseURL+"/varde/v1/settings", merged, nil)
	if err != nil {
		return nil, err
	}
	var hook struct {
		Store map[string]any `json:"store"`
	}
	if err := json.Unmarshal(hookReply, &hook); err != nil {
		return nil, pluginUnavailable(plugin)
	}
	for key, value := range hook.Store {
		field, found := findPluginField(plugin.Settings, key)
		if !found || !field.Hidden {
			return nil, pluginUnavailable(plugin)
		}
		if err := field.Validate(value); err != nil {
			return nil, pluginUnavailable(plugin)
		}
		merged[key] = value
	}
	sealed, err := s.sealPluginSettings(req.GroupId, plugin, merged)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	row := store.GroupPlugin{
		GroupID: req.GroupId, PluginID: plugin.ID, SettingsJSON: sealed,
		EnabledBy: u.ID, CreatedAt: now, UpdatedAt: now,
	}
	if old, err := s.Store.GroupPlugin(ctx, req.GroupId, plugin.ID); err == nil {
		row.CreatedAt = old.CreatedAt
	}
	if err := s.Store.UpsertGroupPlugin(ctx, row); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, nil, "group", req.GroupId, "plugin.enabled", u.ID, map[string]any{"plugin_id": plugin.ID})
	return gen.PutGroupPlugin200JSONResponse(groupPluginResponse(plugin, true, merged, nil)), nil
}

func (s *Server) DeleteGroupPlugin(ctx context.Context, req gen.DeleteGroupPluginRequestObject) (gen.DeleteGroupPluginResponseObject, error) {
	plugin, ok := s.Plugins.Get(req.PluginId)
	if !ok {
		return nil, errResp(gen.NotFound, "plugin not found", nil)
	}
	if s.Secrets == nil {
		return nil, errResp(gen.SecretsUnconfigured, "set VARDE_SECRETS_KEY (same value on every control plane replica) to use plugins", nil)
	}
	if serviceFrom(ctx) != nil {
		return nil, errResp(gen.Forbidden, "owner or admin required", nil)
	}
	u, role, err := s.authorizePluginGroup(ctx, req.GroupId, plugin, "admin")
	if err != nil {
		return nil, err
	}
	if row, err := s.Store.GroupPlugin(ctx, req.GroupId, plugin.ID); err == nil {
		settings, openErr := s.openPluginSettings(req.GroupId, plugin, row.SettingsJSON)
		if openErr == nil {
			if _, callErr := s.callPlugin(ctx, req.GroupId, plugin, pluginUser(u, role), "disable", plugin.BaseURL+"/varde/v1/disable", settings, nil); callErr != nil && s.Log != nil {
				s.Log.Warn("plugin disable hook failed", "plugin_id", plugin.ID, "error", callErr)
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err := s.Store.DeleteGroupPlugin(ctx, req.GroupId, plugin.ID); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, nil, "group", req.GroupId, "plugin.disabled", u.ID, map[string]any{"plugin_id": plugin.ID})
	return gen.DeleteGroupPlugin204Response{}, nil
}

func (s *Server) ListPluginPage(ctx context.Context, req gen.ListPluginPageRequestObject) (gen.ListPluginPageResponseObject, error) {
	plugin, page, _, err := s.pluginPageAccess(ctx, req.GroupId, req.PluginId, req.PageId, "")
	if err != nil {
		return nil, err
	}
	if page.List == nil {
		return nil, errResp(gen.NotFound, "plugin page has no list endpoint", nil)
	}
	settings, user, role, err := s.pluginCallContext(ctx, req.GroupId, plugin)
	if err != nil {
		return nil, err
	}
	reply, err := s.callPlugin(ctx, req.GroupId, plugin, pluginUser(user, role), "list", plugin.BaseURL+page.List.Endpoint, settings, nil)
	if err != nil {
		return nil, err
	}
	var response gen.PluginListResponse
	if err := json.Unmarshal(reply, &response); err != nil || response.Items == nil {
		return nil, pluginUnavailable(plugin)
	}
	for _, item := range response.Items {
		if item.Title == "" || (item.Href != nil && !validHTTPURL(*item.Href)) {
			return nil, pluginUnavailable(plugin)
		}
	}
	return gen.ListPluginPage200JSONResponse(response), nil
}

func (s *Server) InvokePluginAction(ctx context.Context, req gen.InvokePluginActionRequestObject) (gen.InvokePluginActionResponseObject, error) {
	plugin, _, action, err := s.pluginPageAccess(ctx, req.GroupId, req.PluginId, req.PageId, req.ActionId)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	settings, user, role, err := s.pluginCallContext(ctx, req.GroupId, plugin)
	if err != nil {
		return nil, err
	}
	values, err := validateActionValues(ctx, s.Store, req.GroupId, action.Fields, req.Body.Values)
	if err != nil {
		return nil, errResp(gen.Validation, err.Error(), nil)
	}
	reply, err := s.callPlugin(ctx, req.GroupId, plugin, pluginUser(user, role), "action:"+action.ID, plugin.BaseURL+action.Endpoint, settings, values)
	if err != nil {
		return nil, err
	}
	var response gen.PluginActionResponse
	if err := json.Unmarshal(reply, &response); err != nil || response.Message == "" {
		return nil, pluginUnavailable(plugin)
	}
	if response.Notes == nil {
		notes := []string{}
		response.Notes = &notes
	}
	if response.Href != nil && !validHTTPURL(*response.Href) {
		return nil, pluginUnavailable(plugin)
	}
	return gen.InvokePluginAction200JSONResponse(response), nil
}

func (s *Server) pluginPageAccess(ctx context.Context, groupID, pluginID, pageID, actionID string) (*plugins.Plugin, plugins.Page, plugins.Action, error) {
	plugin, ok := s.Plugins.Get(pluginID)
	if !ok {
		return nil, plugins.Page{}, plugins.Action{}, errResp(gen.NotFound, "plugin not found", nil)
	}
	if s.Secrets == nil {
		return nil, plugins.Page{}, plugins.Action{}, errResp(gen.SecretsUnconfigured, "set VARDE_SECRETS_KEY (same value on every control plane replica) to use plugins", nil)
	}
	if _, _, err := s.authorizePluginGroup(ctx, groupID, plugin, "member"); err != nil {
		return nil, plugins.Page{}, plugins.Action{}, err
	}
	if _, err := s.Store.GroupPlugin(ctx, groupID, pluginID); errors.Is(err, sql.ErrNoRows) {
		return nil, plugins.Page{}, plugins.Action{}, errResp(gen.NotFound, "plugin not enabled", nil)
	} else if err != nil {
		return nil, plugins.Page{}, plugins.Action{}, err
	}
	for _, page := range plugin.Pages {
		if page.ID != pageID {
			continue
		}
		if actionID == "" {
			return plugin, page, plugins.Action{}, nil
		}
		for _, action := range page.Actions {
			if action.ID == actionID {
				return plugin, page, action, nil
			}
		}
	}
	return nil, plugins.Page{}, plugins.Action{}, errResp(gen.NotFound, "plugin page or action not found", nil)
}

func (s *Server) pluginCallContext(ctx context.Context, groupID string, plugin *plugins.Plugin) (map[string]any, *store.User, string, error) {
	user, role, err := s.authorizePluginGroup(ctx, groupID, plugin, "member")
	if err != nil {
		return nil, nil, "", err
	}
	row, err := s.Store.GroupPlugin(ctx, groupID, plugin.ID)
	if err != nil {
		return nil, nil, "", err
	}
	settings, err := s.openPluginSettings(groupID, plugin, row.SettingsJSON)
	return settings, user, role, err
}

func (s *Server) authorizePluginGroup(ctx context.Context, groupID string, plugin *plugins.Plugin, minRole string) (*store.User, string, error) {
	if serviceFrom(ctx) != nil {
		return nil, "", errResp(gen.Forbidden, "service tokens cannot access plugin endpoints", nil)
	}
	return s.requireRole(ctx, groupID, minRole)
}

func pluginUser(user *store.User, role string) map[string]any {
	return map[string]any{"id": user.ID, "display_name": user.DisplayName, "role": role}
}

func (s *Server) callPlugin(ctx context.Context, groupID string, plugin *plugins.Plugin, user map[string]any, op, endpoint string, settings, values map[string]any) ([]byte, error) {
	now := time.Now()
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return nil, pluginUnavailable(plugin)
	}
	claims := jwt.MapClaims{
		"iss": "varde-control-plane", "aud": plugin.ID,
		"sub": user["id"], "grp": groupID, "role": user["role"],
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
		"jti": base64.RawURLEncoding.EncodeToString(jti),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(plugin.Secret)
	if err != nil {
		return nil, pluginUnavailable(plugin)
	}
	envelope := map[string]any{"group_id": groupID, "user": user, "settings": settings}
	if values != nil {
		envelope["values"] = values
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, pluginUnavailable(plugin)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, pluginUnavailable(plugin)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Varde-Op", op)
	client := s.PluginHTTP
	if client == nil {
		client = outbound.NewClient(10 * time.Second)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, pluginUnavailable(plugin)
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, err := outbound.ReadLimited(resp.Body)
	if err != nil {
		return nil, pluginUnavailable(plugin)
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		var reply struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(responseBody, &reply) == nil && reply.Error != "" {
			message := reply.Error
			if runes := []rune(message); len(runes) > 300 {
				message = string(runes[:300])
			}
			return nil, errResp(gen.Validation, message, nil)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !json.Valid(responseBody) {
		return nil, pluginUnavailable(plugin)
	}
	return responseBody, nil
}

func pluginUnavailable(plugin *plugins.Plugin) error {
	return errResp(gen.PluginUnavailable, fmt.Sprintf("%s is unavailable", plugin.Name), nil)
}

func (s *Server) openPluginSettings(groupID string, plugin *plugins.Plugin, raw string) (map[string]any, error) {
	settings := make(map[string]any)
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return nil, err
	}
	for _, field := range plugin.Settings {
		sealedValue, ok := settings[field.Name].(string)
		if !ok || (field.Type != "secret" && !field.Secret && !field.Hidden) {
			continue
		}
		opened, err := s.Secrets.Open(sealedValue, pluginAAD(groupID, plugin.ID, field.Name))
		if err != nil {
			return nil, err
		}
		if field.Hidden {
			var value any
			if err := json.Unmarshal([]byte(opened), &value); err != nil {
				return nil, err
			}
			settings[field.Name] = value
		} else {
			settings[field.Name] = opened
		}
	}
	return settings, nil
}

func (s *Server) sealPluginSettings(groupID string, plugin *plugins.Plugin, settings map[string]any) (string, error) {
	for _, field := range plugin.Settings {
		if field.Type != "secret" && !field.Secret && !field.Hidden {
			continue
		}
		value, ok := settings[field.Name]
		if !ok || value == nil || value == "" {
			continue
		}
		plaintext := ""
		if field.Hidden {
			encoded, err := json.Marshal(value)
			if err != nil {
				return "", err
			}
			plaintext = string(encoded)
		} else {
			var isString bool
			plaintext, isString = value.(string)
			if !isString {
				return "", fmt.Errorf("%s must be a string", field.Name)
			}
		}
		sealed, err := s.Secrets.Seal(plaintext, pluginAAD(groupID, plugin.ID, field.Name))
		if err != nil {
			return "", err
		}
		settings[field.Name] = sealed
	}
	data, err := json.Marshal(settings)
	return string(data), err
}

func pluginAAD(groupID, pluginID, field string) string {
	return groupID + "/plugin:" + pluginID + "/" + field
}

func mergePluginSettings(plugin *plugins.Plugin, previous, input map[string]any) (map[string]any, error) {
	merged := make(map[string]any, len(previous)+len(input))
	for key, value := range previous {
		merged[key] = value
	}
	for key := range input {
		field, ok := findPluginField(plugin.Settings, key)
		if !ok {
			return nil, fmt.Errorf("unknown setting %q", key)
		}
		if field.Hidden {
			return nil, fmt.Errorf("hidden setting %q cannot be supplied", key)
		}
	}
	for _, field := range plugin.Settings {
		value, supplied := input[field.Name]
		if !supplied {
			if _, exists := merged[field.Name]; !exists && field.Default != nil {
				merged[field.Name] = field.Default
			}
			continue
		}
		if field.Type == "secret" || field.Secret {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a string", field.Name)
			}
			if text == "" {
				continue
			}
		}
		if err := field.Validate(value); err != nil {
			return nil, err
		}
		merged[field.Name] = value
	}
	for _, field := range plugin.Settings {
		if field.Hidden {
			continue
		}
		value, ok := merged[field.Name]
		if !ok {
			if err := field.Validate(nil); err != nil {
				return nil, err
			}
			continue
		}
		if err := field.Validate(value); err != nil {
			return nil, err
		}
	}
	return merged, nil
}

func findPluginField(fields []plugins.Field, name string) (plugins.Field, bool) {
	for _, field := range fields {
		if field.Name == name {
			return field, true
		}
	}
	return plugins.Field{}, false
}

func validateActionValues(ctx context.Context, st *store.Store, groupID string, fields []plugins.Field, input map[string]any) (map[string]any, error) {
	result := make(map[string]any)
	for key := range input {
		if _, ok := findPluginField(fields, key); !ok {
			return nil, fmt.Errorf("unknown field %q", key)
		}
	}
	members, err := st.ListMembers(ctx, groupID)
	if err != nil {
		return nil, err
	}
	memberByID := make(map[string]store.Member, len(members))
	for _, member := range members {
		memberByID[member.UserID] = member
	}
	for _, field := range fields {
		value, supplied := input[field.Name]
		if !supplied {
			if err := field.Validate(nil); err != nil {
				return nil, err
			}
			continue
		}
		if err := field.Validate(value); err != nil {
			return nil, err
		}
		if field.Type != "member_multiselect" {
			result[field.Name] = value
			continue
		}
		selected := value.([]any)
		expanded := make([]map[string]any, 0, len(selected))
		missingSteam := []string{}
		for _, rawID := range selected {
			id := rawID.(string)
			member, ok := memberByID[id]
			if !ok {
				return nil, fmt.Errorf("selected user %s is not a group member", id)
			}
			steamID := ""
			if member.SteamID != nil {
				steamID = *member.SteamID
			}
			if field.Requires == "steam_id" && steamID == "" {
				missingSteam = append(missingSteam, member.DisplayName)
			}
			expanded = append(expanded, map[string]any{"user_id": id, "display_name": member.DisplayName, "steam_id": steamID})
		}
		if len(missingSteam) > 0 {
			return nil, fmt.Errorf("%s have no Steam ID", strings.Join(missingSteam, ", "))
		}
		result[field.Name] = expanded
	}
	return result, nil
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
