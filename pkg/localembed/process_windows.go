package localembed

import (
	"os/exec"
	"strconv"
	"syscall"
)

func processGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000200}
}
func kill(cmd *exec.Cmd, _ bool) {
	cleanup := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	cleanup.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cleanup.Run()
}
