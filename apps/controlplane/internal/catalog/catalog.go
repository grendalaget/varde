// Package catalog is the data-only game catalog (embedded JSON). Core code
// has no game-specific logic; this data drives validation and UI forms.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"slices"
)

//go:embed catalog.json
var data []byte

type Port struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

type ConfigField struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"` // string|int|bool|select|secret
	Default  any      `json:"default,omitempty"`
	Required bool     `json:"required,omitempty"`
	Secret   bool     `json:"secret,omitempty"`
	Options  []string `json:"options,omitempty"`
	Help     string   `json:"help,omitempty"`
	Min      *float64 `json:"min,omitempty"`
	Max      *float64 `json:"max,omitempty"`
}

type Game struct {
	ID            string              `json:"id"`
	Name          string              `json:"name"`
	Ports         []Port              `json:"ports"`
	OS            []string            `json:"os"`
	Arch          []string            `json:"arch"`
	MinMemoryMB   int64               `json:"min_memory_mb"`
	MinDiskMB     int64               `json:"min_disk_mb,omitempty"`
	SteamApp      int64               `json:"steam_app,omitempty"`
	Runtimes      []string            `json:"runtimes"`
	AddonTargets  []string            `json:"addon_targets"`
	AddonTargetOS map[string][]string `json:"addon_target_os,omitempty"`
	ConfigFields  []ConfigField       `json:"config_fields"`
}

var games []Game

func init() {
	if err := json.Unmarshal(data, &games); err != nil {
		panic("catalog: " + err.Error())
	}
	if err := validateAddonTargetOS(games); err != nil {
		panic("catalog: " + err.Error())
	}
}

func validateAddonTargetOS(games []Game) error {
	for _, game := range games {
		for target, oses := range game.AddonTargetOS {
			if !slices.Contains(game.AddonTargets, target) {
				return fmt.Errorf("game %q addon_target_os target %q is not in addon_targets", game.ID, target)
			}
			for _, os := range oses {
				if !slices.Contains(game.OS, os) {
					return fmt.Errorf("game %q addon_target_os target %q includes unsupported os %q", game.ID, target, os)
				}
			}
		}
	}
	return nil
}

// PlacementOS returns the game OS values that satisfy every addon target.
// An empty target represents a legacy addon row whose target was not stored,
// so all target-specific OS restrictions are applied conservatively.
func PlacementOS(game *Game, targets []string) []string {
	if game == nil {
		return nil
	}

	allowed := append([]string(nil), game.OS...)
	constrained := false
	intersect := func(targetOS []string) {
		if !constrained {
			allowed = append([]string{}, game.OS...)
			constrained = true
		}
		filtered := make([]string, 0, len(allowed))
		for _, os := range allowed {
			if slices.Contains(targetOS, os) {
				filtered = append(filtered, os)
			}
		}
		allowed = filtered
	}

	for _, target := range targets {
		if target == "" {
			for _, targetOS := range game.AddonTargetOS {
				intersect(targetOS)
			}
			continue
		}
		if targetOS, ok := game.AddonTargetOS[target]; ok {
			intersect(targetOS)
		}
	}
	if constrained {
		return allowed
	}
	return append([]string(nil), game.OS...)
}

// All returns the catalog.
func All() []Game { return games }

// Get returns a game by id or nil.
func Get(id string) *Game {
	for i := range games {
		if games[i].ID == id {
			return &games[i]
		}
	}
	return nil
}

// ValidateConfig checks a server config against the game's fields and
// returns validation errors (empty = ok). Unknown fields are ignored.
func (g *Game) ValidateConfig(cfg map[string]any) []string {
	var errs []string
	for _, f := range g.ConfigFields {
		v, ok := cfg[f.Name]
		if !ok || v == nil {
			if f.Required && f.Default == nil {
				errs = append(errs, fmt.Sprintf("%s: required", f.Name))
			}
			continue
		}
		switch f.Type {
		case "string", "secret":
			s, ok := v.(string)
			if !ok {
				errs = append(errs, fmt.Sprintf("%s: expected string", f.Name))
				continue
			}
			if f.Required && s == "" {
				errs = append(errs, fmt.Sprintf("%s: required", f.Name))
			}
		case "select":
			s, ok := v.(string)
			if !ok {
				errs = append(errs, fmt.Sprintf("%s: expected string", f.Name))
				continue
			}
			found := false
			for _, o := range f.Options {
				if o == s {
					found = true
				}
			}
			if !found {
				errs = append(errs, fmt.Sprintf("%s: not one of %v", f.Name, f.Options))
			}
		case "int":
			n, ok := v.(float64)
			if !ok || n != math.Trunc(n) {
				errs = append(errs, fmt.Sprintf("%s: expected int", f.Name))
				continue
			}
			if f.Min != nil && n < *f.Min {
				errs = append(errs, fmt.Sprintf("%s: must be at least %g", f.Name, *f.Min))
			}
			if f.Max != nil && n > *f.Max {
				errs = append(errs, fmt.Sprintf("%s: must be at most %g", f.Name, *f.Max))
			}
		case "bool":
			if _, ok := v.(bool); !ok {
				errs = append(errs, fmt.Sprintf("%s: expected bool", f.Name))
			}
		}
	}
	return errs
}
