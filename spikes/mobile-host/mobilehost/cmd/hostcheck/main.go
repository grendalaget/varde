package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"mobilehost"
)

func main() {
	socket := "/tmp/mobilehost-mesh.sock"
	if len(os.Args) > 1 {
		socket = os.Args[1]
	}
	env, _ := json.Marshal(map[string]string{
		"SFU_PORT":               "18505",
		"SFU_CONTROL_PORT":       "18506",
		"SFU_CONTROL_HOST":       "127.0.0.1",
		"SFU_METRICS_PORT":       "0",
		"ICE_UDP_MUX_PORT":       "3478",
		"DISABLE_STUN":           "true",
		"SFU_CALL_ALONE_TIMEOUT": "0",
	})
	args, _ := json.Marshal([]string{"--ipc", socket})
	if err := mobilehost.StartSFU(string(env)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := mobilehost.StartMesh(string(args)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		mobilehost.Stop()
		os.Exit(1)
	}
	fmt.Printf("SFU ready at http://127.0.0.1:18505/health; mesh IPC listening at %s\n", socket)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	mobilehost.Stop()
}
