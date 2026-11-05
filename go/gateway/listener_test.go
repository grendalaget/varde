package gateway

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func idlePipe(t *testing.T, timeout time.Duration) (*readIdleConn, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	return &readIdleConn{Conn: server, timeout: timeout}, client
}

func TestReadIdleTimeoutAllowsProgress(t *testing.T) {
	conn, client := idlePipe(t, 200*time.Millisecond)
	done := make(chan error, 1)
	go func() {
		for range 5 {
			time.Sleep(60 * time.Millisecond)
			if _, err := client.Write([]byte("x")); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("progressing upload = %v, want success beyond one idle interval", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReadIdleTimeoutRejectsStallsAndPreservesHeaderDeadline(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "stalled body", true: "header deadline"}[explicit], func(t *testing.T) {
			conn, _ := idlePipe(t, 200*time.Millisecond)
			if explicit {
				if err := conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { _, err := conn.Read(make([]byte, 1)); done <- err }()
			wait := time.Second
			if explicit {
				wait = 150 * time.Millisecond
			}
			select {
			case err := <-done:
				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatalf("read error = %v, want timeout", err)
				}
			case <-time.After(wait):
				t.Fatal("read did not respect its deadline")
			}
		})
	}
}

func TestReadIdleTimeoutListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if got := WithReadIdleTimeout(listener, 0); got != listener {
		t.Error("disabled idle timeout changed listener")
	}
	wrapped := WithReadIdleTimeout(listener, time.Second)
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	conn, err := wrapped.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := conn.(*readIdleConn); !ok {
		t.Errorf("accepted connection = %T, want *readIdleConn", conn)
	}
	_ = conn.Close()
	_ = wrapped.Close()
	if _, err := wrapped.Accept(); err == nil {
		t.Error("closed listener accepted a connection")
	}
}
