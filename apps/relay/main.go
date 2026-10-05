// Command p2pgames-relay forwards opaque encrypted UDP payloads between
// mesh nodes that cannot establish a direct path. This is a stub: it binds
// the socket and logs datagrams; registration and forwarding land in
// milestone 3.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	addr := flag.String("addr", envOr("P2PGAMES_ADDR", ":7777"), "UDP listen address")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	conn, err := net.ListenUDP("udp", mustResolveUDPAddr(*addr))
	if err != nil {
		log.Error("listen failed", "addr", *addr, "error", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	log.Info("relay listening", "addr", conn.LocalAddr().String())

	buf := make([]byte, 64*1024)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("read failed", "error", err)
			os.Exit(1)
		}
		log.Debug("datagram", "src", src.String(), "bytes", n)
	}
}

func mustResolveUDPAddr(addr string) *net.UDPAddr {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		slog.Error("invalid addr", "addr", addr, "error", err)
		os.Exit(2)
	}
	return ua
}
