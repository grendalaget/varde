// Package catalog is the data-only game catalog (embedded JSON). Core code
// has no game-specific logic; this data drives validation and UI forms.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
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
}

type Game struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Ports        []Port        `json:"ports"`
	OS           []string      `json:"os"`
	Arch         []string      `json:"arch"`
	MinMemoryMB  int64         `json:"min_memory_mb"`
	Runtimes     []string      `json:"runtimes"`
	ConfigFields []ConfigField `json:"config_fields"`
}

var games []Game

func init() {
	if err := json.Unmarshal(data, &games); err != nil {
		panic("catalog: " + err.Error())
	}
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
			if _, ok := v.(float64); !ok {
				errs = append(errs, fmt.Sprintf("%s: expected int", f.Name))
			}
		case "bool":
			if _, ok := v.(bool); !ok {
				errs = append(errs, fmt.Sprintf("%s: expected bool", f.Name))
			}
		}
	}
	return errs
}
