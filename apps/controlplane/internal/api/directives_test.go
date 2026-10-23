package api

import (
	"testing"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
)

func directiveDigestFixture() gen.AgentDirectives {
	d := gen.AgentDirectives{
		ServerTimeUnixMs:    1000,
		HeartbeatIntervalMs: 5000,
		LeaseTtlMs:          20000,
		Executions: &[]gen.ExecutionDirective{{
			ExecutionId:          "exec_a",
			ServerId:             "srv_a",
			ServerName:           "server",
			Epoch:                1,
			LeaseExpiresAtUnixMs: 21000,
			Action:               gen.ExecutionDirectiveAction("run"),
			Freeze:               ptr(true),
			StopReason:           ptr(gen.ExecutionDirectiveStopReasonUser),
			SnapshotRequests: &[]gen.SnapshotRequest{{
				RequestId: "request_a",
				Reason:    gen.SnapshotRequestReason("manual"),
			}},
			LatestSafeSaveOn: &[]gen.SaveHolder{{
				NodeId: "node_a", LastSeenAt: ptr(int64(1000)),
			}},
		}},
		Peers: &[]gen.DirectivePeer{{
			NodeId: "node_b", Name: "peer", PublicKey: "key",
			Endpoints: &[]string{"192.0.2.1:1", "192.0.2.2:2"},
			RelayIds:  &[]string{"relay_a", "relay_b"},
		}},
		Routes: &[]gen.DirectiveRoute{{
			ServiceId: "service_a", ServerId: "srv_a", ServerName: "server",
			LoopbackIp: "127.77.0.1", HostNodeId: ptr("node_a"),
			DrainUntilUnixMs: ptr(int64(1500)),
		}},
		Relays: &[]gen.DirectiveRelay{{
			RelayId: "relay_a", Addr: "relay.example:3478", Token: "token_a",
		}},
		ReplicationTasks: &[]gen.ReplicationTask{{
			SnapshotId: "snap_a", ServerId: "srv_a", ManifestDigest: "digest_a",
			SourceNodeIds: []string{"node_b", "node_a"},
		}},
		DeleteSnapshots: &[]string{"snap_a"},
	}
	d.Node.Name = "node_a"
	d.Node.HostingEnabled = true
	d.Node.Anchor = true
	d.Node.AdminState = gen.AgentDirectivesNodeAdminStateActive
	return d
}

func TestDirectivesDigestIgnoresPerCallValuesAndOrdering(t *testing.T) {
	first := directiveDigestFixture()
	second := directiveDigestFixture()
	second.ServerTimeUnixMs += 1000
	(*second.Executions)[0].LeaseExpiresAtUnixMs += 1000
	(*second.Executions)[0].LatestSafeSaveOn = &[]gen.SaveHolder{
		{NodeId: "node_a", LastSeenAt: ptr(int64(2000))},
	}
	(*second.Routes)[0].DrainUntilUnixMs = ptr(int64(2500))
	(*second.Relays)[0].Token = "token_b"
	(*second.ReplicationTasks)[0].SourceNodeIds = []string{"node_a", "node_b"}
	(*second.Peers)[0].Endpoints = &[]string{"192.0.2.2:2", "192.0.2.1:1"}
	(*second.Peers)[0].RelayIds = &[]string{"relay_b", "relay_a"}

	if got, want := directivesDigest(first), directivesDigest(second); got != want {
		t.Fatalf("volatile values changed digest: %s != %s", got, want)
	}
}

func TestDirectivesDigestTracksDesiredStateChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*gen.AgentDirectives)
	}{
		{"execution action", func(d *gen.AgentDirectives) {
			(*d.Executions)[0].Action = gen.ExecutionDirectiveAction("stop")
		}},
		{"freeze", func(d *gen.AgentDirectives) {
			(*d.Executions)[0].Freeze = ptr(false)
		}},
		{"stop reason", func(d *gen.AgentDirectives) {
			(*d.Executions)[0].StopReason = ptr(gen.ExecutionDirectiveStopReasonMigration)
		}},
		{"snapshot request", func(d *gen.AgentDirectives) {
			(*d.Executions)[0].SnapshotRequests = &[]gen.SnapshotRequest{{
				RequestId: "request_b", Reason: gen.SnapshotRequestReason("manual"),
			}}
		}},
		{"peer membership", func(d *gen.AgentDirectives) {
			*d.Peers = append(*d.Peers, gen.DirectivePeer{NodeId: "node_c", Name: "peer c"})
		}},
		{"route", func(d *gen.AgentDirectives) {
			(*d.Routes)[0].HostNodeId = ptr("node_b")
		}},
		{"relay membership", func(d *gen.AgentDirectives) {
			*d.Relays = append(*d.Relays, gen.DirectiveRelay{
				RelayId: "relay_b", Addr: "relay-b.example:3478",
			})
		}},
		{"replication task", func(d *gen.AgentDirectives) {
			(*d.ReplicationTasks)[0].SnapshotId = "snap_b"
		}},
		{"snapshot deletion", func(d *gen.AgentDirectives) {
			*d.DeleteSnapshots = append(*d.DeleteSnapshots, "snap_b")
		}},
		{"node flags", func(d *gen.AgentDirectives) {
			d.Node.HostingEnabled = false
		}},
	}

	want := directivesDigest(directiveDigestFixture())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := directiveDigestFixture()
			tt.mutate(&d)
			if got := directivesDigest(d); got == want {
				t.Fatalf("directive change did not change digest %s", got)
			}
		})
	}
}
