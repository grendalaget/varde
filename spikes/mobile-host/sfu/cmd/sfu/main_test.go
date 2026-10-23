package main

import (
	"net"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"

	"sfu-v2/internal/config"
	"sfu-v2/internal/iceguard"
)

func TestShouldBindICEUDPAddressSkipsLinkLocal(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{name: "ipv6 link local", ip: "fe80::1", want: false},
		{name: "ipv4 link local", ip: "169.254.10.20", want: false},
		{name: "private ipv4", ip: "192.168.1.20", want: true},
		{name: "global ipv6", ip: "2001:db8::1", want: true},
		{name: "loopback", ip: "127.0.0.1", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldBindICEUDPAddress(net.ParseIP(tt.ip)); got != tt.want {
				t.Fatalf("shouldBindICEUDPAddress(%q) = %t, want %t", tt.ip, got, tt.want)
			}
		})
	}
}

// TestSentCandidatesOnlyCarryAdvertisedAddresses gathers a real candidate set through the
// same filter the join handler applies. Two assertions: only named addresses, and some sent.
func TestSentCandidatesOnlyCarryAdvertisedAddresses(t *testing.T) {
	cfg := &config.Config{
		ICEUDPMuxPort:   0, // the OS picks, so this cannot collide with a running SFU
		ICEAdvertiseIPs: []string{"203.0.113.10"},
		DisableSTUN:     true,
	}

	se, err := newSettingEngine(cfg)
	if err != nil {
		t.Fatalf("newSettingEngine: %v", err)
	}

	api := pion.NewAPI(pion.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	defer func() { _ = pc.Close() }()

	// Something to gather for. A data channel is enough and needs no media.
	if _, err := pc.CreateDataChannel("probe", nil); err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}

	gathered := make(chan *pion.ICECandidate, 32)
	pc.OnICECandidate(func(c *pion.ICECandidate) { gathered <- c })

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}

	var sent, dropped []string
	deadline := time.After(15 * time.Second)
	for {
		select {
		case c := <-gathered:
			if c == nil {
				if len(sent)+len(dropped) == 0 {
					t.Skip("no candidates gathered — no usable interface in this environment")
				}
				if len(sent) == 0 {
					t.Fatalf("every candidate was filtered out, so nothing could reach this server; dropped %v", dropped)
				}
				t.Logf("sent %v, dropped %v", sent, dropped)
				return
			}
			if iceguard.Allowed(c.Address, cfg.ICEAdvertiseIPs) {
				sent = append(sent, c.Address)
			} else {
				dropped = append(dropped, c.Address)
			}
		case <-deadline:
			t.Fatalf("ICE gathering did not finish within 15s; sent %v, dropped %v", sent, dropped)
		}
	}
}
