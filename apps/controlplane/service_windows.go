//go:build windows

// Windows service integration, mirroring the agent's svc.rs: `service
// install` registers a LocalSystem auto-start service whose STOP/PRESHUTDOWN
// feed the same graceful shutdown as SIGTERM; real failures exit non-zero so
// the SCM's restart actions apply.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const stopWaitHintMs = 30_000

func serviceInstall() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve exe path: %w", err)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("open SCM: %w", err)
	}
	defer m.Disconnect()
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
		DisplayName:  serviceDisplay,
		Description:  "Varde control plane: accounts, scheduling and rendezvous for a Varde group.",
	}, "service", "run")
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	restart := func(d time.Duration) mgr.RecoveryAction {
		return mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: d}
	}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		restart(5 * time.Second), restart(15 * time.Second), restart(60 * time.Second),
	}, 86_400); err != nil {
		return fmt.Errorf("set failure actions: %w", err)
	}
	// also restart when we stop with a non-zero exit code
	_ = s.SetRecoveryActionsOnNonCrashFailures(true)
	if err := secureCPDir(); err != nil {
		return fmt.Errorf("restrict data dir: %w", err)
	}
	fmt.Printf("service %s installed (LocalSystem, auto-start)\n", serviceName)
	return nil
}

// cpDataDir is where the service's sqlite db and logs live by default.
func cpDataDir() string {
	return filepath.Join(os.Getenv("ProgramData"), "VardeCP")
}

// secureCPDir restricts %ProgramData%\VardeCP to SYSTEM and Administrators:
// ProgramData lets any user create files, and the db holds accounts and
// invites. Same shape as the agent's secure_data_dir.
func secureCPDir() error {
	const admins = "*S-1-5-32-544"
	dir := cpDataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	icacls := func(args ...string) error {
		out, err := exec.Command("icacls", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("icacls %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := icacls(dir, "/setowner", admins, "/T", "/C", "/Q"); err != nil {
		return err
	}
	if err := icacls(dir, "/reset", "/Q"); err != nil {
		return err
	}
	return icacls(dir, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F")
}

func serviceUninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("open SCM: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	_, _ = s.Control(svc.Stop)
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, err := s.Query()
		if err == nil && st.State == svc.Stopped {
			break
		}
		if err != nil || time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "service %s did not stop cleanly (%v); removing anyway\n", serviceName, err)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	fmt.Printf("service %s removed\n", serviceName)
	return nil
}

func serviceRun(args []string) error {
	inSvc, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !inSvc {
		return errors.New("service run is only used by the Service Control Manager")
	}
	fs, cfg := serveFlags()
	if err := fs.Parse(args); err != nil {
		return err
	}
	// a service's stdout is nowhere; log to daily files next to the db.
	w, err := dailyLog(cpLogDir(cfg))
	if err != nil {
		fmt.Fprintf(os.Stderr, "log file unavailable: %v\n", err)
	}
	if w != nil {
		defer w.Close()
	}
	return svc.Run(serviceName, &cpHandler{cfg: cfg, logOut: w})
}

func cpLogDir(cfg *serveCfg) string {
	if p := sqlitePath(cfg.dbURL); p != "" {
		return filepath.Join(filepath.Dir(p), "logs")
	}
	return filepath.Join(cpDataDir(), "logs")
}

type cpHandler struct {
	cfg    *serveCfg
	logOut io.Writer
}

func (h *cpHandler) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, h.cfg, h.logOut) }()

	changes <- svc.Status{
		State:   svc.Running,
		Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown,
	}
	var checkpoint uint32 = 1
	for {
		select {
		case err := <-errCh:
			if err != nil {
				return true, 1 // service-specific failure: SCM restart actions apply
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.PreShutdown, svc.Shutdown:
				// keep reporting progress so the SCM doesn't kill mid-shutdown
				changes <- svc.Status{
					State:      svc.StopPending,
					CheckPoint: checkpoint,
					WaitHint:   stopWaitHintMs,
				}
				checkpoint++
				cancel()
			}
		}
	}
}

// dailyLog appends to cp-<date>.log under dir, rotating at local midnight and
// pruning files older than a week — mirrors the agent's service logging.
func dailyLog(dir string) (*dailyWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &dailyWriter{dir: dir}, nil
}

type dailyWriter struct {
	mu   sync.Mutex
	dir  string
	day  string
	file *os.File
}

func (w *dailyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	if w.file == nil || w.day != today {
		if w.file != nil {
			_ = w.file.Close()
		}
		w.pruneLocked()
		f, err := os.OpenFile(filepath.Join(w.dir, "cp-"+today+".log"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return 0, err
		}
		w.file, w.day = f, today
	}
	return w.file.Write(p)
}

func (w *dailyWriter) pruneLocked() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "cp-") || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(w.dir, e.Name()))
		}
	}
}

func (w *dailyWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}
