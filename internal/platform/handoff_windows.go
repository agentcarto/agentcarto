//go:build windows

package platform

import (
	"errors"
	"os"
	"os/exec"
)

// execProcess runs the launch command as a foreground child process and then exits with its
// exit code. Windows has no Exec-style process replacement, so this approximates it.
func execProcess(path string, argv, env []string) error {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	e := cmd.Run()
	var ee *exec.ExitError
	if errors.As(e, &ee) {
		os.Exit(ee.ExitCode())
	}
	if e != nil {
		return e
	}
	os.Exit(0)
	return nil
}
