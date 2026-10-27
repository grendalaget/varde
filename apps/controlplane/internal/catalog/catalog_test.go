package catalog

import "testing"

func TestCS2Config(t *testing.T) {
	game := Get("cs2")
	if game == nil {
		t.Fatal("cs2 catalog entry missing")
	}
	if game.MinDiskMB != 75000 {
		t.Fatalf("cs2 min_disk_mb = %d, want 75000", game.MinDiskMB)
	}
	if game.SteamApp != 730 || Get("valheim").SteamApp != 0 {
		t.Fatal("CS2 must prefer Steam app 730, other games default to no installed-app policy")
	}
	if got := game.AddonTargetOS["game"]; len(got) != 1 || got[0] != "linux" {
		t.Fatalf("CS2 addon_target_os[game] = %v, want [linux]", got)
	}
	if Get("valheim").MinDiskMB != 0 {
		t.Fatal("omitted min_disk_mb must default to zero")
	}
	for _, tc := range []struct {
		key     string
		value   any
		wantErr bool
	}{
		{"max_players", float64(1), true}, {"max_players", float64(2), false},
		{"max_players", float64(64), false}, {"max_players", float64(65), true},
		{"max_players", 10.5, true}, {"bots", float64(-1), true},
		{"bots", float64(0), false}, {"bots", float64(64), false}, {"bots", float64(65), true},
		{"game_mode", "competitive", false}, {"game_mode", "casual", false},
		{"game_mode", "wingman", false}, {"game_mode", "arms_race", false},
		{"game_mode", "deathmatch", false}, {"game_mode", "unknown", true},
		{"password", "", false}, {"gslt", "", false}, {"server_name", "", true},
	} {
		cfg := map[string]any{"server_name": "Varde"}
		cfg[tc.key] = tc.value
		errs := game.ValidateConfig(cfg)
		if (len(errs) > 0) != tc.wantErr {
			t.Errorf("%s=%v: errors=%v wantErr=%v", tc.key, tc.value, errs, tc.wantErr)
		}
	}
	if errs := game.ValidateConfig(map[string]any{"server_name": "Varde"}); len(errs) > 0 {
		t.Fatalf("minimal config: %v", errs)
	}
	if errs := game.ValidateConfig(map[string]any{}); len(errs) == 0 {
		t.Fatal("missing server_name accepted")
	}
	for _, name := range []string{"password", "gslt"} {
		found := false
		for _, field := range game.ConfigFields {
			if field.Name == name {
				found = field.Secret && field.Type == "secret" && !field.Required
			}
		}
		if !found {
			t.Errorf("%s must be an optional secret", name)
		}
	}
}

func TestValidateAddonTargetOS(t *testing.T) {
	for _, tc := range []struct {
		name    string
		game    Game
		wantErr bool
	}{
		{
			name: "target not supported",
			game: Game{ID: "example", OS: []string{"linux"}, AddonTargets: []string{"server"},
				AddonTargetOS: map[string][]string{"game": {"linux"}}},
			wantErr: true,
		},
		{
			name: "os not supported",
			game: Game{ID: "example", OS: []string{"linux"}, AddonTargets: []string{"game"},
				AddonTargetOS: map[string][]string{"game": {"windows"}}},
			wantErr: true,
		},
		{
			name: "valid",
			game: Game{ID: "example", OS: []string{"linux", "windows"}, AddonTargets: []string{"game"},
				AddonTargetOS: map[string][]string{"game": {"linux"}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAddonTargetOS([]Game{tc.game})
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("validateAddonTargetOS(%+v) error = %v, wantErr %v", tc.game, err, tc.wantErr)
			}
		})
	}
}

func TestPlacementOS(t *testing.T) {
	game := &Game{
		OS: []string{"linux", "windows"},
		AddonTargetOS: map[string][]string{
			"game":   {"linux"},
			"server": {"windows"},
		},
	}
	for _, tc := range []struct {
		name    string
		targets []string
		want    []string
	}{
		{name: "no addons", want: []string{"linux", "windows"}},
		{name: "game addon", targets: []string{"game"}, want: []string{"linux"}},
		{name: "server addon", targets: []string{"server"}, want: []string{"windows"}},
		{name: "conflicting targets", targets: []string{"game", "server"}, want: []string{}},
		{name: "unknown legacy target", targets: []string{""}, want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PlacementOS(game, tc.targets)
			if len(got) != len(tc.want) {
				t.Fatalf("PlacementOS(%v) = %v, want %v", tc.targets, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("PlacementOS(%v) = %v, want %v", tc.targets, got, tc.want)
				}
			}
		})
	}
}

func TestValheimSaveIntervalBounds(t *testing.T) {
	game := Get("valheim")
	if game == nil {
		t.Fatal("valheim catalog entry missing")
	}

	for _, tc := range []struct {
		value   float64
		wantErr bool
	}{
		{value: 59, wantErr: true},
		{value: 60},
		{value: 3600},
		{value: 3601, wantErr: true},
		{value: 300.5, wantErr: true},
	} {
		cfg := map[string]any{
			"server_name":     "Varde",
			"world_name":      "World",
			"password":        "secret",
			"save_interval_s": tc.value,
		}
		errs := game.ValidateConfig(cfg)
		if gotErr := len(errs) > 0; gotErr != tc.wantErr {
			t.Errorf("save_interval_s=%v: got errors %v, wantErr=%v", tc.value, errs, tc.wantErr)
		}
	}
}
