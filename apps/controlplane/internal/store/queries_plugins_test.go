package store

import (
	"context"
	"errors"
	"testing"
)

func TestInstalledPluginCredentialsAndTokenReuse(t *testing.T) {
	for name, s := range testStores(t) {
		t.Run(name, func(t *testing.T) {
			defer func() { _ = s.Close() }()
			ctx := context.Background()
			now := s.NowMs()
			token := &ServiceToken{
				ID: "st_plugin_" + name, Name: "plugin-autotournament",
				TokenHash: "hash-first-" + name, ScopesJSON: `["members"]`, CreatedAt: now,
			}
			plugin := &InstalledPlugin{
				ID: "autotournament", Version: "0.1.0", SealedSecret: "sealed",
				AssetURL: "https://example.test/plugin.tar.gz", ArchiveSHA256: "archive-digest",
				BinarySHA256: "binary-digest", ManifestJSON: `{"id":"autotournament"}`,
				Port: 7310, Status: "installing", InstalledAt: now, UpdatedAt: now,
			}
			if err := s.CreatePluginInstall(ctx, token, plugin); err != nil {
				t.Fatal(err)
			}
			if plugin.ServiceTokenID != token.ID {
				t.Fatalf("service token id = %q, want %q", plugin.ServiceTokenID, token.ID)
			}
			if err := s.CreatePluginInstall(ctx, &ServiceToken{
				ID: "st_conflict_" + name, Name: token.Name, TokenHash: "hash-conflict-" + name,
				ScopesJSON: `["members"]`, CreatedAt: now,
			}, &InstalledPlugin{ID: "other", Version: "1", SealedSecret: "other", Port: 7000, Status: "installing", InstalledAt: now, UpdatedAt: now}); !errors.Is(err, ErrConflict) {
				t.Fatalf("active token-name conflict = %v, want ErrConflict", err)
			}
			if err := s.RevokeServiceToken(ctx, token.ID); err != nil {
				t.Fatal(err)
			}
			replacement := &ServiceToken{
				ID: "st_replacement_" + name, Name: token.Name,
				TokenHash: "hash-second-" + name, ScopesJSON: `["members","servers"]`, CreatedAt: now + 1,
			}
			plugin.SealedSecret = "sealed-again"
			plugin.UpdatedAt = now + 1
			if err := s.CreatePluginInstall(ctx, replacement, plugin); err != nil {
				t.Fatal(err)
			}
			if replacement.ID != token.ID || plugin.ServiceTokenID != token.ID {
				t.Fatalf("reused token ids = %q/%q, want %q", replacement.ID, plugin.ServiceTokenID, token.ID)
			}
			got, err := s.InstalledPluginByServiceTokenID(ctx, token.ID)
			if err != nil || got.SealedSecret != "sealed-again" || got.AssetURL != plugin.AssetURL ||
				got.ArchiveSHA256 != plugin.ArchiveSHA256 || got.BinarySHA256 != plugin.BinarySHA256 ||
				got.ManifestJSON != plugin.ManifestJSON {
				t.Fatalf("installed plugin = %+v, error = %v", got, err)
			}
			storedToken, err := s.ServiceTokenByID(ctx, token.ID)
			if err != nil || storedToken.TokenHash != replacement.TokenHash || storedToken.RevokedAt != nil {
				t.Fatalf("reused service token = %+v, error = %v", storedToken, err)
			}
			if err := s.SetInstalledPluginAndTokenScopes(ctx, plugin, `["members","servers"]`); err != nil {
				t.Fatal(err)
			}
			storedToken, err = s.ServiceTokenByID(ctx, token.ID)
			if err != nil || !storedToken.Allows("servers") {
				t.Fatalf("updated service token scopes = %+v, error = %v", storedToken, err)
			}
			if err := s.UpdateInstalledPluginStatus(ctx, plugin.ID, "running", ""); err != nil {
				t.Fatal(err)
			}
			got, err = s.InstalledPlugin(ctx, plugin.ID)
			if err != nil || got.Status != "running" {
				t.Fatalf("updated installed plugin = %+v, error = %v", got, err)
			}
			if err := s.DeleteInstalledPlugin(ctx, plugin.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.InstalledPlugin(ctx, plugin.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted plugin lookup error = %v, want ErrNotFound", err)
			}
		})
	}
}
