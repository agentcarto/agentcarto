package summary

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
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
	// Releasing twice is not an error, so cleanup need not know what ran before it.
	if err := second.Release(); err != nil {
		t.Errorf("Release of a released lock: %v", err)
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

// What the file says has no bearing on who holds the lock: it is there for doctor to
// name a holder, and the holding is the kernel's. Whatever was left in it is written
// over.
func TestLockIgnoresWhatTheFileSays(t *testing.T) {
	for _, content := range []string{"", "not-a-pid", "   \n", "1 2026-08-24T12:00:00Z and more than a pid would ever need\n"} {
		p := filepath.Join(t.TempDir(), "summarize.lock")
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		l, err := TakeLock(p)
		if err != nil {
			t.Errorf("a lock file holding %q could not be taken: %v", content, err)
			continue
		}
		if pid, _, err := LockHolder(p); err != nil || pid != os.Getpid() {
			t.Errorf("after a file holding %q: LockHolder=%d, %v", content, pid, err)
		}
		_ = l.Release()
	}
}

// A process id in the file proves nothing: pids are reused, and one that belonged to
// a dead worker can come to name anything. Read as a holder, it would stop every
// worker for as long as that other process ran.
func TestALockNamingALiveProcessIsFreeUnlessHeld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "summarize.lock")
	if err := os.WriteFile(p, []byte(fmt.Sprintf("%d %s\n", os.Getpid(), time.Now().Format(time.RFC3339))), 0600); err != nil {
		t.Fatal(err)
	}
	if pid, _, err := LockHolder(p); err == nil {
		t.Errorf("LockHolder=%d for a lock nothing holds", pid)
	}
	l, err := TakeLock(p)
	if err != nil {
		t.Fatalf("a lock nothing holds could not be taken: %v", err)
	}
	_ = l.Release()
}

// TestHelperHoldLock is not a test: it is the process the next test kills. It holds the
// lock named in the environment until it is killed.
func TestHelperHoldLock(t *testing.T) {
	path := os.Getenv("AGENTCARTO_TEST_HOLD_LOCK")
	if path == "" {
		t.Skip("run by TestALockEndsWithTheProcessHoldingIt")
	}
	if _, err := TakeLock(path); err != nil {
		t.Fatal(err)
	}
	fmt.Println("held")
	time.Sleep(time.Minute)
}

// A worker can die in any way at all. Killed outright it runs none of its cleanup, and
// here nothing has collected it either, which leaves a zombie on Unix: its parent may
// be the agent's CLI after a resume, which knows nothing of it. None of that keeps the
// lock.
func TestALockEndsWithTheProcessHoldingIt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "summarize.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$")
	cmd.Env = append(os.Environ(), "AGENTCARTO_TEST_HOLD_LOCK="+p)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("the helper said %q, %v", line, err)
	}

	if pid, _, err := LockHolder(p); err != nil || pid != cmd.Process.Pid {
		t.Fatalf("LockHolder=%d, %v, want the helper (%d)", pid, err, cmd.Process.Pid)
	}
	if _, err := TakeLock(p); err == nil {
		t.Fatal("the lock was taken while the helper held it")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, _, err := LockHolder(p); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lock outlived the process holding it")
		}
	}
	l, err := TakeLock(p)
	if err != nil {
		t.Fatalf("the lock of a dead process could not be taken: %v", err)
	}
	_ = l.Release()
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

// A version from before the kernel held this lock goes by the pid in the file, and a
// worker it started that has exited stays a zombie it reads as running. It can still be
// open when the program is replaced under it, and it starts the new program's workers.
// A released lock must name nothing it would mistake for one.
func TestAReleasedLockNamesNoProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "summarize.lock")
	l, err := TakeLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	pid, _, err := readLock(p)
	if err != nil {
		t.Fatalf("the file no longer reads as a lock, which those versions refuse to take over: %v", err)
	}
	if pid > 0 {
		t.Errorf("the file names pid %d", pid)
	}
	if pid == 0 {
		t.Error("the file names pid 0, which those versions refuse to take over")
	}
}
