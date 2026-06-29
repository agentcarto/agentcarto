//go:build !windows

package platform

import "syscall"

// execProcess replaces the current process with the executable at argv[0].
// It does not return on success.
func execProcess(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
