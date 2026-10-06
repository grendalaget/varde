// Command varde-mesh is the per-node QUIC mesh daemon. The Rust agent
// spawns and supervises it; all control happens over local IPC (UDS on
// Linux, named pipe on Windows) via MeshService.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
)

// Set by -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ipc := flag.String("ipc", "", "IPC endpoint: unix socket path (Linux) or named pipe path (Windows)")
	parentPID := flag.Int("parent-pid", 0, "exit when this spawning parent disappears (0 disables monitoring)")
	debugListen := flag.String("debug-listen", "", "optional 127.0.0.1:port for /metrics")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if *ipc == "" {
		log.Error("--ipc is required")
		os.Exit(2)
	}

	n := NewNode(log, DefaultTimings(), nil)
	defer n.Close()

	if *debugListen != "" {
		la, err := net.ResolveTCPAddr("tcp", *debugListen)
		if err != nil || !la.IP.IsLoopback() {
			log.Error("--debug-listen must be a loopback address", "addr", *debugListen)
			os.Exit(2)
		}
		mux := http.NewServeMux()
		mux.Handle("/metrics", n.metrics.handler(n))
		go func() {
			ln, err := net.ListenTCP("tcp", la)
			if err != nil {
				log.Error("debug listen failed", "error", err)
				return
			}
			_ = http.Serve(ln, mux)
		}()
	}

	lis, err := listenIPC(*ipc)
	if err != nil {
		log.Error("ipc listen failed", "ipc", *ipc, "error", err)
		os.Exit(1)
	}

	srv := grpc.NewServer()
	meshv1.RegisterMeshServiceServer(srv, newMeshServer(log, n))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parentGone := watchParent(ctx, *parentPID)
	go func() {
		select {
		case <-parentGone:
			log.Info("spawning parent exited", "parent_pid", *parentPID)
			// Active IPC streams must not prevent releasing the orphan's listeners.
			srv.Stop()
		case <-ctx.Done():
		}
	}()

	go func() {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		srv.GracefulStop()
	}()

	log.Info("mesh IPC listening", "ipc", *ipc)
	if err := srv.Serve(lis); err != nil {
		log.Error("serve failed", "error", err)
		os.Exit(1)
	}
}

func watchParent(ctx context.Context, parentPID int) <-chan struct{} {
	if parentPID <= 0 {
		return nil
	}
	gone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			// Reparenting detects exit even if the original PID has been reused.
			if os.Getppid() != parentPID {
				close(gone)
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return gone
}
