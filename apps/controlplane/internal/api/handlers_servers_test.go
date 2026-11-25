package api

import (
	"github.com/grendalaget/varde/apps/controlplane/internal/catalog"
	"testing"
)

func TestGenGameInstalledSteamPolicy(t *testing.T) {
	game := genGame(catalog.Get("cs2"))
	if game.SteamApp == nil || *game.SteamApp != 730 || game.MinDiskMb == nil || *game.MinDiskMb != 75000 {
		t.Fatalf("CS2 policy missing from API: %+v", game)
	}
	other := genGame(catalog.Get("valheim"))
	if other.SteamApp != nil || other.MinDiskMb != nil {
		t.Fatalf("unexpected installed-app policy: %+v", other)
	}
}

func TestGenGameConfigBounds(t *testing.T) {
	game := genGame(catalog.Get("cs2"))
	for _, field := range game.ConfigFields {
		if field.Name != "max_players" {
			continue
		}
		if field.Min == nil || *field.Min != 2 || field.Max == nil || *field.Max != 64 {
			t.Fatalf("CS2 player bounds missing from API: %+v", field)
		}
		return
	}
	t.Fatal("CS2 max_players missing from API")
}
