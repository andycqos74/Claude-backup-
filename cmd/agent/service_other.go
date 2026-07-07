//go:build !windows

package main

import (
	"fmt"
	"os"
)

// maybeRunAsWindowsService is a no-op outside Windows.
func maybeRunAsWindowsService(stateDir, configPath string) bool { return false }

func cmdService(args []string) {
	fmt.Fprintln(os.Stderr, "the service subcommand is Windows-only; on Linux use the systemd unit installed by install-agent.sh")
	os.Exit(2)
}
