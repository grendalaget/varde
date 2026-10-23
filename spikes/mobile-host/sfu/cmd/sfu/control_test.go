package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTakenControlPortStopsStartup is a second SFU on one machine, finding the control port
// held. It has to exit before its public port can point a server at somebody else's SFU.
func TestTakenControlPortStopsStartup(t *testing.T) {
	if os.Getenv("GRYT_SFU_TEST_MAIN") == "1" {
		main()
		return
	}

	held, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("hold a control port: %v", err)
	}
	defer held.Close()
	controlPort := held.Addr().(*net.TCPAddr).Port
	signallingPort := freePort(t, "tcp")
	mediaPort := freePort(t, "udp")

	cmd := exec.Command(os.Args[0], "-test.run=^TestTakenControlPortStopsStartup$")
	cmd.Env = append(os.Environ(),
		"GRYT_SFU_TEST_MAIN=1",
		fmt.Sprintf("SFU_PORT=%d", signallingPort),
		fmt.Sprintf("SFU_CONTROL_PORT=%d", controlPort),
		fmt.Sprintf("ICE_UDP_MUX_PORT=%d", mediaPort),
		"SFU_METRICS_PORT=0",
		"DISABLE_STUN=true",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the SFU: %v", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stop := func() {
		_ = cmd.Process.Kill()
		<-exited
	}

	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.After(30 * time.Second)
	for {
		select {
		case err := <-exited:
			if err == nil {
				t.Fatalf("the SFU exited cleanly with its control port taken:\n%s", output.String())
			}
			if !strings.Contains(output.String(), "SFU_CONTROL_PORT") {
				t.Fatalf("the SFU exited without saying the control port was the problem:\n%s", output.String())
			}
			return
		case <-deadline:
			stop()
			t.Fatalf("the SFU kept running with its control port held by another process:\n%s", output.String())
		case <-time.After(50 * time.Millisecond):
			res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/server", signallingPort))
			if err != nil {
				continue
			}
			res.Body.Close()
			if res.Header.Get("X-Gryt-Control-Port") == strconv.Itoa(controlPort) {
				stop()
				t.Fatalf("the SFU pointed registration at %d, which another process holds:\n%s", controlPort, output.String())
			}
		}
	}
}

// TestControlHostKeepsRegistrationOffTheNetwork runs the SFU twice and dials this machine's own
// network address. Signalling answers there both times; registration only when no host is set.
func TestControlHostKeepsRegistrationOffTheNetwork(t *testing.T) {
	if os.Getenv("GRYT_SFU_TEST_MAIN") == "1" {
		main()
		return
	}

	network := networkAddress(t)

	for _, tt := range []struct {
		name        string
		host        string
		fromNetwork bool
	}{
		{name: "unset", host: "", fromNetwork: true},
		{name: "loopback", host: "127.0.0.1", fromNetwork: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			controlPort := freePort(t, "tcp")
			signallingPort := freePort(t, "tcp")
			mediaPort := freePort(t, "udp")

			cmd := exec.Command(os.Args[0], "-test.run=^TestControlHostKeepsRegistrationOffTheNetwork$")
			cmd.Env = append(os.Environ(),
				"GRYT_SFU_TEST_MAIN=1",
				fmt.Sprintf("SFU_PORT=%d", signallingPort),
				fmt.Sprintf("SFU_CONTROL_PORT=%d", controlPort),
				"SFU_CONTROL_HOST="+tt.host,
				fmt.Sprintf("ICE_UDP_MUX_PORT=%d", mediaPort),
				"SFU_METRICS_PORT=0",
				"DISABLE_STUN=true",
			)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Start(); err != nil {
				t.Fatalf("start the SFU: %v", err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			defer func() {
				_ = cmd.Process.Kill()
				<-exited
			}()

			client := &http.Client{Timeout: 2 * time.Second}
			get := func(host string, port int, path string) (int, error) {
				res, err := client.Get(fmt.Sprintf("http://%s/%s", net.JoinHostPort(host, strconv.Itoa(port)), path))
				if err != nil {
					return 0, err
				}
				res.Body.Close()
				return res.StatusCode, nil
			}

			deadline := time.After(30 * time.Second)
			for ready := false; !ready; {
				select {
				case err := <-exited:
					exited <- err
					t.Fatalf("the SFU exited before signalling came up (%v):\n%s", err, output.String())
				case <-deadline:
					t.Fatalf("signalling never came up on %d:\n%s", signallingPort, output.String())
				case <-time.After(50 * time.Millisecond):
					_, err := get("127.0.0.1", signallingPort, "health")
					ready = err == nil
				}
			}

			if _, err := get(network, signallingPort, "health"); err != nil {
				t.Fatalf("signalling did not answer on %s, and clients on the network need it: %v\n%s", network, err, output.String())
			}
			if status, err := get("127.0.0.1", controlPort, "server"); err != nil || status != http.StatusBadRequest {
				t.Fatalf("registration did not answer on 127.0.0.1 (status %d, %v), where this machine's servers dial it:\n%s", status, err, output.String())
			}

			status, err := get(network, controlPort, "server")
			switch {
			case tt.fromNetwork && (err != nil || status != http.StatusBadRequest):
				t.Fatalf("with no host set, registration did not answer on %s (status %d, %v):\n%s", network, status, err, output.String())
			case !tt.fromNetwork && err == nil:
				t.Fatalf("with SFU_CONTROL_HOST=%s, registration answered on %s with %d:\n%s", tt.host, network, status, output.String())
			}
		})
	}
}

// networkAddress is one of this machine's IPv4 addresses off loopback that it can dial. With none,
// a loopback bind looks like any other from here, so there is nothing to test.
func networkAddress(t *testing.T) string {
	t.Helper()

	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("list interfaces: %v", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil || ipNet.IP.IsLinkLocalUnicast() {
				continue
			}
			probe, err := net.Listen("tcp", net.JoinHostPort(ipNet.IP.String(), "0"))
			if err != nil {
				continue
			}
			conn, err := net.DialTimeout("tcp", probe.Addr().String(), time.Second)
			probe.Close()
			if err != nil {
				continue
			}
			conn.Close()
			return ipNet.IP.String()
		}
	}

	t.Skip("no IPv4 address off loopback that this machine can dial")
	return ""
}

func freePort(t *testing.T, network string) int {
	t.Helper()

	if network == "udp" {
		conn, err := net.ListenPacket("udp", ":0")
		if err != nil {
			t.Fatalf("find a free UDP port: %v", err)
		}
		defer conn.Close()
		return conn.LocalAddr().(*net.UDPAddr).Port
	}

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("find a free TCP port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
