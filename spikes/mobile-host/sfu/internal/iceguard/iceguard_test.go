package iceguard

import "testing"

func TestAllowed(t *testing.T) {
	advertise := []string{"203.0.113.10", "2001:db8::1"}

	cases := []struct {
		name      string
		address   string
		advertise []string
		want      bool
	}{
		{
			// The ordinary deployment: nothing is asserted, so nothing is
			// refused. Getting this wrong breaks every LAN server at once.
			name:    "no advertise list lets everything through",
			address: "192.168.1.20",
			want:    true,
		},
		{
			name:      "an advertised address is allowed",
			address:   "203.0.113.10",
			advertise: advertise,
			want:      true,
		},
		{
			name:      "an advertised IPv6 address is allowed",
			address:   "2001:db8::1",
			advertise: advertise,
			want:      true,
		},
		{
			// The leak this exists for: a reflexive candidate carrying whatever
			// a STUN server reported.
			name:      "an address nobody named is refused",
			address:   "198.51.100.7",
			advertise: advertise,
			want:      false,
		},
		{
			// A private address is the shape GRYT-768 leaked, and it gets no special
			// treatment — it is refused for being unnamed, not for being private.
			name:      "a private address is refused when a list is set",
			address:   "192.168.50.147",
			advertise: advertise,
			want:      false,
		},
		{
			// Exact, not prefix. "203.0.113.100" starts with an advertised
			// address as a string and is a different host.
			name:      "a longer address sharing a prefix is refused",
			address:   "203.0.113.100",
			advertise: advertise,
			want:      false,
		},
		{
			name:      "an empty address is refused when a list is set",
			address:   "",
			advertise: advertise,
			want:      false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := Allowed(tt.address, tt.advertise); got != tt.want {
				t.Fatalf("Allowed(%q, %v) = %t, want %t", tt.address, tt.advertise, got, tt.want)
			}
		})
	}
}
