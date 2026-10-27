package plugins

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

//go:embed catalog.json
var catalogFS embed.FS

var (
	catalogSHA256        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	catalogTargetPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)
)

type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type CatalogEntry struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Version     string           `json:"version"`
	Scopes      []string         `json:"scopes"`
	Assets      map[string]Asset `json:"assets"`
}

type Catalog struct {
	entries []CatalogEntry
	byID    map[string]CatalogEntry
}

func LoadCatalog() (*Catalog, error) {
	data, err := catalogFS.ReadFile("catalog.json")
	if err != nil {
		return nil, err
	}
	return ParseCatalog(data)
}

func ParseCatalog(data []byte) (*Catalog, error) {
	var entries []CatalogEntry
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entries); err != nil {
		return nil, fmt.Errorf("plugin catalog: decode: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, errors.New("plugin catalog: trailing JSON value")
		}
		return nil, fmt.Errorf("plugin catalog: trailing data: %w", err)
	}
	catalog := &Catalog{entries: entries, byID: make(map[string]CatalogEntry, len(entries))}
	for _, entry := range entries {
		if err := validateCatalogEntry(entry); err != nil {
			return nil, fmt.Errorf("plugin catalog %s: %w", entry.ID, err)
		}
		if _, exists := catalog.byID[entry.ID]; exists {
			return nil, fmt.Errorf("plugin catalog: duplicate id %q", entry.ID)
		}
		catalog.byID[entry.ID] = entry
	}
	return catalog, nil
}

func validateCatalogEntry(entry CatalogEntry) error {
	if !pluginIDPattern.MatchString(entry.ID) {
		return fmt.Errorf("invalid plugin id %q", entry.ID)
	}
	if strings.TrimSpace(entry.Name) == "" || strings.TrimSpace(entry.Description) == "" {
		return errors.New("name and description are required")
	}
	if !safePluginVersion(entry.Version) {
		return fmt.Errorf("invalid version %q", entry.Version)
	}
	if len(entry.Scopes) == 0 {
		return errors.New("at least one service-token scope is required")
	}
	seenScopes := make(map[string]struct{}, len(entry.Scopes))
	for _, scope := range entry.Scopes {
		if strings.TrimSpace(scope) == "" {
			return errors.New("service-token scopes must not be empty")
		}
		if _, ok := seenScopes[scope]; ok {
			return fmt.Errorf("duplicate service-token scope %q", scope)
		}
		seenScopes[scope] = struct{}{}
	}
	if len(entry.Assets) == 0 {
		return errors.New("at least one runtime asset is required")
	}
	for target, asset := range entry.Assets {
		if !catalogTargetPattern.MatchString(target) {
			return fmt.Errorf("invalid runtime target %q", target)
		}
		parsed, err := url.Parse(asset.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return fmt.Errorf("asset %s URL must be absolute HTTP or HTTPS", target)
		}
		if !catalogSHA256.MatchString(asset.SHA256) {
			return fmt.Errorf("asset %s SHA-256 must be a lowercase 64-character digest", target)
		}
	}
	return nil
}

func safePluginVersion(version string) bool {
	if version == "" || strings.TrimSpace(version) != version ||
		strings.ContainsAny(version, `/\`) || strings.Contains(version, "..") ||
		!isVersionStart(version[0]) {
		return false
	}
	for _, r := range version {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			strings.ContainsRune(".+-", r):
		default:
			return false
		}
	}
	return true
}

func isVersionStart(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func (c *Catalog) Get(id string) (CatalogEntry, bool) {
	if c == nil {
		return CatalogEntry{}, false
	}
	entry, ok := c.byID[id]
	return cloneCatalogEntry(entry), ok
}

func (c *Catalog) All() []CatalogEntry {
	if c == nil {
		return nil
	}
	entries := make([]CatalogEntry, 0, len(c.entries))
	for _, entry := range c.entries {
		entries = append(entries, cloneCatalogEntry(entry))
	}
	return entries
}

func cloneCatalogEntry(entry CatalogEntry) CatalogEntry {
	entry.Scopes = append([]string(nil), entry.Scopes...)
	assets := make(map[string]Asset, len(entry.Assets))
	for target, asset := range entry.Assets {
		assets[target] = asset
	}
	entry.Assets = assets
	return entry
}
