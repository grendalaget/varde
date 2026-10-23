package secrets

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
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
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("key permissions = %o, want 600", info.Mode().Perm())
	}
}
