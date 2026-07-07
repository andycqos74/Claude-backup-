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
		exe, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		exe, _ = filepath.Abs(exe)
		m, err := mgr.Connect()
		if err != nil {
			log.Fatalf("connect to service manager (run as Administrator): %v", err)
		}
		defer m.Disconnect()
		if s, err := m.OpenService(svcName); err == nil {
			s.Close()
			log.Fatalf("service %s already installed", svcName)
		}
		s, err := m.CreateService(svcName, exe, mgr.Config{
			DisplayName: "Central Backup Agent",
			Description: "Backs up this machine to the central backup server.",
			StartType:   mgr.StartAutomatic,
		}, "run")
		if err != nil {
			log.Fatalf("create service: %v", err)
		}
		defer s.Close()
		eventlog.InstallAsEventCreate(svcName, eventlog.Error|eventlog.Warning|eventlog.Info)
		if err := s.Start(); err != nil {
			log.Printf("service installed but failed to start: %v", err)
		} else {
			fmt.Println("service installed and started")
		}
	case "uninstall":
		m, err := mgr.Connect()
		if err != nil {
			log.Fatal(err)
		}
		defer m.Disconnect()
		s, err := m.OpenService(svcName)
		if err != nil {
			log.Fatalf("service not installed: %v", err)
		}
		defer s.Close()
		s.Control(svc.Stop)
		if err := s.Delete(); err != nil {
			log.Fatal(err)
		}
		eventlog.Remove(svcName)
		fmt.Println("service uninstalled")
	case "start", "stop":
		m, err := mgr.Connect()
		if err != nil {
			log.Fatal(err)
		}
		defer m.Disconnect()
		s, err := m.OpenService(svcName)
		if err != nil {
			log.Fatal(err)
		}
		defer s.Close()
		if args[0] == "start" {
			err = s.Start()
		} else {
			_, err = s.Control(svc.Stop)
		}
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("ok")
	default:
		usage()
	}
}
