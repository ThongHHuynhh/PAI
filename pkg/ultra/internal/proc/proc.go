// Package proc starts commands so that stopping one stops everything it
// started: agent CLIs and check commands spawn shells and tools that must
// not outlive a cancel or a timeout.
package proc

import (
	"context"
	osexec "os/exec"
	"time"
)

// waitDelay bounds how long Wait waits for output pipes after the process
// tree is gone, so a stray descendant holding a pipe cannot block forever.
const waitDelay = 5 * time.Second

// Command is exec.CommandContext whose cancel stops the whole tree.
func Command(ctx context.Context, name string, args ...string) *osexec.Cmd {
	cmd := osexec.CommandContext(ctx, name, args...)
	group(cmd)
	cmd.Cancel = func() error { Kill(cmd); return nil }
	cmd.WaitDelay = waitDelay
	return cmd
}

// Kill stops cmd and its descendants. It is safe before Start and after exit.
func Kill(cmd *osexec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	killTree(cmd)
}
