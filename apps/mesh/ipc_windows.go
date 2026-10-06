//go:build windows

package main

import (
	"fmt"
	"net"

	winio "github.com/Microsoft/go-winio"
)

func listenIPC(path string) (net.Listener, error) {
	lis, err := winio.ListenPipe(path, nil)
	if err != nil {
		return nil, fmt.Errorf("listen pipe %s: %w", path, err)
	}
	return lis, nil
}
