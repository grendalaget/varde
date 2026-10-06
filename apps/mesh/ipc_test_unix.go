//go:build !windows

package main

import (
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func ipcSockPath(dir string) string { return filepath.Join(dir, "mesh.sock") }

func grpcIPCClient(sock string) (*grpc.ClientConn, error) {
	return grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
}
