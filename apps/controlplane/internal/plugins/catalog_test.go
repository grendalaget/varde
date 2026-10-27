package plugins

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEmbeddedCatalog(t *testing.T) {
	catalog, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := catalog.Get("autotournament")
	if !ok {
		t.Fatal("Auto Tournament entry missing")
	}
	if entry.Version != "0.1.0" || len(entry.Assets) != 6 {
		t.Fatalf("catalog version/assets = %q/%d", entry.Version, len(entry.Assets))
	}
	for target, asset := range entry.Assets {
		if !strings.HasPrefix(asset.URL, "https://github.com/grendalaget/varde-plugin-autotournament/") {
			t.Errorf("%s URL = %q", target, asset.URL)
		}
		if asset.SHA256 != strings.Repeat("0", 64) {
			t.Errorf("%s digest = %q, want release placeholder", target, asset.SHA256)
		}
	}
}

func TestParseCatalogValidatesEntries(t *testing.T) {
	entry := CatalogEntry{
		ID: "sample-plugin", Name: "Sample", Description: "Example", Version: "1.0.0",
		Scopes: []string{"members"},
		Assets: map[string]Asset{"linux/amd64": {
			URL: "https://example.test/plugin.tar.gz", SHA256: strings.Repeat("a", 64),
		}},
	}
	tests := []struct {
		name    string
		mutate  func(*CatalogEntry)
		wantErr string
	}{
		{
			name: "invalid target",
			mutate: func(entry *CatalogEntry) {
				entry.Assets["linux/amd64"] = Asset{}
				entry.Assets["linux/amd64"] = Asset{
					URL: "https://example.test/plugin.tar.gz", SHA256: strings.Repeat("a", 64),
				}
				entry.Assets["linux"] = entry.Assets["linux/amd64"]
				delete(entry.Assets, "linux/amd64")
			},
			wantErr: "invalid runtime target",
		},
		{
			name: "invalid digest",
			mutate: func(entry *CatalogEntry) {
				asset := entry.Assets["linux/amd64"]
				asset.SHA256 = "not-a-digest"
				entry.Assets["linux/amd64"] = asset
			},
			wantErr: "SHA-256",
		},
		{
			name: "unsafe version",
			mutate: func(entry *CatalogEntry) {
				entry.Version = "../latest"
			},
			wantErr: "invalid version",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := cloneCatalogEntry(entry)
			tt.mutate(&value)
			data, err := json.Marshal([]CatalogEntry{value})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseCatalog(data); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseCatalog error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
