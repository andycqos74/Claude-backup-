package agent

import (
	"context"
	"runtime"
	"testing"
	"time"

	"centralbackup/internal/proto"
)

func TestBuildShellCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		if _, err := buildShellCommand(proto.ShellPowerShell, "echo hi"); err != nil {
			t.Errorf("powershell should be allowed on Windows: %v", err)
		}
		if _, err := buildShellCommand(proto.ShellSh, "ls"); err == nil {
			t.Error("sh should be rejected on Windows")
		}
	} else {
		if _, err := buildShellCommand(proto.ShellSh, "echo hi"); err != nil {
			t.Errorf("sh should be allowed off Windows: %v", err)
		}
		if _, err := buildShellCommand(proto.ShellPowerShell, "echo hi"); err == nil {
			t.Error("powershell should be rejected off Windows")
		}
		if _, err := buildShellCommand(proto.ShellCmd, "dir"); err == nil {
			t.Error("cmd should be rejected off Windows")
		}
	}
	if _, err := buildShellCommand("bash", "echo"); err == nil {
		t.Error("unknown shell should be rejected")
	}
}

// execCommand streams output through a.send, which is a no-op without a
// connection, so these tests exercise the exit-code and cancellation paths.
func TestExecCommandExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	a := &Agent{}
	code, err := a.execCommand(context.Background(),
		proto.RunCommand{CommandID: "x", Shell: proto.ShellSh, Command: "echo hi; exit 7"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
}

func TestExecCommandZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	a := &Agent{}
	code, err := a.execCommand(context.Background(),
		proto.RunCommand{CommandID: "x", Shell: proto.ShellSh, Command: "true"})
	if err != nil || code != 0 {
		t.Errorf("code=%d err=%v, want 0/nil", code, err)
	}
}

func TestExecCommandCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	a := &Agent{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := a.execCommand(ctx,
		proto.RunCommand{CommandID: "x", Shell: proto.ShellSh, Command: "sleep 10"})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("cancel took too long: %s", elapsed)
	}
	if err != nil {
		t.Errorf("cancelled command should not report a launch error: %v", err)
	}
	if ctx.Err() != context.Canceled {
		t.Errorf("ctx should be cancelled")
	}
}
