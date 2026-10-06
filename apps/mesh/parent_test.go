//go:build !windows

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
	"github.com/grendalaget/varde/go/identity"
)

func TestParentLifetimeHelper(t *testing.T) {
	switch os.Getenv("VARDE_MESH_TEST_HELPER") {
	case "parent":
		mesh := exec.Command(os.Args[0], "-test.run=^TestParentLifetimeHelper$")
		mesh.Env = append(os.Environ(), "VARDE_MESH_TEST_HELPER=mesh",
			"VARDE_MESH_TEST_PARENT="+strconv.Itoa(os.Getpid()))
		mesh.Stdout, mesh.Stderr = os.Stderr, os.Stderr
		if err := mesh.Start(); err != nil {
			t.Fatal(err)
		}
		fmt.Println(mesh.Process.Pid)
		if err := mesh.Wait(); err != nil {
			t.Fatal(err)
		}
	case "mesh":
		flag.CommandLine = flag.NewFlagSet("mesh", flag.ExitOnError)
		os.Args = []string{"mesh", "--ipc", os.Getenv("VARDE_MESH_TEST_IPC"),
			"--parent-pid", os.Getenv("VARDE_MESH_TEST_PARENT")}
		main()
		os.Exit(0)
	}
}

func TestParentWatchDisabled(t *testing.T) {
	if watchParent(context.Background(), 0) != nil {
		t.Fatal("standalone mesh should not monitor a parent")
	}
}

func TestParentWatchAlreadyGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	select {
	case <-watchParent(ctx, os.Getppid()+1):
	case <-time.After(3 * time.Second):
		t.Fatal("parent lost before mesh startup was not detected")
	}
}

func TestParentDeathReleasesConfiguredPorts(t *testing.T) {
	// macOS's default temp directory can exceed the Unix socket path limit.
	dir, err := os.MkdirTemp("/tmp", "varde-parent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	_, key, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "node.key")
	if err := identity.SavePrivateKey(keyPath, key); err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv6unspecified})
	if err != nil {
		t.Fatal(err)
	}
	meshPort := udp.LocalAddr().(*net.UDPAddr).Port
	_ = udp.Close()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	routePort := tcp.Addr().(*net.TCPAddr).Port
	_ = tcp.Close()
	ipc := ipcSockPath(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	startParent := func() (*exec.Cmd, meshv1.MeshServiceClient) {
		parent := exec.Command(os.Args[0], "-test.run=^TestParentLifetimeHelper$")
		parent.Env = append(os.Environ(), "VARDE_MESH_TEST_HELPER=parent", "VARDE_MESH_TEST_IPC="+ipc)
		parent.Stderr = os.Stderr
		stdout, err := parent.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := parent.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = parent.Process.Kill(); _ = parent.Wait() })
		pid := make(chan int, 1)
		go func() {
			line, _ := bufio.NewReader(stdout).ReadString('\n')
			number, _ := strconv.Atoi(strings.TrimSpace(line))
			pid <- number
		}()
		select {
		case number := <-pid:
			if number <= 0 {
				t.Fatal("parent helper did not report its mesh PID")
			}
			mesh, err := os.FindProcess(number)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = mesh.Kill(); _ = mesh.Release() })
		case <-ctx.Done():
			t.Fatal("parent helper did not start")
		}
		conn, err := grpcIPCClient(ipc)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		cli := meshv1.NewMeshServiceClient(conn)
		waitFor(t, "mesh configure", 5*time.Second, func() bool {
			_, err := cli.Configure(ctx, &meshv1.ConfigureRequest{
				NodeId: "parent-test", IdentityKeyPath: keyPath, ListenPort: uint32(meshPort),
			})
			return err == nil
		})
		status, err := cli.GetStatus(ctx, &meshv1.GetStatusRequest{})
		if err != nil {
			t.Fatal(err)
		}
		_, boundPort, err := net.SplitHostPort(status.ListenAddrs[len(status.ListenAddrs)-1])
		if err != nil || boundPort != strconv.Itoa(meshPort) {
			t.Fatalf("configured port was not reclaimed: %v", status.ListenAddrs)
		}
		routes, err := cli.SetRoutes(ctx, &meshv1.SetRoutesRequest{Routes: []*meshv1.ServiceRoute{{
			ServiceId: "parent-test", HostNodeId: "remote", LoopbackIp: "127.0.0.1",
			Ports: []*meshv1.PortSpec{
				{Port: uint32(routePort), Protocol: meshv1.Protocol_PROTOCOL_TCP},
				{Port: uint32(routePort), Protocol: meshv1.Protocol_PROTOCOL_UDP},
			},
		}}})
		if err != nil || len(routes.Errors) != 0 {
			t.Fatalf("route listeners were not reclaimed: %v, %v", routes, err)
		}
		return parent, cli
	}
	parent, cli := startParent()
	stream, err := cli.WatchEvents(ctx, &meshv1.WatchEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { _, err := stream.Recv(); closed <- err }()
	select {
	case err := <-closed:
		t.Fatalf("mesh exited while its parent was alive: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("event stream stayed open")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mesh outlived its killed parent with an active IPC stream")
	}
	waitFor(t, "configured UDP port released", 5*time.Second, func() bool {
		socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv6unspecified, Port: meshPort})
		if err != nil {
			return false
		}
		_ = socket.Close()
		return true
	})
	_, _ = startParent()
}
