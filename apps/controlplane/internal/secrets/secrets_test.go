package secrets

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSealOpen(t *testing.T) {
	manager, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := manager.Seal("fleet-secret", "group/autotournament/fleet_key")
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) < len(version) || sealed[:len(version)] != version {
		t.Fatalf("Seal() = %q, want version prefix %q", sealed, version)
	}
	got, err := manager.Open(sealed, "group/autotournament/fleet_key")
	if err != nil || got != "fleet-secret" {
		t.Fatalf("Open() = %q, %v; want fleet-secret, nil", got, err)
	}
}

func TestOpenRejectsWrongAAD(t *testing.T) {
	manager, _ := New(bytes.Repeat([]byte{8}, 32))
	sealed, err := manager.Seal("token", "group/autotournament/api_token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Open(sealed, "other/autotournament/api_token"); err == nil {
		t.Fatal("Open() accepted ciphertext with the wrong AAD")
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	manager, _ := New(bytes.Repeat([]byte{9}, 32))
	sealed, err := manager.Seal("token", "group/autotournament/api_token")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.StdEncoding.DecodeString(sealed[len(version):])
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)-1] ^= 1
	tampered := version + base64.StdEncoding.EncodeToString(payload)
	if _, err := manager.Open(tampered, "group/autotournament/api_token"); err == nil {
		t.Fatal("Open() accepted tampered ciphertext")
	}
}

func TestLoadCreatesPrivateKeyBesideSQLiteDB(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".secrets.key.tmp-stale"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "state.db")
	manager, path, err := Load("", "", "sqlite://"+db)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "secrets.key") {
		t.Fatalf("Load() path = %q, want %q", path, filepath.Join(dir, "secrets.key"))
	}
	key, err := os.ReadFile(path)
	if err != nil || len(key) != 32 {
		t.Fatalf("key file length = %d, err = %v; want 32 bytes", len(key), err)
	}
	sealed, err := manager.Seal("test", "aad")
	if err != nil {
		t.Fatal(err)
	}
	manager2, _, err := Load("", path, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := manager2.Open(sealed, "aad"); err != nil || got != "test" {
		t.Fatalf("Open() = %q, %v; want test, nil", got, err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil {
			t.Fatal(err)
		} else if info.Mode().Perm() != 0o600 {
			t.Fatalf("key permissions = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestLoadLeavesSharedDatabaseUnconfiguredWithoutKey(t *testing.T) {
	manager, path, err := Load("", "", "postgres://db.example.test/varde")
	if err != nil || manager != nil || path != "" {
		t.Fatalf("Load() = %v, %q, %v; want nil manager and path", manager, path, err)
	}
}

func TestLoadReadsConfiguredSharedDatabaseKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.key")
	key := bytes.Repeat([]byte{3}, 32)
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, loadedPath, err := Load("", path, "postgres://db.example.test/varde")
	if err != nil || manager == nil || loadedPath != path {
		t.Fatalf("Load() = %v, %q, %v; want configured key", manager, loadedPath, err)
	}
}

func TestLoadEnvironmentKeyOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.key")
	encodedKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	manager, loadedPath, err := Load(encodedKey, path, "postgres://db.example.test/varde")
	if err != nil || manager == nil || loadedPath != "" {
		t.Fatalf("Load() = %v, %q, %v; want environment key and no key-file path", manager, loadedPath, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("environment key path was accessed: stat error = %v", err)
	}
}

func TestLoadDoesNotGenerateMissingSharedDatabaseKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.key")
	if _, _, err := Load("", path, "postgres://db.example.test/varde"); err == nil {
		t.Fatal("Load() succeeded without the configured shared database key")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing key file was created: stat error = %v", err)
	}
}

func TestLoadWrongLengthKeyIncludesRecoveryGuidance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.key")
	if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load("", path, "postgres://db.example.test/varde")
	if err == nil || !strings.Contains(err.Error(), path) ||
		!strings.Contains(err.Error(), "restore it from backup") ||
		!strings.Contains(err.Error(), "delete it only if no Auto Tournament integrations are linked") {
		t.Fatalf("Load() error = %v; want path and recovery guidance", err)
	}
}
