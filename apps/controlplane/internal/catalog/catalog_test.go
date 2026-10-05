package catalog

import "testing"

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
