package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grendalaget/varde/apps/controlplane/internal/enroll"
	"github.com/grendalaget/varde/apps/controlplane/internal/plugins"
	"github.com/grendalaget/varde/apps/controlplane/internal/secrets"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

const (
	credentialAAD = "installed-plugin:"
	installStatus = "installing"
	runningStatus = "running"
	errorStatus   = "error"
	stoppedStatus = "stopped"
)

type Leader interface {
	IsLeader() bool
}

type Options struct {
	DataDir           string
	ControlPlaneURL   string
	Store             *store.Store
	Registry          *plugins.Registry
	Catalog           *plugins.Catalog
	Secrets           *secrets.Manager
	HTTPClient        *http.Client
	Leader            Leader
	Log               *slog.Logger
	HealthTimeout     time.Duration
	RestartMin        time.Duration
	RestartMax        time.Duration
	ReconcileInterval time.Duration
}

type credentials struct {
	PluginSecret     string `json:"plugin_secret"`
	ServiceToken     string `json:"service_token"`
	ServiceTokenName string `json:"service_token_name"`
}

type managedProcess struct {
	cancel   context.CancelFunc
	done     chan struct{}
	ready    chan struct{}
	readyErr error
	once     sync.Once
	healthy  atomic.Bool
	retired  atomic.Bool
}

type Supervisor struct {
	opts Options

	opMu    sync.Mutex
	mu      sync.Mutex
	rootCtx context.Context
	running map[string]*managedProcess
}

func New(opts Options) (*Supervisor, error) {
	if opts.DataDir == "" {
		return nil, errors.New("plugin supervisor data directory is required")
	}
	if opts.Store == nil || opts.Registry == nil || opts.Catalog == nil {
		return nil, errors.New("plugin supervisor store, registry, and catalog are required")
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.HealthTimeout <= 0 {
		opts.HealthTimeout = 20 * time.Second
	}
	if opts.RestartMin <= 0 {
		opts.RestartMin = time.Second
	}
	if opts.RestartMax < opts.RestartMin {
		opts.RestartMax = 60 * time.Second
	}
	if opts.ReconcileInterval <= 0 {
		opts.ReconcileInterval = 5 * time.Second
	}
	if opts.ControlPlaneURL == "" {
		opts.ControlPlaneURL = "http://127.0.0.1:8080"
	}
	if err := checkStaticServiceTokenCollisions(context.Background(), opts.Store, opts.Registry); err != nil {
		return nil, err
	}
	return &Supervisor{opts: opts, running: make(map[string]*managedProcess)}, nil
}

func (s *Supervisor) Catalog() []plugins.CatalogEntry {
	return s.opts.Catalog.All()
}

func (s *Supervisor) Installed(ctx context.Context) ([]store.InstalledPlugin, error) {
	return s.opts.Store.ListInstalledPlugins(ctx)
}

func (s *Supervisor) Install(ctx context.Context, id string) (store.InstalledPlugin, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.opts.Leader != nil && !s.opts.Leader.IsLeader() {
		return store.InstalledPlugin{}, errors.New("plugin installation is available only on the primary control plane")
	}
	if strings.TrimSpace(os.Getenv("VARDE_SECRETS_KEY")) == "" {
		return store.InstalledPlugin{}, errors.New("VARDE_SECRETS_KEY is required to install plugins")
	}
	if s.opts.Secrets == nil {
		return store.InstalledPlugin{}, errors.New("secrets manager is not configured")
	}
	entry, ok := s.opts.Catalog.Get(id)
	if !ok {
		return store.InstalledPlugin{}, fmt.Errorf("plugin %q is not in the catalog", id)
	}
	if owner := staticServiceTokenOwner(s.opts.Registry, "plugin-"+id); owner != nil {
		return store.InstalledPlugin{}, fmt.Errorf("cannot install plugin %q: service token name conflicts with static plugin %q", id, owner.ID)
	}
	asset, err := s.assetFor(entry)
	if err != nil {
		return store.InstalledPlugin{}, err
	}
	if isZeroDigest(asset.SHA256) {
		return store.InstalledPlugin{}, errors.New("catalog entry not released yet")
	}
	if existing, err := s.opts.Store.InstalledPlugin(ctx, id); err == nil {
		if err := s.ensureInstalled(ctx, *existing); err != nil {
			return *existing, err
		}
		if err := s.startInstalled(*existing, true); err != nil {
			return *existing, err
		}
		latest, err := s.opts.Store.InstalledPlugin(ctx, id)
		if err == nil {
			return *latest, nil
		}
		return *existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.InstalledPlugin{}, err
	}
	if registered, ok := s.opts.Registry.Get(id); ok {
		return store.InstalledPlugin{}, fmt.Errorf("plugin %q is already registered from %s", id, registered.ManifestPath)
	}

	port, err := availableLoopbackPort()
	if err != nil {
		return store.InstalledPlugin{}, err
	}
	assetDir := s.versionDir(entry)
	manifestData, binaryPath, err := s.downloadAndExtract(ctx, entry, asset, assetDir)
	if err != nil {
		return store.InstalledPlugin{}, err
	}
	_ = binaryPath
	if err := validateArchiveManifest(manifestData, entry); err != nil {
		return store.InstalledPlugin{}, err
	}
	stateDir := filepath.Join(s.pluginDir(id), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return store.InstalledPlugin{}, fmt.Errorf("create plugin state directory: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return store.InstalledPlugin{}, fmt.Errorf("secure plugin state directory: %w", err)
	}

	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return store.InstalledPlugin{}, fmt.Errorf("generate plugin secret: %w", err)
	}
	serviceToken, err := enroll.ServiceToken()
	if err != nil {
		return store.InstalledPlugin{}, err
	}
	secret := base64.StdEncoding.EncodeToString(secretBytes)
	tokenName := "plugin-" + id
	now := s.opts.Store.NowMs()
	tokenIDBytes := make([]byte, 16)
	if _, err := rand.Read(tokenIDBytes); err != nil {
		return store.InstalledPlugin{}, fmt.Errorf("generate service token id: %w", err)
	}
	tokenID := "st_" + hex.EncodeToString(tokenIDBytes)
	scopes, err := json.Marshal(entry.Scopes)
	if err != nil {
		return store.InstalledPlugin{}, err
	}
	token := &store.ServiceToken{
		ID: tokenID, Name: tokenName, TokenHash: enroll.Hash(serviceToken),
		ScopesJSON: string(scopes), CreatedAt: now,
	}
	sealed, err := s.sealCredentials(id, credentials{
		PluginSecret: secret, ServiceToken: serviceToken, ServiceTokenName: tokenName,
	})
	if err != nil {
		return store.InstalledPlugin{}, err
	}
	installed := &store.InstalledPlugin{
		ID: id, Version: entry.Version, SealedSecret: sealed, Port: int64(port),
		Status: installStatus, InstalledAt: now, UpdatedAt: now,
	}
	if err := s.opts.Store.CreatePluginInstall(ctx, token, installed); errors.Is(err, store.ErrConflict) {
		token.Name, err = suffixedTokenName(id)
		if err != nil {
			return store.InstalledPlugin{}, err
		}
		if owner := staticServiceTokenOwner(s.opts.Registry, token.Name); owner != nil {
			return store.InstalledPlugin{}, fmt.Errorf("cannot install plugin %q: service token name conflicts with static plugin %q", id, owner.ID)
		}
		installed.SealedSecret, err = s.sealCredentials(id, credentials{
			PluginSecret: secret, ServiceToken: serviceToken, ServiceTokenName: token.Name,
		})
		if err != nil {
			return store.InstalledPlugin{}, err
		}
		if err := s.opts.Store.CreatePluginInstall(ctx, token, installed); err != nil {
			return store.InstalledPlugin{}, err
		}
	} else if err != nil {
		return store.InstalledPlugin{}, err
	}

	plugin, err := s.makePlugin(manifestData, entry, *installed)
	if err != nil {
		_ = s.opts.Store.RevokeServiceToken(ctx, token.ID)
		_ = s.opts.Store.DeleteInstalledPlugin(ctx, id)
		return store.InstalledPlugin{}, err
	}
	if err := s.startInstalled(*installed, true); err != nil {
		_ = s.opts.Store.UpdateInstalledPluginStatus(ctx, id, errorStatus, err.Error())
		latest, getErr := s.opts.Store.InstalledPlugin(ctx, id)
		if getErr == nil {
			return *latest, err
		}
		return *installed, err
	}
	if err := s.opts.Registry.Add(plugin); err != nil {
		s.stopInstalled(id)
		_ = s.opts.Store.UpdateInstalledPluginStatus(ctx, id, errorStatus, err.Error())
		return *installed, err
	}
	latest, err := s.opts.Store.InstalledPlugin(ctx, id)
	if err == nil {
		return *latest, nil
	}
	return *installed, nil
}

func staticServiceTokenOwner(registry *plugins.Registry, tokenName string) *plugins.Plugin {
	if registry == nil {
		return nil
	}
	for _, plugin := range registry.All() {
		if plugin.ManifestPath != "" && plugin.ServiceToken == tokenName {
			return plugin
		}
	}
	return nil
}

func checkStaticServiceTokenCollisions(ctx context.Context, db *store.Store, registry *plugins.Registry) error {
	if db == nil || registry == nil {
		return nil
	}
	installed, err := db.ListInstalledPlugins(ctx)
	if err != nil {
		return fmt.Errorf("list installed plugins for service-token validation: %w", err)
	}
	for _, row := range installed {
		token, err := db.ServiceTokenByID(ctx, row.ServiceTokenID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("load service token for installed plugin %q: %w", row.ID, err)
		}
		if owner := staticServiceTokenOwner(registry, token.Name); owner != nil {
			return fmt.Errorf("static plugin %q service_token conflicts with installed plugin %q", owner.ID, row.ID)
		}
	}
	return nil
}

func (s *Supervisor) Uninstall(ctx context.Context, id string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.opts.Leader != nil && !s.opts.Leader.IsLeader() {
		return errors.New("plugin uninstall is available only on the primary control plane")
	}
	installed, err := s.opts.Store.InstalledPlugin(ctx, id)
	if err != nil {
		return err
	}
	s.stopInstalled(id)
	if err := s.opts.Store.RevokeServiceToken(ctx, installed.ServiceTokenID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if plugin, ok := s.opts.Registry.Get(id); ok && plugin.ManifestPath == "" {
		if err := s.opts.Registry.Remove(id); err != nil {
			return err
		}
	}
	if err := s.opts.Store.DeleteInstalledPlugin(ctx, id); err != nil {
		return err
	}
	entry, ok := s.opts.Catalog.Get(id)
	if ok {
		if err := os.RemoveAll(s.versionDir(entry)); err != nil {
			return fmt.Errorf("remove plugin files: %w", err)
		}
	}
	return nil
}

func (s *Supervisor) Run(ctx context.Context) {
	s.mu.Lock()
	s.rootCtx = ctx
	s.mu.Unlock()
	ticker := time.NewTicker(s.opts.ReconcileInterval)
	defer ticker.Stop()
	for {
		s.reconcile(ctx)
		select {
		case <-ctx.Done():
			s.stopAll()
			return
		case <-ticker.C:
		}
	}
}

func (s *Supervisor) reconcile(ctx context.Context) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	installed, err := s.opts.Store.ListInstalledPlugins(ctx)
	if err != nil {
		s.opts.Log.Error("list installed plugins", "error", err)
		return
	}
	known := make(map[string]struct{}, len(installed))
	leader := s.opts.Leader == nil || s.opts.Leader.IsLeader()
	for _, row := range installed {
		known[row.ID] = struct{}{}
		if err := s.ensureInstalled(ctx, row); err != nil {
			s.opts.Log.Error("prepare installed plugin", "plugin", row.ID, "error", err)
			_ = s.opts.Store.UpdateInstalledPluginStatus(ctx, row.ID, errorStatus, err.Error())
			continue
		}
		if leader {
			if err := s.startInstalled(row, false); err != nil {
				s.opts.Log.Error("start installed plugin", "plugin", row.ID, "error", err)
			}
		} else {
			s.stopInstalled(row.ID)
		}
	}
	stale := make([]string, 0)
	for _, id := range s.opts.Registry.DynamicIDs() {
		if _, ok := known[id]; !ok {
			stale = append(stale, id)
		}
	}
	for _, id := range stale {
		s.stopInstalled(id)
		_ = s.opts.Registry.Remove(id)
	}
}

func (s *Supervisor) ensureInstalled(ctx context.Context, row store.InstalledPlugin) error {
	entry, ok := s.opts.Catalog.Get(row.ID)
	if !ok {
		return fmt.Errorf("installed plugin %q is missing from catalog", row.ID)
	}
	if entry.Version != row.Version {
		if s.opts.Leader == nil || s.opts.Leader.IsLeader() {
			return s.upgradeInstalled(ctx, row, entry)
		}
	}
	asset, err := s.assetFor(entry)
	if err != nil {
		return err
	}
	manifestData, _, err := s.loadOrDownload(ctx, entry, asset)
	if err != nil {
		return err
	}
	plugin, err := s.makePlugin(manifestData, entry, row)
	if err != nil {
		return err
	}
	if existing, ok := s.opts.Registry.Get(row.ID); ok {
		if existing.ManifestPath != "" {
			return fmt.Errorf("installed plugin %q conflicts with static manifest %s", row.ID, existing.ManifestPath)
		}
		return nil
	}
	return s.opts.Registry.Add(plugin)
}

func (s *Supervisor) upgradeInstalled(ctx context.Context, current store.InstalledPlugin, entry plugins.CatalogEntry) (result error) {
	defer func() {
		if result != nil {
			_ = s.opts.Store.UpdateInstalledPluginStatus(ctx, current.ID, errorStatus, result.Error())
		}
	}()
	asset, err := s.assetFor(entry)
	if err != nil {
		return err
	}
	manifestData, binaryPath, err := s.downloadAndExtract(ctx, entry, asset, s.versionDir(entry))
	if err != nil {
		return err
	}
	if err := validateArchiveManifest(manifestData, entry); err != nil {
		return err
	}
	port, err := availableLoopbackPort()
	if err != nil {
		return err
	}
	next := current
	next.Version = entry.Version
	next.Port = int64(port)
	next.Status = installStatus
	next.Error = ""
	next.UpdatedAt = s.opts.Store.NowMs()
	plugin, err := s.makePlugin(manifestData, entry, next)
	if err != nil {
		return err
	}
	candidate, err := s.startCandidate(ctx, next, binaryPath)
	if err != nil {
		return fmt.Errorf("start upgraded plugin: %w", err)
	}
	previous, added, err := s.replacePlugin(plugin)
	if err != nil {
		stopCandidate(candidate)
		return err
	}
	next.Status = runningStatus
	if err := s.opts.Store.SetInstalledPlugin(ctx, &next); err != nil {
		s.rollbackPlugin(plugin.ID, previous, added)
		stopCandidate(candidate)
		return fmt.Errorf("save upgraded plugin: %w", err)
	}

	parent := s.rootCtx
	if parent == nil {
		parent = context.Background()
	}
	runCtx, cancel := context.WithCancel(parent)
	process := &managedProcess{
		cancel: cancel,
		done:   make(chan struct{}),
		ready:  make(chan struct{}),
	}
	process.healthy.Store(true)
	s.signalReady(process, nil)

	s.mu.Lock()
	oldProcess := s.running[current.ID]
	if oldProcess != nil {
		oldProcess.retired.Store(true)
		oldProcess.cancel()
	}
	s.running[current.ID] = process
	s.mu.Unlock()
	if oldProcess != nil {
		<-oldProcess.done
	}
	go s.runProcessLoop(runCtx, next, process, candidate)
	if oldDir := filepath.Join(s.pluginDir(current.ID), current.Version); oldDir != s.versionDir(entry) {
		if err := os.RemoveAll(oldDir); err != nil {
			s.opts.Log.Warn("remove previous plugin version", "plugin", current.ID, "version", current.Version, "error", err)
		}
	}
	s.opts.Log.Info("upgraded managed plugin", "plugin", current.ID, "version", entry.Version)
	return nil
}

func (s *Supervisor) startCandidate(ctx context.Context, row store.InstalledPlugin, binaryPath string) (*runningCommand, error) {
	raw, err := s.opts.Secrets.Open(row.SealedSecret, credentialAAD+row.ID)
	if err != nil {
		return nil, fmt.Errorf("open plugin credentials: %w", err)
	}
	var creds credentials
	if err := jsonUnmarshalCredentials(raw, &creds); err != nil {
		return nil, err
	}
	stateDir := filepath.Join(s.pluginDir(row.ID), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create plugin state directory: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure plugin state directory: %w", err)
	}
	cmd := exec.Command(binaryPath)
	cmd.Dir = stateDir
	cmd.Env = pluginEnvironment(s.opts.ControlPlaneURL, netJoinHostPort("127.0.0.1", int(row.Port)), stateDir, creds)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start plugin candidate: %w", err)
	}
	candidate := &runningCommand{cmd: cmd, waitResult: make(chan error, 1)}
	go func() { candidate.waitResult <- cmd.Wait() }()
	healthy, exited, healthErr := s.waitHealthy(ctx, int(row.Port), candidate.waitResult)
	if !healthy {
		if !exited && cmd.Process != nil {
			_ = cmd.Process.Kill()
			healthErr = errors.Join(healthErr, <-candidate.waitResult)
		}
		if healthErr == nil {
			healthErr = errors.New("plugin candidate did not become healthy")
		}
		return nil, healthErr
	}
	return candidate, nil
}

func (s *Supervisor) replacePlugin(plugin *plugins.Plugin) (*plugins.Plugin, bool, error) {
	if current, ok := s.opts.Registry.Get(plugin.ID); ok {
		if current.ManifestPath != "" {
			return nil, false, fmt.Errorf("installed plugin %q conflicts with static manifest %s", plugin.ID, current.ManifestPath)
		}
		previous, err := s.opts.Registry.ReplaceDynamic(plugin)
		return previous, false, err
	}
	if err := s.opts.Registry.Add(plugin); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

func (s *Supervisor) rollbackPlugin(id string, previous *plugins.Plugin, added bool) {
	if added {
		_ = s.opts.Registry.Remove(id)
	} else if previous != nil {
		_, _ = s.opts.Registry.ReplaceDynamic(previous)
	}
}

func stopCandidate(candidate *runningCommand) {
	if candidate == nil || candidate.cmd == nil || candidate.cmd.Process == nil {
		return
	}
	_ = candidate.cmd.Process.Kill()
	<-candidate.waitResult
}

func (s *Supervisor) makePlugin(manifestData []byte, entry plugins.CatalogEntry, row store.InstalledPlugin) (*plugins.Plugin, error) {
	if s.opts.Secrets == nil {
		return nil, errors.New("secrets manager is not configured")
	}
	raw, err := s.opts.Secrets.Open(row.SealedSecret, credentialAAD+row.ID)
	if err != nil {
		return nil, fmt.Errorf("open plugin credentials: %w", err)
	}
	var secret credentials
	if err := json.Unmarshal([]byte(raw), &secret); err != nil {
		return nil, fmt.Errorf("decode plugin credentials: %w", err)
	}
	if secret.PluginSecret == "" || secret.ServiceToken == "" || secret.ServiceTokenName == "" {
		return nil, errors.New("stored plugin credentials are incomplete")
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", row.Port)
	plugin, err := plugins.NewWithSecret(manifestData, baseURL, secret.ServiceTokenName, []byte(secret.PluginSecret))
	if err != nil {
		return nil, fmt.Errorf("build plugin manifest: %w", err)
	}
	if plugin.ID != entry.ID {
		return nil, fmt.Errorf("archive manifest id %q does not match catalog id %q", plugin.ID, entry.ID)
	}
	return plugin, nil
}

func (s *Supervisor) sealCredentials(id string, creds credentials) (string, error) {
	if s.opts.Secrets == nil {
		return "", errors.New("secrets manager is not configured")
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		return "", err
	}
	sealed, err := s.opts.Secrets.Seal(string(raw), credentialAAD+id)
	if err != nil {
		return "", fmt.Errorf("seal plugin credentials: %w", err)
	}
	return sealed, nil
}

func (s *Supervisor) assetFor(entry plugins.CatalogEntry) (plugins.Asset, error) {
	target := runtime.GOOS + "/" + runtime.GOARCH
	asset, ok := entry.Assets[target]
	if !ok {
		return plugins.Asset{}, fmt.Errorf("plugin %s has no asset for %s", entry.ID, target)
	}
	return asset, nil
}

func (s *Supervisor) pluginDir(id string) string {
	return filepath.Join(s.opts.DataDir, "plugins", id)
}

func (s *Supervisor) versionDir(entry plugins.CatalogEntry) string {
	return filepath.Join(s.pluginDir(entry.ID), entry.Version)
}

func availableLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate plugin loopback port: %w", err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func isZeroDigest(value string) bool {
	return value == strings.Repeat("0", 64)
}

func suffixedTokenName(id string) (string, error) {
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "plugin-" + id + "-" + hex.EncodeToString(random), nil
}
