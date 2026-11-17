//go:build !windows

package lockfile

import (
	"errors"
	"syscall"
)

// alive is whether process pid is running. When unsure it says yes, so a
// lock is never taken from a live holder on a guess.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
