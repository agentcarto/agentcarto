//go:build !windows

package summary

import (
	"errors"
	"os"
	"slices"
	"syscall"

	"github.com/shirou/gopsutil/v4/process"
)

// processAlive reports whether a process id belongs to something running.
// Signal 0 performs the existence and permission checks without delivering
// anything. A process owned by someone else answers EPERM, which still means it
// is there.
//
// A zombie answers too, so it has to be asked about separately. platform.Detach
// collects the workers it starts, but not after a resume: the process it was in has
// become the agent's CLI, which is left the parent of a worker it knows nothing
// about. A worker dying there would keep its lock until that session ended, and no
// other would start.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err != nil && !errors.Is(err, os.ErrPermission) {
		return false
	}
	return !isZombie(pid)
}

// isZombie reports whether a process has exited and is waiting to be collected. Not
// being able to tell reads as no: taking the lock of a worker that is alive would run
// two of them, and pay for the same calls twice.
func isZombie(pid int) bool {
	// Not process.NewProcess, which checks over again what processAlive has just
	// asked. This runs on every look at the status while a worker is running.
	status, err := (&process.Process{Pid: int32(pid)}).Status()
	return err == nil && slices.Contains(status, process.Zombie)
}

// terminate asks a process to stop.
func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
