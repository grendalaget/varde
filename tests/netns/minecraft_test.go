//go:build netns && minecraft

// Real-game e2e: Minecraft Java on the netns topology. Scenario 1 kills
// the hosting node's whole namespace and asserts committed progress
// survives the failover; scenario 2 covers an owner SIGTERM shutdown
// (final snapshot + replication hold) and a hard power-off after a
// manual save. A watchdog goroutine samples `ip netns pids` for every
// node namespace and fails the test if server.jar ever runs in two
// namespaces at once.

package netns

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

var mcNodeNames = []string{"arne", "kari", "nas", "player"}

var mcServerConfig = map[string]any{
	"version":       "1.21.4",
	"eula_accepted": true,
	"gamemode":      "creative",
	"difficulty":    "peaceful",
	"online_mode":   false,
	"view_distance": 4,
	"seed":          "varde-e2e",
}

// ---------- node binary (bot runs under sudo; PATH may be bare) ----------

func nodePath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("VARDE_NODE"); p != "" {
		return p
	}
	if p, err := exec.LookPath("node"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	for _, p := range []string{"/usr/local/bin/node", "/usr/bin/node"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("node not found; set VARDE_NODE")
	return ""
}

func (e *gameEnv) createMCServer(name, preferred string) string {
	return e.createMCServerWithReplication(name, preferred, 3, 1)
}

func (e *gameEnv) createMCServerWithReplication(name, preferred string, replicationFactor, minCommitReplicas int) string {
	srv := apiJSON(e.t, "POST", e.cpURL+"/v1/groups/"+e.groupID+"/servers", e.tok, map[string]any{
		"name": name, "game_id": "minecraft", "config": mcServerConfig,
		"replication_factor": replicationFactor, "min_commit_replicas": minCommitReplicas,
		"preferred_node_id": e.nodes[preferred].nodeID,
	}, 201)
	id := srv["id"].(string)
	e.serverIDs = append(e.serverIDs, id)
	return id
}

func offlineMinecraftUUID(name string) string {
	h := md5.Sum([]byte("OfflinePlayer:" + name))
	h[6] = h[6]&0x0f | 0x30
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func (e *gameEnv) seedMinecraftOps(serverID string) {
	e.t.Helper()
	ops := fmt.Sprintf(`[{"uuid":"%s","name":"varde_tester","level":4,"bypassesPlayerLimit":false}]`,
		offlineMinecraftUUID("varde_tester"))
	for name, nd := range e.nodes {
		serverDir := filepath.Join(nd.dataDir, "servers", serverID)
		nsExec(e.t, name, "mkdir", "-p", serverDir)
		if err := os.WriteFile(filepath.Join(serverDir, "ops.json"), []byte(ops+"\n"), 0o644); err != nil {
			e.t.Fatalf("write Minecraft ops on %s: %v", name, err)
		}
	}
}

func TestOfflineMinecraftUUID(t *testing.T) {
	const want = "42a18005-ab18-37a2-8c21-96df97493ad3"
	if got := offlineMinecraftUUID("varde_tester"); got != want {
		t.Fatalf("offlineMinecraftUUID(varde_tester) = %s, want %s", got, want)
	}
}

// ---------- bot ----------

// bot runs bot.mjs inside a node namespace; returns stdout (one JSON
// line). Non-zero exit => error.
func (e *gameEnv) bot(ns string, args ...string) (string, error) {
	botScript := filepath.Join(repoRoot, "tests", "minecraft-bot", "bot.mjs")
	full := append([]string{nodePath(e.t), botScript}, args...)
	return nsTry(ns, "env", append([]string{"HOME=/root"}, full...)...)
}

func (e *gameEnv) botArgs(action, addr string, col int) []string {
	return []string{action,
		"--host", strings.Split(addr, ":")[0],
		"--port", strings.Split(addr, ":")[1],
		"--version", "1.21.4",
		"--user", "varde_tester",
		"--col", fmt.Sprint(col)}
}

// botWrite writes a nonce column; returns the ref ("x,y,z") it used.
func (e *gameEnv) botWrite(ns, addr string, col int, nonce, ref string) string {
	t := e.t
	args := e.botArgs("write", addr, col)
	args = append(args, "--nonce", nonce)
	if ref != "" {
		args = append(args, "--ref", ref)
	}
	out, err := e.bot(ns, args...)
	if err != nil {
		t.Fatalf("bot write col%d ns=%s: %v\n%s", col, ns, err, out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("bot write output %q: %v", out, err)
	}
	r, _ := m["ref"].(string)
	if r == "" {
		t.Fatalf("bot write returned no ref: %s", out)
	}
	return r
}

// botRead probes a column; ok=false on connect failure (server down).
// nonce is nil-able.
func (e *gameEnv) botRead(ns, addr string, col int, ref string) (nonce string, blocks []string, ok bool) {
	args := e.botArgs("read", addr, col)
	if ref != "" {
		args = append(args, "--ref", ref)
	}
	out, err := e.bot(ns, args...)
	if err != nil {
		return "", nil, false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		return "", nil, false
	}
	if m["nonce"] == nil {
		for _, b := range m["blocks"].([]any) {
			blocks = append(blocks, b.(string))
		}
		return "", blocks, true
	}
	return m["nonce"].(string), nil, true
}

type minecraftBotHold struct {
	cmd    *exec.Cmd
	events <-chan string
	done   <-chan struct{}
	stderr *bytes.Buffer
}

func (e *gameEnv) startBotHold(ns, addr string, seconds int) *minecraftBotHold {
	e.t.Helper()
	botScript := filepath.Join(repoRoot, "tests", "minecraft-bot", "bot.mjs")
	args := e.botArgs("hold", addr, 0)
	args = append(args, "--seconds", fmt.Sprint(seconds))
	full := append([]string{"netns", "exec", ns, "env", "HOME=/root", nodePath(e.t), botScript}, args...)
	cmd := exec.Command(toolPath()["ip"], full...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		e.t.Fatalf("hold bot stdout pipe: %v", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("start hold bot: %v", err)
	}
	events := make(chan string, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(events)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			events <- scanner.Text()
		}
	}()
	e.t.Cleanup(func() {
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return &minecraftBotHold{cmd: cmd, events: events, done: done, stderr: stderr}
}

func (b *minecraftBotHold) nextEvent(t *testing.T, d time.Duration) map[string]any {
	t.Helper()
	select {
	case line, ok := <-b.events:
		if !ok {
			<-b.done
			_ = b.cmd.Wait()
			t.Fatalf("hold bot exited before emitting an event: %s", b.stderr.String())
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid hold bot event %q: %v", line, err)
		}
		return event
	case <-time.After(d):
		t.Fatalf("timed out waiting for hold bot event")
		return nil
	}
}

func (b *minecraftBotHold) wait(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-b.done:
	case <-time.After(d):
		t.Fatalf("timed out waiting for hold bot exit")
	}
	if err := b.cmd.Wait(); err != nil {
		t.Fatalf("hold bot exited with error: %v\n%s", err, b.stderr.String())
	}
}

func randNonce(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

// waitMinecraft waits for a successful bot read through the stable address.
// If wantHost is non-empty, it also requires the server to run there.
func (e *gameEnv) waitMinecraft(serverID, wantHost string, d time.Duration) string {
	return e.waitMinecraftFrom(serverID, wantHost, "player", d)
}

func (e *gameEnv) waitMinecraftFrom(serverID, wantHost, clientNS string, d time.Duration) string {
	t := e.t
	var addr, cur string
	polls := 0
	what := "minecraft up"
	if wantHost != "" {
		what += " on " + wantHost
	}
	waitFor(t, d, what, func() bool {
		cur = e.hostOf(serverID)
		addr = e.svcAddr(serverID)
		if addr == "" || cur == "" || (wantHost != "" && cur != e.nodes[wantHost].nodeID) {
			return false
		}
		_, _, ok := e.botRead(clientNS, addr+":25565", 0, "")
		if polls%20 == 0 {
			t.Logf("waitMinecraft: host=%q svc=%s bot=%v", cur, addr, ok)
		}
		polls++
		return ok
	})
	return addr + ":25565"
}

// ---------- scenario 1 ----------

func TestMinecraftFailover(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (ip netns); run via `sudo make e2e-minecraft`")
	}
	e := newGameEnv(t, mcNodeNames)
	wd := startWatchdog(t, mcNodeNames, "server.jar")

	for _, n := range mcNodeNames {
		e.enroll(n, n == "nas")
	}
	for _, n := range mcNodeNames {
		e.startAgent(n)
	}
	e.setHosting("nas", false)
	e.setHosting("player", false)
	// keep kari out of placement until arne is up: the first JRE+jar
	// download exceeds the lease TTL, and if kari is eligible the
	// migration can land the exec there before arne finishes preparing
	e.setHosting("kari", false)
	e.waitOnline(30*time.Second, mcNodeNames...)

	serverID := e.createMCServer("mc-a", "arne")
	e.seedMinecraftOps(serverID)
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/start", e.tok, map[string]any{}, 200)

	// first start downloads Temurin + server.jar on arne
	svc := e.waitMinecraft(serverID, "arne", 6*time.Minute)
	t.Logf("server on arne at %s", svc)
	e.setHosting("kari", true)

	n1 := randNonce(t)
	ref := e.botWrite("player", svc, 0, n1, "")
	t.Logf("wrote col0=%s at ref=%s", n1, ref)

	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/snapshots", e.tok, nil, 202)
	e.waitCommitted(serverID, e.nodes["arne"].nodeID)
	t.Logf("snapshot committed and replicated off arne")

	n2 := randNonce(t)
	e.botWrite("player", svc, 1, n2, ref)
	t.Logf("wrote col1=%s (unsaved)", n2)

	t0 := time.Now()
	killNamespace(t, "arne")
	t.Logf("[%s] killed arne's namespace", t0.Format("15:04:05.000"))

	// recovery on kari + bot read works
	var rto time.Duration
	waitFor(t, 4*time.Minute, "server recovered on kari", func() bool {
		if e.hostOf(serverID) != e.nodes["kari"].nodeID {
			return false
		}
		_, _, ok := e.botRead("player", svc, 0, ref)
		if ok && rto == 0 {
			rto = time.Since(t0)
		}
		return ok
	})
	t.Logf("RTO (kill -> first successful read): %s", rto)

	got0, blocks0, _ := e.botRead("player", svc, 0, ref)
	if got0 != n1 {
		t.Fatalf("col0 after failover: got nonce=%q blocks=%v, want %q", got0, blocks0, n1)
	}
	got1, blocks1, _ := e.botRead("player", svc, 1, ref)
	switch {
	case got1 == n2:
		t.Logf("col1 survived failover (unsaved write made it into the snapshot)")
	case got1 == "":
		t.Logf("col1 lost (unsaved write, expected): blocks=%v", blocks1)
	default:
		t.Fatalf("col1 corrupt after failover: got %q blocks=%v, want %q or empty", got1, blocks1, n2)
	}

	// bring arne back; the server must not migrate back or double-host
	e.startAgent("arne")
	e.waitOnline(30*time.Second, "arne")
	time.Sleep(30 * time.Second)
	if h := e.hostOf(serverID); h != e.nodes["kari"].nodeID {
		t.Fatalf("server migrated after arne returned: host=%s", e.nodeName(h))
	}
	wd.check(t)
}

// ---------- scenario 2 ----------

func TestMinecraftOwnerShutdown(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (ip netns); run via `sudo make e2e-minecraft`")
	}
	e := newGameEnv(t, mcNodeNames)
	wd := startWatchdog(t, mcNodeNames, "server.jar")

	for _, n := range mcNodeNames {
		e.enroll(n, n == "nas")
	}
	for _, n := range []string{"arne", "nas", "player"} { // kari enrolled, agent stopped
		e.startAgent(n)
	}
	e.setHosting("nas", false)
	e.setHosting("player", false)
	e.waitOnline(30*time.Second, "arne", "nas", "player")

	serverID := e.createMCServer("mc-2a", "arne")
	e.seedMinecraftOps(serverID)
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/start", e.tok, map[string]any{}, 200)
	svc := e.waitMinecraft(serverID, "arne", 6*time.Minute)

	n1 := randNonce(t)
	ref := e.botWrite("player", svc, 0, n1, "")
	t.Logf("2a: wrote col0=%s ref=%s; no save-now", n1, ref)

	// SIGTERM the agent only: graceful stop -> final snapshot -> hold
	arneProc := e.nodes["arne"].proc
	if err := arneProc.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM arne agent: %v", err)
	}
	// reap the process in the background so ProcessState populates
	go func() { _ = arneProc.cmd.Wait() }()
	waitFor(t, 3*time.Minute, "arne agent exited", func() bool {
		return arneProc.cmd.ProcessState != nil && arneProc.cmd.ProcessState.Exited()
	})
	t.Logf("arne agent exited after SIGTERM")

	// the shutdown path must have committed a snapshot replicated to nas
	e.waitCommitted(serverID, "")
	st, b := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/snapshots", e.tok, nil)
	if st != 200 {
		t.Fatalf("snapshots: %d %s", st, b)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	var newest map[string]any
	var newestAt float64
	for _, s := range m["snapshots"].([]any) {
		sm := s.(map[string]any)
		if sm["state"] != "committed" {
			continue
		}
		at, _ := sm["created_at"].(float64)
		if newest == nil || at > newestAt {
			newest, newestAt = sm, at
		}
	}
	if newest == nil {
		t.Fatalf("no committed snapshot after owner shutdown")
	}
	nasReady := false
	for _, r := range newest["replicas"].([]any) {
		rm := r.(map[string]any)
		if rm["state"] == "ready" && rm["node_id"] == e.nodes["nas"].nodeID {
			nasReady = true
		}
	}
	if !nasReady {
		t.Fatalf("newest committed snapshot %v has no ready replica on nas: %s",
			newest["id"], b)
	}
	t.Logf("2a: final snapshot committed with ready replica on nas")

	// start kari: the CP must re-place automatically — the draining-flag
	// + refused-exec reports should route around arne with no /start
	e.startAgent("kari")
	e.waitOnline(30*time.Second, "kari")
	placedAt := time.Now()
	waitFor(t, 30*time.Second, "2a: server re-placed automatically", func() bool {
		return e.placedNodeOf(serverID) == e.nodes["kari"].nodeID
	})
	t.Logf("2a: server placed on kari automatically (%.1fs after kari online)", time.Since(placedAt).Seconds())
	waitFor(t, 3*time.Minute, "server running on kari", func() bool {
		if e.hostOf(serverID) != e.nodes["kari"].nodeID {
			return false
		}
		_, _, ok := e.botRead("player", svc, 0, ref)
		return ok
	})
	got0, blocks0, _ := e.botRead("player", svc, 0, ref)
	if got0 != n1 {
		t.Fatalf("2a: col0 after owner shutdown: got %q blocks=%v, want %q", got0, blocks0, n1)
	}
	t.Logf("2a: col0 survived owner shutdown")

	// 2a done: stop it before 2b (one minecraft per namespace on 25565)
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/stop", e.tok, map[string]any{}, 200)
	waitFor(t, 90*time.Second, "2a server stopped", func() bool {
		return e.hostOf(serverID) == ""
	})

	// ---------- 2b: hard power-off after a save ----------
	// "kari's agent is stopped" means the node is down — kill the whole
	// namespace: killing only the agent leaves server.jar orphaned and
	// still listening on 25565, which trips the one-host invariant
	killNamespace(t, "kari")
	time.Sleep(2 * time.Second)

	// arne's agent was SIGTERM'd in 2a — bring it back before hosting again
	e.startAgent("arne")
	e.waitOnline(30*time.Second, "arne")

	server2 := e.createMCServer("mc-2b", "arne")
	e.seedMinecraftOps(server2)
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+server2+"/start", e.tok, map[string]any{}, 200)
	svc2 := e.waitMinecraft(server2, "arne", 3*time.Minute)

	n1b := randNonce(t)
	ref2 := e.botWrite("player", svc2, 0, n1b, "")
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+server2+"/snapshots", e.tok, nil, 202)
	e.waitCommitted(server2, e.nodes["arne"].nodeID)
	t.Logf("2b: saved col0=%s, committed+replicated", n1b)

	n2b := randNonce(t)
	e.botWrite("player", svc2, 1, n2b, ref2)
	killNamespace(t, "arne")
	t.Logf("2b: wrote col1=%s then hard-killed arne", n2b)

	e.startAgent("kari")
	e.waitOnline(30*time.Second, "kari")
	auto2 := false
	deadline2 := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline2) {
		if e.placedNodeOf(server2) == e.nodes["kari"].nodeID {
			auto2 = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !auto2 {
		t.Logf("2b: REQUIRED manual /start (no auto failover within 60s)")
		apiJSON(t, "POST", e.cpURL+"/v1/servers/"+server2+"/start", e.tok, map[string]any{}, 200)
	} else {
		t.Logf("2b: server failed over to kari automatically")
	}
	waitFor(t, 3*time.Minute, "server2 running on kari", func() bool {
		if e.hostOf(server2) != e.nodes["kari"].nodeID {
			return false
		}
		_, _, ok := e.botRead("player", svc2, 0, ref2)
		return ok
	})
	gotb0, blockb0, _ := e.botRead("player", svc2, 0, ref2)
	if gotb0 != n1b {
		t.Fatalf("2b: col0 after hard kill: got %q blocks=%v, want %q", gotb0, blockb0, n1b)
	}
	gotb1, blockb1, _ := e.botRead("player", svc2, 1, ref2)
	if gotb1 != n2b && gotb1 != "" {
		t.Fatalf("2b: col1 corrupt: got %q blocks=%v, want %q or empty", gotb1, blockb1, n2b)
	}
	t.Logf("2b: col0=%s survived; col1=%s (nonce match=%v)", gotb0, gotb1, gotb1 == n2b)
	wd.check(t)
}

func TestMinecraftNoAnchor(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (ip netns); run via `sudo make e2e-minecraft`")
	}
	names := []string{"arne", "kari", "player"}
	e := newGameEnv(t, names)
	wd := startWatchdog(t, names, "server.jar")

	for _, name := range names {
		nd := e.enroll(name, false)
		apiJSON(t, "PATCH", e.cpURL+"/v1/nodes/"+nd.nodeID, e.tok,
			map[string]any{"name": name}, 200)
	}
	e.startAgent("arne")
	e.startAgent("player")
	e.setHosting("player", false)
	e.waitOnline(30*time.Second, "arne", "player")

	serverID := e.createMCServerWithReplication("mc-no-anchor", "arne", 1, 1)
	e.seedMinecraftOps(serverID)
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/start", e.tok, map[string]any{}, 200)
	svc := e.waitMinecraft(serverID, "arne", 6*time.Minute)

	nonce := randNonce(t)
	ref := e.botWrite("player", svc, 0, nonce, "")
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/snapshots", e.tok, nil, 202)
	e.waitCommitted(serverID, "")

	status, body := apiCall("GET", e.cpURL+"/v1/servers/"+serverID, e.tok, nil)
	if status != 200 {
		t.Fatalf("get server summary: %d %s", status, body)
	}
	var server map[string]any
	if err := json.Unmarshal(body, &server); err != nil {
		t.Fatalf("decode server summary: %v", err)
	}
	summary, ok := server["summary"].(map[string]any)
	if !ok {
		t.Fatalf("server response has no summary: %s", body)
	}
	latestSafeSave, ok := summary["latest_safe_save"].(map[string]any)
	if !ok {
		t.Fatalf("server response has no latest_safe_save: %s", body)
	}
	replicas, ok := latestSafeSave["replicas"].([]any)
	if !ok || len(replicas) != 1 {
		t.Fatalf("latest_safe_save should have exactly one ready replica, got %v", latestSafeSave["replicas"])
	}
	replica, ok := replicas[0].(map[string]any)
	if !ok || replica["name"] != "arne" || replica["anchor"] != false {
		t.Fatalf("latest_safe_save replica should be arne and not an anchor, got %v", replicas[0])
	}
	t.Logf("latest safe save replica: %v", replica)
	t.Log("expected post-snapshot loss: none; col0 was the only write and it reached the committed snapshot")

	killNamespace(t, "arne")
	t.Logf("[%s] killed arne's namespace", time.Now().Format("15:04:05.000"))
	e.startAgent("kari")
	e.waitOnline(30*time.Second, "kari")

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		execStatus, executionsJSON := apiCall("GET",
			e.cpURL+"/v1/servers/"+serverID+"/executions", e.tok, nil)
		if execStatus == 200 {
			var executionList map[string]any
			if err := json.Unmarshal(executionsJSON, &executionList); err == nil {
				if executions, ok := executionList["executions"].([]any); ok {
					for _, value := range executions {
						execution, ok := value.(map[string]any)
						if ok && execution["node_id"] == e.nodes["kari"].nodeID {
							t.Fatalf("Kari received an execution while the only committed save was offline: %s", executionsJSON)
						}
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	status, body = apiCall("GET", e.cpURL+"/v1/servers/"+serverID, e.tok, nil)
	if status != 200 {
		t.Fatalf("get waiting server state: %d %s", status, body)
	}
	if err := json.Unmarshal(body, &server); err != nil {
		t.Fatalf("decode waiting server state: %v", err)
	}
	if observed, _ := server["observed_state"].(string); observed != "recovering" {
		t.Fatalf("server observed state = %q, want recovering: %s", observed, body)
	}

	status, body = apiCall("GET", e.cpURL+"/v1/groups/"+e.groupID+"/events?limit=200", e.tok, nil)
	if status != 200 {
		t.Fatalf("get group events: %d %s", status, body)
	}
	var eventList map[string]any
	if err := json.Unmarshal(body, &eventList); err != nil {
		t.Fatalf("decode group events: %v", err)
	}
	foundUnavailable := false
	if events, ok := eventList["events"].([]any); ok {
		for _, value := range events {
			event, ok := value.(map[string]any)
			if !ok || event["server_id"] != serverID || event["type"] != "server.save_unavailable" {
				continue
			}
			eventData, _ := event["data"].(map[string]any)
			if eventData["snapshot_id"] == latestSafeSave["snapshot_id"] {
				foundUnavailable = true
				break
			}
		}
	}
	if !foundUnavailable {
		t.Fatalf("missing server.save_unavailable event for snapshot %v: %s", latestSafeSave["snapshot_id"], body)
	}

	e.startAgent("arne")
	e.waitOnline(30*time.Second, "arne")
	var recoveredHost string
	recovered := false
	recoveryDeadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(recoveryDeadline) {
		host := e.hostOf(serverID)
		if host == e.nodes["arne"].nodeID || host == e.nodes["kari"].nodeID {
			got, _, ok := e.botRead("player", svc, 0, ref)
			if ok && got == nonce {
				recovered, recoveredHost = true, e.nodeName(host)
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !recovered {
		_, executionsJSON := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/executions", e.tok, nil)
		t.Errorf("server did not recover col0=%s from the committed snapshot: %s", nonce, executionsJSON)
	} else {
		t.Logf("recovered committed col0 on %s: %s", recoveredHost, nonce)
	}
	wd.check(t)
}

func TestMinecraftCutOff(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (ip netns); run via `sudo make e2e-minecraft`")
	}
	e := newGameEnv(t, mcNodeNames)
	wd := startWatchdog(t, mcNodeNames, "server.jar")

	for _, name := range mcNodeNames {
		e.enroll(name, name == "nas")
	}
	for _, name := range mcNodeNames {
		e.startAgent(name)
	}
	e.setHosting("nas", false)
	e.setHosting("player", false)
	e.setHosting("kari", false)
	e.waitOnline(30*time.Second, mcNodeNames...)

	serverID := e.createMCServer("mc-cut-off", "arne")
	e.seedMinecraftOps(serverID)
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/start", e.tok, map[string]any{}, 200)
	svc := e.waitMinecraft(serverID, "arne", 6*time.Minute)
	e.setHosting("kari", true)

	nonce := randNonce(t)
	ref := e.botWrite("player", svc, 0, nonce, "")
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/snapshots", e.tok, nil, 202)
	e.waitCommitted(serverID, e.nodes["arne"].nodeID)

	status, executionsJSON := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/executions", e.tok, nil)
	if status != 200 {
		t.Fatalf("get initial executions: %d %s", status, executionsJSON)
	}
	var executionList struct {
		Executions []struct {
			ID      string `json:"id"`
			NodeID  string `json:"node_id"`
			State   string `json:"state"`
			EndedAt *int64 `json:"ended_at"`
		} `json:"executions"`
	}
	if err := json.Unmarshal(executionsJSON, &executionList); err != nil {
		t.Fatalf("decode initial executions: %v", err)
	}
	var arneExecutionID string
	for _, execution := range executionList.Executions {
		if execution.NodeID == e.nodes["arne"].nodeID && execution.State == "running" && execution.EndedAt == nil {
			arneExecutionID = execution.ID
			break
		}
	}
	if arneExecutionID == "" {
		t.Fatalf("no running execution on arne: %s", executionsJSON)
	}

	t0 := time.Now()
	e.cutOff("arne")
	t.Logf("[%s] cut off arne", t0.Format("15:04:05.000"))

	var javaExitAt, lostAt time.Time
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && (javaExitAt.IsZero() || lostAt.IsZero()) {
		if javaExitAt.IsZero() {
			if at, ok := wd.transitionTime("arne", ""); ok && !at.Before(t0) {
				javaExitAt = at
			}
		}
		if lostAt.IsZero() {
			status, body := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/executions", e.tok, nil)
			if status == 200 {
				var executions struct {
					Executions []struct {
						ID        string `json:"id"`
						State     string `json:"state"`
						EndReason string `json:"end_reason"`
					} `json:"executions"`
				}
				if json.Unmarshal(body, &executions) == nil {
					for _, execution := range executions.Executions {
						if execution.ID == arneExecutionID &&
							(execution.State == "lost" || execution.EndReason == "lost") {
							lostAt = time.Now()
							break
						}
					}
				}
			}
		}
		if javaExitAt.IsZero() || lostAt.IsZero() {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if javaExitAt.IsZero() {
		t.Errorf("watchdog did not observe arne's server.jar exit after cutoff")
	}
	if lostAt.IsZero() {
		t.Errorf("control plane did not mark arne's execution lost")
	}
	if !javaExitAt.IsZero() && !lostAt.IsZero() && !javaExitAt.Before(lostAt) {
		t.Errorf("server.jar exit was not observed before CP marked the execution lost: exit=%s lost=%s",
			javaExitAt.Format(time.RFC3339Nano), lostAt.Format(time.RFC3339Nano))
	}
	if javaExitAt.IsZero() || lostAt.IsZero() {
		_, body := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/executions", e.tok, nil)
		t.Logf("execution JSON after cutoff: %s", body)
	}
	arneLogs := e.nodes["arne"].proc.buf.String()
	if !strings.Contains(arneLogs, arneExecutionID) || !strings.Contains(arneLogs, "lease deadline expired") {
		t.Errorf("arne agent log did not show fencing for %s", arneExecutionID)
	}

	var rto time.Duration
	waitFor(t, 4*time.Minute, "server recovered on kari", func() bool {
		if e.hostOf(serverID) != e.nodes["kari"].nodeID {
			return false
		}
		got, _, ok := e.botRead("player", svc, 0, ref)
		if ok && got == nonce && rto == 0 {
			rto = time.Since(t0)
		}
		return ok && got == nonce
	})
	got, blocks, _ := e.botRead("player", svc, 0, ref)
	if got != nonce {
		t.Fatalf("col0 after cutoff recovery: got nonce=%q blocks=%v, want %q", got, blocks, nonce)
	}
	t.Logf("recovered col0 on kari: %s", nonce)

	e.restore("arne")
	e.waitOnline(30*time.Second, "arne")
	stableDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(stableDeadline) {
		if host := e.hostOf(serverID); host != e.nodes["kari"].nodeID {
			t.Fatalf("server left kari after arne rejoined: host=%s", e.nodeName(host))
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !javaExitAt.IsZero() {
		t.Logf("fence latency (cutoff to arne server.jar exit): %s", javaExitAt.Sub(t0))
	}
	t.Logf("RTO (cutoff to first successful read): %s", rto)
	wd.check(t)
}

func TestMinecraftRelayed(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (ip netns); run via `sudo make e2e-minecraft`")
	}
	e := newGameEnv(t, mcNodeNames)
	wd := startWatchdog(t, mcNodeNames, "server.jar")
	blockDirectUDP(t, mcNodeNames)

	for _, name := range mcNodeNames {
		nd := e.enroll(name, name == "nas")
		apiJSON(t, "PATCH", e.cpURL+"/v1/nodes/"+nd.nodeID, e.tok,
			map[string]any{"name": name}, 200)
	}
	for _, name := range mcNodeNames {
		e.startAgent(name)
	}
	e.setHosting("nas", false)
	e.setHosting("player", false)
	e.setHosting("kari", false)
	e.waitOnline(30*time.Second, mcNodeNames...)

	serverID := e.createMCServer("mc-relayed", "arne")
	e.seedMinecraftOps(serverID)
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/start", e.tok, map[string]any{}, 200)
	svc := e.waitMinecraft(serverID, "arne", 6*time.Minute)

	waitFor(t, 90*time.Second, "player reports a relayed path to arne", func() bool {
		return e.nodeConnections("player")["arne"] == "relayed"
	})
	waitFor(t, 30*time.Second, "arne reports a relayed path to player", func() bool {
		return e.nodeConnections("arne")["player"] == "relayed"
	})

	nonce := randNonce(t)
	ref := e.botWrite("player", svc, 0, nonce, "")
	got, blocks, _ := e.botRead("player", svc, 0, ref)
	if got != nonce {
		t.Fatalf("col0 over relayed path: got nonce=%q blocks=%v, want %q", got, blocks, nonce)
	}
	t.Logf("relayed player↔arne path preserved col0=%s", nonce)
	wd.check(t)
}

func TestMinecraftMoveAndRestart(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (ip netns); run via `sudo make e2e-minecraft`")
	}
	e := newGameEnv(t, mcNodeNames)
	wd := startWatchdog(t, mcNodeNames, "server.jar")
	for _, name := range mcNodeNames {
		e.enroll(name, name == "nas")
	}
	for _, name := range mcNodeNames {
		e.startAgent(name)
	}
	e.setHosting("nas", false)
	e.setHosting("player", false)
	e.setHosting("kari", false)
	e.waitOnline(30*time.Second, mcNodeNames...)

	serverID := e.createMCServer("mc-move-restart", "arne")
	e.seedMinecraftOps(serverID)
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/start", e.tok, map[string]any{}, 200)
	svc := e.waitMinecraft(serverID, "arne", 6*time.Minute)
	e.setHosting("kari", true)

	nonce := randNonce(t)
	ref := e.botWrite("player", svc, 0, nonce, "")
	hold := e.startBotHold("player", svc, 90)
	connected := hold.nextEvent(t, 45*time.Second)
	if connected["connected"] != true {
		t.Fatalf("hold bot did not report connected: %v", connected)
	}

	moveStarted := time.Now()
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/move", e.tok,
		map[string]any{"target_node_id": e.nodes["kari"].nodeID}, 200)
	disconnected := hold.nextEvent(t, 2*time.Minute)
	if disconnected["disconnected"] != true {
		t.Fatalf("hold bot did not report disconnected: %v", disconnected)
	}
	reason, _ := disconnected["reason"].(string)
	if strings.HasPrefix(reason, "timeout after") {
		t.Fatalf("hold bot timed out instead of disconnecting during move: %s", reason)
	}
	hold.wait(t, 10*time.Second)
	t.Logf("hold bot disconnected: %s", reason)

	svc = e.waitMinecraft(serverID, "kari", 6*time.Minute)
	got, blocks, ok := e.botRead("player", svc, 0, ref)
	if !ok || got != nonce {
		t.Fatalf("col0 after move: ok=%v nonce=%q blocks=%v, want %q", ok, got, blocks, nonce)
	}
	t.Logf("move downtime (request to readable on kari): %s", time.Since(moveStarted))

	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/stop", e.tok, map[string]any{}, 200)
	waitFor(t, 2*time.Minute, "server stopped", func() bool {
		status, body := apiCall("GET", e.cpURL+"/v1/servers/"+serverID, e.tok, nil)
		if status != 200 {
			return false
		}
		var server map[string]any
		if json.Unmarshal(body, &server) != nil {
			return false
		}
		return server["observed_state"] == "stopped" && e.hostOf(serverID) == ""
	})
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/start", e.tok, map[string]any{}, 200)
	svc = e.waitMinecraft(serverID, "", 6*time.Minute)

	got, blocks, ok = e.botRead("player", svc, 0, ref)
	if !ok || got != nonce {
		t.Fatalf("col0 after clean stop/start: ok=%v nonce=%q blocks=%v, want %q", ok, got, blocks, nonce)
	}
	nextNonce := randNonce(t)
	nextRef := e.botWrite("player", svc, 1, nextNonce, ref)
	got, blocks, ok = e.botRead("player", svc, 1, nextRef)
	if !ok || got != nextNonce {
		t.Fatalf("col1 after clean stop/start: ok=%v nonce=%q blocks=%v, want %q", ok, got, blocks, nextNonce)
	}
	wd.check(t)
}

func (e *gameEnv) waitCommittedReplicas(serverID string, names ...string) string {
	e.t.Helper()
	required := make(map[string]bool, len(names))
	for _, name := range names {
		required[e.nodes[name].nodeID] = false
	}
	var snapshotID string
	waitFor(e.t, 120*time.Second, "committed snapshot replicas on "+strings.Join(names, ", "), func() bool {
		status, body := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/snapshots", e.tok, nil)
		if status != 200 {
			return false
		}
		var response struct {
			Snapshots []struct {
				ID       string `json:"id"`
				State    string `json:"state"`
				Replicas []struct {
					NodeID string `json:"node_id"`
					State  string `json:"state"`
				} `json:"replicas"`
			} `json:"snapshots"`
		}
		if json.Unmarshal(body, &response) != nil {
			return false
		}
		for _, snapshot := range response.Snapshots {
			if snapshot.State != "committed" {
				continue
			}
			for nodeID := range required {
				required[nodeID] = false
			}
			for _, replica := range snapshot.Replicas {
				if replica.State == "ready" {
					if _, ok := required[replica.NodeID]; ok {
						required[replica.NodeID] = true
					}
				}
			}
			ready := true
			for _, found := range required {
				ready = ready && found
			}
			if ready {
				snapshotID = snapshot.ID
				return true
			}
		}
		return false
	})
	return snapshotID
}

func corruptMinecraftSnapshotChunks(t *testing.T, targetDir, sourceDir, snapshotID string) []string {
	t.Helper()
	manifestPath := filepath.Join(targetDir, "storage", "snapshots", snapshotID+".json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read Kari snapshot manifest: %v", err)
	}
	var manifest struct {
		Files []struct {
			Chunks []string `json:"chunks"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode snapshot manifest: %v", err)
	}
	chunkSet := map[string]bool{}
	for _, file := range manifest.Files {
		for _, chunk := range file.Chunks {
			chunkSet[chunk] = true
		}
	}
	chunks := make([]string, 0, len(chunkSet))
	for chunk := range chunkSet {
		chunks = append(chunks, chunk)
	}
	sort.Strings(chunks)
	if len(chunks) == 0 {
		t.Fatal("snapshot manifest has no chunks")
	}
	for _, chunk := range chunks {
		if len(chunk) < 2 {
			t.Fatalf("invalid chunk ID in manifest: %q", chunk)
		}
		targetPath := filepath.Join(targetDir, "storage", "chunks", chunk[:2], chunk)
		sourcePath := filepath.Join(sourceDir, "storage", "chunks", chunk[:2], chunk)
		target, err := os.ReadFile(targetPath)
		if err != nil {
			t.Fatalf("read Kari chunk %s: %v", chunk, err)
		}
		source, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatalf("read Nas chunk %s: %v", chunk, err)
		}
		if !bytes.Equal(target, source) {
			t.Fatalf("chunk %s differs between Kari and Nas before corruption", chunk)
		}
		if len(target) == 0 {
			t.Fatalf("chunk %s is empty", chunk)
		}
		target[len(target)/2] ^= 0xff
		if err := os.WriteFile(targetPath, target, 0o644); err != nil {
			t.Fatalf("corrupt Kari chunk %s: %v", chunk, err)
		}
	}
	return chunks
}

// KNOWN FAILING: Kari restore fails on a corrupt local chunk instead of fetching Nas's good copy.
func TestMinecraftCorruptReplica(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (ip netns); run via `sudo make e2e-minecraft`")
	}
	e := newGameEnv(t, mcNodeNames)
	wd := startWatchdog(t, mcNodeNames, "server.jar")
	for _, name := range []string{"arne", "kari", "nas"} {
		e.enroll(name, name == "nas")
	}
	for _, name := range []string{"arne", "kari", "nas"} {
		e.startAgent(name)
	}
	e.setHosting("kari", false)
	e.setHosting("nas", false)
	e.waitOnline(30*time.Second, "arne", "kari", "nas")

	serverID := e.createMCServer("mc-corrupt-replica", "arne")
	e.seedMinecraftOps(serverID)
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/start", e.tok, map[string]any{}, 200)
	svc := e.waitMinecraftFrom(serverID, "arne", "nas", 6*time.Minute)

	nonce := randNonce(t)
	ref := e.botWrite("nas", svc, 0, nonce, "")
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/snapshots", e.tok, nil, 202)
	snapshotID := e.waitCommittedReplicas(serverID, "kari", "nas")
	t.Logf("committed snapshot %s has ready replicas on Kari and Nas", snapshotID)

	chunks := corruptMinecraftSnapshotChunks(
		t, e.nodes["kari"].dataDir, e.nodes["nas"].dataDir, snapshotID)
	t.Logf("corrupted %d committed snapshot chunks on Kari; first=%s", len(chunks), chunks[0])
	e.setHosting("kari", true)
	killNamespace(t, "arne")
	t.Log("killed Arne; Nas is the only other enrolled replica source")

	var svcAfter string
	waitFor(t, 6*time.Minute, "Minecraft recovery with the committed nonce on kari", func() bool {
		if e.hostOf(serverID) != e.nodes["kari"].nodeID {
			return false
		}
		addr := e.svcAddr(serverID)
		if addr == "" {
			return false
		}
		got, blocks, ok := e.botRead("nas", addr+":25565", 0, ref)
		if !ok {
			return false
		}
		if got != nonce {
			unloaded := len(blocks) > 0
			for _, block := range blocks {
				if block != "unloaded" {
					unloaded = false
					break
				}
			}
			if unloaded {
				return false
			}
			_, executionsJSON := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/executions", e.tok, nil)
			t.Fatalf("recovery on Kari lost col0: got nonce=%q blocks=%v, want %q; execution JSON: %s",
				got, blocks, nonce, executionsJSON)
		}
		svcAfter = addr + ":25565"
		return true
	})
	t.Logf("recovered col0=%s on Kari at %s", nonce, svcAfter)

	status, executionsJSON := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/executions", e.tok, nil)
	if status != 200 {
		t.Fatalf("get executions after recovery: %d %s", status, executionsJSON)
	}
	t.Logf("execution JSON after recovery: %s", executionsJSON)
	var executionList map[string]any
	if err := json.Unmarshal(executionsJSON, &executionList); err != nil {
		t.Fatalf("decode executions after recovery: %v", err)
	}
	executions, ok := executionList["executions"].([]any)
	if !ok {
		t.Fatalf("execution JSON has no executions list: %s", executionsJSON)
	}
	restored := false
	for _, value := range executions {
		execution, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if execution["node_id"] == e.nodes["kari"].nodeID &&
			execution["state"] == "running" && execution["ended_at"] == nil {
			restored = execution["restore_snapshot_id"] == snapshotID
			break
		}
	}
	if !restored {
		t.Fatalf("Kari is not running from snapshot %s: %s", snapshotID, executionsJSON)
	}

	quarantineDir := filepath.Join(e.nodes["kari"].dataDir, "storage", "chunks", "quarantine")
	quarantined, err := os.ReadDir(quarantineDir)
	if err != nil {
		t.Fatalf("read Kari chunk quarantine: %v", err)
	}
	kariLogs := e.nodes["kari"].proc.buf.String()
	if len(quarantined) == 0 && !strings.Contains(kariLogs, "hash mismatch") {
		t.Fatalf("no evidence of corrupt chunk rejection in quarantine or Kari agent logs")
	}
	for _, chunk := range chunks {
		kariPath := filepath.Join(e.nodes["kari"].dataDir, "storage", "chunks", chunk[:2], chunk)
		nasPath := filepath.Join(e.nodes["nas"].dataDir, "storage", "chunks", chunk[:2], chunk)
		kariChunk, err := os.ReadFile(kariPath)
		if err != nil {
			t.Fatalf("read recovered Kari chunk %s: %v", chunk, err)
		}
		nasChunk, err := os.ReadFile(nasPath)
		if err != nil {
			t.Fatalf("read Nas chunk %s: %v", chunk, err)
		}
		if !bytes.Equal(kariChunk, nasChunk) {
			t.Fatalf("Kari chunk %s was not restored byte-for-byte from Nas", chunk)
		}
	}
	t.Logf("corrupt chunks were rejected and all %d snapshot chunks on Kari match Nas", len(chunks))
	wd.check(t)
}
