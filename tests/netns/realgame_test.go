//go:build netns && (minecraft || valheim)

package netns

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var realGameNodeNames = []string{"arne", "kari", "nas", "player"}

type gameWatchdog struct {
	mu           sync.Mutex
	processMatch string
	violations   []string
	last         string
	stop         chan struct{}
	done         chan struct{}
}

func processHosts(names []string, processMatch string) map[string]bool {
	out := map[string]bool{}
	for _, ns := range names {
		b, err := exec.Command(toolPath()["ip"], "netns", "pids", ns).Output()
		if err != nil {
			continue
		}
		for _, pid := range strings.Fields(string(b)) {
			cmd, err := os.ReadFile("/proc/" + pid + "/cmdline")
			if err == nil && strings.Contains(string(cmd), processMatch) {
				out[ns] = true
				break
			}
		}
	}
	return out
}

func startWatchdog(t *testing.T, names []string, processMatch string) *gameWatchdog {
	t.Helper()
	w := &gameWatchdog{
		processMatch: processMatch,
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	go func() {
		defer close(w.done)
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-tick.C:
				hosts := processHosts(names, w.processMatch)
				w.mu.Lock()
				if len(hosts) > 1 {
					var ns []string
					for h := range hosts {
						ns = append(ns, h)
					}
					w.violations = append(w.violations,
						fmt.Sprintf("%s: %s in %v", time.Now().Format("15:04:05.000"), w.processMatch, ns))
				}
				cur := ""
				if len(hosts) == 1 {
					for h := range hosts {
						cur = h
					}
				}
				if cur != w.last {
					t.Logf("[%s] watchdog: %s host %q -> %q",
						time.Now().Format("15:04:05.000"), w.processMatch, w.last, cur)
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

func (w *gameWatchdog) check(t *testing.T) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.violations) > 0 {
		t.Fatalf("watchdog: %s ran in multiple namespaces:\n%s",
			w.processMatch, strings.Join(w.violations, "\n"))
	}
}

type gameEnv struct {
	t       *testing.T
	tmp     string
	cpURL   string
	tok     string
	groupID string
	token   string
	nodes   map[string]*node
	cp      *proc
	cpArgs  []string
	cpLogs  []*proc

	serverIDs []string
}

type cpOpts struct {
	LeaseTTLms  int
	HeartbeatMs int
	SuspectMs   int
	OfflineMs   int
}

func (o cpOpts) withDefaults() cpOpts {
	if o.LeaseTTLms == 0 {
		o.LeaseTTLms = 4000
	}
	if o.HeartbeatMs == 0 {
		o.HeartbeatMs = 500
	}
	if o.SuspectMs == 0 {
		o.SuspectMs = 1500
	}
	if o.OfflineMs == 0 {
		o.OfflineMs = 6000
	}
	return o
}

func newGameEnv(t *testing.T, names []string) *gameEnv {
	return newGameEnvOpts(t, names, cpOpts{})
}

func newGameEnvOpts(t *testing.T, names []string, options cpOpts) *gameEnv {
	t.Helper()
	options = options.withDefaults()
	tmp := t.TempDir()
	setupTopology(t, names)
	cpPort := 18080
	cpURL := fmt.Sprintf("http://%s:%d", wanIP, cpPort)
	e := &gameEnv{t: t, tmp: tmp, cpURL: cpURL, nodes: map[string]*node{}}
	e.cpArgs = []string{
		"--listen", fmt.Sprintf("%s:%d", wanIP, cpPort),
		"--db", "sqlite://" + filepath.Join(tmp, "cp.db"),
		"--public-url", cpURL,
		"--signup", "open",
		"--heartbeat-interval-ms", fmt.Sprint(options.HeartbeatMs),
		"--lease-ttl-ms", fmt.Sprint(options.LeaseTTLms),
		"--suspect-after-ms", fmt.Sprint(options.SuspectMs),
		"--offline-after-ms", fmt.Sprint(options.OfflineMs),
		"--embedded-relay", fmt.Sprintf(":%d", relayUDP),
		"--embedded-relay-addr", fmt.Sprintf("%s:%d", wanIP, relayUDP),
		"--log-level", "warn",
	}
	e.startCP()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for i, p := range e.cpLogs {
			t.Logf("--- cp log %d ---\n%s", i+1, p.buf.String())
		}
	})
	e.waitCPUp(15 * time.Second)
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

func (e *gameEnv) startCP() {
	e.cp = spawnLogged(e.t, bin("varde-control-plane"), e.cpArgs...)
	e.cpLogs = append(e.cpLogs, e.cp)
}

func (e *gameEnv) waitCPUp(d time.Duration) {
	waitFor(e.t, d, "control plane up", func() bool {
		st, _ := apiCall("GET", e.cpURL+"/healthz", "", nil)
		return st == 200
	})
}

func (e *gameEnv) restartCP() {
	e.t.Helper()
	old := e.cp
	if old == nil || old.cmd.Process == nil {
		e.t.Fatal("control plane process is not running")
	}
	if err := old.cmd.Process.Kill(); err != nil {
		e.t.Fatalf("kill control plane: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- old.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		e.t.Fatal("timed out waiting for control plane to exit")
	}
	e.startCP()
	e.waitCPUp(15 * time.Second)
}

func (e *gameEnv) enroll(name string, anchor bool) *node {
	return e.enrollWithMargin(name, anchor, 1000)
}

func (e *gameEnv) enrollWithMargin(name string, anchor bool, fenceMarginMs int) *node {
	t := e.t
	dir := filepath.Join(e.tmp, "node-"+name)
	args := []string{"enroll", "--server", e.cpURL, "--token", e.token,
		"--data-dir", dir, "--fence-margin-ms", fmt.Sprint(fenceMarginMs)}
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

func (e *gameEnv) cutOff(ns string) {
	t := e.t
	iptables := toolPath()["iptables"]
	nsExec(t, ns, iptables, "-I", "INPUT", "1", "!", "-i", "lo", "-j", "DROP")
	nsExec(t, ns, iptables, "-I", "OUTPUT", "1", "!", "-o", "lo", "-j", "DROP")
}

func (e *gameEnv) restore(ns string) {
	t := e.t
	iptables := toolPath()["iptables"]
	nsExec(t, ns, iptables, "-D", "INPUT", "!", "-i", "lo", "-j", "DROP")
	nsExec(t, ns, iptables, "-D", "OUTPUT", "!", "-o", "lo", "-j", "DROP")
}

func (e *gameEnv) nodeConnections(name string) map[string]string {
	e.t.Helper()
	status, body := apiCall("GET", e.cpURL+"/v1/nodes/"+e.nodes[name].nodeID, e.tok, nil)
	if status != 200 {
		e.t.Fatalf("get node %s: %d %s", name, status, body)
	}
	var node map[string]any
	if err := json.Unmarshal(body, &node); err != nil {
		e.t.Fatalf("decode node %s: %v", name, err)
	}
	connections, _ := node["connections"].([]any)
	out := make(map[string]string, len(connections))
	for _, value := range connections {
		connection, ok := value.(map[string]any)
		if !ok {
			continue
		}
		peer, _ := connection["name"].(string)
		path, _ := connection["path"].(string)
		if peer != "" && path != "" {
			out[peer] = path
		}
	}
	return out
}

func (e *gameEnv) startAgent(name string) {
	t := e.t
	nd := e.nodes[name]
	nd.proc = spawnInEnv(t, name,
		[]string{"VARDE_MESH_LOG_LEVEL=debug"},
		rustBin("varde-agent"), "run",
		"--data-dir", nd.dataDir, "--mesh-bin", bin("varde-mesh"))
}

func (e *gameEnv) setHosting(name string, enabled bool) {
	apiJSON(e.t, "PATCH", e.cpURL+"/v1/nodes/"+e.nodes[name].nodeID, e.tok,
		map[string]any{"hosting_enabled": enabled}, 200)
}

func (e *gameEnv) nodeName(id string) string {
	for _, nd := range e.nodes {
		if nd.nodeID == id {
			return nd.name
		}
	}
	return id
}

func (e *gameEnv) waitOnline(d time.Duration, names ...string) {
	t := e.t
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
}

func (e *gameEnv) dumpServerState() {
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

func (e *gameEnv) hostOf(serverID string) string {
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

func (e *gameEnv) placedNodeOf(serverID string) string {
	st, b := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/executions", e.tok, nil)
	if st != 200 {
		return ""
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, ex := range m["executions"].([]any) {
		em := ex.(map[string]any)
		if em["ended_at"] == nil && em["state"] != "stopped" && em["state"] != "failed" {
			return em["node_id"].(string)
		}
	}
	return ""
}

func (e *gameEnv) svcAddr(serverID string) string {
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

func (e *gameEnv) waitCommitted(serverID, excludeID string) {
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
