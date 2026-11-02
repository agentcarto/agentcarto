//go:build !windows

package summary

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// terminate asks a process to stop.
func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

// runInOwnGroup starts cmd as the leader of a process group, and has stopping it ask
// the whole group.
func runInOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
}

// killGroup kills what is left of the group runInOwnGroup started. A group with nothing
// left in it is the usual case, and not an error.
func killGroup(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
