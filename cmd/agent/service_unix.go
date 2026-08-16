//go:build !windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Service management for Linux (systemd) and macOS (launchd), so the agent
// binary can install itself as a background service the same way it already
// could on Windows. Before this, Linux needed install-agent.sh — and that
// bootstrap script, not the agent, was where installs went wrong.

// maybeRunAsWindowsService is a no-op outside Windows.
func maybeRunAsWindowsService(stateDir, configPath string) bool { return false }

const (
	systemdUnit  = "/etc/systemd/system/backup-agent.service"
	launchdPlist = "/Library/LaunchDaemons/com.centralbackup.agent.plist"
	launchdLabel = "com.centralbackup.agent"
)

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

func requireRoot() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("this must be run as root (try sudo)")
	}
	return nil
}

// installService writes the platform's service definition and starts it.
func installService(stateDir, configPath string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)

	// Run from a stable location: a service pointing at a binary in a temp
	// or download directory breaks the moment that file is tidied away.
	installed := "/usr/local/bin/backup-agent"
	if exe != installed {
		if err := copyExecutable(exe, installed); err != nil {
			return fmt.Errorf("install binary to %s: %w", installed, err)
		}
		exe = installed
	}

	if runtime.GOOS == "darwin" {
		return installLaunchd(exe, stateDir, configPath)
	}
	return installSystemd(exe, stateDir, configPath)
}

func installSystemd(exe, stateDir, configPath string) error {
	unit := fmt.Sprintf(`[Unit]
Description=Central Backup Agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s run --state-dir %s --config %s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, exe, stateDir, configPath)

	if err := os.WriteFile(systemdUnit, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := run("systemctl", "enable", "--now", "backup-agent"); err != nil {
		return err
	}
	fmt.Println("service installed and started (systemctl status backup-agent)")
	return nil
}

func installLaunchd(exe, stateDir, configPath string) error {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string><string>run</string>
    <string>--state-dir</string><string>%s</string>
    <string>--config</string><string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict>
</plist>
`, launchdLabel, exe, stateDir, configPath)

	if err := os.WriteFile(launchdPlist, []byte(plist), 0o644); err != nil {
		return err
	}
	// bootstrap is the modern verb; fall back to load on older macOS.
	if err := run("launchctl", "bootstrap", "system", launchdPlist); err != nil {
		if err := run("launchctl", "load", "-w", launchdPlist); err != nil {
			return err
		}
	}
	fmt.Println("service installed and started (launchctl print system/" + launchdLabel + ")")
	return nil
}

func uninstallService() error {
	if err := requireRoot(); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		run("launchctl", "bootout", "system/"+launchdLabel)
		run("launchctl", "unload", launchdPlist)
		os.Remove(launchdPlist)
	} else {
		run("systemctl", "disable", "--now", "backup-agent")
		os.Remove(systemdUnit)
		run("systemctl", "daemon-reload")
	}
	fmt.Println("service uninstalled (credentials and jobs kept)")
	return nil
}

func controlService(action string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		verb := map[string]string{"start": "kickstart", "stop": "kill"}[action]
		return run("launchctl", verb, "system/"+launchdLabel)
	}
	return run("systemctl", action, "backup-agent")
}

// copyExecutable installs the binary at dst, replacing any previous copy.
// The old file is removed rather than truncated: overwriting a running
// binary in place fails with ETXTBSY, which is exactly the reinstall case.
func copyExecutable(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	os.Remove(dst)
	return os.WriteFile(dst, data, 0o755)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return nil
}
