//go:build netns && valheim

package netns

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const valheimStartupTimeout = 20 * time.Minute

func TestValheimFailover(t *testing.T) {
	e, watch := newValheimEnv(t)
	serverID, deploymentID := e.createValheimServer("Varde E2E", "VardeE2E", false)
	e.seedValheim(deploymentID)
	e.stopValheimAtCleanup(serverID)

	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/start", e.tok, map[string]any{}, 200)
	_, arnePrepare, _ := e.waitValheim(serverID, "arne", valheimStartupTimeout)
	t.Logf("Valheim prepare-to-running node=arne: %s", arnePrepare)
	e.setHosting("kari", true)

	execID := e.activeExecutionID(serverID)
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/stop", e.tok, map[string]any{}, 200)
	e.waitExecutionStopped(serverID, execID, 3*time.Minute)
	e.waitCommitted(serverID, e.nodes["arne"].nodeID)
	snapshotID := e.latestCommittedSnapshotID(serverID)

	arneServerDir := valheimDataDir(e, "arne", serverID)
	assertValheimWorldFiles(t, arneServerDir, "VardeE2E")
	reference := hashValheimFiles(t, arneServerDir, "VardeE2E")
	if len(reference) == 0 {
		t.Fatal("shutdown-save hash map is empty")
	}
	t.Logf("Valheim shutdown-save hash-map file count: %d", len(reference))

	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/start", e.tok, map[string]any{}, 200)
	restartExecID, _, _ := e.waitValheim(serverID, "arne", valheimStartupTimeout)
	restarted := e.activeExecution(serverID)
	if restarted == nil || restarted["id"] != restartExecID ||
		restarted["restore_snapshot_id"] != snapshotID {
		t.Fatalf("restart did not restore committed snapshot %s: execution=%v", snapshotID, restarted)
	}
	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/snapshots", e.tok, nil, 202)
	e.waitCommitted(serverID, e.nodes["arne"].nodeID)
	t.Log("manual live snapshot committed off arne")

	killedAt := time.Now()
	killNamespace(t, "arne")
	t.Logf("[%s] killed arne's namespace", killedAt.Format("15:04:05.000"))
	_, kariPrepare, runningAt := e.waitValheim(serverID, "kari", valheimStartupTimeout)
	t.Logf("Valheim prepare-to-running node=kari: %s", kariPrepare)
	t.Logf("Valheim RTO (kill -> running on kari): %s", runningAt.Sub(killedAt))

	kariServerDir := valheimDataDir(e, "kari", serverID)
	got := hashValheimFiles(t, kariServerDir, "VardeE2E")
	if !reflect.DeepEqual(reference, got) {
		t.Fatalf("Valheim save files differ after failover:\nreference (%d files): %v\nafter failover (%d files): %v",
			len(reference), reference, len(got), got)
	}
	t.Logf("Valheim failover preserved byte-identical save files (%d files)", len(got))
	watch.check(t)
}

func TestValheimCrossplayFailover(t *testing.T) {
	e, watch := newValheimEnv(t)
	serverID, deploymentID := e.createValheimServer("Varde E2E XP", "VardeXP", true)
	e.seedValheim(deploymentID)
	e.stopValheimAtCleanup(serverID)

	apiJSON(t, "POST", e.cpURL+"/v1/servers/"+serverID+"/start", e.tok, map[string]any{}, 200)
	_, arnePrepare, _ := e.waitValheim(serverID, "arne", valheimStartupTimeout)
	t.Logf("Valheim Crossplay prepare-to-running node=arne: %s", arnePrepare)
	code1 := e.waitJoinCode(serverID, valheimStartupTimeout)
	assertCrossplayAddressAndRoutes(t, e, serverID, "arne")
	e.setHosting("kari", true)

	killedAt := time.Now()
	killNamespace(t, "arne")
	t.Logf("[%s] killed arne's namespace", killedAt.Format("15:04:05.000"))
	_, kariPrepare, runningAt := e.waitValheim(serverID, "kari", valheimStartupTimeout)
	t.Logf("Valheim Crossplay prepare-to-running node=kari: %s", kariPrepare)
	t.Logf("Valheim Crossplay RTO (kill -> running on kari): %s", runningAt.Sub(killedAt))
	code2 := e.waitJoinCode(serverID, valheimStartupTimeout)
	assertCrossplayAddressAndRoutes(t, e, serverID, "kari")
	t.Logf("Valheim Crossplay join codes: code1=%s code2=%s changed=%t", code1, code2, code1 != code2)
	watch.check(t)
}

func newValheimEnv(t *testing.T) (*gameEnv, *gameWatchdog) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root (ip netns); run via `sudo make e2e-valheim`")
	}
	e := newGameEnv(t, realGameNodeNames)
	watch := startWatchdog(t, realGameNodeNames, "valheim_server")
	for _, name := range realGameNodeNames {
		e.enroll(name, name == "nas")
	}
	for _, name := range realGameNodeNames {
		e.startAgent(name)
	}
	e.setHosting("kari", false)
	e.setHosting("nas", false)
	e.setHosting("player", false)
	e.waitOnline(45*time.Second, realGameNodeNames...)
	return e, watch
}

func (e *gameEnv) createValheimServer(name, world string, crossplay bool) (string, string) {
	cfg := map[string]any{
		"server_name":     name,
		"world_name":      world,
		"password":        "vardee2e1",
		"modifiers":       "normal",
		"save_interval_s": 3600,
		"crossplay":       crossplay,
	}
	server := apiJSON(e.t, "POST", e.cpURL+"/v1/groups/"+e.groupID+"/servers", e.tok, map[string]any{
		"name": name, "game_id": "valheim", "config": cfg,
		"replication_factor": 3, "min_commit_replicas": 1,
		"preferred_node_id": e.nodes["arne"].nodeID,
	}, 201)
	serverID := server["id"].(string)
	deploymentID, _ := server["deployment_id"].(string)
	if deploymentID == "" {
		deploymentID = "valheim"
	}
	e.serverIDs = append(e.serverIDs, serverID)
	return serverID, deploymentID
}

func (e *gameEnv) seedValheim(deploymentID string) {
	e.t.Helper()
	seedDir := os.Getenv("VARDE_VALHEIM_SEED_DIR")
	if seedDir == "" {
		e.t.Log("VARDE_VALHEIM_SEED_DIR is unset; Valheim artifacts will be installed normally")
		return
	}
	for _, name := range realGameNodeNames {
		target := valheimDeploymentServerDir(e, name, deploymentID)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			e.t.Fatalf("create deployment directory for %s: %v", name, err)
		}
		cmd := exec.Command("cp", "-a", seedDir, target)
		if output, err := cmd.CombinedOutput(); err != nil {
			e.t.Fatalf("seed Valheim artifacts on %s: %v\n%s", name, err, output)
		}
		e.t.Logf("seeded Valheim artifacts node=%s source=%s target=%s", name, seedDir, target)
	}
}

func valheimDeploymentServerDir(e *gameEnv, nodeName, deploymentID string) string {
	return filepath.Join(e.nodes[nodeName].dataDir, "deployments", deploymentID, "server")
}

func valheimDataDir(e *gameEnv, nodeName, serverID string) string {
	return filepath.Join(e.nodes[nodeName].dataDir, "servers", serverID)
}

func (e *gameEnv) activeExecution(serverID string) map[string]any {
	status, body := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/executions", e.tok, nil)
	if status != 200 {
		return nil
	}
	var response map[string]any
	if json.Unmarshal(body, &response) != nil {
		return nil
	}
	executions, _ := response["executions"].([]any)
	for _, value := range executions {
		execution, _ := value.(map[string]any)
		if execution != nil && execution["ended_at"] == nil &&
			execution["state"] != "stopped" && execution["state"] != "failed" {
			return execution
		}
	}
	return nil
}

func (e *gameEnv) activeExecutionID(serverID string) string {
	execution := e.activeExecution(serverID)
	if execution == nil {
		e.t.Fatalf("no active execution for server %s", serverID)
	}
	id, _ := execution["id"].(string)
	if id == "" {
		e.t.Fatalf("active execution for server %s has no ID: %v", serverID, execution)
	}
	return id
}

func (e *gameEnv) waitExecutionStopped(serverID, executionID string, d time.Duration) {
	waitFor(e.t, d, "Valheim execution stopped", func() bool {
		status, body := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/executions", e.tok, nil)
		if status != 200 {
			return false
		}
		var response map[string]any
		if json.Unmarshal(body, &response) != nil {
			return false
		}
		executions, _ := response["executions"].([]any)
		for _, value := range executions {
			execution, _ := value.(map[string]any)
			if execution != nil && execution["id"] == executionID {
				return execution["ended_at"] != nil
			}
		}
		return false
	})
}

func (e *gameEnv) latestCommittedSnapshotID(serverID string) string {
	e.t.Helper()
	status, body := apiCall("GET", e.cpURL+"/v1/servers/"+serverID+"/snapshots", e.tok, nil)
	if status != 200 {
		e.t.Fatalf("get Valheim snapshots: %d %s", status, body)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		e.t.Fatalf("decode Valheim snapshots: %v", err)
	}
	snapshots, _ := response["snapshots"].([]any)
	var latestID string
	var latestAt float64
	for _, value := range snapshots {
		snapshot, _ := value.(map[string]any)
		if snapshot == nil || snapshot["state"] != "committed" {
			continue
		}
		createdAt, _ := snapshot["created_at"].(float64)
		if latestID == "" || createdAt > latestAt {
			latestID, _ = snapshot["id"].(string)
			latestAt = createdAt
		}
	}
	if latestID == "" {
		e.t.Fatalf("server %s has no committed Valheim snapshot: %s", serverID, body)
	}
	return latestID
}

func (e *gameEnv) waitValheim(serverID, wantHost string, d time.Duration) (string, time.Duration, time.Time) {
	var execID string
	var prepareAt, runningAt time.Time
	nextLogCheck := time.Time{}
	waitFor(e.t, d, "Valheim running and connected on "+wantHost, func() bool {
		execution := e.activeExecution(serverID)
		if execution == nil {
			return false
		}
		nodeID, _ := execution["node_id"].(string)
		if nodeID != e.nodes[wantHost].nodeID {
			return false
		}
		execID, _ = execution["id"].(string)
		state, _ := execution["state"].(string)
		if state == "preparing" && prepareAt.IsZero() {
			prepareAt = time.Now()
		}
		if state != "running" {
			return false
		}
		if prepareAt.IsZero() {
			prepareAt = time.Now()
		}
		if runningAt.IsZero() {
			runningAt = time.Now()
		}
		if time.Now().Before(nextLogCheck) {
			return false
		}
		nextLogCheck = time.Now().Add(500 * time.Millisecond)
		return e.executionLogsContain(serverID, execID, "Game server connected")
	})
	if execID == "" || runningAt.IsZero() {
		e.t.Fatalf("Valheim execution on %s reached connected without timing data", wantHost)
	}
	return execID, runningAt.Sub(prepareAt), runningAt
}

func (e *gameEnv) executionLogsContain(serverID, executionID, phrase string) bool {
	if executionID == "" {
		return false
	}
	endpoint := e.cpURL + "/v1/servers/" + serverID + "/logs?execution_id=" + url.QueryEscape(executionID)
	status, body := apiCall("GET", endpoint, e.tok, nil)
	if status != 200 {
		return false
	}
	var response map[string]any
	if json.Unmarshal(body, &response) != nil {
		return false
	}
	lines, _ := response["lines"].([]any)
	for _, value := range lines {
		line, _ := value.(map[string]any)
		if line != nil && strings.Contains(fmt.Sprint(line["line"]), phrase) {
			return true
		}
	}
	return false
}

func (e *gameEnv) waitJoinCode(serverID string, d time.Duration) string {
	var code string
	nextCheck := time.Time{}
	waitFor(e.t, d, "Valheim Crossplay join code", func() bool {
		if time.Now().Before(nextCheck) {
			return false
		}
		nextCheck = time.Now().Add(500 * time.Millisecond)
		status, body := apiCall("GET", e.cpURL+"/v1/servers/"+serverID, e.tok, nil)
		if status != 200 {
			return false
		}
		var server map[string]any
		if json.Unmarshal(body, &server) != nil {
			return false
		}
		summary, _ := server["summary"].(map[string]any)
		code, _ = summary["join_code"].(string)
		return len(code) == 6 && allASCIIDigits(code)
	})
	return code
}

func allASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func assertCrossplayAddressAndRoutes(t *testing.T, e *gameEnv, serverID, host string) {
	t.Helper()
	status, body := apiCall("GET", e.cpURL+"/v1/servers/"+serverID, e.tok, nil)
	if status != 200 {
		t.Fatalf("get Crossplay server: %d %s", status, body)
	}
	var server map[string]any
	if err := json.Unmarshal(body, &server); err != nil {
		t.Fatalf("decode Crossplay server response: %v", err)
	}
	summary, _ := server["summary"].(map[string]any)
	if address := summary["address"]; address != nil && address != "" {
		t.Fatalf("Crossplay summary address must be empty: %v", address)
	}

	serviceIP := e.svcAddr(serverID)
	if serviceIP == "" {
		t.Log("Crossplay server has no service address or route")
		return
	}
	service, _ := server["service"].(map[string]any)
	ports, _ := service["ports"].([]any)
	if len(ports) == 0 {
		t.Fatalf("Crossplay server service has no ports: %s", body)
	}
	for _, peer := range realGameNodeNames {
		if peer == host {
			continue
		}
		output, err := nsTry(peer, "ss", "-H", "-lntu")
		if err != nil {
			t.Fatalf("list routes in %s: %v\n%s", peer, err, output)
		}
		for _, portValue := range ports {
			port, _ := portValue.(map[string]any)
			portNumber := int(port["port"].(float64))
			listener := fmt.Sprintf("%s:%d", serviceIP, portNumber)
			if strings.Contains(output, listener) {
				t.Fatalf("Crossplay route listener %s exists on peer %s:\n%s", listener, peer, output)
			}
		}
	}
}

func assertValheimWorldFiles(t *testing.T, serverDir, world string) {
	t.Helper()
	worldDir := filepath.Join(serverDir, "saves", "worlds_local", world)
	for _, pattern := range []string{"_main.*.db2", "_main.*.fwl2"} {
		matches, err := filepath.Glob(filepath.Join(worldDir, pattern))
		if err != nil {
			t.Fatalf("glob Valheim save files: %v", err)
		}
		if len(matches) == 0 {
			t.Fatalf("no %s files under %s", pattern, worldDir)
		}
	}
}

func hashValheimFiles(t *testing.T, serverDir, world string) map[string]string {
	t.Helper()
	savesDir := filepath.Join(serverDir, "saves")
	worldDir := filepath.Join(savesDir, "worlds_local", world)
	hashes := map[string]string{}
	add := func(path string) error {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		relative, err := filepath.Rel(serverDir, path)
		if err != nil {
			return err
		}
		hashes[filepath.ToSlash(relative)] = hex.EncodeToString(sum[:])
		return nil
	}
	err := filepath.WalkDir(worldDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		return add(path)
	})
	if err != nil {
		t.Fatalf("walk Valheim world save files under %s: %v", worldDir, err)
	}
	textFiles, err := filepath.Glob(filepath.Join(savesDir, "*.txt"))
	if err != nil {
		t.Fatalf("glob Valheim save text files: %v", err)
	}
	for _, path := range textFiles {
		if err := add(path); err != nil {
			t.Fatalf("hash Valheim save text file %s: %v", path, err)
		}
	}
	return hashes
}

func (e *gameEnv) stopValheimAtCleanup(serverID string) {
	e.t.Cleanup(func() {
		if e.hostOf(serverID) == "" {
			return
		}
		status, body := apiCall("POST", e.cpURL+"/v1/servers/"+serverID+"/stop", e.tok, map[string]any{})
		if status != 200 {
			e.t.Logf("cleanup stop for Valheim server %s failed: %d %s", serverID, status, body)
			return
		}
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			if e.hostOf(serverID) == "" {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		e.t.Logf("cleanup timed out stopping Valheim server %s", serverID)
	})
}
