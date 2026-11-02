//go:build !windows

package summary

import (
	"errors"
	"os"
	"syscall"
)

// lockFile tries to lock a file without waiting, and reports whether it did. The lock
// belongs to this open file, is not passed to a child process (Go opens files
// close-on-exec), and ends when the file is closed or the process ends.
func lockFile(f *os.File, exclusive bool) (bool, error) {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		case errors.Is(err, syscall.EINTR):
			continue
		default:
			return false, err
		}
	}
}
