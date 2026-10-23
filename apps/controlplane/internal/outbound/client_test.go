package outbound

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestNewClientDoesNotFollowRedirects(t *testing.T) {
	var destinationCalled bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationCalled = true
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer source.Close()

	resp, err := NewClient(time.Second).Get(source.URL)
	if err != nil {
		t.Fatalf("NewClient().Get(%q) error = %v", source.URL, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("response status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	if destinationCalled {
		t.Error("redirect destination was requested")
	}
}

func TestReadLimited(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "within limit", size: MaxResponseBytes},
		{name: "over limit", size: MaxResponseBytes + 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadLimited(strings.NewReader(strings.Repeat("x", tc.size)))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ReadLimited(%d bytes) error = %v, want error presence %t", tc.size, err, tc.wantErr)
			}
			if !tc.wantErr && len(got) != tc.size {
				t.Errorf("ReadLimited(%d bytes) length = %d, want %d", tc.size, len(got), tc.size)
			}
		})
	}
}

func TestReadLimitedPropagatesReaderError(t *testing.T) {
	_, err := ReadLimited(errorReader{})
	if err == nil {
		t.Fatal("ReadLimited(errorReader{}) error = nil, want reader error")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestIsDisallowedAddress(t *testing.T) {
	for _, tc := range []struct {
		address string
		want    bool
	}{
		{address: "1.1.1.1"},
		{address: "2606:4700::1111"},
		{address: "127.0.0.1", want: true},
		{address: "10.0.0.1", want: true},
		{address: "100.64.0.1", want: true},
		{address: "0.1.2.3", want: true},
		{address: "192.0.2.1", want: true},
		{address: "198.18.0.1", want: true},
		{address: "198.51.100.1", want: true},
		{address: "203.0.113.1", want: true},
		{address: "240.0.0.1", want: true},
		{address: "255.255.255.255", want: true},
		{address: "169.254.1.1", want: true},
		{address: "::", want: true},
		{address: "::1", want: true},
		{address: "ff02::1", want: true},
		{address: "fc00::1", want: true},
		{address: "fe80::1", want: true},
		{address: "64:ff9b::808:808", want: true},
		{address: "64:ff9b:1::1", want: true},
		{address: "2001:db8::1", want: true},
		{address: "2002::1", want: true},
		{address: "100::1", want: true},
		{address: "::ffff:127.0.0.1", want: true},
	} {
		t.Run(tc.address, func(t *testing.T) {
			addr := netip.MustParseAddr(tc.address)
			if got := IsDisallowedAddress(addr); got != tc.want {
				t.Errorf("IsDisallowedAddress(%s) = %t, want %t", tc.address, got, tc.want)
			}
		})
	}
}
