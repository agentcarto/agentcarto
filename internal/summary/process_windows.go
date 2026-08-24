//go:build windows

package summary

import (
	"errors"

	"golang.org/x/sys/windows"
)

// processAlive reports whether a process id belongs to something running.
//
// Windows has no signals, and os.FindProcess there succeeds for any number at
// all — using it the way Unix does would report every stale lock as held, and
// summarizing would stop for good after one worker was killed. Opening the
// process and asking whether it has exited is the question that has an answer.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied means it exists and belongs to someone else, which is
		// still a running process; anything else means it is gone.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return true // the handle opened, so something is there
	}
	const stillActive = 259 // STILL_ACTIVE
	return code == stillActive
}
