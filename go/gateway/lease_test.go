package gateway

import (
	"testing"

	"github.com/grendalaget/varde/go/gatewaytoken"
)

func TestEpochFencing(t *testing.T) {
	old := gatewaytoken.Claims{Label: "at", GroupID: "g", ServerID: "s", Epoch: 9}
	s := &Server{leases: map[string]lease{"at": {claims: old}}}
	for _, tc := range []struct {
		name                 string
		epoch                uint64
		group, server, label string
		want                 bool
	}{
		{"new label", 1, "g", "s", "other", true},
		{"old epoch", 8, "g", "s", "at", false},
		{"reconnect", 9, "g", "s", "at", true},
		{"equal foreign server", 9, "g", "other", "at", false},
		{"equal foreign group", 9, "other", "s", "at", false},
		{"newer owner", 10, "other", "other", "at", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.allowed(gatewaytoken.Claims{Label: tc.label, GroupID: tc.group, ServerID: tc.server, Epoch: tc.epoch}); got != tc.want {
				t.Errorf("allowed = %v, want %v", got, tc.want)
			}
		})
	}
	s.closed = true
	if s.allowed(gatewaytoken.Claims{Label: "new", Epoch: 100}) {
		t.Fatal("closed server accepted lease")
	}
}
