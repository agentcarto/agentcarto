package platform

import (
	"github.com/agentcarto/core/domain"
	"os"
	"os/exec"
)

// Handoff transfers control to the resume/fork launch command after the TUI has exited.
// Launching an interactive agent while the TUI still holds the alternate screen and raw
// mode would corrupt the terminal handover, so the launch happens here, only once the TUI
// has fully restored the terminal. On Unix the current process is replaced (chdir + execvp;
// does not return on success); on Windows, where Exec-style replacement is unavailable, the
// command runs as a foreground child process.
func Handoff(c domain.Command) error {
	if c.WorkingDirectory != "" {
		if e := os.Chdir(c.WorkingDirectory); e != nil {
			return e
		}
	}
	path, e := exec.LookPath(c.Executable)
	if e != nil {
		return e
	}
	return execProcess(path, append([]string{c.Executable}, c.Args...), os.Environ())
}
