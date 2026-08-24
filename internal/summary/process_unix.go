//go:build !windows

package summary

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether a process id belongs to something running.
// Signal 0 performs the existence and permission checks without delivering
// anything. A process owned by someone else answers EPERM, which still means it
// is there.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}
