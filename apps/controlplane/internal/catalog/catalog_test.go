package catalog

import "testing"

func TestCS2Config(t *testing.T) {
	game := Get("cs2")
	if game == nil {
		t.Fatal("cs2 catalog entry missing")
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
