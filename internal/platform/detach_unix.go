//go:build !windows

package platform

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the child in a session of its own.
//
// Without Setsid the child stays in the parent's process group, and a Ctrl-C in
// the terminal — or a shell cleaning up its job — signals the whole group. The
// worker would then be killed by the very act of quitting the program that
// started it, which is what detaching is meant to prevent.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
