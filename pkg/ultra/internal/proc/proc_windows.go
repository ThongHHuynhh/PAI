package proc

import (
	"context"
	osexec "os/exec"
	"strconv"
	"time"
)

// taskkillTime bounds taskkill itself.
const taskkillTime = 10 * time.Second

func group(*osexec.Cmd) {}

// killTree ends the process and every descendant with taskkill /T; if that
// fails or hangs, the process itself is still killed.
func killTree(cmd *osexec.Cmd) {
	ctx, cancel := context.WithTimeout(context.Background(), taskkillTime)
	defer cancel()
	_ = osexec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	_ = cmd.Process.Kill()
}
