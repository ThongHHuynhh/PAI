//go:build !windows

package localembed

import (
	"os/exec"
	"syscall"
)

func processGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func kill(cmd *exec.Cmd, force bool) {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, signal)
}
