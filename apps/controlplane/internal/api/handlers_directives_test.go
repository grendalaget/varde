package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func (a *agent) waitForDirectives(digest string, timeoutMs int) apiResp {
	path := fmt.Sprintf(
		"/v1/agent/directives/wait?digest=%s&timeout_ms=%d",
		digest, timeoutMs,
	)
	return a.signedDo("GET", path, nil, 0, nil)
}

func (a *agent) currentDirectivesDigest() string {
	a.e.t.Helper()
	d, r := a.heartbeat(nil)
	if r.Status != http.StatusOK {
		a.e.t.Fatalf("heartbeat: %d %s", r.Status, r.Raw)
	}
	if d.DirectivesDigest == "" {
		a.e.t.Fatal("heartbeat omitted directives_digest")
	}
	return d.DirectivesDigest
}

func decodeWaitResponse(t *testing.T, r apiResp) (string, bool) {
	t.Helper()
	var response struct {
		Digest  string `json:"digest"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(r.Raw, &response); err != nil {
		t.Fatalf("decode wait response: %v: %s", err, r.Raw)
	}
	return response.Digest, response.Changed
}

func TestAgentHeartbeatDirectiveDigestStable(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("node-a")

	first := a.currentDirectivesDigest()
	second := a.currentDirectivesDigest()
	if first != second {
		t.Fatalf("unchanged heartbeat digests differ: %q != %q", first, second)
	}
}

func TestAgentDirectivesWaitTimeoutAndWrongDigest(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("node-a")
	digest := a.currentDirectivesDigest()

	started := time.Now()
	r := a.waitForDirectives(digest, 1000)
	elapsed := time.Since(started)
	if r.Status != http.StatusOK {
		t.Fatalf("wait timeout: %d %s", r.Status, r.Raw)
	}
	got, changed := decodeWaitResponse(t, r)
	if changed || got != digest {
		t.Fatalf("timeout response = (%q, %t), want (%q, false)", got, changed, digest)
	}
	if elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("1s wait took %s", elapsed)
	}

	started = time.Now()
	r = a.waitForDirectives("wrong-digest", 5000)
	elapsed = time.Since(started)
	if r.Status != http.StatusOK {
		t.Fatalf("wrong digest: %d %s", r.Status, r.Raw)
	}
	got, changed = decodeWaitResponse(t, r)
	if !changed || got != digest {
		t.Fatalf("wrong digest response = (%q, %t), want (%q, true)", got, changed, digest)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("wrong digest was not returned immediately: %s", elapsed)
	}
}

func TestAgentDirectivesWaitWakesOnServerStop(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("node-a")
	b := e.newAgent("node-b")
	serverID := e.createServer("testgame", "wait-stop", nil)
	host, _, _ := e.startToRunning(serverID, a, b)
	digest := host.currentDirectivesDigest()

	type waitResult struct {
		response apiResp
		elapsed  time.Duration
	}
	result := make(chan waitResult, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		startedAt := time.Now()
		result <- waitResult{
			response: host.waitForDirectives(digest, 20_000),
			elapsed:  time.Since(startedAt),
		}
	}()
	<-started
	select {
	case early := <-result:
		t.Fatalf("wait returned before stop request: %d %s", early.response.Status, early.response.Raw)
	case <-time.After(50 * time.Millisecond):
	}

	stopDone := make(chan apiResp, 1)
	go func() {
		stopDone <- e.do("POST", "/v1/servers/"+serverID+"/stop", nil, e.token)
	}()
	if r := <-stopDone; r.Status != http.StatusOK {
		t.Fatalf("stop request: %d %s", r.Status, r.Raw)
	}
	e.reconcile()

	select {
	case r := <-result:
		if r.response.Status != http.StatusOK {
			t.Fatalf("wait after stop: %d %s", r.response.Status, r.response.Raw)
		}
		newDigest, changed := decodeWaitResponse(t, r.response)
		if !changed || newDigest == digest {
			t.Fatalf("wait response = (%q, %t), old digest %q", newDigest, changed, digest)
		}
		if r.elapsed >= 750*time.Millisecond {
			t.Fatalf("directive wait wake took %s", r.elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("directive wait did not wake within 1 second")
	}
}

func TestAgentDirectivesWaitRequiresSignature(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("node-a")
	digest := a.currentDirectivesDigest()
	path := fmt.Sprintf("/v1/agent/directives/wait?digest=%s&timeout_ms=1000", digest)
	if r := e.do("GET", path, nil, ""); r.Status != http.StatusUnauthorized {
		t.Fatalf("unsigned wait status = %d, want 401", r.Status)
	}
}
