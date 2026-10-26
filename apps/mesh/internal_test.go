package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
)

func TestExchangeInternalLargeResponseAndErrors(t *testing.T) {
	a := newTestNode(t, "node_a")
	b := newTestNode(t, "node_b")
	a.configure(t, nil, false)
	b.configure(t, nil, false)
	a.setPeers(t, peerOf(b))
	b.setPeers(t, peerOf(a))
	waitFor(t, "direct", 10*time.Second, func() bool {
		return pathOf(t, a, "node_b") == meshv1.PathKind_PATH_KIND_DIRECT
	})

	payload := bytes.Repeat([]byte("snapshot-chunk\x00"), 400000)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(backend.Close)
	if _, err := b.cli.SetInternalServices(context.Background(), &meshv1.SetInternalServicesRequest{
		Services: []*meshv1.InternalService{{Name: "agent.chunks", TargetPort: uint32(backend.Listener.Addr().(*net.TCPAddr).Port)}},
	}); err != nil {
		t.Fatal(err)
	}
	conn := a.n.peer("node_b").current()
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		wire, err := exchangeInternalBytes(ctx, a.cli, "node_b", "agent.chunks",
			[]byte("GET /v1/chunks/example HTTP/1.1\r\nhost: x\r\nconnection: close\r\n\r\n"))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(wire)), nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || !bytes.Equal(body, payload) {
			t.Fatalf("transfer %d: received %d bytes, want %d: %v", i, len(body), len(payload), err)
		}
		if a.n.peer("node_b").current() != conn {
			t.Fatal("transfer replaced the cached peer connection")
		}
	}
	for _, tc := range []struct {
		name string
		peer string
		svc  string
		code codes.Code
	}{
		{"unknown peer", "missing", "agent.chunks", codes.NotFound},
		{"unknown service", "node_b", "missing", codes.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			data, err := exchangeInternalBytes(ctx, a.cli, tc.peer, tc.svc, nil)
			if status.Code(err) != tc.code || len(data) != 0 {
				t.Fatalf("data=%q error=%v, want no data and %v", data, err, tc.code)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if data, err := exchangeInternalBytes(ctx, a.cli, "node_b", "agent.chunks", nil); status.Code(err) != codes.Canceled || len(data) != 0 {
		t.Fatalf("canceled exchange returned data=%q error=%v", data, err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	t.Cleanup(stalled.Close)
	defer close(release)
	if _, err := b.cli.SetInternalServices(context.Background(), &meshv1.SetInternalServicesRequest{
		Services: []*meshv1.InternalService{{Name: "agent.chunks", TargetPort: uint32(stalled.Listener.Addr().(*net.TCPAddr).Port)}},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	out := &internalResponseStream{ctx: ctx}
	done := make(chan error, 1)
	go func() {
		done <- newMeshServer(testLog(), a.n).ExchangeInternal(&meshv1.ExchangeInternalRequest{
			PeerNodeId: "node_b", Name: "agent.chunks",
			Request: []byte("GET /stall HTTP/1.1\r\nhost: x\r\nconnection: close\r\n\r\n"),
		}, out)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("internal request never reached the peer")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || out.sent != 0 {
			t.Fatalf("canceled exchange returned error=%v, sent=%d", err, out.sent)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation left an internal stream blocked")
	}
}

type internalResponseStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent int
}

func (s *internalResponseStream) Context() context.Context { return s.ctx }

func (s *internalResponseStream) Send(*meshv1.ExchangeInternalResponse) error {
	s.sent++
	return nil
}

func exchangeInternalBytes(ctx context.Context, client meshv1.MeshServiceClient, peer, name string, request []byte) ([]byte, error) {
	stream, err := client.ExchangeInternal(ctx, &meshv1.ExchangeInternalRequest{
		PeerNodeId: peer, Name: name, Request: request,
	})
	if err != nil {
		return nil, err
	}
	var data []byte
	for {
		part, err := stream.Recv()
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return data, err
		}
		data = append(data, part.GetData()...)
	}
}
