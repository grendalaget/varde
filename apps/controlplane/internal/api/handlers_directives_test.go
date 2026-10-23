package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/grendalaget/varde/apps/controlplane/internal/store"
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

func TestAgentHeartbeatDigestMatchesAfterSnapshotRequestClaim(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("node-a")
	b := e.newAgent("node-b")
	serverID := e.createServer("testgame", "snapshot-digest", nil)
	host, _, _ := e.startToRunning(serverID, a, b)
	if err := e.st.CreateSnapshotRequest(context.Background(), &store.SnapshotRequest{
		ID: "snr_digest_test", ServerID: serverID, Reason: "manual", CreatedAt: e.st.NowMs(),
	}); err != nil {
		t.Fatal(err)
	}

	directives, heartbeat := host.heartbeat(nil)
	if heartbeat.Status != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", heartbeat.Status, heartbeat.Raw)
	}
	if !strings.Contains(string(heartbeat.Raw), "snr_digest_test") {
		t.Fatal("heartbeat did not deliver the pending snapshot request")
	}
	var claimed int
	if err := e.st.DB.Get(&claimed, e.st.Rebind(
		`SELECT claimed FROM snapshot_requests WHERE id=?`), "snr_digest_test"); err != nil {
		t.Fatal(err)
	}
	if claimed != 1 {
		t.Fatalf("heartbeat left snapshot request claimed=%d, want 1", claimed)
	}

	response := host.waitForDirectives(directives.DirectivesDigest, 1000)
	if response.Status != http.StatusOK {
		t.Fatalf("wait: %d %s", response.Status, response.Raw)
	}
	got, changed := decodeWaitResponse(t, response)
	if changed || got != directives.DirectivesDigest {
		t.Fatalf("wait response = (%q, %t), want (%q, false)", got, changed, directives.DirectivesDigest)
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

func TestAgentDirectivesWaitWakesOnReconcilerChange(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("node-a")
	b := e.newAgent("node-b")
	serverID := e.createServer("testgame", "wait-reconcile", nil)
	host, execID, _ := e.startToRunning(serverID, a, b)
	e.reconcile()
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
	time.Sleep(50 * time.Millisecond)

	if _, err := e.st.DB.Exec(
		`UPDATE server_executions SET lease_expires_at=? WHERE id=?`,
		e.clk.ms-1, execID,
	); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	e.reconcile()

	select {
	case r := <-result:
		if r.response.Status != http.StatusOK {
			t.Fatalf("wait after reconcile: %d %s", r.response.Status, r.response.Raw)
		}
		newDigest, changed := decodeWaitResponse(t, r.response)
		if !changed || newDigest == digest {
			t.Fatalf("wait response = (%q, %t), old digest %q", newDigest, changed, digest)
		}
		if r.elapsed >= 1500*time.Millisecond {
			t.Fatalf("reconciler directive wake took %s", r.elapsed)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("reconciler directive change did not wake the wait")
	}
}

func TestOtherGroupHeartbeatDoesNotWakeDirectiveWait(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("node-a")
	groupB := e.createGroup("g2")
	b := e.newAgentInGroup("node-b", groupB)
	digest := a.currentDirectivesDigest()

	result := make(chan apiResp, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		result <- a.waitForDirectives(digest, 1000)
	}()
	<-started
	time.Sleep(50 * time.Millisecond)

	if _, r := b.heartbeat(nil); r.Status != http.StatusOK {
		t.Fatalf("other-group heartbeat: %d %s", r.Status, r.Raw)
	}
	select {
	case r := <-result:
		if r.Status != http.StatusOK {
			t.Fatalf("wait after other-group heartbeat: %d %s", r.Status, r.Raw)
		}
		got, changed := decodeWaitResponse(t, r)
		if changed || got != digest {
			t.Fatalf("wait response = (%q, %t), want (%q, false)", got, changed, digest)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("other-group heartbeat did not leave the wait to time out")
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
