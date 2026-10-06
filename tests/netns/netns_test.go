//go:build netns

// Linux network-namespace demo (networking.md §64): the control plane and
// its embedded relay live on a shared "internet" bridge in the root
// namespace; every game node (a, b, c, anchor) sits in its own netns behind
// its own NAT namespace (iptables MASQUERADE). There is no connectivity
// between nodes except through their NATs, so a↔b connectivity can only
// come from the mesh's hole punching — asserted via ListPeers path kind.
// Phase 2 blocks node-to-node UDP so only relay traffic survives, then
// asserts the same flows work RELAYED.

package netns

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var repoRoot = func() string {
	wd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}()

func bin(name string) string     { return filepath.Join(repoRoot, "bin", name) }
func rustBin(name string) string { return filepath.Join(repoRoot, "target", "debug", name) }

const (
	wanNet      = "10.200.77.1/24"
	wanIP       = "10.200.77.1"
	bridge      = "vbr-varde"
	relayUDP    = 23478
	meshUDPPort = 41641
)

type proc struct {
	cmd       *exec.Cmd
	buf       *bytes.Buffer
	outputMu  sync.Mutex
	logFile   *os.File
	closeOnce sync.Once
	waitOnce  sync.Once
	waitErr   error
}

func (p *proc) Write(data []byte) (int, error) {
	p.outputMu.Lock()
	defer p.outputMu.Unlock()
	if _, err := p.buf.Write(data); err != nil {
		return 0, err
	}
	if p.logFile != nil {
		n, err := p.logFile.Write(data)
		if err != nil {
			return n, err
		}
		if n != len(data) {
			return n, io.ErrShortWrite
		}
	}
	return len(data), nil
}

func (p *proc) output() string {
	p.outputMu.Lock()
	defer p.outputMu.Unlock()
	return p.buf.String()
}

func (p *proc) closeLog() {
	p.closeOnce.Do(func() {
		if p.logFile != nil {
			_ = p.logFile.Close()
		}
	})
}

func (p *proc) wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = p.cmd.Wait()
		p.closeLog()
	})
	return p.waitErr
}

func (p *proc) kill() {
	if p.cmd.Process == nil || p.cmd.ProcessState != nil {
		p.closeLog()
		return
	}
	_ = p.cmd.Process.Kill()
	go func() { _ = p.wait() }()
}

func TestProcWritesToMemoryAndFile(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "nested", "agent.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	p := &proc{buf: &bytes.Buffer{}, logFile: logFile}
	want := "stdout and stderr"
	if n, err := p.Write([]byte(want)); err != nil || n != len(want) {
		t.Fatalf("Write() = %d, %v; want %d, nil", n, err, len(want))
	}
	p.closeLog()
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if p.output() != want || string(got) != want {
		t.Fatalf("output = %q, file = %q; want %q", p.output(), got, want)
	}
}

func spawnLogged(t *testing.T, name string, args ...string) *proc {
	t.Helper()
	p := spawnLoggedNoCleanup(t, name, args...)
	t.Cleanup(p.kill)
	return p
}

func spawnLoggedNoCleanup(t *testing.T, name string, args ...string) *proc {
	return spawnLoggedNoCleanupWithLog(t, "", name, args...)
}

func spawnLoggedNoCleanupWithLog(t *testing.T, logPath, name string, args ...string) *proc {
	t.Helper()
	c := exec.Command(name, args...)
	p := startLoggedCmd(t, c, logPath, name, args...)
	return p
}

func startLoggedCmd(t *testing.T, c *exec.Cmd, logPath, name string, args ...string) *proc {
	t.Helper()
	p := &proc{buf: &bytes.Buffer{}}
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			t.Fatalf("create process log directory for %s: %v", name, err)
		}
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("create process log %s: %v", logPath, err)
		}
		p.logFile = logFile
		t.Logf("process log: %s", logPath)
	}
	c.Stdout, c.Stderr = p, p
	if err := c.Start(); err != nil {
		p.closeLog()
		t.Fatalf("spawn %s %v: %v", name, args, err)
	}
	p.cmd = c
	return p
}

// ip/iptables live in /usr/sbin which a bare sudo PATH may drop — resolve
// once so the test works under `sudo -n env PATH=...`.
var toolPath = sync.OnceValue(func() map[string]string {
	out := map[string]string{}
	for _, tool := range []string{"ip", "iptables"} {
		for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
			if _, err := os.Stat(filepath.Join(dir, tool)); err == nil {
				out[tool] = filepath.Join(dir, tool)
				break
			}
		}
	}
	return out
})

// spawnIn runs a long-lived process inside a netns.
func spawnIn(t *testing.T, ns, name string, args ...string) *proc {
	t.Helper()
	full := append([]string{"netns", "exec", ns, name}, args...)
	return spawnLogged(t, toolPath()["ip"], full...)
}

// spawnInEnv is spawnIn with extra environment variables.
func spawnInEnv(t *testing.T, ns string, env []string, name string, args ...string) *proc {
	return spawnInEnvWithLog(t, ns, env, "", name, args...)
}

func spawnInEnvWithLog(t *testing.T, ns string, env []string, logPath, name string, args ...string) *proc {
	t.Helper()
	full := append([]string{"netns", "exec", ns, name}, args...)
	c := exec.Command(toolPath()["ip"], full...)
	c.Env = append(os.Environ(), env...)
	p := startLoggedCmd(t, c, logPath, name, args...)
	t.Cleanup(p.kill)
	return p
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// nsExec runs a command inside a netns.
func nsExec(t *testing.T, ns, name string, args ...string) string {
	t.Helper()
	full := append([]string{"netns", "exec", ns, name}, args...)
	return run(t, toolPath()["ip"], full...)
}

func nsTry(ns, name string, args ...string) (string, error) {
	full := append([]string{"netns", "exec", ns, name}, args...)
	out, err := exec.Command(toolPath()["ip"], full...).CombinedOutput()
	return string(out), err
}

func waitFor(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	if waitUntil(d, f) {
		return
	}
	t.Fatalf("timeout waiting for %s", what)
}

func waitUntil(d time.Duration, f func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// ---------- control-plane API ----------

func apiCall(method, url, token string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, nil
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func apiJSON(t *testing.T, method, url, token string, body any, want int) map[string]any {
	st, b := apiCall(method, url, token, body)
	if st != want {
		t.Fatalf("%s %s -> %d (want %d): %s", method, url, st, want, b)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// ---------- topology ----------

var nodeNames = []string{"a", "b", "c", "anchor"}

// setupTopology builds: root bridge `vbr-varde` (10.200.77.1/24, the
// "internet"), per node a NAT namespace (10.200.77.1i) that masquerades
// everything leaving it, and a node namespace on a private 192.168.i.0/24
// link. Between nodes there is no route except via their NATs.
func setupTopology(t *testing.T, names []string) {
	// idempotent: clear leftovers from a previous run first
	for _, n := range names {
		_ = exec.Command(toolPath()["ip"], "netns", "del", n).Run()
		_ = exec.Command(toolPath()["ip"], "netns", "del", "nat-"+n).Run()
	}
	_ = exec.Command(toolPath()["ip"], "link", "del", bridge).Run()
	run(t, toolPath()["ip"], "link", "add", bridge, "type", "bridge")
	run(t, toolPath()["ip"], "addr", "add", wanNet, "dev", bridge)
	run(t, toolPath()["ip"], "link", "set", bridge, "up")
	// outbound internet for the nodes (runtime downloads, DNS): the host
	// forwards bridge traffic and masquerades it on egress. docker hosts
	// set FORWARD policy DROP, so ACCEPT must be explicit.
	run(t, toolPath()["iptables"], "-I", "FORWARD", "1", "-i", bridge, "-j", "ACCEPT")
	run(t, toolPath()["iptables"], "-I", "FORWARD", "1", "-o", bridge, "-j", "ACCEPT")
	run(t, toolPath()["iptables"], "-t", "nat", "-A", "POSTROUTING",
		"-s", "10.200.77.0/24", "!", "-o", bridge, "-j", "MASQUERADE")
	run(t, "mkdir", "-p", "/etc/netns")
	// per-netns resolv.conf: ip netns exec bind-mounts /etc/netns/<ns>/X
	// onto /etc/X. Without this the namespace inherits the host's
	// systemd-resolved stub (127.0.0.53), unreachable inside the ns.
	dns := upstreamDNS()
	for i, n := range names {
		idx := i + 1
		nat := "nat-" + n
		run(t, "ip", "netns", "add", nat)
		run(t, "ip", "netns", "add", n)
		// nat <-> internet bridge
		run(t, toolPath()["ip"], "link", "add", "vnat-"+n, "type", "veth", "peer", "name", "vwan-"+n)
		run(t, toolPath()["ip"], "link", "set", "vnat-"+n, "netns", nat)
		run(t, toolPath()["ip"], "link", "set", "vwan-"+n, "master", bridge, "up")
		nsExec(t, nat, "ip", "addr", "add",
			fmt.Sprintf("10.200.77.%d/24", 10+idx), "dev", "vnat-"+n)
		nsExec(t, nat, "ip", "link", "set", "vnat-"+n, "up")
		nsExec(t, nat, "ip", "link", "set", "lo", "up")
		nsExec(t, nat, "sysctl", "-w", "net.ipv4.ip_forward=1")
		// nodes reach the real internet through the bridge (outbound
		// only): default route via the bridge's host-side address
		nsExec(t, nat, "ip", "route", "add", "default", "via", wanIP)
		nsExec(t, nat, toolPath()["iptables"], "-t", "nat", "-A", "POSTROUTING",
			"-o", "vnat-"+n, "-j", "MASQUERADE")
		run(t, "mkdir", "-p", "/etc/netns/"+n)
		if err := os.WriteFile("/etc/netns/"+n+"/resolv.conf",
			[]byte("nameserver "+dns+"\n"), 0o644); err != nil {
			t.Fatalf("resolv.conf for ns %s: %v", n, err)
		}
		// node <-> its nat
		run(t, toolPath()["ip"], "link", "add", "veth-"+n, "type", "veth", "peer", "name", "vnod-"+n)
		run(t, toolPath()["ip"], "link", "set", "veth-"+n, "netns", nat)
		run(t, toolPath()["ip"], "link", "set", "vnod-"+n, "netns", n)
		nsExec(t, nat, "ip", "addr", "add",
			fmt.Sprintf("192.168.%d.1/24", idx), "dev", "veth-"+n)
		nsExec(t, nat, "ip", "link", "set", "veth-"+n, "up")
		nsExec(t, n, "ip", "addr", "add",
			fmt.Sprintf("192.168.%d.2/24", idx), "dev", "vnod-"+n)
		nsExec(t, n, "ip", "link", "set", "vnod-"+n, "up")
		nsExec(t, n, "ip", "link", "set", "lo", "up")
		nsExec(t, n, "ip", "route", "add", "default",
			"via", fmt.Sprintf("192.168.%d.1", idx))
		// Pin the node's mesh UDP port to the same external port on its
		// NAT (SNAT --to-source): kernel MASQUERADE allocates a fresh
		// external port per destination (symmetric NAT), which no amount
		// of punching can traverse. A fixed source map gives the node a
		// stable, endpoint-independent public endpoint.
		nsExec(t, nat, toolPath()["iptables"], "-t", "nat", "-I", "POSTROUTING", "1",
			"-p", "udp", "-s", fmt.Sprintf("192.168.%d.2", idx),
			"--sport", fmt.Sprint(meshUDPPort),
			"-j", "SNAT", "--to-source",
			fmt.Sprintf("10.200.77.%d:%d", 10+idx, meshUDPPort))
		// Port-forward inbound UDP to the mesh port. A restricted-cone
		// (reply-only) map cannot be modelled with kernel NAT on a shared
		// port pair: an unsolicited inbound punch creates its own conntrack
		// entry whose tuple collides with the outbound flow's reply tuple,
		// so conntrack_confirm fails and the outbound punch is never
		// tracked — deadlock. The DNAT forward yields distinct reply
		// tuples, models an endpoint-independent NAT, and still exercises
		// the real punch path (the mesh dials the observed public
		// endpoint, not a private one).
		nsExec(t, nat, toolPath()["iptables"], "-t", "nat", "-I", "PREROUTING", "1",
			"-p", "udp", "-d", fmt.Sprintf("10.200.77.%d", 10+idx),
			"--dport", fmt.Sprint(meshUDPPort),
			"-j", "DNAT", "--to-destination",
			fmt.Sprintf("192.168.%d.2:%d", idx, meshUDPPort))
	}
	t.Cleanup(func() {
		for _, n := range names {
			_ = exec.Command(toolPath()["ip"], "netns", "del", n).Run()
			_ = exec.Command(toolPath()["ip"], "netns", "del", "nat-"+n).Run()
		}
		_ = exec.Command(toolPath()["ip"], "link", "del", bridge).Run()
		_ = exec.Command(toolPath()["iptables"], "-D", "FORWARD", "-i", bridge, "-j", "ACCEPT").Run()
		_ = exec.Command(toolPath()["iptables"], "-D", "FORWARD", "-o", bridge, "-j", "ACCEPT").Run()
		_ = exec.Command(toolPath()["iptables"], "-t", "nat", "-D", "POSTROUTING",
			"-s", "10.200.77.0/24", "!", "-o", bridge, "-j", "MASQUERADE").Run()
	})
}

// upstreamDNS returns the host's real DNS upstream (the systemd-resolved
// stub 127.0.0.53 is unreachable inside a netns).
func upstreamDNS() string {
	for _, f := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) == 2 && f[0] == "nameserver" &&
				!strings.HasPrefix(f[1], "127.") {
				return f[1]
			}
		}
	}
	return "8.8.8.8"
}

// blockDirectUDP drops forwarded UDP except traffic to the embedded relay
// and DNS queries, in every NAT namespace. It also removes the inbound DNAT
// port-forward: otherwise inbound punches still reach the node and the
// half-open state (inbound delivered, outbound dropped) wedges the path
// instead of falling back to relay.
func blockDirectUDP(t *testing.T, names []string) {
	for i, n := range names {
		idx := i + 1
		nat := "nat-" + n
		nsExec(t, nat, toolPath()["iptables"], "-t", "nat", "-D", "PREROUTING",
			"-p", "udp", "-d", fmt.Sprintf("10.200.77.%d", 10+idx),
			"--dport", fmt.Sprint(meshUDPPort),
			"-j", "DNAT", "--to-destination",
			fmt.Sprintf("192.168.%d.2:%d", idx, meshUDPPort))
		// replies to permitted outbound flows (i.e. from the relay) must
		// pass or the relay path dies along with direct
		nsExec(t, nat, toolPath()["iptables"], "-I", "FORWARD", "1",
			"-m", "conntrack", "--ctstate", "ESTABLISHED", "-j", "ACCEPT")
		nsExec(t, nat, toolPath()["iptables"], "-I", "FORWARD", "2",
			"-p", "udp", "-d", wanIP, "--dport", fmt.Sprint(relayUDP), "-j", "ACCEPT")
		nsExec(t, nat, toolPath()["iptables"], "-I", "FORWARD", "3",
			"-p", "udp", "-d", upstreamDNS(), "--dport", "53", "-j", "ACCEPT")
		nsExec(t, nat, toolPath()["iptables"], "-A", "FORWARD", "-p", "udp", "-j", "DROP")
		// flush conntrack: existing ESTABLISHED entries bypass both the
		// NAT-rule and filter drops, so old direct flows would keep working
		nsTry(nat, "conntrack", "-F")
	}
}

// ---------- mesh inspection ----------

func meshClient(t *testing.T, sock string) meshv1.MeshServiceClient {
	t.Helper()
	cc, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("mesh ipc %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return meshv1.NewMeshServiceClient(cc)
}

func peerPath(t *testing.T, sock, peerID string) meshv1.PathKind {
	t.Helper()
	resp, err := meshClient(t, sock).ListPeers(t.Context(), &meshv1.ListPeersRequest{})
	if err != nil {
		return meshv1.PathKind_PATH_KIND_NONE
	}
	for _, p := range resp.Peers {
		if p.NodeId == peerID {
			return p.Path
		}
	}
	return meshv1.PathKind_PATH_KIND_NONE
}

func observedAddrs(t *testing.T, sock string) []string {
	t.Helper()
	resp, err := meshClient(t, sock).GetStatus(t.Context(), &meshv1.GetStatusRequest{})
	if err != nil {
		return nil
	}
	return resp.ListenAddrs
}

func observedEndpoints(t *testing.T, sock string) []string {
	t.Helper()
	resp, err := meshClient(t, sock).GetStatus(t.Context(), &meshv1.GetStatusRequest{})
	if err != nil {
		return nil
	}
	return resp.ObservedEndpoints
}

// ---------- game client (runs inside a node netns via bash /dev/tcp) ----------

func gameCmd(t *testing.T, ns, addr, cmd string) string {
	t.Helper()
	host, port, _ := strings.Cut(addr, ":")
	out, err := nsTry(ns, "bash", "-c",
		fmt.Sprintf("exec 3<>/dev/tcp/%s/%s; printf '%%s\\n' %s >&3; IFS= read -r -t 5 l <&3; printf '%%s' \"$l\"", host, port, cmd))
	res := strings.TrimSpace(out)
	t.Logf("gameCmd %s %q err=%v out=%q", ns, cmd, err, out)
	if err != nil || res == "" {
		return ""
	}
	return res
}

// ---------- the scenario ----------

type node struct {
	name    string
	dataDir string
	sock    string
	proc    *proc
	nodeID  string
}

func TestNetnsDemo(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (ip netns); run via `sudo make e2e-netns`")
	}
	tmp := t.TempDir()
	setupTopology(t, nodeNames)

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
	defer cp.kill()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("--- cp log ---\n%s", cp.output())
		}
	})

	waitFor(t, 15*time.Second, "control plane up", func() bool {
		st, _ := apiCall("GET", cpURL+"/healthz", "", nil)
		return st == 200
	})

	// operator + group + enrollment tokens
	auth := apiJSON(t, "POST", cpURL+"/v1/auth/signup", "", map[string]any{
		"email": "op@example.com", "password": "hunter22!", "display_name": "Op",
	}, 201)
	tok := auth["token"].(string)
	grp := apiJSON(t, "POST", cpURL+"/v1/groups", tok, map[string]any{"name": "friends"}, 201)
	groupID := grp["id"].(string)
	et := apiJSON(t, "POST", cpURL+"/v1/groups/"+groupID+"/enrollment-tokens", tok,
		map[string]any{"max_uses": 4}, 201)
	token := et["token"].(string)

	// enroll + run all four nodes inside their netns (anchor flagged)
	nodes := map[string]*node{}
	for _, n := range nodeNames {
		dir := filepath.Join(tmp, "node-"+n)
		args := []string{"enroll", "--server", cpURL, "--token", token,
			"--data-dir", dir, "--fence-margin-ms", "1000"}
		if n == "anchor" {
			args = append(args, "--anchor")
		}
		nsExec(t, n, rustBin("varde-agent"), args...)
		// pin the mesh to a known UDP port so the NAT map (installed before
		// any traffic) applies to the first packet out
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
		nd := &node{name: n, dataDir: dir, sock: filepath.Join(dir, "run", "mesh.sock")}
		nd.proc = spawnInEnv(t, n,
			[]string{"VARDE_MESH_LOG_LEVEL=debug"},
			rustBin("varde-agent"), "run",
			"--data-dir", dir, "--mesh-bin", bin("varde-mesh"))
		nodes[n] = nd
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, nd := range nodes {
			// keep the last 40 non-heartbeat lines; per-heartbeat mesh
			// "configured" spam drowns the useful output
			var keep []string
			for _, l := range strings.Split(nd.proc.output(), "\n") {
				if strings.Contains(l, `"msg":"configured"`) ||
					strings.Contains(l, "direct dial failed") {
					continue
				}
				keep = append(keep, l)
			}
			if len(keep) > 40 {
				keep = keep[len(keep)-40:]
			}
			t.Logf("--- node %s log tail ---\n%s", nd.name, strings.Join(keep, "\n"))
		}
	})

	// all four online
	waitFor(t, 20*time.Second, "4 nodes online", func() bool {
		st, b := apiCall("GET", cpURL+"/v1/groups/"+groupID+"/nodes", tok, nil)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return st == 200 && len(m["nodes"].([]any)) == 4
	})
	for _, nd := range nodes {
		cfg, _ := os.ReadFile(filepath.Join(nd.dataDir, "config.toml"))
		for _, line := range strings.Split(string(cfg), "\n") {
			if strings.HasPrefix(line, "node_id") {
				nd.nodeID = strings.Trim(strings.TrimSpace(strings.SplitN(line, "=", 2)[1]), `"`)
			}
		}
	}

	// create + start a testgame server
	srv := apiJSON(t, "POST", cpURL+"/v1/groups/"+groupID+"/servers", tok, map[string]any{
		"name": "netns", "game_id": "testgame", "config": map[string]any{},
		"min_commit_replicas": 1, "replication_factor": 2, "preferred_node_id": nodes["a"].nodeID,
	}, 201)
	serverID := srv["id"].(string)
	apiJSON(t, "POST", cpURL+"/v1/servers/"+serverID+"/start", tok, map[string]any{}, 200)

	hostOf := func() string {
		st, b := apiCall("GET", cpURL+"/v1/servers/"+serverID+"/executions", tok, nil)
		var m map[string]any
		if st != 200 {
			return ""
		}
		_ = json.Unmarshal(b, &m)
		for _, e := range m["executions"].([]any) {
			em := e.(map[string]any)
			if em["state"] == "running" && em["ended_at"] == nil {
				return em["node_id"].(string)
			}
		}
		return ""
	}
	var hostNode, serviceAddr string
	waitFor(t, 90*time.Second, "server running", func() bool {
		hostNode = hostOf()
		if hostNode == "" {
			return false
		}
		st, b := apiCall("GET", cpURL+"/v1/servers/"+serverID, tok, nil)
		var m map[string]any
		if st != 200 {
			return false
		}
		_ = json.Unmarshal(b, &m)
		if svc, ok := m["service"].(map[string]any); ok {
			serviceAddr, _ = svc["loopback_ip"].(string)
		}
		return serviceAddr != ""
	})
	hostName := "a"
	for _, nd := range nodes {
		if nd.nodeID == hostNode {
			hostName = nd.name
		}
	}
	t.Logf("host=%s (%s) service=%s", hostNode, hostName, serviceAddr)

	// hole punching must be what connects b<->host: assert DIRECT path both
	// directions plus publicly observed endpoints (their NAT-mapped addrs).
	waitFor(t, 75*time.Second, "b↔host DIRECT", func() bool {
		b2h := peerPath(t, nodes["b"].sock, hostNode)
		h2b := peerPath(t, nodes[hostName].sock, nodes["b"].nodeID)
		t.Logf("paths: b->host=%s host->b=%s", b2h, h2b)
		return b2h == meshv1.PathKind_PATH_KIND_DIRECT && h2b == meshv1.PathKind_PATH_KIND_DIRECT
	})
	obsA := observedEndpoints(t, nodes[hostName].sock)
	obsB := observedEndpoints(t, nodes["b"].sock)
	if len(obsA) == 0 || len(obsB) == 0 {
		t.Fatalf("expected public observed endpoints; got %v / %v", obsA, obsB)
	}
	for _, o := range append(obsA, obsB...) {
		if !strings.HasPrefix(o, "10.200.77.") {
			t.Fatalf("observed endpoint %q is not a NAT-mapped address", o)
		}
	}
	t.Logf("observed endpoints host=%v b=%v", obsA, obsB)

	// client on b uses the stable address: real 127.77.x.y inside b's netns
	svc := serviceAddr + ":7777"
	for i := 1; i <= 5; i++ {
		waitFor(t, 20*time.Second, fmt.Sprintf("INCR to %d", i), func() bool {
			got := gameCmd(t, "b", svc, "INCR")
			return got == fmt.Sprint(i) || gameCmd(t, "b", svc, "GET") == fmt.Sprint(i)
		})
	}

	// snapshot committed and replicated off-host (anchor or peer)
	apiJSON(t, "POST", cpURL+"/v1/servers/"+serverID+"/snapshots", tok, nil, 202)
	waitFor(t, 60*time.Second, "snapshot committed", func() bool {
		st, b := apiCall("GET", cpURL+"/v1/servers/"+serverID+"/snapshots", tok, nil)
		var m map[string]any
		if st != 200 {
			return false
		}
		_ = json.Unmarshal(b, &m)
		for _, s := range m["snapshots"].([]any) {
			sm := s.(map[string]any)
			if sm["state"] != "committed" {
				continue
			}
			for _, r := range sm["replicas"].([]any) {
				rm := r.(map[string]any)
				if rm["state"] == "ready" && rm["node_id"] != hostNode {
					return true
				}
			}
		}
		return false
	})

	// hard-kill everything in the host's namespace
	out, err := exec.Command(toolPath()["ip"], "netns", "pids", hostName).CombinedOutput()
	if err != nil {
		t.Fatalf("netns pids %s: %v\n%s", hostName, err, out)
	}
	if len(strings.Fields(string(out))) == 0 {
		t.Fatalf("no processes in ns %s to kill", hostName)
	}
	for _, pid := range strings.Fields(string(out)) {
		exec.Command("kill", "-9", pid).Run()
	}
	var newHost string
	polls := 0
	waitFor(t, 90*time.Second, "server recovered off host", func() bool {
		newHost = hostOf()
		if polls%10 == 0 {
			_, b := apiCall("GET", cpURL+"/v1/servers/"+serverID+"/executions", tok, nil)
			t.Logf("newHost=%q executions=%s", newHost, b)
		}
		polls++
		return newHost != "" && newHost != hostNode
	})
	waitFor(t, 45*time.Second, "GET after recovery = 5 via same address", func() bool {
		return gameCmd(t, "b", svc, "GET") == "5"
	})

	// ---- phase 2: relay fallback ----
	blockDirectUDP(t, nodeNames)
	// restart every node's agent so fresh dials can't punch through
	for _, nd := range nodes {
		nd.proc.kill()
	}
	// ensure no stale mesh still holds UDP :41641 in the namespace;
	// pkill is not namespace-scoped, so kill by pid from `ip netns pids`
	for _, nd := range nodes {
		out, _ := exec.Command(toolPath()["ip"], "netns", "pids", nd.name).Output()
		for _, pid := range strings.Fields(string(out)) {
			exec.Command("kill", "-9", pid).Run()
		}
	}
	for _, nd := range nodes {
		nd.proc = spawnIn(t, nd.name, rustBin("varde-agent"), "run",
			"--data-dir", nd.dataDir, "--mesh-bin", bin("varde-mesh"))
	}
	polls = 0
	waitFor(t, 30*time.Second, "4 nodes online again", func() bool {
		st, b := apiCall("GET", cpURL+"/v1/groups/"+groupID+"/nodes", tok, nil)
		var m map[string]any
		if st != 200 {
			return false
		}
		_ = json.Unmarshal(b, &m)
		online := 0
		var states []string
		for _, nn := range m["nodes"].([]any) {
			lv, _ := nn.(map[string]any)["liveness"].(string)
			states = append(states, lv)
			if lv == "online" {
				online++
			}
		}
		if polls%10 == 0 {
			t.Logf("node states=%v raw=%s", states, b)
		}
		polls++
		return online >= 3
	})
	newHostName := hostName
	for _, nd := range nodes {
		if nd.nodeID == newHost {
			newHostName = nd.name
		}
	}
	// assert the relayed path on a non-host node (the host may be b itself)
	peerName := "b"
	if newHostName == "b" {
		peerName = "c"
	}
	polls = 0
	waitFor(t, 60*time.Second, peerName+"↔newhost RELAYED", func() bool {
		p2h := peerPath(t, nodes[peerName].sock, newHost)
		h2p := peerPath(t, nodes[newHostName].sock, nodes[peerName].nodeID)
		if polls%10 == 0 {
			t.Logf("relayed phase: %s->%s=%s %s->%s=%s",
				peerName, newHostName, p2h, newHostName, peerName, h2p)
		}
		polls++
		return p2h == meshv1.PathKind_PATH_KIND_RELAYED &&
			h2p == meshv1.PathKind_PATH_KIND_RELAYED
	})
	waitFor(t, 30*time.Second, "GET relayed = 5", func() bool {
		return gameCmd(t, "b", svc, "GET") == "5"
	})
}
