package scheduler

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func okNode(id string) NodeView {
	return NodeView{
		ID: id, Online: true, AdminState: "active", HostingEnabled: true,
		OS: "linux", Arch: "amd64", Drivers: []string{"testgame"},
		MemoryAvailableMB: 4096, DiskFreeBytes: 1 << 40,
	}
}

var testGame = GameView{ID: "testgame", MinMemoryMB: 1024, OS: []string{"linux"}, Arch: []string{"amd64"}}
var testSrv = ServerView{ID: "srv_1", GameID: "testgame"}

func TestPlacePicksOnlyEligible(t *testing.T) {
	d, err := Place(testSrv, testGame, []NodeView{okNode("node_b"), okNode("node_a")}, SnapView{})
	if err != nil {
		t.Fatal(err)
	}
	// tie → smaller node_id wins
	if d.NodeID != "node_a" {
		t.Fatalf("want node_a, got %s", d.NodeID)
	}
}

func TestPlaceOSNilAndEmpty(t *testing.T) {
	windows := okNode("node_windows")
	windows.OS = "windows"
	for _, tc := range []struct {
		name    string
		oses    []string
		wantErr bool
	}{
		{name: "nil is unrestricted"},
		{name: "empty rejects every OS", oses: []string{}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			game := testGame
			game.OS = tc.oses
			_, err := Place(testSrv, game, []NodeView{windows}, SnapView{})
			if (err != nil) != tc.wantErr {
				t.Errorf("Place(OS=%v) error = %v, wantErr %v", tc.oses, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "unsupported os windows") {
				t.Errorf("Place(OS=%v) error = %v, want unsupported os reason", tc.oses, err)
			}
		})
	}
}

func TestPlaceMinimumDisk(t *testing.T) {
	for _, tc := range []struct {
		name       string
		minDiskMB  int64
		deployment int64
		restore    int64
		free       int64
		wantErr    bool
	}{
		{name: "unset minimum"},
		{name: "fresh install insufficient", minDiskMB: 75000, free: 74_999_999_999, wantErr: true},
		{name: "fresh install exact", minDiskMB: 75000, free: 75_000_000_000},
		{name: "deployment larger than minimum", minDiskMB: 75000, deployment: 80_000_000_000, free: 75_000_000_000, wantErr: true},
		{name: "deployment exact", minDiskMB: 75000, deployment: 80_000_000_000, free: 80_000_000_000},
		{name: "restore needs headroom", minDiskMB: 75000, restore: 1000, free: 75_000_001_999, wantErr: true},
		{name: "restore exact", minDiskMB: 75000, restore: 1000, free: 75_000_002_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			game := testGame
			game.MinDiskMB = tc.minDiskMB
			node := okNode("node_a")
			node.DiskFreeBytes = tc.free
			_, err := Place(testSrv, game, []NodeView{node}, SnapView{DeploymentSize: tc.deployment, RestoreBytes: tc.restore})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Place() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "insufficient disk") {
				t.Fatalf("expected insufficient disk, got %v", err)
			}
		})
	}
}

func TestPlaceInstalledSteamApp(t *testing.T) {
	game := testGame
	game.SteamApp, game.MinDiskMB, game.Runtimes = 730, 75000, []string{"steamcmd"}
	for _, tc := range []struct {
		name     string
		apps     []int64
		runtimes []string
		free     int64
		restore  int64
		wantErr  bool
	}{
		{name: "complete client or previous SteamCMD install on restart or failover", apps: []int64{730}, free: 1_000_000},
		{name: "installed still needs restore space", apps: []int64{730}, free: 1999, restore: 1000, wantErr: true},
		{name: "installed restore exact", apps: []int64{730}, free: 2000, restore: 1000},
		{name: "fallback disk gate", runtimes: []string{"steamcmd"}, free: 74_999_999_999, wantErr: true},
		{name: "fallback allowed", runtimes: []string{"steamcmd"}, free: 75_000_000_000},
		{name: "fallback needs runtime", free: 75_000_000_000, wantErr: true},
		{name: "wrong app not sufficient", apps: []int64{896660}, free: 1_000_000, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := okNode("node_a")
			node.SteamApps, node.Runtimes, node.DiskFreeBytes = tc.apps, tc.runtimes, tc.free
			_, err := Place(testSrv, game, []NodeView{node}, SnapView{RestoreBytes: tc.restore})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Place() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
	fallback, installed := okNode("node_a"), okNode("node_z")
	fallback.Runtimes = []string{"steamcmd"}
	installed.SteamApps, installed.DiskFreeBytes = []int64{730}, 1_000_000
	d, err := Place(testSrv, game, []NodeView{fallback, installed}, SnapView{})
	if err != nil || d.NodeID != installed.ID {
		t.Fatalf("installed app should win: %v %v", d, err)
	}
	if !slices.Contains(d.Reasons, "Steam app 730 already installed") {
		t.Fatalf("missing reason: %v", d.Reasons)
	}
}

func TestPlaceDeterministic(t *testing.T) {
	nodes := []NodeView{okNode("node_c"), okNode("node_a"), okNode("node_b")}
	for i := 0; i < 5; i++ {
		d, err := Place(testSrv, testGame, nodes, SnapView{})
		if err != nil || d.NodeID != "node_a" {
			t.Fatalf("non-deterministic: %v %v", d, err)
		}
	}
}

func TestPlaceHardConstraints(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*NodeView)
	}{
		{"offline", func(n *NodeView) { n.Online = false }},
		{"shutting down", func(n *NodeView) { n.Draining = true }},
		{"drained", func(n *NodeView) { n.AdminState = "drained" }},
		{"hosting disabled", func(n *NodeView) { n.HostingEnabled = false }},
		{"missing driver", func(n *NodeView) { n.Drivers = nil }},
		{"wrong os", func(n *NodeView) { n.OS = "windows" }},
		{"wrong arch", func(n *NodeView) { n.Arch = "arm64" }},
		{"low memory", func(n *NodeView) { n.MemoryAvailableMB = 512 }},
		{"over max memory", func(n *NodeView) { m := int64(512); n.MaxMemoryMB = &m }},
		{"low disk", func(n *NodeView) { n.DiskFreeBytes = 10 }},
		{"missing runtime", func(n *NodeView) { n.Runtimes = nil }},
	}
	for _, tc := range cases {
		bad := okNode("node_bad")
		tc.mut(&bad)
		game := testGame
		if tc.name == "missing runtime" {
			game.Runtimes = []string{"jre"}
		}
		snap := SnapView{}
		if tc.name == "low disk" {
			snap.RestoreBytes = 1 << 20
		}
		_, err := Place(testSrv, game, []NodeView{bad}, snap)
		if err == nil {
			t.Fatalf("%s: expected unschedulable", tc.name)
		}
		se, ok := err.(*Error)
		if !ok || len(se.Reasons) == 0 {
			t.Fatalf("%s: expected reasons", tc.name)
		}
	}
}

func TestPlaceSkipsDrainingNode(t *testing.T) {
	// draining node has snapshot-local + preferred and still loses to an
	// eligible node; with no eligible node the reason is reported
	drain := okNode("node_drain")
	drain.Draining = true
	drain.HasRestoreLocal = true
	srv := testSrv
	srv.PreferredNodeID = drain.ID
	d, err := Place(srv, testGame, []NodeView{drain, okNode("node_ok")}, SnapView{})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if d.NodeID != "node_ok" {
		t.Fatalf("placed on draining node: %+v", d)
	}
	_, err = Place(srv, testGame, []NodeView{drain}, SnapView{})
	se, ok := err.(*Error)
	if !ok || !slices.Contains(se.Reasons, "node_drain: shutting down") {
		t.Fatalf("expected shutting-down reason, got %v", err)
	}
}

func TestPlaceWeights(t *testing.T) {
	// battery penalty dominates everything
	a := okNode("node_a")
	a.OnBattery = true
	a.Anchor = true
	b := okNode("node_b")
	d, err := Place(testSrv, testGame, []NodeView{a, b}, SnapView{})
	if err != nil || d.NodeID != "node_b" {
		t.Fatalf("battery: %v %v", d, err)
	}

	// anchor beats non-anchor
	a = okNode("node_a")
	b = okNode("node_b")
	b.Anchor = true
	d, _ = Place(testSrv, testGame, []NodeView{a, b}, SnapView{})
	if d.NodeID != "node_b" {
		t.Fatalf("anchor weight: %s", d.NodeID)
	}

	// local restore beats anchor
	a = okNode("node_a")
	a.HasRestoreLocal = true
	d, _ = Place(testSrv, testGame, []NodeView{a, b}, SnapView{})
	if d.NodeID != "node_a" {
		t.Fatalf("restore local: %s", d.NodeID)
	}

	// preferred node
	a = okNode("node_a")
	b = okNode("node_b")
	srv := testSrv
	srv.PreferredNodeID = "node_b"
	d, _ = Place(srv, testGame, []NodeView{a, b}, SnapView{})
	if d.NodeID != "node_b" {
		t.Fatalf("preferred: %s", d.NodeID)
	}

	// user active penalty
	a = okNode("node_a")
	a.UserActive = true
	b = okNode("node_b")
	d, _ = Place(testSrv, testGame, []NodeView{a, b}, SnapView{})
	if d.NodeID != "node_b" {
		t.Fatalf("user active: %s", d.NodeID)
	}
}

func TestReplicationTargets(t *testing.T) {
	cands := []ReplCandidate{
		{NodeID: "node_prod", Online: true},
		{NodeID: "node_anchor", Online: true, Anchor: true},
		{NodeID: "node_host", Online: true, HostingEnabled: true, DiskFreeBytes: 100},
		{NodeID: "node_parent", Online: true, HasParent: true, DiskFreeBytes: 50},
		{NodeID: "node_off", Online: false, Anchor: true},
	}
	got := SelectReplicationTargets("node_prod", 3, cands)
	// anchor first, then best-scored non-anchor (parent has +50 > host+20+disk)
	want := []string{"node_anchor", "node_parent"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}

	// replicationFactor 1 → producer only
	if got := SelectReplicationTargets("node_prod", 1, cands); len(got) != 0 {
		t.Fatalf("rf=1 should select none: %v", got)
	}

	// deterministic across permutations
	c2 := []ReplCandidate{cands[3], cands[0], cands[2], cands[4], cands[1]}
	if !reflect.DeepEqual(SelectReplicationTargets("node_prod", 3, c2), want) {
		t.Fatal("not deterministic")
	}
}
