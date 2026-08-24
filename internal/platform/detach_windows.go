//go:build windows

package platform

import (
	"os/exec"
	"syscall"
)

// detachProcess starts the child in its own process group with no console.
//
// CREATE_NEW_PROCESS_GROUP is the counterpart of Setsid: it keeps a Ctrl-C in
// the console from reaching the worker. DETACHED_PROCESS withholds the parent's
// console, which would otherwise be inherited and print into whatever the user
// is looking at.
func detachProcess(cmd *exec.Cmd) {
	const detachedProcess = 0x00000008
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	}
}
