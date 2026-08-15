//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"centralbackup/internal/agent"
)

const svcName = "CentralBackupAgent"

// maybeRunAsWindowsService detects being launched by the service control
// manager and runs under it. Returns true if it handled execution.
func maybeRunAsWindowsService(stateDir, configPath string) bool {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return false
	}
	elog, _ := eventlog.Open(svcName)
	if elog != nil {
		defer elog.Close()
	}
	run := func() error {
		return svc.Run(svcName, &agentService{stateDir: stateDir, configPath: configPath})
	}
	if err := run(); err != nil && elog != nil {
		elog.Error(1, fmt.Sprintf("service failed: %v", err))
	}
	return true
}

type agentService struct {
	stateDir   string
	configPath string
}

func (s *agentService) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		a, err := agent.New(s.stateDir, s.configPath)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- a.Run(ctx)
	}()

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-errCh:
				case <-time.After(15 * time.Second):
				}
				return false, 0
			}
		case err := <-errCh:
			if err != nil && ctx.Err() == nil {
				return true, 1
			}
			return false, 0
		}
	}
}

func cmdService(args []string) {
	if len(args) < 1 {
		usage()
	}
	switch args[0] {
	case "install":
		if err := installService(defaultStateDir(), defaultConfigPath(defaultStateDir())); err != nil {
			log.Fatal(err)
		}
	case "uninstall":
		if err := uninstallService(); err != nil {
			log.Fatal(err)
		}
	case "start", "stop":
		if err := controlService(args[0]); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
	}
}

// installService registers the agent as an automatically started Windows
// service. stateDir and configPath are accepted for parity with the Unix
// implementation; the service reads them from their defaults (or the
// CB_STATE_DIR / CB_CONFIG environment) at run time.
func installService(stateDir, configPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(svcName); err == nil {
		s.Close()
		return fmt.Errorf("service %s is already installed", svcName)
	}
	s, err := m.CreateService(svcName, exe, mgr.Config{
		DisplayName: "Central Backup Agent",
		Description: "Backs up this machine to the central backup server.",
		StartType:   mgr.StartAutomatic,
	}, "run", "--state-dir", stateDir, "--config", configPath)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	// Restart on failure, so a crash or a killed process doesn't silently
	// leave the machine unprotected until someone notices.
	s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400)

	eventlog.InstallAsEventCreate(svcName, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err := s.Start(); err != nil {
		return fmt.Errorf("service installed but failed to start: %w", err)
	}
	fmt.Println("service installed and started")
	return nil
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(svcName)
	if err != nil {
		return fmt.Errorf("service not installed: %w", err)
	}
	defer s.Close()
	s.Control(svc.Stop)
	if err := s.Delete(); err != nil {
		return err
	}
	eventlog.Remove(svcName)
	fmt.Println("service uninstalled (credentials and jobs kept)")
	return nil
}

func controlService(action string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(svcName)
	if err != nil {
		return fmt.Errorf("service not installed: %w", err)
	}
	defer s.Close()
	if action == "start" {
		err = s.Start()
	} else {
		_, err = s.Control(svc.Stop)
	}
	if err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}
