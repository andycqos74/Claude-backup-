//go:build windows

package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"centralbackup/internal/proto"
)

// schedRunPowerShell runs a script via -EncodedCommand (base64 UTF-16LE).
// Passing the script this way, rather than on stdin via `-Command -`, is
// reliable from a non-interactive service process, where `-NonInteractive`
// can decline to read stdin and leave the script silently unrun.
func schedRunPowerShell(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodePowerShellCommand(script))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("%s", firstLine(msg))
	}
	return stdout.Bytes(), nil
}

func schedRunInventory(ctx context.Context) proto.SchedInventory {
	out, err := schedRunPowerShell(ctx, schedInventoryScript)
	if err != nil {
		return proto.SchedInventory{Available: false, Error: err.Error()}
	}
	tasks, err := parseSchedInventory(out)
	if err != nil {
		return proto.SchedInventory{Available: false, Error: err.Error()}
	}
	return proto.SchedInventory{Available: true, Tasks: tasks}
}

func schedRunScript(ctx context.Context, script string) error {
	_, err := schedRunPowerShell(ctx, script)
	return err
}

// firstLine keeps PowerShell's often-multiline error output to its first,
// most relevant line for display in the GUI.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
