//go:build windows

package summary

import (
	"os"
	"os/exec"
)

// terminate ends a process. Windows has no signal to ask one to stop with.
func terminate(p *os.Process) error { return p.Kill() }

// runInOwnGroup does nothing: reaching a process's whole tree on Windows takes a job
// object, and what is started directly is all that is stopped.
func runInOwnGroup(*exec.Cmd) {}

// killGroup does nothing, for the same reason.
func killGroup(*exec.Cmd) {}
