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
