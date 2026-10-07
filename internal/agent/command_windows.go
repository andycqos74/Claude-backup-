package agent

import (
	"os/exec"
	"strconv"
)

// setProcGroup is a no-op on Windows; the tree is torn down with taskkill.
func setProcGroup(*exec.Cmd) {}

// killProcessTree terminates the command and any child processes. taskkill
// /T walks the child tree (which Process.Kill would leave running, e.g. a
// program launched by the shell); Kill is a fallback if taskkill is missing.
func killProcessTree(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(c.Process.Pid)).Run()
	c.Process.Kill()
}
