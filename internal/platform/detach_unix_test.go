//go:build !windows

package platform

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The TUI runs for hours and starts a worker whenever work is waiting. A child it
// never collects stays a zombie until it quits, one per worker.
func TestADetachedChildIsCollectedWhenItExits(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.Command("sh", "-c", "echo $$ > "+pidFile+".tmp; mv "+pidFile+".tmp "+pidFile)
	if err := startDetached(cmd); err != nil {
		t.Fatal(err)
	}

	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never ran")
		}
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child exited and is still there: signal 0 to %d gave %v", pid, err)
		}
	}
}
