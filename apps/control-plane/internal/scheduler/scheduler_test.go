package scheduler

import (
	"reflect"
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
