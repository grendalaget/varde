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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// ---------- server.jar host watchdog ----------

type jarWatchdog struct {
	mu         sync.Mutex
	violations []string
	last       string
	stop       chan struct{}
	done       chan struct{}
}

// jarHosts returns the set of namespaces currently running a server.jar
// process (checks each pid's /proc/<pid>/cmdline).
func jarHosts(names []string) map[string]bool {
	out := map[string]bool{}
	for _, ns := range names {
		b, err := exec.Command(toolPath()["ip"], "netns", "pids", ns).Output()
		if err != nil {
			continue
		}
		for _, pid := range strings.Fields(string(b)) {
			cmd, err := os.ReadFile("/proc/" + pid + "/cmdline")
			if err == nil && strings.Contains(string(cmd), "server.jar") {
				out[ns] = true
				break
			}
		}
	}
	return out
}

func startWatchdog(t *testing.T, names []string) *jarWatchdog {
	t.Helper()
	w := &jarWatchdog{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-tick.C:
				hosts := jarHosts(names)
				w.mu.Lock()
				if len(hosts) > 1 {
					var ns []string
					for h := range hosts {
						ns = append(ns, h)
					}
					w.violations = append(w.violations,
						fmt.Sprintf("%s: server.jar in %v", time.Now().Format("15:04:05.000"), ns))
				}
				cur := ""
				if len(hosts) == 1 {
					for h := range hosts {
						cur = h
					}
				}
				if cur != w.last {
					t.Logf("[%s] watchdog: server.jar host %q -> %q",
						time.Now().Format("15:04:05.000"), w.last, cur)
					w.last = cur
				}
				w.mu.Unlock()
			}
		}
	}()
	t.Cleanup(func() {
		close(w.stop)
		<-w.done
	})
	return w
}

func (w *jarWatchdog) check(t *testing.T) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.violations) > 0 {
		t.Fatalf("watchdog: server.jar ran in multiple namespaces:\n%s",
			strings.Join(w.violations, "\n"))
	}
}

// ---------- environment ----------

type mcEnv struct {
	t       *testing.T
	tmp     string
	cpURL   string
	tok     string
	groupID string
	token   string
	nodes   map[string]*node
	cp      *proc

	serverIDs []string
}

func newMCEnv(t *testing.T, names []string) *mcEnv {
	t.Helper()
	tmp := t.TempDir()
	setupTopology(t, names)
	cpPort := 18080
	cpURL := fmt.Sprintf("http://%s:%d", wanIP, cpPort)
	cp := spawnLogged(t, bin("varde-control-plane"),
		"--listen", fmt.Sprintf("%s:%d", wanIP, cpPort),
		"--db", "sqlite://"+filepath.Join(tmp, "cp.db"),
		"--public-url", cpURL,
		"--signup", "open",
		"--heartbeat-interval-ms", "500",
		"--lease-ttl-ms", "4000",
		"--suspect-after-ms", "1500",
		"--offline-after-ms", "6000",
		"--embedded-relay", fmt.Sprintf(":%d", relayUDP),
		"--embedded-relay-addr", fmt.Sprintf("%s:%d", wanIP, relayUDP),
		"--log-level", "warn",
	)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("--- cp log ---\n%s", cp.buf.String())
		}
	})
	waitFor(t, 15*time.Second, "control plane up", func() bool {
		st, _ := apiCall("GET", cpURL+"/healthz", "", nil)
		return st == 200
	})
	e := &mcEnv{t: t, tmp: tmp, cpURL: cpURL, cp: cp, nodes: map[string]*node{}}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		e.dumpServerState()
		for _, nd := range e.nodes {
			if nd.proc == nil {
				continue
			}
			var keep []string
			for _, l := range strings.Split(nd.proc.buf.String(), "\n") {
				// drop mesh-child slog lines ({"time":...}) — they bury the
				// agent's tracing lines in relay/dial spam
				if strings.HasPrefix(l, `{"time":`) {
					continue
				}
				keep = append(keep, l)
			}
			if len(keep) > 400 {
				keep = keep[len(keep)-400:]
			}
			t.Logf("--- node %s agent log tail ---\n%s", nd.name, strings.Join(keep, "\n"))
		}
	})
	auth := apiJSON(t, "POST", cpURL+"/v1/auth/signup", "", map[string]any{
		"email": "op@example.com", "password": "hunter22!", "display_name": "Op",
	}, 201)
	e.tok = auth["token"].(string)
	grp := apiJSON(t, "POST", cpURL+"/v1/groups", e.tok, map[string]any{"name": "friends"}, 201)
	e.groupID = grp["id"].(string)
	et := apiJSON(t, "POST", cpURL+"/v1/groups/"+e.groupID+"/enrollment-tokens", e.tok,
		map[string]any{"max_uses": 8}, 201)
	e.token = et["token"].(string)
	return e
}

// enroll runs `varde-agent enroll` inside the namespace and pins the mesh
// UDP port so the NAT map applies to the first packet out.
func (e *mcEnv) enroll(name string, anchor bool) *node {
	t := e.t
	dir := filepath.Join(e.tmp, "node-"+name)
	args := []string{"enroll", "--server", e.cpURL, "--token", e.token,
		"--data-dir", dir, "--fence-margin-ms", "1000"}
	if anchor {
		args = append(args, "--anchor")
	}
	nsExec(t, name, rustBin("varde-agent"), args...)
	cfgPath := filepath.Join(dir, "config.toml")
	cfgb, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath,
		[]byte(strings.Replace(string(cfgb), "mesh_listen_port = 0",
			fmt.Sprintf("mesh_listen_port = %d", meshUDPPort), 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	nd := &node{name: name, dataDir: dir, sock: filepath.Join(dir, "run", "mesh.sock")}
	for _, line := range strings.Split(string(cfgb), "\n") {
		if strings.HasPrefix(line, "node_id") {
			nd.nodeID = strings.Trim(strings.TrimSpace(strings.SplitN(line, "=", 2)[1]), `"`)
		}
	}
	e.nodes[name] = nd
	return nd
}

func (e *mcEnv) startAgent(name string) {
	t := e.t
	nd := e.nodes[name]
	nd.proc = spawnInEnv(t, name,
		[]string{"VARDE_MESH_LOG_LEVEL=debug"},
		rustBin("varde-agent"), "run",
		"--data-dir", nd.dataDir, "--mesh-bin", bin("varde-mesh"))
}

func (e *mcEnv) setHosting(name string, enabled bool) {
	apiJSON(e.t, "PATCH", e.cpURL+"/v1/nodes/"+e.nodes[name].nodeID, e.tok,
		map[string]any{"hosting_enabled": enabled}, 200)
}

func (e *mcEnv) nodeName(id string) string {
	for _, nd := range e.nodes {
		if nd.nodeID == id {
			return nd.name
		}
	}
	return id
}

// waitOnline waits until every named node reports liveness=online.
func (e *mcEnv) waitOnline(d time.Duration, names ...string) {
	t := e.t
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	polls := 0
	waitFor(t, d, fmt.Sprintf("nodes online: %v", names), func() bool {
		st, b := apiCall("GET", e.cpURL+"/v1/groups/"+e.groupID+"/nodes", e.tok, nil)
		var m map[string]any
		if st != 200 {
			return false
		}
		_ = json.Unmarshal(b, &m)
		online := map[string]bool{}
		for _, nn := range m["nodes"].([]any) {
			nm := nn.(map[string]any)
			if nm["liveness"] == "online" {
				online[nm["id"].(string)] = true
			}
		}
		missing := []string{}
		for _, n := range names {
			if !online[e.nodes[n].nodeID] {
				missing = append(missing, n)
			}
		}
		if polls%20 == 0 && len(missing) > 0 {
			t.Logf("waiting online, missing=%v", missing)
		}
		polls++
		return len(missing) == 0
	})
	_ = want
}

func (e *mcEnv) createMCServer(name, preferred string) string {
	srv := apiJSON(e.t, "POST", e.cpURL+"/v1/groups/"+e.groupID+"/servers", e.tok, map[string]any{
		"name": name, "game_id": "minecraft", "config": mcServerConfig,
		"replication_factor": 3, "min_commit_replicas": 1,
		"preferred_node_id": e.nodes[preferred].nodeID,
	}, 201)
	id := srv["id"].(string)
	e.serverIDs = append(e.serverIDs, id)
	return id
}

// dumpServerState logs executions + node liveness for diagnosis.
func (e *mcEnv) dumpServerState() {
	for _, id := range e.serverIDs {
		st, b := apiCall("GET", e.cpURL+"/v1/servers/"+id+"/executions", e.tok, nil)
		e.t.Logf("server %s executions (%d): %s", id, st, b)
	}
	st, b := apiCall("GET", e.cpURL+"/v1/groups/"+e.groupID+"/nodes", e.tok, nil)
	if st == 200 {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		for _, nn := range m["nodes"].([]any) {
			nm := nn.(map[string]any)
			e.t.Logf("node %s: liveness=%v hosting=%v",
				e.nodeName(nm["id"].(string)), nm["liveness"], nm["hosting_enabled"])
		}
	}
}

func (e *mcEnv) hostOf(serverID string) string {
	st, b := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/executions", e.tok, nil)
	if st != 200 {
		return ""
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, ex := range m["executions"].([]any) {
		em := ex.(map[string]any)
		if em["state"] == "running" && em["ended_at"] == nil {
			return em["node_id"].(string)
		}
	}
	return ""
}

func (e *mcEnv) svcAddr(serverID string) string {
	st, b := apiCall("GET", e.cpURL+"/v1/servers/"+serverID, e.tok, nil)
	if st != 200 {
		return ""
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if svc, ok := m["service"].(map[string]any); ok {
		ip, _ := svc["loopback_ip"].(string)
		return ip
	}
	return ""
}

// waitCommitted waits for a committed snapshot with a ready replica on a
// node other than excludeID (empty string = any node).
func (e *mcEnv) waitCommitted(serverID, excludeID string) {
	t := e.t
	waitFor(t, 120*time.Second, "committed snapshot replicated off-host", func() bool {
		st, b := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/snapshots", e.tok, nil)
		if st != 200 {
			return false
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		for _, s := range m["snapshots"].([]any) {
			sm := s.(map[string]any)
			if sm["state"] != "committed" {
				continue
			}
			for _, r := range sm["replicas"].([]any) {
				rm := r.(map[string]any)
				if rm["state"] == "ready" && rm["node_id"] != excludeID {
					return true
				}
			}
		}
		return false
	})
}

// ---------- bot ----------

// bot runs bot.mjs inside a node namespace; returns stdout (one JSON
// line). Non-zero exit => error.
func (e *mcEnv) bot(ns string, args ...string) (string, error) {
	botScript := filepath.Join(repoRoot, "tests", "minecraft-bot", "bot.mjs")
	full := append([]string{nodePath(e.t), botScript}, args...)
	return nsTry(ns, "env", append([]string{"HOME=/root"}, full...)...)
}

func (e *mcEnv) botArgs(action, addr string, col int) []string {
	return []string{action,
		"--host", strings.Split(addr, ":")[0],
		"--port", strings.Split(addr, ":")[1],
		"--version", "1.21.4",
		"--user", "varde_tester",
		"--col", fmt.Sprint(col)}
}

// botWrite writes a nonce column; returns the ref ("x,y,z") it used.
func (e *mcEnv) botWrite(ns, addr string, col int, nonce, ref string) string {
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
func (e *mcEnv) botRead(ns, addr string, col int, ref string) (nonce string, blocks []string, ok bool) {
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

func randNonce(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

// killNamespace SIGKILLs every pid in the namespace.
func killNamespace(t *testing.T, ns string) {
	t.Helper()
	out, err := exec.Command(toolPath()["ip"], "netns", "pids", ns).CombinedOutput()
	if err != nil {
		t.Fatalf("netns pids %s: %v\n%s", ns, err, out)
	}
	if len(strings.Fields(string(out))) == 0 {
		t.Fatalf("no processes in ns %s to kill", ns)
	}
	for _, pid := range strings.Fields(string(out)) {
		exec.Command("kill", "-9", pid).Run()
	}
}

// waitMinecraft waits until the server runs on wantHost and a bot read
// succeeds via the stable address from the player namespace. First start
// downloads the JRE + server jar, so the deadline is generous.
func (e *mcEnv) waitMinecraft(serverID, wantHost string, d time.Duration) string {
	t := e.t
	var addr, cur string
	polls := 0
	waitFor(t, d, fmt.Sprintf("minecraft up on %s", wantHost), func() bool {
		cur = e.hostOf(serverID)
		addr = e.svcAddr(serverID)
		if addr == "" || (wantHost != "" && cur != e.nodes[wantHost].nodeID) {
			return false
		}
		_, _, ok := e.botRead("player", addr+":25565", 0, "")
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
	e := newMCEnv(t, mcNodeNames)
	wd := startWatchdog(t, mcNodeNames)

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
	e := newMCEnv(t, mcNodeNames)
	wd := startWatchdog(t, mcNodeNames)

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

	// start kari; failover should pick it up, else start manually
	e.startAgent("kari")
	e.waitOnline(30*time.Second, "kari")
	autoFailover := false
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if e.hostOf(serverID) == e.nodes["kari"].nodeID {
			autoFailover = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if autoFailover {
		t.Logf("2a: server failed over to kari automatically")
	} else {
		t.Logf("2a: no automatic failover within 60s; POST /start")
		apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/start", e.tok, map[string]any{}, 200)
	}
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
	e.nodes["kari"].proc.kill()
	time.Sleep(2 * time.Second)

	server2 := e.createMCServer("mc-2b", "arne")
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
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if e.hostOf(server2) == e.nodes["kari"].nodeID {
			auto2 = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !auto2 {
		t.Logf("2b: no automatic failover; POST /start")
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
