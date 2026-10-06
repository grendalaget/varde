//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"

	winio "github.com/Microsoft/go-winio"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func ipcSockPath(_ string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return `\\.\pipe\varde-mesh-test-` + hex.EncodeToString(b[:])
}

func grpcIPCClient(sock string) (*grpc.ClientConn, error) {
	return grpc.NewClient(
		"passthrough:///"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return winio.DialPipeContext(ctx, sock)
		}),
	)
}
