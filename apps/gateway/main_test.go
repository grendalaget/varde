package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveCPKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, encoded := range []string{base64.StdEncoding.EncodeToString(pub), base64.RawURLEncoding.EncodeToString(pub)} {
		got, err := resolveCPKey(encoded)
		if err != nil || !pub.Equal(got) {
			t.Fatal("valid public key rejected")
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key":
			_, _ = fmt.Fprintf(w, `{"public_key":%q}`, base64.StdEncoding.EncodeToString(pub))
		case "/empty":
			_, _ = fmt.Fprint(w, `{}`)
		default:
			http.Error(w, "missing", 404)
		}
	}))
	defer srv.Close()
	if got, err := resolveCPKey(srv.URL + "/key"); err != nil || !pub.Equal(got) {
		t.Fatal("public-key endpoint failed")
	}
	for _, input := range []string{"", "invalid", srv.URL + "/empty", srv.URL + "/missing"} {
		if _, err := resolveCPKey(input); err == nil {
			t.Fatal("invalid public key accepted")
		}
	}
}

func TestProductionRequiresTLSAndDevIsLoopbackOnly(t *testing.T) {
	if err := run(":8443", "example.com", "", "", "", "", false, 128, 1024); err == nil {
		t.Fatal("production did not require TLS")
	}
	if err := run(":8080", "example.com", "", "", "", "", true, 128, 1024); err == nil {
		t.Fatal("dev mode bound publicly")
	}
}
