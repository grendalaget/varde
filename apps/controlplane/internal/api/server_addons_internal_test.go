package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/secrets"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func TestDirectiveServerAddonsUnsealsStoredSpec(t *testing.T) {
	st, err := store.Open("sqlite://"+filepath.ToSlash(filepath.Join(t.TempDir(), "cp.db")), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	spec := gen.ServerAddonSpec{
		Url: "https://example.test/addon.zip", Sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Archive: gen.ServerAddonSpecArchiveZip, Target: "server",
		DriverOptions: map[string]any{"enabled": true}, Files: map[string]string{"cfg/x.cfg": "addon file"},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := secrets.New(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := manager.Seal(string(raw), serverAddonAAD("group", "server", "plug", "addon"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertServerAddon(t.Context(), store.ServerAddon{
		ServerID: "server", PluginID: "plug", AddonID: "addon", Spec: sealed,
		CreatedAt: 1, UpdatedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Store: st, Secrets: manager,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	got := srv.directiveServerAddons(t.Context(), &store.Server{ID: "server", GroupID: "group"})
	if len(*got) != 1 {
		t.Fatalf("directiveServerAddons() returned %d items, want 1", len(*got))
	}
	addon := (*got)[0]
	if addon.PluginId != "plug" || addon.AddonId != "addon" || addon.Files["cfg/x.cfg"] != "addon file" ||
		addon.DriverOptions["enabled"] != true {
		t.Errorf("directive addon = %+v, want decrypted server addon", addon)
	}
}
