package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/plugins"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func (s *Server) AdminListPlugins(ctx context.Context, _ gen.AdminListPluginsRequestObject) (gen.AdminListPluginsResponseObject, error) {
	if _, err := s.requireOperator(ctx); err != nil {
		return nil, err
	}
	if s.PluginManager == nil {
		return nil, errResp(gen.Internal, "plugin supervisor is unavailable", nil)
	}
	installed, err := s.PluginManager.Installed(ctx)
	if err != nil {
		return nil, err
	}
	installedByID := make(map[string]store.InstalledPlugin, len(installed))
	for _, row := range installed {
		installedByID[row.ID] = row
	}
	response := gen.AdminListPlugins200JSONResponse{Plugins: []gen.AdminPlugin{}}
	for _, entry := range s.PluginManager.Catalog() {
		row, isInstalled := installedByID[entry.ID]
		response.Plugins = append(response.Plugins, adminPlugin(entry, row, isInstalled))
	}
	return response, nil
}

func (s *Server) AdminInstallPlugin(ctx context.Context, req gen.AdminInstallPluginRequestObject) (gen.AdminInstallPluginResponseObject, error) {
	if _, err := s.requireOperator(ctx); err != nil {
		return nil, err
	}
	if s.PluginManager == nil {
		return nil, errResp(gen.Internal, "plugin supervisor is unavailable", nil)
	}
	row, err := s.PluginManager.Install(ctx, req.Id)
	if err != nil {
		if strings.Contains(err.Error(), "not in the catalog") {
			return nil, errResp(gen.NotFound, err.Error(), nil)
		}
		if strings.Contains(err.Error(), "catalog entry not released yet") ||
			strings.Contains(err.Error(), "already registered") ||
			errors.Is(err, store.ErrConflict) {
			return nil, errResp(gen.Conflict, err.Error(), nil)
		}
		return nil, fmt.Errorf("install plugin: %w", err)
	}
	for _, entry := range s.PluginManager.Catalog() {
		if entry.ID == req.Id {
			return gen.AdminInstallPlugin200JSONResponse(adminPlugin(entry, row, true)), nil
		}
	}
	return nil, errResp(gen.NotFound, "plugin not in catalog", nil)
}

func (s *Server) AdminUpdatePlugin(ctx context.Context, req gen.AdminUpdatePluginRequestObject) (gen.AdminUpdatePluginResponseObject, error) {
	if _, err := s.requireOperator(ctx); err != nil {
		return nil, err
	}
	if s.PluginManager == nil {
		return nil, errResp(gen.Internal, "plugin supervisor is unavailable", nil)
	}
	row, err := s.PluginManager.Update(ctx, req.Id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || strings.Contains(err.Error(), "not in the catalog") {
			return nil, errResp(gen.NotFound, err.Error(), nil)
		}
		if strings.Contains(err.Error(), "must be the leader") ||
			strings.Contains(err.Error(), "not leader") ||
			strings.Contains(err.Error(), "primary control plane") {
			return nil, errResp(gen.PluginUnavailable, "plugin updates run on the leader", nil)
		}
		return nil, fmt.Errorf("update plugin: %w", err)
	}
	for _, entry := range s.PluginManager.Catalog() {
		if entry.ID == req.Id {
			return gen.AdminUpdatePlugin200JSONResponse(adminPlugin(entry, row, true)), nil
		}
	}
	return nil, errResp(gen.NotFound, "plugin not in catalog", nil)
}

func (s *Server) AdminUninstallPlugin(ctx context.Context, req gen.AdminUninstallPluginRequestObject) (gen.AdminUninstallPluginResponseObject, error) {
	if _, err := s.requireOperator(ctx); err != nil {
		return nil, err
	}
	if s.PluginManager == nil {
		return nil, errResp(gen.Internal, "plugin supervisor is unavailable", nil)
	}
	if err := s.PluginManager.Uninstall(ctx, req.Id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errResp(gen.NotFound, "plugin is not installed", nil)
		}
		return nil, fmt.Errorf("uninstall plugin: %w", err)
	}
	return gen.AdminUninstallPlugin204Response{}, nil
}

func adminPlugin(entry plugins.CatalogEntry, row store.InstalledPlugin, installed bool) gen.AdminPlugin {
	assets := make(map[string]struct {
		Sha256 string `json:"sha256"`
		Url    string `json:"url"`
	}, len(entry.Assets))
	for target, asset := range entry.Assets {
		assets[target] = struct {
			Sha256 string `json:"sha256"`
			Url    string `json:"url"`
		}{Sha256: asset.SHA256, Url: asset.URL}
	}
	result := gen.AdminPlugin{
		Id: entry.ID, Name: entry.Name, Description: entry.Description, Version: entry.Version,
		Scopes: entry.Scopes, Assets: assets, Installed: installed,
		UpdateAvailable: installed && entry.Version != row.Version,
	}
	if installed {
		status, message := row.Status, row.Error
		result.Status = &status
		result.Error = &message
		upgradeError := row.UpgradeError
		result.UpgradeError = &upgradeError
		result.InstalledAt = &row.InstalledAt
		result.UpdatedAt = &row.UpdatedAt
	}
	return result
}
