//go:build !windows

package proc

import (
	osexec "os/exec"
	"syscall"
)

// group starts cmd in its own process group, so the group can be killed.
func group(cmd *osexec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killTree(cmd *osexec.Cmd) {
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGKILL) // the whole group
	_ = cmd.Process.Kill()
}
