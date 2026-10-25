package api_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Bodies are capped before agentAuth buffers them for the signature check
// and before the strict decoder reads them; oversized requests get 413.
func TestRequestBodyCaps(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("node-a")
	big := strings.Repeat("x", 2<<20)

	hb, _ := json.Marshal(map[string]any{"agent_version": big})
	if r := a.signedDo("POST", "/v1/agent/heartbeat", hb, 0, nil); r.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("2 MiB heartbeat: want 413, got %d %s", r.Status, r.Raw)
	}
	if r := e.do("POST", "/v1/groups", map[string]any{"name": big}, e.token); r.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("2 MiB public body: want 413, got %d %.200s", r.Status, r.Raw)
	}
	if r := e.do("POST", "/v1/agent/enroll/token", map[string]any{"token": big}, ""); r.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("2 MiB enroll body: want 413, got %d %.200s", r.Status, r.Raw)
	}

	// a full log batch of long lines still fits
	srv := e.createServer("testgame", "s1", nil)
	host, execID, _ := e.startToRunning(srv, a)
	lines := make([]map[string]any, 100)
	for i := range lines {
		lines[i] = map[string]any{"at": e.clk.ms, "stream": "stdout", "line": strings.Repeat("y", 20_000)}
	}
	logs, _ := json.Marshal(map[string]any{"lines": lines})
	if r := host.signedDo("POST", "/v1/agent/executions/"+execID+"/logs", logs, 0, nil); r.Status != http.StatusNoContent {
		t.Fatalf("2 MB log batch: want 204, got %d %s", r.Status, r.Raw)
	}
}

// Failed device-code lookups are limited per user, so codes can't be
// enumerated to approve strangers' machines, without locking out other users.
func TestDeviceLinkGuessingIsRateLimited(t *testing.T) {
	e := newEnv(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	code := e.mustOK(e.do("POST", "/v1/agent/enroll/device", map[string]any{
		"public_key": base64.StdEncoding.EncodeToString(pub),
		"hostname":   "desktop", "os": "linux", "arch": "x86_64",
	}, ""))["user_code"].(string)

	for i := 0; i < 10; i++ {
		if r := e.do("GET", "/v1/device-links/WOLF-1"+string(rune('0'+i))+"-KITE", nil, e.token); r.Status != 404 {
			t.Fatalf("guess %d: want 404, got %d %s", i, r.Status, r.Raw)
		}
	}
	limited := func(r apiResp) bool { return r.Status == 403 && r.Body["code"] == "limit_exceeded" }
	if r := e.do("GET", "/v1/device-links/"+code, nil, e.token); !limited(r) {
		t.Fatalf("lookup after 10 failures: want limit_exceeded, got %d %s", r.Status, r.Raw)
	}
	if r := e.do("POST", "/v1/device-links/approve",
		map[string]any{"user_code": code, "group_id": e.group, "name": "desktop"}, e.token); !limited(r) {
		t.Fatalf("approve after 10 failures: want limit_exceeded, got %d %s", r.Status, r.Raw)
	}
	// other accounts (often behind the same proxy address) are unaffected
	other := e.signup("other@example.com", "password123")
	if r := e.do("GET", "/v1/device-links/"+code, nil, other); r.Status != 200 {
		t.Fatalf("other user: want 200, got %d %s", r.Status, r.Raw)
	}

	e.clk.advance(60_000)
	e.mustOK(e.do("GET", "/v1/device-links/"+code, nil, e.token))
}
