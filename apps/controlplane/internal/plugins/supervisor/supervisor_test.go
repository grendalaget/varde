package supervisor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/grendalaget/varde/apps/controlplane/internal/enroll"
	"github.com/grendalaget/varde/apps/controlplane/internal/plugins"
	"github.com/grendalaget/varde/apps/controlplane/internal/secrets"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func TestExtractRejectsTraversalAndSymlink(t *testing.T) {
	for _, test := range []struct {
		name string
		hdr  *tar.Header
		data []byte
	}{
		{name: "traversal", hdr: &tar.Header{Name: "../outside", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}, data: []byte("x")},
		{name: "symlink", hdr: &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "target"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := writeTestArchive(t, []archiveEntry{{header: test.hdr, data: test.data}})
			path := filepath.Join(t.TempDir(), "plugin.tar.gz")
			if err := os.WriteFile(path, archive, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := extractTarGz(path, t.TempDir()); err == nil {
				t.Fatal("extractTarGz succeeded for unsafe archive")
			}
		})
	}
}

func TestDownloadRejectsSHA256Mismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not the pinned asset"))
	}))
	defer server.Close()
	supervisor := &Supervisor{opts: Options{DataDir: t.TempDir(), HTTPClient: server.Client()}}
	_, _, err := supervisor.downloadAndExtract(context.Background(), plugins.CatalogEntry{
		ID: "test-plugin", Version: "1.0.0",
	}, plugins.Asset{URL: server.URL, SHA256: "1" + strings.Repeat("0", 63)}, filepath.Join(t.TempDir(), "plugins", "test-plugin", "1.0.0"))
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("download error = %v, want SHA-256 mismatch", err)
	}
}

func TestInstallRestartAndUninstall(t *testing.T) {
	binary := buildTestPlugin(t)
	manifest := []byte(`{"id":"test-plugin","name":"Test plugin","version":"1.0.0","base_url":"http://127.0.0.1:1","secret_env":"UNUSED","service_token":"plugin-test-plugin","pages":[{"id":"home","title":"Home","list":{"endpoint":"/list"}}]}`)
	archive := releaseArchive(t, binary, manifest)
	hash := sha256.Sum256(archive)
	digest := hex.EncodeToString(hash[:])
	assetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer assetServer.Close()
	catalog, err := plugins.ParseCatalog(mustJSON(t, []plugins.CatalogEntry{{
		ID: "test-plugin", Name: "Test plugin", Description: "Supervisor integration test",
		Version: "1.0.0", Scopes: []string{"members", "servers"},
		Assets: map[string]plugins.Asset{runtime.GOOS + "/" + runtime.GOARCH: {URL: assetServer.URL, SHA256: digest}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	db, err := store.Open("sqlite://"+filepath.Join(root, "cp.db"), store.RealClock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	key := bytes.Repeat([]byte("k"), 32)
	manager, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("VARDE_SECRETS_KEY", base64.StdEncoding.EncodeToString(key))
	registry, err := plugins.Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := New(Options{
		DataDir: root, ControlPlaneURL: "http://127.0.0.1:8080", Store: db,
		Registry: registry, Catalog: catalog, Secrets: manager, HTTPClient: assetServer.Client(),
		HealthTimeout: 5 * time.Second, RestartMin: 25 * time.Millisecond,
		RestartMax: 100 * time.Millisecond, ReconcileInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		supervisor.Run(ctx)
		close(runDone)
	}()
	defer func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(3 * time.Second):
			t.Error("supervisor did not stop")
		}
	}()

	installed, err := supervisor.Install(ctx, "test-plugin")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if installed.Status != runningStatus {
		t.Fatalf("installed status = %q, want %q", installed.Status, runningStatus)
	}
	if _, ok := registry.Get("test-plugin"); !ok {
		t.Fatal("installed plugin was not registered")
	}
	stateDir := filepath.Join(root, "plugins", "test-plugin", "state")
	info, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("state directory mode = %o, want 700", info.Mode().Perm())
	}

	port := installed.Port
	listURL := fmt.Sprintf("http://127.0.0.1:%d/list", port)
	if body := getTestURL(t, listURL); !strings.Contains(body, `"title":"start 1"`) {
		t.Fatalf("plugin list response = %s", body)
	}
	crashURL := fmt.Sprintf("http://127.0.0.1:%d/crash", port)
	if body := getTestURL(t, crashURL); !strings.Contains(body, "restarting") {
		t.Fatalf("plugin crash response = %s", body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := tryGetTestURL(listURL); err == nil && strings.Contains(body, `"title":"start 2"`) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if body := getTestURL(t, listURL); !strings.Contains(body, `"title":"start 2"`) {
		t.Fatalf("plugin did not restart after crash, last list response = %s", body)
	}

	if err := supervisor.Uninstall(ctx, "test-plugin"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	token, err := db.ServiceTokenByID(ctx, installed.ServiceTokenID)
	if err != nil {
		t.Fatal(err)
	}
	if token.RevokedAt == nil {
		t.Fatal("uninstall did not revoke the service token")
	}
	if _, ok := registry.Get("test-plugin"); ok {
		t.Fatal("uninstall left the plugin registered")
	}
	if _, err := db.InstalledPlugin(ctx, "test-plugin"); err == nil {
		t.Fatal("uninstall left the installed-plugin row")
	}
	if _, err := os.Stat(stateDir); err != nil {
		t.Fatalf("uninstall removed persistent state: %v", err)
	}
}

func TestCatalogUpgradeKeepsOldProcessUntilCandidateIsHealthy(t *testing.T) {
	binary := buildTestPlugin(t)
	manifestV1 := []byte(`{"id":"test-plugin","name":"Test plugin","version":"1.0.0","base_url":"http://127.0.0.1:1","secret_env":"UNUSED","service_token":"plugin-test-plugin"}`)
	manifestV2 := []byte(`{"id":"test-plugin","name":"Test plugin","version":"2.0.0","base_url":"http://127.0.0.1:1","secret_env":"UNUSED","service_token":"plugin-test-plugin"}`)
	archiveV1 := releaseArchive(t, binary, manifestV1)
	archiveV2 := releaseArchive(t, binary, manifestV2)
	invalidBinary := filepath.Join(t.TempDir(), binaryName("test-plugin"))
	if err := os.WriteFile(invalidBinary, []byte("not an executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	archiveV2Invalid := releaseArchive(t, invalidBinary, manifestV2)
	archives := map[string][]byte{
		"/v1":       archiveV1,
		"/v2":       archiveV2,
		"/v2-invalid": archiveV2Invalid,
	}
	assetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archive, ok := archives[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(archive)
	}))
	defer assetServer.Close()
	catalogFor := func(version, path string, archive []byte) *plugins.Catalog {
		hash := sha256.Sum256(archive)
		catalog, err := plugins.ParseCatalog(mustJSON(t, []plugins.CatalogEntry{{
			ID: "test-plugin", Name: "Test plugin", Description: "Supervisor upgrade test",
			Version: version, Scopes: []string{"members"},
			Assets: map[string]plugins.Asset{runtime.GOOS + "/" + runtime.GOARCH: {
				URL: assetServer.URL + path, SHA256: hex.EncodeToString(hash[:]),
			}},
		}}))
		if err != nil {
			t.Fatal(err)
		}
		return catalog
	}

	root := t.TempDir()
	db, err := store.Open("sqlite://"+filepath.Join(root, "cp.db"), store.RealClock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	key := bytes.Repeat([]byte("u"), 32)
	manager, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("VARDE_SECRETS_KEY", base64.StdEncoding.EncodeToString(key))
	registry := mustEmptyRegistry(t)
	supervisor, err := New(Options{
		DataDir: root, ControlPlaneURL: "http://127.0.0.1:8080", Store: db,
		Registry: registry, Catalog: catalogFor("1.0.0", "/v1", archiveV1),
		Secrets: manager, HTTPClient: assetServer.Client(), HealthTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.stopAll()
	installed, err := supervisor.Install(context.Background(), "test-plugin")
	if err != nil {
		t.Fatalf("install initial version: %v", err)
	}
	oldHealth := fmt.Sprintf("http://127.0.0.1:%d/healthz", installed.Port)
	if body := getTestURL(t, oldHealth); body != "ok" {
		t.Fatalf("initial plugin health response = %q", body)
	}

	supervisor.opts.Catalog = catalogFor("2.0.0", "/v2-invalid", archiveV2Invalid)
	supervisor.reconcile(context.Background())
	current, err := db.InstalledPlugin(context.Background(), "test-plugin")
	if err != nil {
		t.Fatal(err)
	}
	if current.Version != "1.0.0" {
		t.Fatalf("failed upgrade changed stored version to %q", current.Version)
	}
	if plugin, ok := registry.Get("test-plugin"); !ok || plugin.Version != "1.0.0" {
		t.Fatalf("failed upgrade changed registry plugin: %#v, registered=%v", plugin, ok)
	}
	if body := getTestURL(t, oldHealth); body != "ok" {
		t.Fatalf("failed upgrade health response = %q", body)
	}

	supervisor.opts.Catalog = catalogFor("2.0.0", "/v2", archiveV2)
	supervisor.reconcile(context.Background())
	current, err = db.InstalledPlugin(context.Background(), "test-plugin")
	if err != nil {
		t.Fatal(err)
	}
	if current.Version != "2.0.0" || current.Port == installed.Port {
		t.Fatalf("successful upgrade row = version %q, port %d; want 2.0.0 on a new port", current.Version, current.Port)
	}
	if plugin, ok := registry.Get("test-plugin"); !ok || plugin.Version != "2.0.0" {
		t.Fatalf("successful upgrade registry plugin: %#v, registered=%v", plugin, ok)
	}
	if _, err := os.Stat(filepath.Join(root, "plugins", "test-plugin", "1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("old version directory remains after upgrade: %v", err)
	}
	newHealth := fmt.Sprintf("http://127.0.0.1:%d/healthz", current.Port)
	if body := getTestURL(t, newHealth); body != "ok" {
		t.Fatalf("upgraded plugin health response = %q", body)
	}
}

func TestNonLeaderDoesNotSpawnInstalledPlugin(t *testing.T) {
	registry, err := plugins.Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := plugins.ParseCatalog([]byte(`[{"id":"test-plugin","name":"Test","description":"Test","version":"1.0.0","scopes":["members"],"assets":{"linux/amd64":{"url":"https://example.com/plugin.tar.gz","sha256":"` + strings.Repeat("0", 64) + `"}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open("sqlite://"+filepath.Join(t.TempDir(), "cp.db"), store.RealClock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	supervisor, err := New(Options{
		DataDir: t.TempDir(), Store: db, Registry: registry, Catalog: catalog, Leader: testLeader(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.startInstalled(store.InstalledPlugin{ID: "test-plugin", Port: 7310}, true); err != nil {
		t.Fatal(err)
	}
	if len(supervisor.running) != 0 {
		t.Fatalf("non-leader process count = %d, want zero", len(supervisor.running))
	}
}

func TestBootStartsInstalledPluginOnlyOnLeader(t *testing.T) {
	binary := buildTestPlugin(t)
	manifest := []byte(`{"id":"test-plugin","name":"Test plugin","version":"1.0.0","base_url":"http://127.0.0.1:1","secret_env":"UNUSED","service_token":"plugin-test-plugin","pages":[{"id":"home","title":"Home","list":{"endpoint":"/list"}}]}`)
	root := t.TempDir()
	db, err := store.Open("sqlite://"+filepath.Join(root, "cp.db"), store.RealClock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	key := bytes.Repeat([]byte("b"), 32)
	manager, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("VARDE_SECRETS_KEY", base64.StdEncoding.EncodeToString(key))
	catalog, err := plugins.ParseCatalog([]byte(`[{"id":"test-plugin","name":"Test plugin","description":"Test","version":"1.0.0","scopes":["members"],"assets":{"` + runtime.GOOS + `/` + runtime.GOARCH + `":{"url":"https://example.com/plugin.tar.gz","sha256":"` + strings.Repeat("0", 64) + `"}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	versionDir := filepath.Join(root, "plugins", "test-plugin", "1.0.0")
	if err := os.MkdirAll(versionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	binaryData, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, binaryName("test-plugin")), binaryData, 0o755); err != nil {
		t.Fatal(err)
	}
	serviceToken, err := enroll.ServiceToken()
	if err != nil {
		t.Fatal(err)
	}
	tempSupervisor, err := New(Options{
		DataDir: root, Store: db, Registry: mustEmptyRegistry(t), Catalog: catalog, Secrets: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := db.NowMs()
	sealed, err := tempSupervisor.sealCredentials("test-plugin", credentials{
		PluginSecret: strings.Repeat("s", 32), ServiceToken: serviceToken, ServiceTokenName: "plugin-test-plugin",
	})
	if err != nil {
		t.Fatal(err)
	}
	row := &store.InstalledPlugin{
		ID: "test-plugin", Version: "1.0.0", SealedSecret: sealed, Port: int64(mustPort(t)),
		Status: installStatus, InstalledAt: now, UpdatedAt: now,
	}
	if err := db.CreatePluginInstall(context.Background(), &store.ServiceToken{
		ID: "st_boot_test", Name: "plugin-test-plugin", TokenHash: enroll.Hash(serviceToken),
		ScopesJSON: `["members"]`, CreatedAt: now,
	}, row); err != nil {
		t.Fatal(err)
	}

	nonLeaderRegistry := mustEmptyRegistry(t)
	nonLeader, err := New(Options{
		DataDir: root, Store: db, Registry: nonLeaderRegistry, Catalog: catalog, Secrets: manager,
		Leader: testLeader(false), ReconcileInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	nonLeaderCtx, stopNonLeader := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { nonLeader.Run(nonLeaderCtx); close(done) }()
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(root, "plugins", "test-plugin", "state", "starts")); !os.IsNotExist(err) {
		t.Fatalf("non-leader started plugin, state marker stat error = %v", err)
	}
	stopNonLeader()
	<-done

	leaderRegistry := mustEmptyRegistry(t)
	leader, err := New(Options{
		DataDir: root, ControlPlaneURL: "http://127.0.0.1:8080", Store: db,
		Registry: leaderRegistry, Catalog: catalog, Secrets: manager, Leader: testLeader(true),
		HealthTimeout: 5 * time.Second, RestartMin: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan struct{})
	go func() {
		leader.Run(ctx)
		close(leaderDone)
	}()
	defer func() {
		cancel()
		select {
		case <-leaderDone:
		case <-time.After(3 * time.Second):
			t.Error("leader supervisor did not stop")
		}
	}()
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", row.Port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := tryGetTestURL(healthURL); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := tryGetTestURL(healthURL); err != nil {
		t.Fatalf("leader did not start installed plugin: %v", err)
	}
}

func mustEmptyRegistry(t *testing.T) *plugins.Registry {
	t.Helper()
	registry, err := plugins.Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func mustPort(t *testing.T) int {
	t.Helper()
	port, err := availableLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	return port
}

type testLeader bool

func (leader testLeader) IsLeader() bool { return bool(leader) }

type archiveEntry struct {
	header *tar.Header
	data   []byte
}

func writeTestArchive(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		if err := tw.WriteHeader(entry.header); err != nil {
			t.Fatal(err)
		}
		if len(entry.data) > 0 {
			if _, err := tw.Write(entry.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func buildTestPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	program := `package main
import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)
func main() {
	state := os.Getenv("STATE_DIR")
	listen := os.Getenv("LISTEN")
	if len(os.Getenv("VARDE_PLUGIN_SECRET")) < 32 ||
		!strings.HasPrefix(os.Getenv("VARDE_SERVICE_TOKEN"), "vst_") ||
		os.Getenv("VARDE_URL") != "http://127.0.0.1:8080" ||
		!strings.HasPrefix(listen, "127.0.0.1:") {
		os.Exit(2)
	}
	countPath := filepath.Join(state, "starts")
	count := 1
	if data, err := os.ReadFile(countPath); err == nil {
		_, _ = fmt.Sscanf(string(data), "%d", &count)
		count++
	}
	if err := os.WriteFile(countPath, []byte(fmt.Sprint(count)), 0600); err != nil {
		os.Exit(3)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/list", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]string{{"title": fmt.Sprintf("start %d", count)}}})
	})
	mux.HandleFunc("/crash", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("restarting"))
		if f, ok := w.(http.Flusher); ok { f.Flush() }
		go func() { time.Sleep(50 * time.Millisecond); os.Exit(0) }()
	})
	if err := http.ListenAndServe(listen, mux); err != nil { os.Exit(4) }
}
`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "varde-plugin-test-plugin"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	output := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", output, source)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test plugin: %v\n%s", err, result)
	}
	return output
}

func releaseArchive(t *testing.T, binary string, manifest []byte) []byte {
	t.Helper()
	binaryData, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	binaryName := filepath.Base(binary)
	return writeTestArchive(t, []archiveEntry{
		{header: &tar.Header{Name: binaryName, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(binaryData))}, data: binaryData},
		{header: &tar.Header{Name: "manifest.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(manifest))}, data: manifest},
		{header: &tar.Header{Name: "LICENSE", Typeflag: tar.TypeReg, Mode: 0o644, Size: 0}},
		{header: &tar.Header{Name: "README.md", Typeflag: tar.TypeReg, Mode: 0o644, Size: 0}},
	})
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func getTestURL(t *testing.T, rawURL string) string {
	t.Helper()
	response, err := http.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer func() { _ = response.Body.Close() }()
	var body bytes.Buffer
	_, _ = body.ReadFrom(response.Body)
	return body.String()
}

func tryGetTestURL(rawURL string) (string, error) {
	response, err := http.Get(rawURL)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	var body bytes.Buffer
	_, err = body.ReadFrom(response.Body)
	return body.String(), err
}
