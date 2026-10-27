package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

type runningCommand struct {
	cmd        *exec.Cmd
	waitResult chan error
}

func (s *Supervisor) startInstalled(row store.InstalledPlugin, wait bool) error {
	if s.opts.Leader != nil && !s.opts.Leader.IsLeader() {
		return nil
	}
	s.mu.Lock()
	if process := s.running[row.ID]; process != nil {
		s.mu.Unlock()
		if !wait {
			return nil
		}
		if process.healthy.Load() {
			return nil
		}
		select {
		case <-process.ready:
			if process.healthy.Load() {
				return nil
			}
			return waitForHealthy(process, s.opts.HealthTimeout+time.Second)
		case <-time.After(s.opts.HealthTimeout + time.Second):
			return errors.New("timed out waiting for plugin process readiness")
		}
	}
	parent := s.rootCtx
	if parent == nil {
		parent = context.Background()
	}
	runCtx, cancel := context.WithCancel(parent)
	process := &managedProcess{
		cancel: cancel,
		done:   make(chan struct{}),
		ready:  make(chan struct{}),
	}
	s.running[row.ID] = process
	s.mu.Unlock()

	go s.runProcessLoop(runCtx, row, process, nil)
	if !wait {
		return nil
	}
	select {
	case <-process.ready:
		return process.readyErr
	case <-time.After(s.opts.HealthTimeout + time.Second):
		return errors.New("timed out waiting for plugin process readiness")
	}
}

func (s *Supervisor) runProcessLoop(ctx context.Context, row store.InstalledPlugin, process *managedProcess, initial *runningCommand) {
	defer close(process.done)
	defer func() {
		s.mu.Lock()
		if s.running[row.ID] == process {
			delete(s.running, row.ID)
		}
		s.mu.Unlock()
	}()
	entry, ok := s.opts.Catalog.Get(row.ID)
	if !ok {
		s.signalReady(process, fmt.Errorf("plugin %s is missing from catalog", row.ID))
		_ = s.opts.Store.UpdateInstalledPluginStatus(context.Background(), row.ID, errorStatus, "plugin missing from catalog")
		return
	}
	asset, err := s.assetFor(entry)
	if err != nil {
		s.signalReady(process, err)
		_ = s.opts.Store.UpdateInstalledPluginStatus(context.Background(), row.ID, errorStatus, err.Error())
		return
	}
	_, binaryPath, err := s.loadOrDownload(ctx, entry, asset)
	if err != nil {
		s.signalReady(process, err)
		_ = s.opts.Store.UpdateInstalledPluginStatus(context.Background(), row.ID, errorStatus, err.Error())
		return
	}
	raw, err := s.opts.Secrets.Open(row.SealedSecret, credentialAAD+row.ID)
	if err != nil {
		s.signalReady(process, err)
		_ = s.opts.Store.UpdateInstalledPluginStatus(context.Background(), row.ID, errorStatus, err.Error())
		return
	}
	var creds credentials
	if err := jsonUnmarshalCredentials(raw, &creds); err != nil {
		s.signalReady(process, err)
		_ = s.opts.Store.UpdateInstalledPluginStatus(context.Background(), row.ID, errorStatus, err.Error())
		return
	}
	stateDir := filepath.Join(s.pluginDir(row.ID), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		s.signalReady(process, err)
		_ = s.opts.Store.UpdateInstalledPluginStatus(context.Background(), row.ID, errorStatus, err.Error())
		return
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		s.signalReady(process, err)
		_ = s.opts.Store.UpdateInstalledPluginStatus(context.Background(), row.ID, errorStatus, err.Error())
		return
	}

	backoff := s.opts.RestartMin
	for {
		if ctx.Err() != nil {
			s.markStopped(row.ID, process)
			s.signalReady(process, ctx.Err())
			return
		}
		listen := netJoinHostPort("127.0.0.1", int(row.Port))
		var cmd *exec.Cmd
		var waitResult chan error
		if initial != nil {
			cmd = initial.cmd
			waitResult = initial.waitResult
			initial = nil
		} else {
			cmd = exec.CommandContext(ctx, binaryPath)
		}
		cmd.Dir = stateDir
		cmd.Env = pluginEnvironment(s.opts.ControlPlaneURL, listen, stateDir, creds)
		if cmd.Process == nil {
			if err := cmd.Start(); err != nil {
				message := fmt.Errorf("start plugin process: %w", err)
				_ = s.opts.Store.UpdateInstalledPluginStatus(context.Background(), row.ID, errorStatus, message.Error())
				s.signalReady(process, message)
				if !waitBackoff(ctx, backoff) {
					return
				}
				backoff = nextBackoff(backoff, s.opts.RestartMax)
				continue
			}
		}
		if waitResult == nil {
			waitResult = make(chan error, 1)
			go func() { waitResult <- cmd.Wait() }()
		}
		healthy, exited, healthErr := s.waitHealthy(ctx, int(row.Port), waitResult)
		if !healthy {
			if !exited && cmd.Process != nil {
				_ = cmd.Process.Kill()
				healthErr = errors.Join(healthErr, <-waitResult)
			}
			if ctx.Err() != nil {
				s.markStopped(row.ID, process)
				s.signalReady(process, ctx.Err())
				return
			}
			if healthErr == nil {
				healthErr = errors.New("plugin did not become healthy")
			}
			_ = s.opts.Store.UpdateInstalledPluginStatus(context.Background(), row.ID, errorStatus, healthErr.Error())
			s.signalReady(process, healthErr)
			if !waitBackoff(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, s.opts.RestartMax)
			continue
		}
		_ = s.opts.Store.UpdateInstalledPluginStatus(context.Background(), row.ID, runningStatus, "")
		process.healthy.Store(true)
		s.signalReady(process, nil)
		err := waitProcess(ctx, cmd, waitResult)
		process.healthy.Store(false)
		if ctx.Err() != nil {
			s.markStopped(row.ID, process)
			return
		}
		if err == nil {
			err = errors.New("plugin process exited")
		}
		_ = s.opts.Store.UpdateInstalledPluginStatus(context.Background(), row.ID, errorStatus, err.Error())
		s.opts.Log.Warn("plugin process exited; restarting", "plugin", row.ID, "error", err, "backoff", backoff)
		if !waitBackoff(ctx, backoff) {
			s.markStopped(row.ID, process)
			return
		}
		backoff = nextBackoff(backoff, s.opts.RestartMax)
	}
}

func (s *Supervisor) markStopped(id string, process *managedProcess) {
	if !process.retired.Load() {
		_ = s.opts.Store.UpdateInstalledPluginStatus(context.Background(), id, stoppedStatus, "")
	}
}

func (s *Supervisor) waitHealthy(ctx context.Context, port int, waitResult <-chan error) (bool, bool, error) {
	deadline := time.NewTimer(s.opts.HealthTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/healthz"
	client := &http.Client{Timeout: time.Second}
	for {
		select {
		case err := <-waitResult:
			if err == nil {
				err = errors.New("plugin process exited before becoming healthy")
			}
			return false, true, err
		case <-deadline.C:
			return false, false, errors.New("plugin health check timed out")
		case <-ctx.Done():
			return false, false, ctx.Err()
		case <-ticker.C:
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return false, false, err
			}
			response, err := client.Do(request)
			if err != nil {
				continue
			}
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return true, false, nil
			}
		}
	}
}

func waitProcess(ctx context.Context, cmd *exec.Cmd, waitResult <-chan error) error {
	select {
	case err := <-waitResult:
		return err
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return <-waitResult
	}
}

func (s *Supervisor) signalReady(process *managedProcess, err error) {
	process.once.Do(func() {
		process.readyErr = err
		close(process.ready)
	})
}

func waitForHealthy(process *managedProcess, timeout time.Duration) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if process.healthy.Load() {
			return nil
		}
		select {
		case <-process.done:
			return errors.New("plugin process stopped before becoming healthy")
		case <-timer.C:
			return errors.New("timed out waiting for plugin process readiness")
		case <-ticker.C:
		}
	}
}

func pluginEnvironment(controlPlaneURL, listen, stateDir string, creds credentials) []string {
	env := []string{
		"LISTEN=" + listen,
		"STATE_DIR=" + stateDir,
		"VARDE_PLUGIN_SECRET=" + creds.PluginSecret,
		"VARDE_URL=" + controlPlaneURL,
		"VARDE_SERVICE_TOKEN=" + creds.ServiceToken,
	}
	if runtime.GOOS == "windows" {
		for _, key := range []string{"SystemRoot", "WINDIR", "TEMP", "TMP", "PATH"} {
			if value := os.Getenv(key); value != "" {
				env = append(env, key+"="+value)
			}
		}
	}
	return env
}

func waitBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum/2 {
		return maximum
	}
	return current * 2
}

func (s *Supervisor) stopInstalled(id string) {
	s.mu.Lock()
	process := s.running[id]
	if process != nil {
		process.cancel()
	}
	s.mu.Unlock()
	if process != nil {
		<-process.done
	}
}

func (s *Supervisor) stopAll() {
	s.mu.Lock()
	processes := make([]*managedProcess, 0, len(s.running))
	for _, process := range s.running {
		process.cancel()
		processes = append(processes, process)
	}
	s.mu.Unlock()
	for _, process := range processes {
		<-process.done
	}
}

func netJoinHostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func jsonUnmarshalCredentials(raw string, creds *credentials) error {
	if err := json.Unmarshal([]byte(raw), creds); err != nil {
		return fmt.Errorf("decode plugin credentials: %w", err)
	}
	if creds.PluginSecret == "" || creds.ServiceToken == "" || creds.ServiceTokenName == "" {
		return errors.New("stored plugin credentials are incomplete")
	}
	return nil
}
