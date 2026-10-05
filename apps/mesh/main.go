// Command p2pgames-mesh is the per-node QUIC mesh daemon. The Rust agent
// spawns and supervises it; all control happens over local IPC (UDS on
// Linux, named pipe on Windows) via MeshService.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	meshv1 "github.com/arnemolland/p2pgames/go/gen/mesh/v1"
)

func main() {
	ipc := flag.String("ipc", "", "IPC endpoint: unix socket path (Linux) or named pipe path (Windows)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if *ipc == "" {
		log.Error("--ipc is required")
		os.Exit(2)
	}

	lis, err := listenIPC(*ipc)
	if err != nil {
		log.Error("ipc listen failed", "ipc", *ipc, "error", err)
		os.Exit(1)
	}

	srv := grpc.NewServer()
	meshv1.RegisterMeshServiceServer(srv, newMeshServer(log))

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
