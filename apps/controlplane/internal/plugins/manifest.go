package plugins

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	pluginIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	elementID       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	fieldName       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

type Field struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Default  any      `json:"default,omitempty"`
	Required bool     `json:"required,omitempty"`
	Secret   bool     `json:"secret,omitempty"`
	Hidden   bool     `json:"hidden,omitempty"`
	Options  []string `json:"options,omitempty"`
	Help     string   `json:"help,omitempty"`
	Requires string   `json:"requires,omitempty"`
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (f Field) Validate(value any) error {
	invalid := func() error { return fmt.Errorf("%s has an invalid value", f.Name) }
	if value == nil {
		if f.Required {
			return fmt.Errorf("%s is required", f.Name)
		}
		return nil
	}
	switch f.Type {
	case "string", "secret", "select":
		text, ok := value.(string)
		if !ok {
			return invalid()
		}
		if f.Required && strings.TrimSpace(text) == "" {
			return fmt.Errorf("%s is required", f.Name)
		}
		if text == "" && !f.Required {
			return nil
		}
		if f.Type == "select" && !contains(f.Options, text) {
			return invalid()
		}
	case "int":
		var number float64
		switch v := value.(type) {
		case float64:
			number = v
		case float32:
			number = float64(v)
		case int:
			number = float64(v)
		case int32:
			number = float64(v)
		case int64:
			number = float64(v)
		case uint:
			number = float64(v)
		case uint32:
			number = float64(v)
		case uint64:
			number = float64(v)
		default:
			return invalid()
		}
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
			return invalid()
		}
	case "bool":
		if _, ok := value.(bool); !ok {
			return invalid()
		}
	case "multiselect", "member_multiselect":
		var items []any
		switch v := value.(type) {
		case []any:
			items = v
		case []string:
			items = make([]any, len(v))
			for i := range v {
				items[i] = v[i]
			}
		default:
			return invalid()
		}
		if f.Required && len(items) == 0 {
			return fmt.Errorf("%s is required", f.Name)
		}
		for _, item := range items {
			text, ok := item.(string)
			if !ok || (f.Type == "multiselect" && !contains(f.Options, text)) {
				return invalid()
			}
		}
	default:
		return invalid()
	}
	return nil
}

type Action struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Endpoint string  `json:"endpoint"`
	Fields   []Field `json:"fields,omitempty"`
}

type List struct {
	Endpoint string `json:"endpoint"`
}

type Page struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	List    *List    `json:"list,omitempty"`
	Actions []Action `json:"actions,omitempty"`
}

type Manifest struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Version      string  `json:"version"`
	BaseURL      string  `json:"base_url"`
	SecretEnv    string  `json:"secret_env,omitempty"`
	SecretFile   string  `json:"secret_file,omitempty"`
	ServiceToken string  `json:"service_token,omitempty"`
	Settings     []Field `json:"settings,omitempty"`
	Pages        []Page  `json:"pages,omitempty"`
}

type Plugin struct {
	Manifest
	Secret       []byte
	ManifestPath string
}

type Registry struct {
	plugins []*Plugin
	byID    map[string]*Plugin
}

func Load(paths []string) (*Registry, error) {
	registry := &Registry{byID: make(map[string]*Plugin)}
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			return nil, errors.New("plugin manifest path must not be empty")
		}
		plugin, err := load(path)
		if err != nil {
			return nil, err
		}
		if previous, ok := registry.byID[plugin.ID]; ok {
			return nil, fmt.Errorf("plugin manifest %s: duplicate plugin id %q (already loaded from %s)", path, plugin.ID, previous.ManifestPath)
		}
		registry.byID[plugin.ID] = plugin
		registry.plugins = append(registry.plugins, plugin)
	}
	return registry, nil
}

func load(path string) (*Plugin, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("plugin manifest %s: read: %w", path, err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("plugin manifest %s: decode: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("plugin manifest %s: trailing JSON value", path)
		}
		return nil, fmt.Errorf("plugin manifest %s: trailing data: %w", path, err)
	}
	if err := validateManifest(&manifest); err != nil {
		return nil, fmt.Errorf("plugin manifest %s: %w", path, err)
	}
	manifest.BaseURL, err = normalizeBaseURL(manifest.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("plugin manifest %s: %w", path, err)
	}
	secret, err := loadSecret(path, manifest.SecretEnv, manifest.SecretFile)
	if err != nil {
		return nil, fmt.Errorf("plugin manifest %s: %w", path, err)
	}
	return &Plugin{Manifest: manifest, Secret: secret, ManifestPath: path}, nil
}

func validateManifest(m *Manifest) error {
	if !pluginIDPattern.MatchString(m.ID) {
		return fmt.Errorf("id %q must match ^[a-z][a-z0-9-]{1,31}$", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.Version) == "" {
		return errors.New("name and version are required")
	}
	if strings.TrimSpace(m.BaseURL) == "" {
		return errors.New("base_url is required")
	}
	if (m.SecretEnv == "") == (m.SecretFile == "") {
		return errors.New("exactly one of secret_env or secret_file is required")
	}
	if m.ServiceToken != "" && strings.TrimSpace(m.ServiceToken) == "" {
		return errors.New("service_token must not be empty")
	}
	if err := validateFields(m.Settings, true); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	pageIDs := make(map[string]struct{}, len(m.Pages))
	for i := range m.Pages {
		page := &m.Pages[i]
		if !elementID.MatchString(page.ID) || strings.TrimSpace(page.Title) == "" {
			return fmt.Errorf("page %q must have a valid id and title", page.ID)
		}
		if _, ok := pageIDs[page.ID]; ok {
			return fmt.Errorf("duplicate page id %q", page.ID)
		}
		pageIDs[page.ID] = struct{}{}
		if page.List != nil {
			if err := validateEndpoint(page.List.Endpoint); err != nil {
				return fmt.Errorf("page %s list endpoint: %w", page.ID, err)
			}
		}
		actionIDs := make(map[string]struct{}, len(page.Actions))
		for j := range page.Actions {
			action := &page.Actions[j]
			if !elementID.MatchString(action.ID) || strings.TrimSpace(action.Title) == "" {
				return fmt.Errorf("page %s action %q must have a valid id and title", page.ID, action.ID)
			}
			if _, ok := actionIDs[action.ID]; ok {
				return fmt.Errorf("page %s: duplicate action id %q", page.ID, action.ID)
			}
			actionIDs[action.ID] = struct{}{}
			if err := validateEndpoint(action.Endpoint); err != nil {
				return fmt.Errorf("page %s action %s endpoint: %w", page.ID, action.ID, err)
			}
			if err := validateFields(action.Fields, false); err != nil {
				return fmt.Errorf("page %s action %s fields: %w", page.ID, action.ID, err)
			}
		}
	}
	return nil
}

func validateFields(fields []Field, setting bool) error {
	names := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if !fieldName.MatchString(field.Name) {
			return fmt.Errorf("field name %q is invalid", field.Name)
		}
		if _, ok := names[field.Name]; ok {
			return fmt.Errorf("duplicate field name %q", field.Name)
		}
		names[field.Name] = struct{}{}
		switch field.Type {
		case "string", "int", "bool", "select", "secret", "multiselect", "member_multiselect":
		default:
			return fmt.Errorf("%s: unsupported type %q", field.Name, field.Type)
		}
		if (field.Type == "select" || field.Type == "multiselect") && len(field.Options) == 0 {
			return fmt.Errorf("%s: options are required for %s", field.Name, field.Type)
		}
		if field.Type != "member_multiselect" && field.Requires != "" {
			return fmt.Errorf("%s: requires is only supported for member_multiselect", field.Name)
		}
		if field.Requires != "" && field.Requires != "steam_id" {
			return fmt.Errorf("%s: unsupported requires value %q", field.Name, field.Requires)
		}
		if !setting && field.Hidden {
			return fmt.Errorf("%s: hidden is only supported for settings", field.Name)
		}
		if field.Default != nil {
			if err := field.Validate(field.Default); err != nil {
				return fmt.Errorf("%s default: %w", field.Name, err)
			}
		}
	}
	return nil
}

func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("base_url must use http or https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("base_url must not include credentials, query, or fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || !strings.HasPrefix(endpoint, "/") || u.Scheme != "" || u.Host != "" || strings.Contains(endpoint, "..") {
		return errors.New("endpoint must be an absolute path without a scheme, host, or '..'")
	}
	return nil
}

func loadSecret(manifestPath, secretEnv, secretFile string) ([]byte, error) {
	var secret []byte
	if secretEnv != "" {
		value, ok := os.LookupEnv(secretEnv)
		if !ok {
			return nil, fmt.Errorf("secret environment variable %s is not set", secretEnv)
		}
		secret = []byte(value)
	} else {
		if !filepath.IsAbs(secretFile) {
			secretFile = filepath.Join(filepath.Dir(manifestPath), secretFile)
		}
		value, err := os.ReadFile(secretFile)
		if err != nil {
			return nil, fmt.Errorf("read secret_file %s: %w", secretFile, err)
		}
		secret = []byte(strings.TrimRight(string(value), "\r\n"))
	}
	if len(secret) < 32 {
		return nil, errors.New("plugin secret must contain at least 32 bytes")
	}
	return append([]byte(nil), secret...), nil
}

func (r *Registry) Get(id string) (*Plugin, bool) {
	if r == nil {
		return nil, false
	}
	plugin, ok := r.byID[id]
	return plugin, ok
}

func (r *Registry) All() []*Plugin {
	if r == nil {
		return nil
	}
	return append([]*Plugin(nil), r.plugins...)
}
