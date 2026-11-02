package summary

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Lock keeps one summarizing worker running at a time.
//
// Every scan and every show may start a worker, so without this a machine that
// opens the TUI twice would summarize the same sessions twice and pay twice.
// One worker draining the queue is enough: whatever the second one would have
// done is still in the queue for the first one to reach.
//
// The holding is done by the kernel, on an open file, and ends when the process does
// however that happens. A file whose existence was the lock had to be told apart from
// one a killed worker left behind, and everything it could be asked was wrong
// sometimes: the pid it named could have been reused, or be a zombie, and two workers
// finding it stale at once could both clear it.
//
// The file is never removed. A worker that opened it just before it was removed would
// lock a file nobody else can see, and run alongside the next one.
type Lock struct{ f *os.File }

// TakeLock locks the file at path, or reports who holds it. What it writes there is
// for reading only: who holds the lock, and since when, for LockHolder to report.
func TakeLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	locked, err := lockFile(f, true)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("summary: could not lock %s: %w", path, err)
	}
	if !locked {
		f.Close()
		holder, taken, err := readLock(path)
		if err != nil {
			// Being written right now, by the worker that got here first.
			return nil, fmt.Errorf("summary: another worker holds %s", path)
		}
		return nil, fmt.Errorf("summary: worker %d is already running (since %s)", holder, taken.Format(time.RFC3339))
	}
	if err := writeHolder(f, os.Getpid()); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// writeHolder replaces what the file says with a process and the time.
func writeHolder(f *os.File, pid int) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.WriteAt([]byte(fmt.Sprintf("%d %s\n", pid, time.Now().Format(time.RFC3339))), 0)
	return err
}

// noHolder is what a released lock's file names. Not 0 and not an empty file, which
// the versions described at Release refuse to take over.
const noHolder = -1

// Release gives the lock up. Closing the file is what does it.
//
// The file is left naming nobody for the sake of a TUI from before the kernel held this
// lock, which can be open when the program is replaced under it. It goes by the pid in
// the file, and it does not collect the workers it starts, so a pid left here would
// read as running, and it would start no other until it was closed.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	_ = writeHolder(f, noHolder)
	return f.Close()
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

// LockHolder reports the process holding the lock, if one is. It is what
// `doctor` uses to say whether a worker is running: the worker is detached and
// has no interface, so without this a run that has gone wrong shows up as
// nothing but an unexplained bill.
//
// It asks by trying for a shared lock, which a worker taking the lock at that moment
// can run into. That worker stops and the next scan starts another, which costs a line
// in the log. Asking is what the status does on every tick, and an exclusive lock
// would have two askers running into each other as well.
func LockHolder(path string) (pid int, taken time.Time, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer f.Close()
	free, err := lockFile(f, false)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("summary: could not check %s: %w", path, err)
	}
	if free {
		return 0, time.Time{}, errors.New("summary: no worker holds the lock")
	}
	return readLock(path)
}
