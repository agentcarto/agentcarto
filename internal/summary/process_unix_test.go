//go:build !windows

package summary

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A worker can die with nobody collecting it: after a resume its parent is the
// agent's CLI, which did not start it. A zombie is not a worker, and its lock must
// not stop every other one from starting.
func TestALockHeldByAZombieIsFree(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Not collected until the test is over, which is what keeps it a zombie.
	defer func() { _ = cmd.Wait() }()

	p := filepath.Join(t.TempDir(), "summarize.lock")
	if err := os.WriteFile(p, []byte(fmt.Sprintf("%d %s\n", cmd.Process.Pid, time.Now().Format(time.RFC3339))), 0600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		pid, _, err := LockHolder(p)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d has exited and still reads as holding the lock", pid)
		}
	}
	l, err := TakeLock(p)
	if err != nil {
		t.Fatalf("the lock of a zombie could not be taken: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}
