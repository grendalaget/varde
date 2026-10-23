package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// TestMetricsHostKeepsMetricsOffTheNetwork runs the SFU twice and dials this machine's own network
// address. Metrics answer there only when no host is set; registration answers both times.
func TestMetricsHostKeepsMetricsOffTheNetwork(t *testing.T) {
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
			metricsPort := freePort(t, "tcp")
			controlPort := freePort(t, "tcp")
			signallingPort := freePort(t, "tcp")
			mediaPort := freePort(t, "udp")

			cmd := exec.Command(os.Args[0], "-test.run=^TestMetricsHostKeepsMetricsOffTheNetwork$")
			cmd.Env = append(os.Environ(),
				"GRYT_SFU_TEST_MAIN=1",
				fmt.Sprintf("SFU_PORT=%d", signallingPort),
				fmt.Sprintf("SFU_CONTROL_PORT=%d", controlPort),
				"SFU_CONTROL_HOST=",
				fmt.Sprintf("SFU_METRICS_PORT=%d", metricsPort),
				"SFU_METRICS_HOST="+tt.host,
				fmt.Sprintf("ICE_UDP_MUX_PORT=%d", mediaPort),
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

			// The metrics listener starts on a goroutine of its own, so signalling being up says nothing about it.
			deadline := time.After(30 * time.Second)
			for ready := false; !ready; {
				select {
				case err := <-exited:
					exited <- err
					t.Fatalf("the SFU exited before metrics came up (%v):\n%s", err, output.String())
				case <-deadline:
					t.Fatalf("metrics never answered on 127.0.0.1:%d:\n%s", metricsPort, output.String())
				case <-time.After(50 * time.Millisecond):
					status, err := get("127.0.0.1", metricsPort, "metrics")
					ready = err == nil && status == http.StatusOK
				}
			}

			status, err := get(network, metricsPort, "metrics")
			switch {
			case tt.fromNetwork && (err != nil || status != http.StatusOK):
				t.Fatalf("with no host set, metrics did not answer on %s (status %d, %v):\n%s", network, status, err, output.String())
			case !tt.fromNetwork && err == nil:
				t.Fatalf("with SFU_METRICS_HOST=%s, metrics answered on %s with %d:\n%s", tt.host, network, status, output.String())
			}

			if status, err := get(network, controlPort, "server"); err != nil || status != http.StatusBadRequest {
				t.Fatalf("with SFU_METRICS_HOST=%q and no control host, registration did not answer on %s (status %d, %v):\n%s", tt.host, network, status, err, output.String())
			}
		})
	}
}
