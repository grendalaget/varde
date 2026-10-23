package mobilehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	mesh "github.com/grendalaget/varde/apps/mesh"
	sfuembed "sfu-v2/embed"
)

type service struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

var (
	mu       sync.Mutex
	services = map[string]*service{}
)

func StartSFU(envJSON string) error {
	var env map[string]string
	if err := json.Unmarshal([]byte(envJSON), &env); err != nil {
		return fmt.Errorf("decode SFU environment: %w", err)
	}
	restore, err := setEnvironment(env)
	if err != nil {
		return err
	}
	current := portFromEnvironment(env)
	s, err := launch("sfu", func(ctx context.Context) error {
		defer restore()
		return sfuembed.Run(ctx)
	})
	if err != nil {
		restore()
		return err
	}
	if err := waitFor(s, func() bool {
		client := http.Client{Timeout: 300 * time.Millisecond}
		res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", current))
		if err != nil {
			return false
		}
		defer res.Body.Close()
		return res.StatusCode == http.StatusOK
	}); err != nil {
		stopService(s)
		return fmt.Errorf("start SFU: %w", err)
	}
	return nil
}

func StartMesh(argsJSON string) error {
	var args []string
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Errorf("decode mesh arguments: %w", err)
	}
	socket, err := ipcPath(args)
	if err != nil {
		return err
	}
	s, err := launch("mesh", func(ctx context.Context) error {
		return mesh.Main(ctx, args)
	})
	if err != nil {
		return err
	}
	if err := waitFor(s, func() bool {
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}); err != nil {
		stopService(s)
		return fmt.Errorf("start mesh: %w", err)
	}
	return nil
}

func Stop() {
	mu.Lock()
	current := make([]*service, 0, len(services))
	for _, s := range services {
		current = append(current, s)
		s.cancel()
	}
	mu.Unlock()
	for _, s := range current {
		<-s.done
	}
}

func launch(name string, run func(context.Context) error) (*service, error) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := services[name]; exists {
		return nil, fmt.Errorf("%s is already running", name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &service{cancel: cancel, done: make(chan struct{})}
	services[name] = s
	go func() {
		s.err = run(ctx)
		close(s.done)
		mu.Lock()
		if services[name] == s {
			delete(services, name)
		}
		mu.Unlock()
	}()
	return s, nil
}

func waitFor(s *service, ready func() bool) error {
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready() {
			return nil
		}
		select {
		case <-s.done:
			if s.err != nil {
				return s.err
			}
			return errors.New("service stopped before becoming ready")
		case <-ticker.C:
		case <-deadline.C:
			return errors.New("timed out waiting for service readiness")
		}
	}
}

func stopService(s *service) {
	s.cancel()
	<-s.done
}

func setEnvironment(env map[string]string) (func(), error) {
	type previousValue struct {
		value string
		set   bool
	}
	previous := make(map[string]previousValue, len(env))
	restore := func() {
		for key, old := range previous {
			if old.set {
				_ = os.Setenv(key, old.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}
	for key, value := range env {
		old, set := os.LookupEnv(key)
		previous[key] = previousValue{value: old, set: set}
		if err := os.Setenv(key, value); err != nil {
			restore()
			return nil, fmt.Errorf("set %s: %w", key, err)
		}
	}
	return restore, nil
}

func portFromEnvironment(env map[string]string) int {
	port := env["SFU_PORT"]
	if port == "" {
		port = env["PORT"]
	}
	if port == "" {
		port = os.Getenv("SFU_PORT")
	}
	if port == "" {
		port = os.Getenv("PORT")
	}
	if port == "" {
		return 5005
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 5005
	}
	return n
}

func ipcPath(args []string) (string, error) {
	for i, arg := range args {
		if arg == "-ipc" || arg == "--ipc" {
			if i+1 < len(args) && args[i+1] != "" {
				return args[i+1], nil
			}
			return "", errors.New("--ipc requires a socket path")
		}
		for _, prefix := range []string{"-ipc=", "--ipc="} {
			if len(arg) > len(prefix) && arg[:len(prefix)] == prefix {
				return arg[len(prefix):], nil
			}
		}
	}
	return "", errors.New("mesh arguments must include --ipc")
}
