//go:build !windows

package summary

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pidIn waits for a script to write a process id to path.
func pidIn(ctx context.Context, path string) int {
	for deadline := time.Now().Add(10 * time.Second); ctx.Err() == nil && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
	}
	return 0
}

// gone reports whether a process has left within a few seconds. Its parent has just
// died, so it can be a moment before anything collects it.
func gone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
	}
	return false
}

// stoppedCall runs script as a call in a group of its own, and cancels it once the
// process the script names has started. It returns that process.
func stoppedCall(t *testing.T, script func(pidFile string) string) int {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pid := make(chan int, 1)
	go func() {
		p := pidIn(ctx, pidFile)
		pid <- p
		cancel()
	}()
	if _, e := runCLI(ctx, time.Minute, true, "", "sh", "-c", script(pidFile)); e == nil || !strings.Contains(e.Error(), "cancelled") {
		t.Errorf("run=%v want a cancellation", e)
	}
	p := <-pid
	if p == 0 {
		t.Fatal("the script never wrote its pid")
	}
	return p
}

// A launcher that does not pass a signal on would leave the program doing the work
// running, and the call being paid for, if only the launcher were asked to stop.
func TestAStoppedCallReachesWhatALauncherStarted(t *testing.T) {
	child := stoppedCall(t, func(pidFile string) string {
		return `sleep 30 & echo $! > ` + pidFile + `.tmp; mv ` + pidFile + `.tmp ` + pidFile + `; wait`
	})
	if !gone(child) {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Errorf("process %d outlived the call", child)
	}
}

// Asked to stop and ignoring it is not a way to keep the call running.
func TestAStoppedCallEndsWhatIgnoresBeingAsked(t *testing.T) {
	child := stoppedCall(t, func(pidFile string) string {
		return `(trap '' TERM; exec sleep 30) & echo $! > ` + pidFile + `.tmp; mv ` + pidFile + `.tmp ` + pidFile + `; wait`
	})
	if !gone(child) {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Errorf("process %d outlived the call", child)
	}
}

// A call in a group of its own that finishes normally is an ordinary call.
func TestACallInItsOwnGroupReturnsItsOutput(t *testing.T) {
	out, e := runCLI(context.Background(), time.Minute, true, "hello", "sh", "-c", "cat")
	if e != nil || string(out) != "hello" {
		t.Errorf("out=%q err=%v", out, e)
	}
}
