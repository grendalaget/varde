package plugins

import (
	"errors"
	"fmt"
	"strings"

	"github.com/grendalaget/varde/apps/controlplane/internal/catalog"
)

type ManagedServer struct {
	Game        string            `json:"game"`
	ModeSetting string            `json:"mode_setting"`
	Config      map[string]string `json:"config"`
	Bindings    map[string]string `json:"bindings"`
}

func (m Manifest) Managed(settings map[string]any) bool {
	if m.ManagedServer == nil {
		return false
	}
	for _, field := range m.Settings {
		if field.Name == m.ManagedServer.ModeSetting {
			value, ok := settings[field.Name]
			return !ok || value == field.Default
		}
	}
	return false
}

func (m Manifest) Bound(name string) bool {
	if m.ManagedServer == nil {
		return false
	}
	_, ok := m.ManagedServer.Bindings[name]
	return ok
}

func validateManagedServer(m *Manifest) error {
	managed := m.ManagedServer
	if managed == nil {
		return nil
	}
	game := catalog.Get(managed.Game)
	if game == nil {
		return errors.New("managed_server.game must name a catalog game")
	}
	fields := make(map[string]Field, len(m.Settings))
	for _, field := range m.Settings {
		fields[field.Name] = field
	}
	mode, ok := fields[managed.ModeSetting]
	if !ok || mode.Type != "select" || mode.Hidden || len(mode.Options) != 2 || mode.Default == nil {
		return errors.New("managed_server.mode_setting must name a visible select with two options and a default")
	}
	config := make(map[string]catalog.ConfigField, len(game.ConfigFields))
	for _, field := range game.ConfigFields {
		config[field.Name] = field
	}
	if managed.Config == nil {
		managed.Config = make(map[string]string)
	}
	for target, source := range managed.Config {
		field, exists := config[target]
		setting, found := fields[source]
		if !exists || !found || field.Secret || field.Type == "secret" || setting.Secret || setting.Type == "secret" || setting.Hidden || source == managed.ModeSetting {
			return fmt.Errorf("managed_server.config %q must map a non-secret game field to a visible setting", target)
		}
		gameType, settingType := field.Type, setting.Type
		if gameType == "select" {
			gameType = "string"
		}
		if settingType == "select" {
			settingType = "string"
		}
		if gameType != settingType {
			return fmt.Errorf("managed_server.config %q has incompatible setting type", target)
		}
	}
	for _, field := range game.ConfigFields {
		if field.Required && field.Default == nil && !field.Secret && field.Type != "secret" {
			if _, ok := managed.Config[field.Name]; !ok {
				return fmt.Errorf("managed_server.config must map required game field %q", field.Name)
			}
		}
	}
	if len(managed.Bindings) == 0 {
		return errors.New("managed_server.bindings must not be empty")
	}
	for target, source := range managed.Bindings {
		field, found := fields[target]
		if !found || field.Hidden || target == managed.ModeSetting || (field.Type != "string" && field.Type != "secret") {
			return fmt.Errorf("managed_server.bindings %q must name a visible string or secret setting", target)
		}
		switch {
		case source == "public_url":
			if game.GatewayPort == 0 {
				return errors.New("managed_server public_url requires a gateway-published game")
			}
		case source == "setup_code":
		case strings.HasPrefix(source, "secret:"):
			secret, exists := config[strings.TrimPrefix(source, "secret:")]
			if !exists || (!secret.Secret && secret.Type != "secret") || (field.Type != "secret" && !field.Secret) {
				return errors.New("managed_server secret binding requires a secret game field and secret setting")
			}
		default:
			return fmt.Errorf("unsupported managed_server runtime source %q", source)
		}
		for _, input := range managed.Config {
			if input == target {
				return errors.New("managed_server config inputs cannot also be runtime bindings")
			}
		}
	}
	for _, field := range game.ConfigFields {
		if !field.Required || field.Default != nil || (!field.Secret && field.Type != "secret") {
			continue
		}
		bound := false
		for _, source := range managed.Bindings {
			bound = bound || source == "secret:"+field.Name
		}
		if !bound {
			return fmt.Errorf("managed_server.bindings must generate required secret %q", field.Name)
		}
	}
	return nil
}
