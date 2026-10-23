package mesh

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
)

var version = "dev"

func Main(ctx context.Context, args []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	flags := flag.NewFlagSet("mesh", flag.ContinueOnError)
	ipc := flags.String("ipc", "", "IPC endpoint: unix socket path (Linux) or named pipe path (Windows)")
	parentPID := flags.Int("parent-pid", 0, "exit when this spawning parent disappears (0 disables monitoring)")
	debugListen := flags.String("debug-listen", "", "optional 127.0.0.1:port for /metrics")
	logLevel := flags.String("log-level", "info", "log level: debug, info, warn, error")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse mesh arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected mesh arguments: %v", flags.Args())
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	if *ipc == "" {
		return fmt.Errorf("--ipc is required")
	}

	n := NewNode(logger, DefaultTimings(), nil)
	defer n.Close()

	var debugServer *http.Server
	if *debugListen != "" {
		la, err := net.ResolveTCPAddr("tcp", *debugListen)
		if err != nil || !la.IP.IsLoopback() {
			return fmt.Errorf("--debug-listen must be a loopback address: %q", *debugListen)
		}
		ln, err := net.ListenTCP("tcp", la)
		if err != nil {
			return fmt.Errorf("debug listen: %w", err)
		}
		mux := http.NewServeMux()
		mux.Handle("/metrics", n.metrics.handler(n))
		debugServer = &http.Server{Handler: mux}
		go func() {
			if err := debugServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("debug serve failed", "error", err)
			}
		}()
		defer debugServer.Close()
	}

	lis, err := listenIPC(*ipc)
	if err != nil {
		return fmt.Errorf("ipc listen %q: %w", *ipc, err)
	}
	defer lis.Close()

	srv := grpc.NewServer()
	meshv1.RegisterMeshServiceServer(srv, newMeshServer(logger, n))
	defer srv.Stop()

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	parentGone := watchParent(serveCtx, *parentPID)
	serveResult := make(chan error, 1)
	go func() {
		logger.Info("mesh IPC listening", "ipc", *ipc)
		serveResult <- srv.Serve(lis)
	}()

	select {
	case <-serveCtx.Done():
		return nil
	case <-parentGone:
		logger.Info("spawning parent exited", "parent_pid", *parentPID)
		return nil
	case err := <-serveResult:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("serve mesh IPC: %w", err)
		}
		return nil
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
