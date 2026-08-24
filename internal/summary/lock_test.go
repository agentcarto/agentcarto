package summary

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Every scan and every show may start a worker. Two of them summarizing the
// same queue would pay twice for the same sessions.
func TestLockAdmitsOne(t *testing.T) {
	p := filepath.Join(t.TempDir(), "summarize.lock")
	first, err := TakeLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TakeLock(p); err == nil {
		t.Fatal("a second worker took the lock")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Errorf("the refusal does not say who holds it: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	// Once released, the next worker gets it.
	second, err := TakeLock(p)
	if err != nil {
		t.Fatalf("the lock was not free after Release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	// Releasing twice is not an error: a worker that crashed after removing the
	// file must not turn its own cleanup into a failure.
	if err := second.Release(); err != nil {
		t.Errorf("Release on a missing lock: %v", err)
	}
}

// A worker killed mid-run leaves its lock behind. Without stale detection, all
// summarizing would stop until someone deleted the file by hand — with nothing
// on screen to say why.
func TestLockIsTakenOverFromADeadHolder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "summarize.lock")
	// A pid that is not running. Very large pids are not in use on any system
	// this runs on, and the check treats "no such process" as dead.
	if err := os.WriteFile(p, []byte(fmt.Sprintf("%d %s\n", 4_000_000, time.Now().Format(time.RFC3339))), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := TakeLock(p)
	if err != nil {
		t.Fatalf("a lock held by a dead process was not taken over: %v", err)
	}
	pid, _, err := readLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if pid != os.Getpid() {
		t.Errorf("the lock names pid %d, want this process (%d)", pid, os.Getpid())
	}
	_ = l.Release()
}

// A lock file that cannot be read is left alone. It may be another worker
// writing it right now, and stealing it would put two workers on one queue.
func TestLockLeavesAnUnreadableFileAlone(t *testing.T) {
	for _, content := range []string{"", "not-a-pid", "   \n"} {
		p := filepath.Join(t.TempDir(), "summarize.lock")
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := TakeLock(p); err == nil {
			t.Errorf("a lock holding %q was taken", content)
		}
	}
}

// The lock names its holder and when it was taken, so `doctor` can say what is
// running rather than only that something is.
func TestLockRecordsHolderAndTime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "summarize.lock")
	before := time.Now().Add(-time.Second)
	l, err := TakeLock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	pid, taken, err := readLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if pid != os.Getpid() {
		t.Errorf("pid=%d want %d", pid, os.Getpid())
	}
	if taken.Before(before) {
		t.Errorf("taken=%s, which is before the lock was taken", taken)
	}
	// Windows has no Unix permission bits; os.Chmod there sets only the
	// read-only flag, so the mode a file reports says nothing about who can read
	// it. Access is controlled by the ACL the directory carries instead.
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("lock file is %o, want no group or other access", perm)
		}
	}
}

// The directory is made if it is not there: a first run has no cache directory
// yet, and failing then would mean summarizing never starts.
func TestTakeLockCreatesItsDirectory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "deeper", "summarize.lock")
	l, err := TakeLock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("the lock file was not created: %v", err)
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("this process is reported as not running")
	}
	for _, pid := range []int{0, -1, 4_000_000} {
		if processAlive(pid) {
			t.Errorf("pid %d is reported as running", pid)
		}
	}
}

// doctor is the only place that says whether a worker is running, since the
// worker itself is detached and has no interface.
func TestLockHolder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "summarize.lock")
	if _, _, err := LockHolder(p); err == nil {
		t.Error("LockHolder reported a holder for a lock that does not exist")
	}
	l, err := TakeLock(p)
	if err != nil {
		t.Fatal(err)
	}
	pid, taken, err := LockHolder(p)
	if err != nil {
		t.Fatalf("LockHolder on a held lock: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("LockHolder=%d want %d", pid, os.Getpid())
	}
	if taken.IsZero() {
		t.Error("LockHolder returned no time")
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LockHolder(p); err == nil {
		t.Error("LockHolder reported a holder after Release")
	}
	// A lock left by a process that is gone is not a running worker.
	if err := os.WriteFile(p, []byte(fmt.Sprintf("%d %s\n", 4_000_000, time.Now().Format(time.RFC3339))), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LockHolder(p); err == nil {
		t.Error("a lock held by a dead process was reported as a running worker")
	}
}
