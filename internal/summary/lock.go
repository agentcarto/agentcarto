package summary

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Lock keeps one summarizing worker running at a time.
//
// Every scan and every show may start a worker, so without this a machine that
// opens the TUI twice would summarize the same sessions twice and pay twice.
// One worker draining the queue is enough: whatever the second one would have
// done is still in the queue for the first one to reach.
type Lock struct{ path string }

// TakeLock creates the lock file, or reports who holds it. The file carries the
// holder's process id and the time it was taken, so a lock left behind by a
// killed worker can be told apart from one that is genuinely in use — a stale
// lock file would otherwise stop all summarizing until someone deleted it by
// hand, with nothing on screen to say why.
func TakeLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		holder, taken, readErr := readLock(path)
		switch {
		case readErr != nil, holder == 0:
			// Unreadable: either it is being written right now, or it was left
			// by a version that wrote something else. Neither is safe to steal.
			return nil, fmt.Errorf("summary: another worker holds %s", path)
		case processAlive(holder):
			return nil, fmt.Errorf("summary: worker %d is already running (since %s)", holder, taken.Format(time.RFC3339))
		}
		// The holder is gone. Remove its lock and try once more; if that race is
		// lost, the winner is a live worker and there is nothing to do here.
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("summary: could not clear the lock of dead worker %d: %w", holder, err)
		}
		if f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600); err != nil {
			return nil, fmt.Errorf("summary: another worker took the lock: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().Format(time.RFC3339)); err != nil {
		os.Remove(path)
		return nil, err
	}
	return &Lock{path: path}, nil
}

// Release removes the lock file.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	err := os.Remove(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func readLock(path string) (pid int, taken time.Time, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	parts := strings.Fields(string(b))
	if len(parts) == 0 {
		return 0, time.Time{}, errors.New("empty lock file")
	}
	if pid, err = strconv.Atoi(parts[0]); err != nil {
		return 0, time.Time{}, err
	}
	if len(parts) > 1 {
		taken, _ = time.Parse(time.RFC3339, parts[1])
	}
	return pid, taken, nil
}

// processAlive reports whether a process id belongs to something running.
// Signal 0 performs the existence and permission checks without delivering
// anything. A process owned by someone else answers EPERM, which still means it
// is there.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}
