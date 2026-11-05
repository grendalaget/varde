package main

import "testing"

func TestParseGateway(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"absent", "", true},
		{"valid", "url=wss://gateway.example.org/v1/tunnel,domain=example.site", true},
		{"plaintext", "url=ws://gateway.example.org/v1/tunnel,domain=example.site", false},
		{"missing domain", "url=wss://gateway.example.org/v1/tunnel", false},
		{"bad domain", "url=wss://gateway.example.org/v1/tunnel,domain=invalid/path", false},
		{"credentials", "url=wss://user:password@gateway.example.org/v1/tunnel,domain=example.site", false},
		{"query", "url=wss://gateway.example.org/v1/tunnel?token=x,domain=example.site", false},
		{"wrong path", "url=wss://gateway.example.org/other,domain=example.site", false},
		{"duplicate", "url=wss://gateway.example.org/v1/tunnel,domain=example.site,domain=other.site", false},
		{"unknown", "url=wss://gateway.example.org/v1/tunnel,domain=example.site,extra=x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGateway(tc.value)
			if (err == nil) != tc.valid {
				t.Errorf("parseGateway(%q) error = %v, want valid %t", tc.value, err, tc.valid)
			}
			if tc.value == "" && got != nil {
				t.Errorf("parseGateway(empty) = %v, want nil", got)
			}
		})
	}
}
