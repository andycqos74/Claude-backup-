package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"

	"centralbackup/internal/proto"
)

// Remote command console. The server sends a shell command; the agent runs it
// in its background service (as LocalSystem or root) and streams stdout/stderr
// back line by line. Cancellation kills the process.

const (
	// maxCommandLines bounds how much output one command may stream, so a
	// runaway command (e.g. `dir /s` on a huge disk) can't flood the server.
	maxCommandLines = 5000
	// maxCommandLineLen truncates a single very long line.
	maxCommandLineLen = 8 << 10
)

func (a *Agent) runCommand(cmd proto.RunCommand) {
	ctx, cancel := context.WithCancel(context.Background())
	a.cmdMu.Lock()
	a.cmds[cmd.CommandID] = cancel
	a.cmdMu.Unlock()
	defer func() {
		cancel()
		a.cmdMu.Lock()
		delete(a.cmds, cmd.CommandID)
		a.cmdMu.Unlock()
	}()

	exitCode, err := a.execCommand(ctx, cmd)
	done := proto.CommandDone{CommandID: cmd.CommandID, ExitCode: exitCode}
	switch {
	case ctx.Err() != nil:
		done.Error = proto.CommandCancelled
		done.ExitCode = -1
		log.Printf("[command %s] cancelled", cmd.CommandID)
	case err != nil:
		done.Error = err.Error()
		if exitCode == 0 {
			done.ExitCode = -1
		}
		log.Printf("[command %s] error: %v", cmd.CommandID, err)
	default:
		log.Printf("[command %s] exit %d", cmd.CommandID, exitCode)
	}
	if err := a.send(proto.MsgCommandDone, done); err != nil {
		log.Printf("[command %s] could not report completion: %v", cmd.CommandID, err)
	}
}

func (a *Agent) execCommand(ctx context.Context, rc proto.RunCommand) (int, error) {
	c, err := buildShellCommand(rc.Shell, rc.Command)
	if err != nil {
		return -1, err
	}
	// Run in its own process group so cancellation can kill the shell *and*
	// its children — otherwise a child (e.g. the `sleep` in `sh -c "sleep"`)
	// keeps the output pipes open and the read below blocks until it exits.
	setProcGroup(c)

	stdout, err := c.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := c.Start(); err != nil {
		return -1, fmt.Errorf("could not start %s: %w", rc.Shell, err)
	}

	// Kill the whole process tree when the operator cancels.
	stopKiller := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killProcessTree(c)
		case <-stopKiller:
		}
	}()

	var lineCount int64
	var wg sync.WaitGroup
	wg.Add(2)
	go a.streamOutput(rc.CommandID, "stdout", stdout, &lineCount, &wg)
	go a.streamOutput(rc.CommandID, "stderr", stderr, &lineCount, &wg)
	wg.Wait() // both pipes reach EOF once the process tree is gone

	err = c.Wait()
	close(stopKiller)
	if ctx.Err() != nil {
		return -1, nil // cancelled; runCommand reports it from ctx
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil // non-zero exit is not a launch error
		}
		return -1, err
	}
	return 0, nil
}

// streamOutput scans one pipe line by line, forwarding each line to the
// server until the shared line budget is exhausted.
func (a *Agent) streamOutput(commandID, stream string, r io.Reader, count *int64, wg *sync.WaitGroup) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4<<10), maxCommandLineLen)
	for sc.Scan() {
		n := atomic.AddInt64(count, 1)
		if n == maxCommandLines+1 {
			a.send(proto.MsgCommandOutput, proto.CommandOutput{
				CommandID: commandID, Stream: "stderr",
				Line: fmt.Sprintf("… output truncated after %d lines …", maxCommandLines),
			})
		}
		if n > maxCommandLines {
			// Keep draining the pipe so the process never blocks on a full
			// stdout buffer, but stop forwarding.
			continue
		}
		a.send(proto.MsgCommandOutput, proto.CommandOutput{
			CommandID: commandID, Stream: stream, Line: sc.Text(),
		})
	}
}

// buildShellCommand wraps the command string in the requested shell. The
// command is passed as a single argument, never re-quoted into another
// shell, so the operator's quoting is preserved exactly. Cancellation is
// handled by the caller (process-group kill), not exec.CommandContext, so a
// forked child is killed too.
func buildShellCommand(shell, command string) (*exec.Cmd, error) {
	switch shell {
	case proto.ShellPowerShell:
		if runtime.GOOS != "windows" {
			return nil, fmt.Errorf("powershell is only available on Windows clients")
		}
		return exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", command), nil
	case proto.ShellCmd:
		if runtime.GOOS != "windows" {
			return nil, fmt.Errorf("cmd is only available on Windows clients")
		}
		return exec.Command("cmd", "/C", command), nil
	case proto.ShellSh, "":
		if runtime.GOOS == "windows" {
			return nil, fmt.Errorf("sh is not available on Windows clients")
		}
		return exec.Command("sh", "-c", command), nil
	default:
		return nil, fmt.Errorf("unknown shell %q", shell)
	}
}
