package gatewaytoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := int64(1000)
	claims := Claims{Audience: Audience, Domain: "servers.example.com", Label: "tournament", GroupID: "group", ServerID: "server", Epoch: 3, ExpUnixMs: 2000}
	token, err := Sign(priv, claims)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(pub, token, claims.Domain, now)
	if err != nil || *got != claims {
		t.Fatal("valid token rejected")
	}
	for _, bad := range []string{"", "broken", token + "x", base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(`{}`)))} {
		if _, err := Verify(pub, bad, claims.Domain, now); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify(other, token, claims.Domain, now); err == nil {
		t.Fatal("wrong signing key accepted")
	}
	if _, err := Verify(nil, token, claims.Domain, now); err == nil {
		t.Fatal("missing key accepted")
	}
	if _, err := Verify(pub, token, "other.example.com", now); err == nil {
		t.Fatal("wrong domain accepted")
	}
	for _, mutate := range []func(*Claims){
		func(c *Claims) { c.Audience = "relay" }, func(c *Claims) { c.GroupID = "" }, func(c *Claims) { c.ServerID = "" }, func(c *Claims) { c.Epoch = 0 }, func(c *Claims) { c.ExpUnixMs = now }, func(c *Claims) { c.Label = "two.parts" }, func(c *Claims) { c.Label = "-invalid" }, func(c *Claims) { c.Label = "UPPER" },
	} {
		c := claims
		mutate(&c)
		tok, _ := Sign(priv, c)
		if _, err := Verify(pub, tok, claims.Domain, now); err == nil {
			t.Fatal("bad claims accepted")
		}
	}
}

func TestDNSValidation(t *testing.T) {
	for _, s := range []string{"a", "hello-123", "123"} {
		if !ValidLabel(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"", "a.b", "a_1", "-a", "a-", "UPPER"} {
		if ValidLabel(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"", "localhost", "a..com", "a.com.", "user@a.com"} {
		if ValidDomain(s) {
			t.Fatal(s)
		}
	}
	if !ValidDomain("servers.example.com") {
		t.Fatal("valid domain rejected")
	}
}
