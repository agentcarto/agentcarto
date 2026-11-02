//go:build windows

package summary

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile tries to lock a file without waiting, and reports whether it did. The lock
// belongs to this handle and ends when it is closed or the process ends.
//
// It covers one byte far past the end of the file. Windows locks are enforced on reads,
// and the file's contents have to stay readable to say who holds it.
func lockFile(f *os.File, exclusive bool) (bool, error) {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{OffsetHigh: 1})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_IO_PENDING):
		return false, nil
	default:
		return false, err
	}
}
