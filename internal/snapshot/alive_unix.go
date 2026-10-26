//go:build !windows

package snapshot

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid may still run. Only a process known to
// be gone counts as dead.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return !errors.Is(err, syscall.ESRCH)
}
