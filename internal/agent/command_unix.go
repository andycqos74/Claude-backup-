//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the command in its own process group so the whole group
// can be signalled at once.
func setProcGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree kills the command's entire process group (negative PID),
// so a shell and any children it forked all die together.
func killProcessTree(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
}
