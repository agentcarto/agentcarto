package platform

import (
	"fmt"
	"os"
	"runtime"

	"github.com/agentcarto/core/domain"
)

// ShellCommand builds the launch command that opens the user's interactive shell
// in dir, for handing off after the TUI exits (same path as resume/fork). The
// directory is validated here so a failure surfaces as an in-TUI flash instead
// of a Handoff error after the TUI is already gone.
func ShellCommand(dir string) (domain.Command, error) {
	if dir == "" {
		return domain.Command{}, fmt.Errorf("session has no working directory")
	}
	if fi, e := os.Stat(dir); e != nil || !fi.IsDir() {
		return domain.Command{}, fmt.Errorf("directory unavailable: %s", dir)
	}
	env, fallback := "SHELL", "/bin/sh"
	if runtime.GOOS == "windows" {
		env, fallback = "COMSPEC", "cmd"
	}
	sh := os.Getenv(env)
	if sh == "" {
		sh = fallback
	}
	return domain.Command{Executable: sh, WorkingDirectory: dir}, nil
}
