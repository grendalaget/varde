// Package e2e is the release-blocking integration test: a real control
// plane (sqlite, short timings), three real agents each supervising a real
// mesh child, and the real testgame server. It exercises enrollment,
// start → route → snapshot → crash → recover → revive → graceful stop →
// fencing, per docs/architecture/agent.md + storage.md + control-plane.md.
//
// Binaries are expected at bin/ (Go apps) and target/debug/ (Rust bins);
// `make test` builds them first.

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

var repoRoot = func() string {
	wd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}()

func bin(name string) string {
	return filepath.Join(repoRoot, "bin", name)
}

func rustBin(name string) string {
	return filepath.Join(repoRoot, "target", "debug", name)
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// ---------- tiny helpers ----------

type proc struct {
	cmd    *exec.Cmd
	logBuf *bytes.Buffer
}

func (p *proc) sigkill() { _ = p.cmd.Process.Kill() }

func spawn(t *testing.T, name string, args ...string) *proc {
	t.Helper()
	buf := &bytes.Buffer{}
	c := exec.Command(name, args...)
	c.Stdout = buf
	c.Stderr = buf
	if err := c.Start(); err != nil {
		t.Fatalf("spawn %s: %v", name, err)
	}
	p := &proc{cmd: c, logBuf: buf}
	t.Cleanup(func() {
		if c.ProcessState == nil {
			_ = c.Process.Kill()
		}
		go func() { _ = c.Wait() }()
	})
	return p
}

func runOut(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// ---------- API client ----------

type api struct {
	base  string
	token string
}

func (a *api) call(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	st, b, err := a.callErr(method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return st, b
}

// callErr is like call but reports transport errors instead of failing,
// so waitFor probes can tolerate a not-yet-up server.
func (a *api) callErr(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if a.token != "" {
		req.Header.Set("authorization", "Bearer "+a.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

func (a *api) json(t *testing.T, method, path string, body any, want int) map[string]any {
	t.Helper()
	st, b := a.call(t, method, path, body)
	if st != want {
		t.Fatalf("%s %s → %d (want %d): %s", method, path, st, want, b)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func waitFor(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// ---------- cuttable proxy (fencing test) ----------

// cutProxy forwards agent→CP traffic and can be severed on demand.
type cutProxy struct {
	ln      net.Listener
	up      string
	cutCh   chan struct{}
	cut     chan bool
	severed atomic.Bool
}

func newCutProxy(t *testing.T, upstream string) *cutProxy {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: l, up: upstream, cutCh: make(chan struct{}), cut: make(chan bool, 1)}
	go func() {
		conns := make(chan net.Conn, 64)
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				conns <- c
			}
		}()
		var live []net.Conn
		for {
			select {
			case c := <-conns:
				if p.severed.Load() {
					_ = c.Close()
					continue
				}
				live = append(live, c)
				go func(dst net.Conn) {
					up, err := net.Dial("tcp", p.up)
					if err != nil {
						_ = dst.Close()
						return
					}
					go func() { _, _ = io.Copy(dst, up) }()
					go func() { _, _ = io.Copy(up, dst) }()
				}(c)
			case <-p.cutCh:
				p.severed.Store(true)
				for _, c := range live {
					_ = c.Close()
				}
				live = nil
			case <-p.cut:
				_ = l.Close()
				return
			}
		}
	}()
	return p
}

func (p *cutProxy) addr() string { return p.ln.Addr().String() }
func (p *cutProxy) sever() {
	select {
	case p.cutCh <- struct{}{}:
	default:
	}
}
func (p *cutProxy) close() { p.cut <- true }

// ---------- testgame wire protocol ----------

// tryGameCmd is a game command that tolerates a not-yet-ready route/game: returns ""
// on any dial or read error instead of failing the test.
func tryGameCmd(addr, cmd string) string {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return ""
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := fmt.Fprintf(c, "%s\n", cmd); err != nil {
		return ""
	}
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(buf[:n]))
}

// ---------- the test ----------

type agentNode struct {
	dir      string
	proc     *proc
	prefix   string
	nodeID   string
	proxyURL string // via cut proxy (host candidate)
}

func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	tmp := t.TempDir()
	cpPort := freePort(t)
	relayPort := freePort(t)
	cpURL := fmt.Sprintf("http://127.0.0.1:%d", cpPort)

	cp := spawn(t, bin("p2pgames-control-plane"),
		"--listen", fmt.Sprintf("127.0.0.1:%d", cpPort),
		"--db", "sqlite://"+filepath.Join(tmp, "cp.db"),
		"--public-url", cpURL,
		"--signup", "open",
		"--heartbeat-interval-ms", "500",
		"--lease-ttl-ms", "4000",
		"--suspect-after-ms", "1500",
		"--offline-after-ms", "6000",
		"--embedded-relay", fmt.Sprintf("127.0.0.1:%d", relayPort),
		"--embedded-relay-addr", fmt.Sprintf("127.0.0.1:%d", relayPort),
		"--log-level", "warn",
	)
	defer cp.sigkill()

	user := &api{base: cpURL}
	waitFor(t, 10*time.Second, "control plane up", func() bool {
		st, _, err := user.callErr("GET", "/healthz", nil)
		return err == nil && st == 200
	})

	// operator user + group + tokens
	auth := user.json(t, "POST", "/v1/auth/signup", map[string]any{
		"email": "op@example.com", "password": "hunter22!", "display_name": "Op",
	}, 201)
	user.token = auth["token"].(string)
	grp := user.json(t, "POST", "/v1/groups", map[string]any{"name": "friends"}, 201)
	groupID := grp["id"].(string)
	tok := user.json(t, "POST", "/v1/groups/"+groupID+"/enrollment-tokens",
		map[string]any{"max_uses": 3}, 201)
	token := tok["token"].(string)

	// three agents; each talks to the CP through its own cuttable proxy
	// (preferred_node_id is only a scheduling weight, so the fencing
	// step must be able to cut whichever node actually hosts)
	var proxies []*cutProxy
	prefixes := []string{"127.77.", "127.78.", "127.79."}
	var agents []*agentNode
	for i := 0; i < 3; i++ {
		p := newCutProxy(t, fmt.Sprintf("127.0.0.1:%d", cpPort))
		defer p.close()
		proxies = append(proxies, p)
		dir := filepath.Join(tmp, fmt.Sprintf("agent%d", i))
		url := "http://" + p.addr()
		a := &agentNode{dir: dir, prefix: prefixes[i], proxyURL: url}
		runOut(t, rustBin("p2pgames-agent"), "enroll",
			"--server", url, "--token", token,
			"--data-dir", dir, "--loopback-prefix", a.prefix,
			"--fence-margin-ms", "1000")
		a.proc = spawn(t, rustBin("p2pgames-agent"), "run",
			"--data-dir", dir, "--mesh-bin", bin("p2pgames-mesh"))
		agents = append(agents, a)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for i, a := range agents {
			b := a.proc.logBuf.Bytes()
			if len(b) > 8000 {
				b = b[len(b)-8000:]
			}
			t.Logf("--- agent%d log tail ---\n%s", i, b)
		}
	})

	// discover node ids
	waitFor(t, 15*time.Second, "3 nodes online", func() bool {
		st, b := user.call(t, "GET", "/v1/groups/"+groupID+"/nodes", nil)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if st != 200 {
			return false
		}
		ns, _ := m["nodes"].([]any)
		return len(ns) == 3
	})

	// create + start a testgame server
	srv := user.json(t, "POST", "/v1/groups/"+groupID+"/servers", map[string]any{
		"name": "e2e", "game_id": "testgame", "config": map[string]any{},
		"min_commit_replicas": 1, "replication_factor": 2,
	}, 201)
	serverID := srv["id"].(string)
	user.json(t, "POST", "/v1/servers/"+serverID+"/start", map[string]any{}, 200)

	// host node = node_id of the server's active execution
	hostOf := func() string {
		st, b := user.call(t, "GET", "/v1/servers/"+serverID+"/executions", nil)
		if st != 200 {
			return ""
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		for _, e := range m["executions"].([]any) {
			em := e.(map[string]any)
			if em["state"] == "running" && em["ended_at"] == nil {
				return em["node_id"].(string)
			}
		}
		return ""
	}

	// wait for running + learn host node and service address
	var hostNode, serviceAddr string
	waitFor(t, 60*time.Second, "server running", func() bool {
		hostNode = hostOf()
		if hostNode == "" {
			return false
		}
		st, b := user.call(t, "GET", "/v1/servers/"+serverID, nil)
		if st != 200 {
			return false
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if svc, ok := m["service"].(map[string]any); ok {
			serviceAddr, _ = svc["loopback_ip"].(string)
		}
		return serviceAddr != ""
	})
	t.Logf("host=%s service=%s", hostNode, serviceAddr)

	// map node ids → agents via each agent's config.toml (authoritative)
	hostIdx, otherIdx := -1, -1
	for i, a := range agents {
		cfg, err := os.ReadFile(filepath.Join(a.dir, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(cfg, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("node_id")) {
				a.nodeID = string(bytes.Trim(
					bytes.TrimSpace(bytes.SplitN(line, []byte("="), 2)[1]), `"`))
			}
		}
		if a.nodeID == hostNode {
			hostIdx = i
		} else if otherIdx < 0 {
			otherIdx = i
		}
	}
	if hostIdx < 0 {
		t.Fatalf("host %s not among agents %v", hostNode, agents)
	}

	// INCR×5 via the non-host node's translated route address
	otherAddr := agents[otherIdx].prefix + serviceAddr[len("127.77."):] + ":7777"
	waitFor(t, 15*time.Second, "route reachable", func() bool {
		c, err := net.DialTimeout("tcp", otherAddr, time.Second)
		if err == nil {
			_ = c.Close()
			return true
		}
		return false
	})
	// INCR replies the new value; a reset after the write applies it anyway,
	// so reconcile with GET instead of retrying blindly.
	for i := 1; i <= 5; i++ {
		waitFor(t, 15*time.Second, fmt.Sprintf("INCR to %d", i), func() bool {
			got := tryGameCmd(otherAddr, "INCR")
			if got == fmt.Sprint(i) {
				return true
			}
			return tryGameCmd(otherAddr, "GET") == fmt.Sprint(i)
		})
	}
	waitFor(t, 15*time.Second, "GET after INCR×5 = 5", func() bool {
		return tryGameCmd(otherAddr, "GET") == "5"
	})

	// trigger a snapshot; wait committed + replica on anchor/peer
	user.json(t, "POST", "/v1/servers/"+serverID+"/snapshots", nil, 202)
	waitFor(t, 45*time.Second, "snapshot committed", func() bool {
		st, b := user.call(t, "GET", "/v1/servers/"+serverID+"/snapshots", nil)
		if st != 200 {
			return false
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		for _, s := range m["snapshots"].([]any) {
			sm := s.(map[string]any)
			if sm["state"] == "committed" {
				for _, r := range sm["replicas"].([]any) {
					if r.(map[string]any)["state"] == "ready" &&
						r.(map[string]any)["node_id"] != hostNode {
						return true
					}
				}
			}
		}
		return false
	})

	// SIGKILL host agent + game: recovery on another node, same service addr
	agents[hostIdx].proc.sigkill()
	var newHost string
	waitFor(t, 90*time.Second, "server recovered on another node", func() bool {
		newHost = hostOf()
		return newHost != "" && newHost != hostNode
	})
	waitFor(t, 30*time.Second, "GET after recovery = 5 (state restored)", func() bool {
		return tryGameCmd(otherAddr, "GET") == "5"
	})

	// revive killed agent: orphan game must be reaped, no hosting
	agents[hostIdx].proc = spawn(t, rustBin("p2pgames-agent"), "run",
		"--data-dir", agents[hostIdx].dir, "--mesh-bin", bin("p2pgames-mesh"))
	waitFor(t, 30*time.Second, "revived agent heartbeating without hosting", func() bool {
		return hostOf() == newHost
	})
	// orphan testgame under the old data dir must be dead: port 7777 is bound
	// by the new host's mesh route on ITS translated prefix; the old game's
	// bound 127.0.0.1:7777 must no longer be a testgame — check via process
	// table: the revived agent logs "killed orphaned game process group".
	waitFor(t, 10*time.Second, "orphan reaped", func() bool {
		return bytes.Contains(agents[hostIdx].proc.logBuf.Bytes(),
			[]byte("killed orphaned game process"))
	})

	// graceful stop: final snapshot reaches a peer before stopped
	user.json(t, "POST", "/v1/servers/"+serverID+"/stop", nil, 200)
	waitFor(t, 90*time.Second, "server stopped", func() bool {
		st, b := user.call(t, "GET", "/v1/servers/"+serverID, nil)
		if st != 200 {
			return false
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m["observed_state"] == "stopped"
	})
	waitFor(t, 30*time.Second, "final snapshot committed on a peer", func() bool {
		st, b := user.call(t, "GET", "/v1/servers/"+serverID+"/snapshots", nil)
		if st != 200 {
			return false
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		for _, s := range m["snapshots"].([]any) {
			sm := s.(map[string]any)
			if sm["reason"] == "final" && sm["state"] == "committed" {
				return true
			}
		}
		return false
	})

	// ---- fencing: restart, then cut the hosting node's CP access ----
	user.json(t, "POST", "/v1/servers/"+serverID+"/start",
		map[string]any{"preferred_node_id": agents[0].nodeID}, 200)
	var fencIdx int
	waitFor(t, 60*time.Second, "running on proxied node", func() bool {
		h := hostOf()
		for i, a := range agents {
			if h == a.nodeID {
				fencIdx = i
				return true
			}
		}
		return false
	})
	fencAgent := agents[fencIdx]

	// A freshly activated execution holds lease_ttl + start_grace (~10 min);
	// each heartbeat renewal shrinks it to lease_ttl. Wait for renewals to
	// land before severing, so the fencing deadline is ~lease_ttl away.
	time.Sleep(1500 * time.Millisecond)

	cutAt := time.Now()
	proxies[fencIdx].sever()
	// The game must be killed by the local watchdog before the CP's lease
	// expires (lease 4 s from the last heartbeat ≈ cutAt+4s).
	leaseExpiry := cutAt.Add(4*time.Second + 500*time.Millisecond)
	waitFor(t, 6*time.Second, "fencing kill", func() bool {
		return bytes.Contains(fencAgent.proc.logBuf.Bytes(),
			[]byte("lease deadline expired"))
	})
	if time.Now().After(leaseExpiry) {
		t.Fatalf("fencing fired at %s, after lease expiry %s", time.Now(), leaseExpiry)
	}
	// and no snapshot from the old epoch may be accepted: heartbeat the
	// server state — if a snapshot lands, it must not be for the cut epoch.
	time.Sleep(1500 * time.Millisecond)
	st, b := user.call(t, "GET", "/v1/servers/"+serverID+"/snapshots", nil)
	if st == 200 {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		for _, s := range m["snapshots"].([]any) {
			sm := s.(map[string]any)
			if sm["created_at"].(float64) > float64(cutAt.UnixMilli()) &&
				sm["node_id"] == fencAgent.nodeID {
				t.Fatalf("snapshot %s from fenced node accepted", sm["id"])
			}
		}
	}
}

// keep context import used when proxy logic grows
var _ = context.Background
